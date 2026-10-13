package gateway

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanxena/matchd/internal/engine"
)

// outQueue is a session's outbound event queue (decision 022). The
// publisher pushes without ever blocking; the session's writer pops.
//
// It has two limits:
//   - soft (gateway Config.OutQueue): a client whose queue stays above it
//     for longer than its patience (Config.WriteTimeout) is a slow
//     consumer. A burst, such as the backlog released at once after a
//     durability stall, may take the queue far above soft; that is fine as
//     long as the client drains back under soft within the patience.
//   - hard (soft + publisher.MaxPending): the memory bound. The publisher
//     holds at most MaxPending events, so no release burst alone can reach
//     it; only a client that also stops draining can.
type outQueue struct {
	mu        sync.Mutex // guards buf, head and overSince: the publisher pushes, the writer pops
	buf       []engine.Event
	head      int       // buf[head:] is queued
	overSince time.Time // when the queue went above soft; zero while at or under it

	soft, hard int
	peak       *atomic.Int64 // the gateway's high-water mark across sessions (nil: not tracked)
	// ready has room for one signal: push leaves one if there is none, so
	// a waiting writer wakes, and a busy writer finds it on its next wait.
	ready chan struct{}
}

func newOutQueue(soft, hard int, peak *atomic.Int64) *outQueue {
	return &outQueue{buf: make([]engine.Event, 0, soft), soft: soft, hard: hard, peak: peak, ready: make(chan struct{}, 1)}
}

// push queues ev, or reports why the client is a slow consumer: the queue
// is at its hard limit, or has been above soft for longer than patience
// (0 = no time limit). now is read only when the queue is above soft.
func (q *outQueue) push(ev engine.Event, patience time.Duration, now func() time.Time) (ok bool, why string) {
	q.mu.Lock()
	n := len(q.buf) - q.head
	switch {
	case n >= q.hard:
		q.mu.Unlock()
		return false, fmt.Sprintf("outbound queue reached its hard limit (%d events)", q.hard)
	case n >= q.soft:
		t := now()
		if q.overSince.IsZero() {
			q.overSince = t
		} else if patience > 0 && t.Sub(q.overSince) > patience {
			q.mu.Unlock()
			return false, fmt.Sprintf("outbound queue above %d events for longer than %v", q.soft, patience)
		}
	}
	q.buf = append(q.buf, ev)
	q.mu.Unlock()
	if q.peak != nil {
		for p := q.peak.Load(); int64(n+1) > p && !q.peak.CompareAndSwap(p, int64(n+1)); p = q.peak.Load() {
		}
	}
	select {
	case q.ready <- struct{}{}:
	default:
	}
	return true, ""
}

// pop moves up to max queued events into dst (in order) and returns it.
// Draining to soft or under ends an over-soft period.
func (q *outQueue) pop(dst []engine.Event, max int) []engine.Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	k := min(len(q.buf)-q.head, max)
	dst = append(dst, q.buf[q.head:q.head+k]...)
	q.head += k
	switch {
	case q.head == len(q.buf): // empty: reuse from the start
		q.buf, q.head = q.buf[:0], 0
		if cap(q.buf) > 4*q.soft { // after a burst: give the memory back
			q.buf = make([]engine.Event, 0, q.soft)
		}
	case q.head > len(q.buf)/2: // mostly consumed: slide down, so a burst's memory is reused
		n := copy(q.buf, q.buf[q.head:])
		q.buf, q.head = q.buf[:n], 0
	}
	if len(q.buf)-q.head <= q.soft {
		q.overSince = time.Time{}
	}
	return dst
}

// len is the number of queued events.
func (q *outQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf) - q.head
}
