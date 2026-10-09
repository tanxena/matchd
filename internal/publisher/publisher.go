// Package publisher fans the engine's events out to the clients they belong
// to (decision 009).
//
// Milestone 3 version: client routing and the slow-consumer policy.
// Milestone 4 adds holding events until their command is durable; milestone 6
// adds market-data subscribers.
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
	"github.com/tanxena/matchd/internal/engine"
)

// Subscriber is a client session as the publisher sees it.
type Subscriber interface {
	// Deliver offers ev to the session's outbound queue. It must not block:
	// it returns false at once if the queue is full.
	Deliver(ev engine.Event) bool
	// SlowConsumer tells the session it fell too far behind. The session
	// must disconnect. Called at most once per registration, from the
	// publisher goroutine, after the session has been unregistered.
	SlowConsumer()
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

	// Counters, read by tests now and by /metrics in milestone 7. Written
	// only by the Run goroutine; atomics so other goroutines can read them.
	delivered     atomic.Uint64
	dropped       atomic.Uint64 // events for clients not connected
	slowConsumers atomic.Uint64
}

// New returns a publisher with no clients.
func New(log *slog.Logger) *Publisher {
	p := &Publisher{log: log}
	empty := registry{}
	p.clients.Store(&empty)
	return p
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

// Run is the publisher goroutine's body. It returns when events is closed
// (after the engine has stopped).
func (p *Publisher) Run(events <-chan engine.Event) {
	for ev := range events {
		p.route(ev)
	}
}

func (p *Publisher) route(ev engine.Event) {
	if ev.Client == 0 {
		// Book deltas (market data, milestone 6) and startup admin
		// commands have no client.
		return
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
}

// Stats returns the current counters. Safe from any goroutine.
func (p *Publisher) Stats() Stats {
	return Stats{
		Delivered:     p.delivered.Load(),
		Dropped:       p.dropped.Load(),
		SlowConsumers: p.slowConsumers.Load(),
	}
}
