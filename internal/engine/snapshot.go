package engine

import (
	"fmt"
	"time"

	"github.com/tanxena/matchd/internal/book"
)

// Snapshot is a frozen, point-in-time view of the whole engine, taken
// between two commands (decision 013). It is handed to a snapshot goroutine,
// which may read it while the engine keeps processing commands, and then
// handed back so the engine can Thaw it.
type Snapshot struct {
	Seq         Seq // the last command included
	LastOrderID book.OrderID
	LastMatchID uint64
	Books       []*book.Frozen // index i is instrument id i+1
	MDSeq       []uint64       // index i is instrument id i+1
}

// Freeze captures the engine's state for a snapshot. It copies only counters
// and per-book level heads (see book.Freeze), never orders, so its cost
// doesn't grow with the number of resting orders. Engine goroutine only.
func (e *Engine) Freeze() *Snapshot {
	s := &Snapshot{Seq: e.lastSeq, LastOrderID: e.lastOrderID, LastMatchID: e.lastMatchID,
		Books: make([]*book.Frozen, 0, len(e.books)-1), MDSeq: append([]uint64(nil), e.mdSeq[1:]...)}
	for _, b := range e.books[1:] {
		s.Books = append(s.Books, b.Freeze())
	}
	return s
}

// Thaw ends a snapshot taken by Freeze. Engine goroutine only, and only once
// nothing reads s any more.
func (e *Engine) Thaw(s *Snapshot) {
	for i, f := range s.Books {
		e.books[i+1].Thaw(f)
	}
	s.Books = nil
}

// InstrumentState is one instrument in a State: its definition and its
// resting orders in priority order.
type InstrumentState struct {
	Spec  book.Instrument
	MDSeq uint64 // md_seq of the instrument's last market-data event
	Bids  []book.RestingOrder
	Asks  []book.RestingOrder
}

// State is the engine's complete state as plain data: what a snapshot file
// stores and what Restore rebuilds from. Instruments[i] has id i+1.
type State struct {
	Seq         Seq
	LastOrderID book.OrderID
	LastMatchID uint64
	Instruments []InstrumentState
}

// State materialises a frozen snapshot. Safe from any goroutine while the
// snapshot is not yet thawed.
func (s *Snapshot) State() State {
	st := State{Seq: s.Seq, LastOrderID: s.LastOrderID, LastMatchID: s.LastMatchID}
	for i, f := range s.Books {
		is := InstrumentState{Spec: f.Instrument(), MDSeq: s.MDSeq[i]}
		f.Each(book.Buy, func(r book.RestingOrder) { is.Bids = append(is.Bids, r) })
		f.Each(book.Sell, func(r book.RestingOrder) { is.Asks = append(is.Asks, r) })
		st.Instruments = append(st.Instruments, is)
	}
	return st
}

// State returns the live engine's state. Engine goroutine only (or after the
// engine has stopped).
func (e *Engine) State() State {
	s := e.Freeze()
	defer e.Thaw(s)
	return s.State()
}

// Restore builds an engine in exactly the state st describes. Its next
// command must have Seq st.Seq+1.
func Restore(st State) (*Engine, error) {
	e := New()
	for i, is := range st.Instruments {
		if _, dup := e.symbols[is.Spec.Symbol]; dup {
			return nil, fmt.Errorf("engine: restore: duplicate symbol %q", is.Spec.Symbol)
		}
		b, err := book.Load(is.Spec, is.Bids, is.Asks)
		if err != nil {
			return nil, fmt.Errorf("engine: restore: %w", err)
		}
		e.books = append(e.books, b)
		e.mdSeq = append(e.mdSeq, is.MDSeq)
		e.symbols[is.Spec.Symbol] = InstrumentID(i + 1)
		for _, side := range [2][]book.RestingOrder{is.Bids, is.Asks} {
			for _, r := range side {
				if r.ID > st.LastOrderID {
					return nil, fmt.Errorf("engine: restore: order id %d above last order id %d", r.ID, st.LastOrderID)
				}
			}
		}
	}
	e.lastSeq, e.lastOrderID, e.lastMatchID = st.Seq, st.LastOrderID, st.LastMatchID
	return e, nil
}

// LastMatchID returns the id given to the most recent match.
func (e *Engine) LastMatchID() uint64 { return e.lastMatchID }

// SnapshotHooks connects Run to a snapshot goroutine.
type SnapshotHooks struct {
	// Every: take a snapshot after this many commands since the last one
	// (0 = no command-count trigger).
	Every uint64
	// Tick: take a snapshot on each tick, if any command arrived since the
	// last one (nil = no time trigger). Whichever trigger fires first wins.
	Tick <-chan time.Time
	// Out receives frozen snapshots. Capacity 1: at most one snapshot is in
	// flight; a trigger while one is in flight waits for it to come back.
	Out chan<- *Snapshot
	// Done returns snapshots once written, to be thawed.
	Done <-chan *Snapshot
}
