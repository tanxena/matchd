package publisher

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeSub is a Subscriber with a bounded queue, like a real session.
type fakeSub struct {
	q    chan engine.Event
	slow chan struct{} // closed by SlowConsumer
	once sync.Once
}

func newFakeSub(capacity int) *fakeSub {
	return &fakeSub{q: make(chan engine.Event, capacity), slow: make(chan struct{})}
}

func (f *fakeSub) Deliver(ev engine.Event) bool {
	select {
	case f.q <- ev:
		return true
	default:
		return false
	}
}

// DeliverSnapshot queues the snapshot's marker, as a session does.
func (f *fakeSub) DeliverSnapshot(s *Snapshot) bool {
	return f.Deliver(engine.Event{Kind: engine.EvSnapshot, Instrument: s.Instrument, MDSeq: s.MDSeq, Qty: book.Qty(len(s.Levels))})
}

func (f *fakeSub) SlowConsumer() { f.once.Do(func() { close(f.slow) }) }

func (f *fakeSub) Closed() bool {
	select {
	case <-f.slow:
		return true
	default:
		return false
	}
}

func ev(client book.ClientID, seq engine.Seq) engine.Event {
	return engine.Event{Kind: engine.EvAck, Client: client, Seq: seq}
}

func TestRoutesByClient(t *testing.T) {
	p := New(quiet, nil, nil)
	a, b := newFakeSub(10), newFakeSub(10)
	p.Register(1, a)
	p.Register(2, b)
	p.route(ev(1, 1))
	p.route(ev(2, 2))
	p.route(ev(1, 3))
	p.route(engine.Event{Kind: engine.EvBookDelta, Seq: 4}) // client 0: not routed
	p.route(ev(9, 5))                                       // not connected: dropped
	if len(a.q) != 2 || len(b.q) != 1 {
		t.Fatalf("a got %d, b got %d; want 2 and 1", len(a.q), len(b.q))
	}
	if got := (<-a.q).Seq; got != 1 {
		t.Fatalf("a's first event seq %d", got)
	}
	if s := p.Stats(); s.Delivered != 3 || s.Dropped != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestOneSessionPerClient(t *testing.T) {
	p := New(quiet, nil, nil)
	first, second := newFakeSub(1), newFakeSub(1)
	if !p.Register(5, first) {
		t.Fatal("first register failed")
	}
	if p.Register(5, second) {
		t.Fatal("second session for the same client was accepted")
	}
	p.Unregister(5, second) // not the registered one: must be a no-op
	p.route(ev(5, 1))
	if len(first.q) != 1 {
		t.Fatal("a stale Unregister removed the live session")
	}
	p.Unregister(5, first)
	if !p.Register(5, second) {
		t.Fatal("client id not reusable after its session unregistered")
	}
}

// A full queue must disconnect the client without blocking, and only once.
func TestSlowConsumer(t *testing.T) {
	p := New(quiet, nil, nil)
	slow, ok := newFakeSub(3), newFakeSub(100)
	p.Register(1, slow)
	p.Register(2, ok)

	done := make(chan struct{}) // closed when routing finishes
	go func() {
		for i := range 50 {
			p.route(ev(1, engine.Seq(2*i)))   // slow never drains its queue
			p.route(ev(2, engine.Seq(2*i+1))) // ok keeps receiving
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("route blocked on a full subscriber")
	}
	select {
	case <-slow.slow:
	default:
		t.Fatal("SlowConsumer not called")
	}
	if len(ok.q) != 50 {
		t.Fatalf("healthy client got %d of 50 events", len(ok.q))
	}
	if s := p.Stats(); s.SlowConsumers != 1 {
		t.Fatalf("slow consumers = %d, want 1", s.SlowConsumers)
	}
	// Events after the disconnect are dropped, not delivered or re-reported.
	if s := p.Stats(); s.Dropped != 50-4 {
		t.Fatalf("dropped = %d, want %d (50 sent, 3 queued, 1 overflowed)", s.Dropped, 50-4)
	}
}

// Register/Unregister from many goroutines while Run routes: run with -race.
func TestConcurrentRegistration(t *testing.T) {
	p := New(quiet, nil, nil)
	events := make(chan engine.Event, 64) // test -> Run goroutine
	finished := make(chan struct{})       // closed when Run returns
	go func() { p.Run(events); close(finished) }()

	var wg sync.WaitGroup // tracks the registering goroutines
	for c := range book.ClientID(8) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				s := newFakeSub(1000)
				if p.Register(c+1, s) {
					p.Unregister(c+1, s)
				}
			}
		}()
	}
	for i := range 5000 {
		events <- ev(book.ClientID(1+i%8), engine.Seq(i))
	}
	wg.Wait()
	close(events)
	<-finished
	if s := p.Stats(); s.Delivered+s.Dropped != 5000 {
		t.Fatalf("delivered %d + dropped %d != 5000", s.Delivered, s.Dropped)
	}
}

