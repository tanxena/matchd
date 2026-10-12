// Package publisher fans the engine's events out to the clients they belong
// to (decision 009).
//
// It does two jobs:
//
//   - Durability gating (milestone 4, decision 012): an event is released
//     only once the command that caused it is on disk. Events wait in a
//     FIFO queue until the durable seq covers them.
//
//   - Routing (milestone 3, decision 009): each released event goes to its
//     client's session, never blocking on a slow one.
//
//   - Market data (milestone 6, decision 016, marketdata.go): per-instrument
//     snapshots, deltas and trades to subscribers, and resends.
//
// The rule that shapes this package: the publisher must never block. The
// engine sends events to it on a channel; if the publisher waited on one
// slow client, every client's matching would stall behind it (decision 005).
// So delivery to a client is a non-blocking offer into that client's bounded
// queue, and a client whose queue is full is disconnected.
package publisher

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
)

// FillRecorder is told about every event the publisher releases (the fill
// log, decision 014). Released runs on the publisher goroutine and must not
// block for long; Flush is called once after each batch of releases.
type FillRecorder interface {
	Released(ev *engine.Event)
	Flush() error
}

// MaxPending bounds the events held for durability. When it is reached the
// publisher stops taking events from the engine until fsync catches up,
// which back-pressures the engine (group commit only; under fsync=always
// events arrive already durable).
const MaxPending = 1 << 16

// Subscriber is a client session as the publisher sees it.
type Subscriber interface {
	// Deliver offers ev to the session's outbound queue. It must not block:
	// it returns false at once if the queue is full.
	Deliver(ev engine.Event) bool
	// DeliverSnapshot hands the session a market-data snapshot to stream to
	// its client, in order with Deliver: after every event delivered before
	// it, before every event delivered after it. It must not block, and
	// returns false if the session can't take it. The session streams the
	// levels itself, with backpressure, instead of queueing one event per
	// level, so a book deeper than the queue still fits (decision 016).
	DeliverSnapshot(s *Snapshot) bool
	// SlowConsumer tells the session it fell too far behind. The session
	// must disconnect. Called from the publisher goroutine; it may be called
	// more than once (from client routing and from market-data fan-out), so
	// it must be idempotent.
	SlowConsumer()
	// Closed reports whether the session has gone away, so market-data
	// subscriptions can be dropped.
	Closed() bool
}

type registry = map[book.ClientID]Subscriber

// Publisher routes events to registered client sessions.
type Publisher struct {
	log *slog.Logger

	// clients is read on every event by the Run goroutine and replaced
	// (never modified in place) by Register/Unregister, which run on gateway
	// goroutines. Copy-on-write behind an atomic pointer: lookups take no
	// lock; logins and logouts, which are rare, pay for copying the map.
	clients atomic.Pointer[registry]
	// mu serialises writers (Register/Unregister) so two concurrent
	// copy-and-swap updates can't lose each other's change. Readers never
	// take it.
	mu sync.Mutex

	// durable is the gate: nil means release at once (unit tests).
	durable *durable.Seq
	// fills records released fills (nil = none). Publisher goroutine only.
	fills      FillRecorder
	fillErrors atomic.Uint64
	// pending holds events not yet durable, oldest first, from index head.
	// Event seqs never decrease, so the releasable ones are always a prefix.
	// Only the Run goroutine touches it.
	pending []engine.Event
	head    int

	// Counters, read by tests now and by /metrics in milestone 7. Written
	// only by the Run goroutine; atomics so other goroutines can read them.
	delivered     atomic.Uint64
	dropped       atomic.Uint64 // events for clients not connected
	slowConsumers atomic.Uint64
	held          atomic.Int64  // events currently waiting for durability
	withheld      atomic.Uint64 // events never released because durability failed
	subscriptions atomic.Uint64 // market-data subscriptions served
	snapshotsSent atomic.Uint64
	resends       atomic.Uint64 // Resend requests received
	resendMisses  atomic.Uint64 // ... answered ResendUnavailable

	// Market data. md is owned by the Run goroutine. requests carries
	// Subscribe/Resend from gateway goroutines to Run; stopped is closed
	// when Run returns, so a request never blocks after shutdown.
	md       map[engine.InstrumentID]*mdBook
	ringSize int
	requests chan request
	stopped  chan struct{}
}

// New returns a publisher with no clients. Events are released only once
// d covers their seq; d == nil releases them immediately. Every released
// event is also given to fills, if non-nil.
func New(log *slog.Logger, d *durable.Seq, fills FillRecorder) *Publisher {
	p := &Publisher{
		log: log, durable: d, fills: fills, pending: make([]engine.Event, 0, 1024),
		md: map[engine.InstrumentID]*mdBook{}, ringSize: DefaultRingSize,
		requests: make(chan request, 256), stopped: make(chan struct{}),
	}
	empty := registry{}
	p.clients.Store(&empty)
	return p
}

// SetRingSize sets the per-instrument resend window. Call before Run.
func (p *Publisher) SetRingSize(n int) {
	if n > 0 {
		p.ringSize = n
	}
}

// Register makes s the session for client id. It returns false, changing
// nothing, if another session is already registered for id: one connection
// per client at a time.
func (p *Publisher) Register(id book.ClientID, s Subscriber) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := *p.clients.Load()
	if _, taken := old[id]; taken {
		return false
	}
	next := make(registry, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[id] = s
	p.clients.Store(&next)
	return true
}

