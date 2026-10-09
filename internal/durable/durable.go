// Package durable tracks which commands are safely on disk (decision 012).
//
// The sequencer side (the WAL appender under fsync=always and never, or the
// group-commit syncer under group) advances it; the publisher reads it and
// releases an event only when the event's command is covered. A failed fsync
// marks it failed forever: nothing more is ever released.
package durable

import (
	"sync"
	"sync/atomic"
)

// Seq is the highest command sequence number known to be durable.
type Seq struct {
	seq atomic.Uint64

	// notify carries "seq advanced" pings to the publisher. Capacity 1 and
	// non-blocking sends: one pending ping is enough, because the publisher
	// re-reads seq when it wakes, so extra pings would add nothing.
	notify chan struct{}

	// failed is closed exactly once, by Fail. errMu guards err.
	failed   chan struct{}
	failOnce sync.Once
	errMu    sync.Mutex
	err      error
}

// New returns a tracker that starts at initial (the last seq already on disk
// at startup).
func New(initial uint64) *Seq {
	d := &Seq{notify: make(chan struct{}, 1), failed: make(chan struct{})}
	d.seq.Store(initial)
	return d
}

// Load returns the highest durable seq.
func (d *Seq) Load() uint64 { return d.seq.Load() }

// Advance raises the durable seq to n (never lowers it) and wakes the
// publisher. It does nothing after Fail: once durability is in doubt,
// nothing more may be released.
func (d *Seq) Advance(n uint64) {
	select {
	case <-d.failed:
		return
	default:
	}
	for {
		cur := d.seq.Load()
		if n <= cur {
			return
		}
		if d.seq.CompareAndSwap(cur, n) {
			break
		}
	}
	select {
	case d.notify <- struct{}{}:
	default: // a ping is already pending
	}
}

// Notify returns the channel that receives a value after Advance.
func (d *Seq) Notify() <-chan struct{} { return d.notify }

// Fail records that an fsync (or write) failed. Commands not yet durable may
// or may not be on disk, and must never be acknowledged.
func (d *Seq) Fail(err error) {
	d.failOnce.Do(func() {
		d.errMu.Lock()
		d.err = err
		d.errMu.Unlock()
		close(d.failed)
	})
}

// Failed returns a channel closed when Fail is called.
func (d *Seq) Failed() <-chan struct{} { return d.failed }

// Err returns the error passed to Fail, or nil.
func (d *Seq) Err() error {
	d.errMu.Lock()
	defer d.errMu.Unlock()
	return d.err
}
