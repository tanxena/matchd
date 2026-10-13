package server

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/publisher"
)

// Admin errors, mapped to HTTP status codes by the handler.
var (
	ErrInvalidInstrument = errors.New("invalid instrument")
	ErrDuplicateSymbol   = errors.New("symbol already exists")
	ErrUnavailable       = errors.New("server is shutting down or has failed")
	ErrAdminTimeout      = errors.New("no answer in time; the instrument may or may not have been added")
)

// admin is the publisher's subscriber for gateway.ReservedClientID: the
// answers to admin commands come back through the same durability gate
// and routing as a client's, matched to their request by ClOrdID.
type admin struct {
	next    atomic.Uint64
	mu      sync.Mutex
	waiting map[book.ClientOrderID]chan engine.Event
}

func newAdmin() *admin { return &admin{waiting: map[book.ClientOrderID]chan engine.Event{}} }

func (a *admin) expect(id book.ClientOrderID) chan engine.Event {
	ch := make(chan engine.Event, 1)
	a.mu.Lock()
	a.waiting[id] = ch
	a.mu.Unlock()
	return ch
}

func (a *admin) forget(id book.ClientOrderID) {
	a.mu.Lock()
	delete(a.waiting, id)
	a.mu.Unlock()
}

// Deliver implements publisher.Subscriber. It never blocks: each request's
// channel has room for its one answer.
func (a *admin) Deliver(ev engine.Event) bool {
	a.mu.Lock()
	ch := a.waiting[ev.ClOrdID]
	delete(a.waiting, ev.ClOrdID)
	a.mu.Unlock()
	if ch != nil {
		ch <- ev
	}
	return true
}

func (a *admin) DeliverSnapshot(*publisher.Snapshot) bool { return false } // never subscribes
func (a *admin) SlowConsumer()                            {}
func (a *admin) Closed() bool                             { return false }

// AddInstrument registers an instrument at runtime (POST /instruments). The
// command goes through the sequencer and WAL like any other, so it is
// replayed on restart and gets the same id; the answer is returned only
// once it is durable.
func (s *Server) AddInstrument(spec book.Instrument) (publisher.InstrumentInfo, error) {
	// Checked before sequencing, so a bad request never reaches the log
	// (the engine checks again, as it must for replay).
	if err := spec.Validate(); err != nil {
		return publisher.InstrumentInfo{}, fmt.Errorf("%w: %v", ErrInvalidInstrument, err)
	}
	id := book.ClientOrderID(s.admin.next.Add(1))
	answer := s.admin.expect(id)
	defer s.admin.forget(id)
	s.pub.ExpectInstrument(gateway.ReservedClientID, id, spec)
	if !s.submit(engine.Command{Kind: engine.CmdAddInstrument, Client: gateway.ReservedClientID, ClOrdID: id, Spec: spec}) {
		return publisher.InstrumentInfo{}, ErrUnavailable
	}
	t := time.NewTimer(s.cfg.AdminTimeout)
	defer t.Stop()
	select {
	case ev := <-answer:
		switch {
		case ev.Kind == engine.EvInstrumentAdded:
			return publisher.InstrumentInfo{ID: ev.Instrument, Symbol: spec.Symbol, TickSize: spec.TickSize, LotSize: spec.LotSize}, nil
		case ev.Reason == book.RejectDuplicateSymbol:
			return publisher.InstrumentInfo{}, fmt.Errorf("%w: %s", ErrDuplicateSymbol, spec.Symbol)
		default:
			return publisher.InstrumentInfo{}, fmt.Errorf("%w: %v", ErrInvalidInstrument, ev.Reason)
		}
	case <-t.C:
		return publisher.InstrumentInfo{}, ErrAdminTimeout
	case <-s.durable.Failed():
		return publisher.InstrumentInfo{}, ErrUnavailable
	}
}