// Unregister removes s as client id's session. It does nothing if a
// different session is registered for id, so a late unregister from an old
// connection can never remove its replacement.
func (p *Publisher) Unregister(id book.ClientID, s Subscriber) {
	p.mu.Lock()
	defer p.mu.Unlock()
	old := *p.clients.Load()
	if old[id] != s {
		return
	}
	next := make(registry, len(old))
	for k, v := range old {
		if k != id {
			next[k] = v
		}
	}
	p.clients.Store(&next)
}

// Run is the publisher goroutine's body. It returns once events is closed
// and every held event has been released, or once durability has failed
// (after which nothing is released, ever).
func (p *Publisher) Run(events <-chan engine.Event) {
	defer close(p.stopped)
	var notify <-chan struct{}
	var failed <-chan struct{}
	if p.durable != nil {
		notify, failed = p.durable.Notify(), p.durable.Failed()
	}
	for {
		if events == nil && p.head == len(p.pending) {
			return // input finished and nothing left to release
		}
		in := events
		if len(p.pending)-p.head >= MaxPending {
			in = nil // full: wait for durability before taking more
		}
		select {
		case ev, ok := <-in:
			if !ok {
				events = nil
				continue
			}
			p.pending = append(p.pending, ev)
			p.held.Add(1)
			p.release()
		case <-notify:
			p.release()
		case r := <-p.requests:
			p.handle(r)
		case <-failed:
			p.withhold(events)
			return
		}
	}
}

// release routes every held event whose command is now durable.
func (p *Publisher) release() {
	limit := ^uint64(0)
	if p.durable != nil {
		limit = p.durable.Load()
	}
	released := false
	for p.head < len(p.pending) && uint64(p.pending[p.head].Seq) <= limit {
		ev := &p.pending[p.head]
		if p.fills != nil {
			p.fills.Released(ev)
		}
		p.route(*ev)
		p.head++
		p.held.Add(-1)
		released = true
	}
	if released && p.fills != nil {
		// The fill log is an audit trail, not part of the durability
		// contract: a write failure is logged and counted, never fatal.
		if err := p.fills.Flush(); err != nil && p.fillErrors.Add(1) == 1 {
			p.log.Error("fill log write failed; replay-check will see a gap", "err", err)
		}
	}
	if p.head == len(p.pending) {
		p.pending, p.head = p.pending[:0], 0 // empty: reuse from the start
	} else if p.head > len(p.pending)/2 {
		n := copy(p.pending, p.pending[p.head:]) // compact so the slice doesn't creep
		p.pending, p.head = p.pending[:n], 0
	}
}

// withhold handles a durability failure: whatever is held is dropped
// unreleased, and the rest of the engine's output is drained (so the engine
// isn't left blocked on a send) and dropped too.
func (p *Publisher) withhold(events <-chan engine.Event) {
	n := uint64(len(p.pending) - p.head)
	p.pending, p.head = p.pending[:0], 0
	p.held.Store(0)
	if events != nil {
		for range events {
			n++
		}
	}
	p.withheld.Add(n)
	p.log.Error("durability failed: events withheld, none will be released",
		"withheld", n, "err", p.durable.Err())
}

func (p *Publisher) route(ev engine.Event) {
	switch ev.Kind {
	case engine.EvBookDelta, engine.EvTrade:
		p.marketData(&ev)
		return
	case engine.EvInstrumentAdded:
		if p.md[ev.Instrument] == nil {
			p.md[ev.Instrument] = newMDBook(0)
		}
	}
	if ev.Client == 0 {
		return // startup and admin commands have no client to answer
	}
	s := (*p.clients.Load())[ev.Client]
	if s == nil {
		// The client isn't connected. Its orders stay on the book, but v1
		// has no order-status query, so events missed while disconnected
		// are lost to that client (a known limitation, decision 009).
		p.dropped.Add(1)
		return
	}
	if s.Deliver(ev) {
		p.delivered.Add(1)
		return
	}
	p.Unregister(ev.Client, s)
	p.slowConsumers.Add(1)
	p.log.Warn("slow consumer disconnected", "client", ev.Client, "seq", ev.Seq)
	s.SlowConsumer()
}

// Stats is a snapshot of the publisher's counters.
type Stats struct {
	Delivered     uint64
	Dropped       uint64
	SlowConsumers uint64
	Held          int64  // waiting for durability right now
	Withheld      uint64 // never released because durability failed
	Subscriptions uint64
	SnapshotsSent uint64
	Resends       uint64
	ResendMisses  uint64
}

// Stats returns the current counters. Safe from any goroutine.
func (p *Publisher) Stats() Stats {
	return Stats{
		Delivered:     p.delivered.Load(),
		Dropped:       p.dropped.Load(),
		SlowConsumers: p.slowConsumers.Load(),
		Held:          p.held.Load(),
		Withheld:      p.withheld.Load(),
		Subscriptions: p.subscriptions.Load(),
		SnapshotsSent: p.snapshotsSent.Load(),
		Resends:       p.resends.Load(),
		ResendMisses:  p.resendMisses.Load(),
	}
}
