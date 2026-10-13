package gateway

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/engine"
)

// The queue's rules (decision 022), with a fake clock.
func TestOutQueuePolicy(t *testing.T) {
	var now time.Time
	clock := func() time.Time { return now }
	const patience = time.Second
	var peak atomic.Int64
	q := newOutQueue(4, 10, &peak)
	push := func(n int) (string, bool) {
		for i := range n {
			if ok, why := q.push(engine.Event{Seq: engine.Seq(i)}, patience, clock); !ok {
				return why, false
			}
		}
		return "", true
	}

	// A burst far above soft is accepted...
	if why, ok := push(9); !ok {
		t.Fatalf("burst refused: %s", why)
	}
	if peak.Load() != 9 {
		t.Fatalf("peak %d, want 9", peak.Load())
	}
	// ...and draining under soft within the patience forgives it.
	now = now.Add(900 * time.Millisecond)
	if got := q.pop(nil, 6); len(got) != 6 || got[0].Seq != 0 || got[5].Seq != 5 {
		t.Fatalf("popped %v", got)
	}
	now = now.Add(5 * time.Second)
	if why, ok := push(1); !ok {
		t.Fatalf("a client that caught up was refused: %s", why)
	}

	// Staying above soft for longer than the patience is slow.
	q2 := newOutQueue(4, 100, nil)
	now = time.Time{}.Add(time.Hour)
	for range 6 { // 2 above soft
		if ok, _ := q2.push(engine.Event{}, patience, clock); !ok {
			t.Fatal("refused before the patience ran out")
		}
	}
	now = now.Add(patience + time.Millisecond)
	q2.pop(nil, 1) // drains a little: 5 left, still above soft
	if ok, why := q2.push(engine.Event{}, patience, clock); ok || !strings.Contains(why, "longer than 1s") {
		t.Fatalf("over soft for longer than the patience: ok=%v %q", ok, why)
	}

	// The hard limit refuses at once, whatever the time.
	q3 := newOutQueue(2, 3, nil)
	for range 3 {
		q3.push(engine.Event{}, 0, clock)
	}
	if ok, why := q3.push(engine.Event{}, 0, clock); ok || !strings.Contains(why, "hard limit") {
		t.Fatalf("at the hard limit: ok=%v %q", ok, why)
	}
	// Patience 0 (no WriteTimeout): only the hard limit applies.
	q4 := newOutQueue(1, 1000, nil)
	q4.push(engine.Event{}, 0, clock)
	q4.push(engine.Event{}, 0, clock)
	now = now.Add(time.Hour)
	if ok, why := q4.push(engine.Event{}, 0, clock); !ok {
		t.Fatalf("patience 0 timed out: %s", why)
	}
}

func TestOutQueueOrderAndReuse(t *testing.T) {
	q := newOutQueue(4, 1<<20, nil)
	clock := time.Now
	var got []engine.Event
	next := engine.Seq(0)
	for round := range 50 {
		for range 37 + round {
			next++
			q.push(engine.Event{Seq: next}, 0, clock)
		}
		got = q.pop(got, 1<<20)
	}
	for i, ev := range got {
		if ev.Seq != engine.Seq(i+1) {
			t.Fatalf("event %d has seq %d: out of order", i, ev.Seq)
		}
	}
	if q.len() != 0 || cap(q.buf) > 4*4 {
		t.Fatalf("after draining: len %d, cap %d (a burst's memory should be given back)", q.len(), cap(q.buf))
	}
}