// ---------------------------------------------------------------------------
// Durability gating (decision 012)

func runGated(t *testing.T, d *durable.Seq, buf int) (*Publisher, chan engine.Event, chan struct{}, *fakeSub) {
	t.Helper()
	p := New(quiet, d, nil)
	sub := newFakeSub(MaxPending + 1000)
	p.Register(1, sub)
	events := make(chan engine.Event, buf) // test -> Run goroutine
	finished := make(chan struct{})        // closed when Run returns
	go func() { p.Run(events); close(finished) }()
	return p, events, finished, sub
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestHeldUntilDurableThenReleasedInOrder(t *testing.T) {
	d := durable.New(0)
	p, events, finished, sub := runGated(t, d, 16)
	for seq := engine.Seq(1); seq <= 5; seq++ {
		events <- ev(1, seq)
		events <- ev(1, seq) // two events per command, same seq
	}
	waitFor(t, "10 held", func() bool { return p.Stats().Held == 10 })
	if len(sub.q) != 0 {
		t.Fatalf("%d events released before any durability", len(sub.q))
	}
	d.Advance(3)
	waitFor(t, "6 released", func() bool { return len(sub.q) == 6 })
	if p.Stats().Held != 4 {
		t.Fatalf("held %d after advancing to 3", p.Stats().Held)
	}
	close(events) // input ends with 4 still held: Run must wait for them
	select {
	case <-finished:
		t.Fatal("Run returned with events still held")
	case <-time.After(50 * time.Millisecond):
	}
	d.Advance(5)
	<-finished
	var seqs []engine.Seq
	for len(sub.q) > 0 {
		seqs = append(seqs, (<-sub.q).Seq)
	}
	want := []engine.Seq{1, 1, 2, 2, 3, 3, 4, 4, 5, 5}
	if fmt.Sprint(seqs) != fmt.Sprint(want) {
		t.Fatalf("released %v, want %v", seqs, want)
	}
}

func TestFailureWithholdsEverything(t *testing.T) {
	d := durable.New(0)
	p, events, finished, sub := runGated(t, d, 16)
	events <- ev(1, 1)
	d.Advance(1)
	waitFor(t, "first released", func() bool { return len(sub.q) == 1 })
	events <- ev(1, 2)
	events <- ev(1, 3)
	waitFor(t, "2 held", func() bool { return p.Stats().Held == 2 })
	d.Fail(errors.New("injected fsync failure"))
	// The engine may still be sending: the publisher must keep draining so
	// it never blocks, but release nothing.
	events <- ev(1, 4)
	d.Advance(10) // ignored after Fail
	close(events)
	<-finished
	if len(sub.q) != 1 {
		t.Fatalf("%d events released; only the one durable before the failure may be", len(sub.q))
	}
	if s := p.Stats(); s.Withheld != 3 || s.Held != 0 {
		t.Fatalf("stats %+v, want 3 withheld", s)
	}
}

// When MaxPending events are held, the publisher stops taking more, so the
// engine's sends block until durability catches up.
func TestPendingLimitBackpressure(t *testing.T) {
	d := durable.New(0)
	p, events, finished, sub := runGated(t, d, 100)
	sent := 0
	for ; sent < MaxPending; sent++ {
		events <- ev(1, engine.Seq(sent+1))
	}
	waitFor(t, "MaxPending held", func() bool { return p.Stats().Held == MaxPending })
	for i := 0; i < 100; i++ { // fills the channel buffer: the publisher isn't taking
		events <- ev(1, engine.Seq(sent+1))
		sent++
	}
	select {
	case events <- ev(1, engine.Seq(sent+1)):
		t.Fatal("publisher kept accepting events past MaxPending")
	case <-time.After(50 * time.Millisecond):
	}
	d.Advance(uint64(sent))
	close(events)
	<-finished
	if len(sub.q) != sent {
		t.Fatalf("released %d of %d", len(sub.q), sent)
	}
}
