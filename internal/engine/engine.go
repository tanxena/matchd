package engine

import (
	"fmt"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/metrics"
)

// Engine owns every order book and applies commands to them one at a time.
//
// An Engine is not safe for concurrent use and has no locks: exactly one
// goroutine (the one calling Run, or a test calling Step) may touch it. All
// concurrency lives outside it, at the channel edges (decision 005).
type Engine struct {
	books   []*book.Book            // indexed by InstrumentID; [0] is unused
	mdSeq   []uint64                // per instrument: md_seq of its last market-data event (decision 016)
	symbols map[string]InstrumentID // registry: symbol -> id

	lastSeq     Seq          // Seq of the last command applied
	lastOrderID book.OrderID // last order id given to an accepted order
	lastMatchID uint64       // last match id given to a fill pair

	fills []book.Fill // scratch buffer reused across commands

	// latency and step, if set, record each command's time in Run
	// (decision 017).
	latency *metrics.Histogram
	step    *metrics.Histogram
}

// SetLatency makes Run record, for every command:
//   - in latency: from taking it off the input channel to sending its last
//     event (PRD 5.8's "engine in -> events emitted"). This includes
//     handing the events over: waking an idle publisher, and any time the
//     engine is blocked on a full output channel, which is what a slow
//     consumer would show up as;
//   - in step: Step alone, the matching work itself.
//
// Either may be nil. Call before Run.
func (e *Engine) SetLatency(latency, step *metrics.Histogram) { e.latency, e.step = latency, step }

// New returns an engine with no instruments.
func New() *Engine {
	return &Engine{
		books:   make([]*book.Book, 1), // slot 0 reserved: InstrumentID 0 is invalid
		mdSeq:   make([]uint64, 1),
		symbols: make(map[string]InstrumentID),
	}
}

// Step applies one command and appends the events it produces to out,
// returning the extended slice. It is the engine's whole behaviour; Run is a
// loop around it.
//
// Step panics if c.Seq is not exactly one more than the previous command's.
// A gap or repeat means the sequencer or WAL replay is broken, and applying
// the command anyway would make the engine's state diverge from the log.
func (e *Engine) Step(c *Command, out []Event) []Event {
	if c.Seq != e.lastSeq+1 {
		panic(fmt.Sprintf("engine: command seq %d after %d; sequence must be gap-free", c.Seq, e.lastSeq))
	}
	e.lastSeq = c.Seq

	first := len(out)
	switch c.Kind {
	case CmdNewOrder:
		out = e.newOrder(c, out)
	case CmdCancel:
		out = e.cancel(c, out)
	case CmdAddInstrument:
		out = e.addInstrument(c, out)
	default:
		// The gateway only builds known kinds and the WAL checksums records,
		// so an unknown kind here is a bug, not client input.
		panic(fmt.Sprintf("engine: command seq %d has unknown kind %v", c.Seq, c.Kind))
	}
	for i := first; i < len(out); i++ {
		out[i].Cmd = c.Kind
	}
	return out
}

// Run is the body of the engine goroutine. It applies commands from in, in
// order, and sends every resulting event to out, in order. When in is closed
// it returns after the last command's events are sent, and closes out so the
// consumer knows the stream is complete.
//
// Backpressure: sends on out block when the consumer is behind, which stalls
// the engine. The consumer (the publisher) is therefore responsible for never
// blocking on a slow client; it must buffer or disconnect instead (PRD 5.5).
//
// If snap is non-nil, Run also takes copy-on-write snapshots between
// commands (decision 013): when Every commands have been applied since the
// last snapshot, or a Tick arrives after at least one command, it freezes
// the engine and sends the snapshot to snap.Out, and thaws it when it comes
// back on snap.Done. At most one snapshot is in flight. Freezing costs
// O(instruments + price levels + chunks), never O(orders), so it doesn't
// stall matching.
func (e *Engine) Run(in <-chan Command, out chan<- Event, snap *SnapshotHooks) {
	defer close(out)
	var buf []Event // reused for every command's events
	var (
		tick     <-chan time.Time
		done     <-chan *Snapshot
		inFlight bool   // a snapshot is out and not yet thawed
		since    uint64 // commands applied since the last snapshot
		wanted   bool   // a trigger fired while a snapshot was in flight
	)
	if snap != nil {
		tick, done = snap.Tick, snap.Done
	}
	trigger := func() {
		if inFlight || since == 0 {
			wanted = inFlight && since > 0
			return
		}
		s := e.Freeze()
		select {
		case snap.Out <- s:
			inFlight, since, wanted = true, 0, false
		default: // the writer isn't ready (it shouldn't happen with capacity 1)
			e.Thaw(s)
		}
	}
	for {
		select {
		case c, ok := <-in:
			if !ok {
				return // a snapshot still in flight is the writer's to finish
			}
			// internal/clock, never time.Now: time.Now can't resolve a
			// sub-microsecond step on Windows (decision 017).
			start := clock.Now()
			buf = e.Step(&c, buf[:0])
			stepped := clock.Now()
			for i := range buf {
				out <- buf[i]
			}
			e.step.Observe(stepped - start)
			e.latency.Observe(clock.Now() - start)
			since++
			if snap != nil && snap.Every > 0 && since >= snap.Every {
				trigger()
			}
		case <-tick:
			trigger()
		case s := <-done:
			e.Thaw(s)
			inFlight = false
			if wanted {
				trigger()
			}
		}
	}
}

func (e *Engine) book(id InstrumentID) *book.Book {
	if id == 0 || int(id) >= len(e.books) {
		return nil
	}
	return e.books[id]
}

func (e *Engine) reject(c *Command, reason book.RejectReason, out []Event) []Event {
	return append(out, Event{
		Seq: c.Seq, Time: c.Time, Kind: EvReject, Instrument: c.Instrument,
		Client: c.Client, ClOrdID: c.ClOrdID, OrderID: c.OrderID,
		Reason: reason,
	})
}

// nextMD returns the next md_seq for instrument id. Market-data events
// (book deltas and trades) are numbered per instrument, gap-free, by the
// engine, so the numbering is part of the deterministic state: replay and
// snapshots reproduce it exactly (decision 016).
func (e *Engine) nextMD(id InstrumentID) uint64 {
	e.mdSeq[id]++
	return e.mdSeq[id]
}

func (e *Engine) delta(c *Command, side book.Side, px book.Price, qty book.Qty) Event {
	return Event{
		Seq: c.Seq, Time: c.Time, Kind: EvBookDelta, Instrument: c.Instrument,
		Side: side, Price: px, Qty: qty, MDSeq: e.nextMD(c.Instrument),
		Orders: uint32(e.books[c.Instrument].Len()),
	}
}

// newOrder submits an order and emits, in this order (decision 006):
//
//	Ack
//	for each opposite-side level traded, best first:
//	    Fill(taker), Fill(maker) for each match at that level
//	    BookDelta for that level
//	Cancelled(expired)           if a market order has an unfilled remainder
//	BookDelta for the own level  if a limit remainder rested
//
// The order id is only consumed if the book accepts the order, so accepted
// orders get gap-free ids.
func (e *Engine) newOrder(c *Command, out []Event) []Event {
	bk := e.book(c.Instrument)
	if bk == nil {
		return e.reject(c, book.RejectUnknownInstrument, out)
	}
	o := book.Order{
		ID: e.lastOrderID + 1, Client: c.Client, ClOrdID: c.ClOrdID,
		Side: c.Side, Type: c.Type, TIF: c.TIF, Price: c.Price, Qty: c.Qty,
	}
	var res book.Result
	res, e.fills = bk.Submit(o, e.fills[:0])
	if res.Reject != book.RejectNone {
		return e.reject(c, res.Reject, out)
	}
	e.lastOrderID = o.ID

	out = append(out, Event{
		Seq: c.Seq, Time: c.Time, Kind: EvAck, Instrument: c.Instrument,
		Client: c.Client, ClOrdID: c.ClOrdID, OrderID: o.ID,
		Side: c.Side, Price: c.Price, Qty: c.Qty,
	})

	opp := c.Side.Opposite()
	leaves := c.Qty
	for i := range e.fills {
		f := &e.fills[i]
		e.lastMatchID++
		leaves -= f.Qty
		out = append(out,
			Event{
				Seq: c.Seq, Time: c.Time, Kind: EvFill, Instrument: c.Instrument,
				Client: c.Client, ClOrdID: c.ClOrdID, OrderID: o.ID,
				Side: c.Side, Price: f.Price, Qty: f.Qty, Leaves: leaves,
				MatchID: e.lastMatchID, Role: RoleTaker,
			},
			Event{
				Seq: c.Seq, Time: c.Time, Kind: EvFill, Instrument: c.Instrument,
				Client: f.MakerClient, ClOrdID: f.MakerClOrdID, OrderID: f.Maker,
				Side: opp, Price: f.Price, Qty: f.Qty, Leaves: f.MakerLeft,
				MatchID: e.lastMatchID, Role: RoleMaker,
			},
			// The public trade print for market data: anonymous, with the
			// aggressor's side.
			Event{
				Seq: c.Seq, Time: c.Time, Kind: EvTrade, Instrument: c.Instrument,
				Side: c.Side, Price: f.Price, Qty: f.Qty,
				MatchID: e.lastMatchID, MDSeq: e.nextMD(c.Instrument),
			},
		)
		// Fills arrive grouped by level, best first. After the last fill at a
		// price, report that level's new size. The book has already finished
		// matching, so LevelQty is the level's final size for this command.
		if i == len(e.fills)-1 || e.fills[i+1].Price != f.Price {
			out = append(out, e.delta(c, opp, f.Price, bk.LevelQty(opp, f.Price)))
		}
	}

	if res.Expired > 0 {
		out = append(out, Event{
			Seq: c.Seq, Time: c.Time, Kind: EvCancelled, Instrument: c.Instrument,
			Client: c.Client, ClOrdID: c.ClOrdID, OrderID: o.ID,
			Side: c.Side, Qty: res.Expired, CancelReason: CancelExpired,
		})
	}
	if res.Rested > 0 {
		out = append(out, e.delta(c, c.Side, c.Price, bk.LevelQty(c.Side, c.Price)))
	}
	return out
}

// cancel removes a resting order and emits Cancelled then a BookDelta for its
// level, or a Reject.
func (e *Engine) cancel(c *Command, out []Event) []Event {
	bk := e.book(c.Instrument)
	if bk == nil {
		return e.reject(c, book.RejectUnknownInstrument, out)
	}
	res := bk.Cancel(c.Client, c.OrderID)
	if res.Reject != book.RejectNone {
		return e.reject(c, res.Reject, out)
	}
	return append(out,
		Event{
			Seq: c.Seq, Time: c.Time, Kind: EvCancelled, Instrument: c.Instrument,
			Client: c.Client, ClOrdID: res.ClOrdID, OrderID: c.OrderID,
			Side: res.Side, Price: res.Price, Qty: res.Cancelled,
			CancelReason: CancelRequested,
		},
		e.delta(c, res.Side, res.Price, bk.LevelQty(res.Side, res.Price)),
	)
}

// addInstrument registers a new instrument and creates its book. Instruments
// are created through the command stream, not directly, so that they are
// logged and replayed like everything else and get the same ids on replay.
func (e *Engine) addInstrument(c *Command, out []Event) []Event {
	if c.Spec.Validate() != nil {
		return e.reject(c, book.RejectInvalidInstrument, out)
	}
	if _, dup := e.symbols[c.Spec.Symbol]; dup {
		return e.reject(c, book.RejectDuplicateSymbol, out)
	}
	bk, err := book.New(c.Spec)
	if err != nil { // unreachable: Spec was validated above
		panic(fmt.Sprintf("engine: book.New after validation: %v", err))
	}
	id := InstrumentID(len(e.books))
	e.books = append(e.books, bk)
	e.mdSeq = append(e.mdSeq, 0)
	e.symbols[c.Spec.Symbol] = id
	return append(out, Event{
		Seq: c.Seq, Time: c.Time, Kind: EvInstrumentAdded, Instrument: id,
		Client: c.Client, ClOrdID: c.ClOrdID, // the admin endpoint matches its answer by these
	})
}

// LastSeq returns the Seq of the last command applied (0 before any).
func (e *Engine) LastSeq() Seq { return e.lastSeq }

// LastOrderID returns the id given to the most recently accepted order.
func (e *Engine) LastOrderID() book.OrderID { return e.lastOrderID }

// Lookup returns the id registered for symbol.
func (e *Engine) Lookup(symbol string) (InstrumentID, bool) {
	id, ok := e.symbols[symbol]
	return id, ok
}

// Book returns the book for id, or nil. The book belongs to the engine
// goroutine; callers may only use it from that goroutine (tests, and later
// snapshotting under the copy-on-write scheme of milestone 5).
func (e *Engine) Book(id InstrumentID) *book.Book { return e.book(id) }

// NumInstruments returns how many instruments are registered.
func (e *Engine) NumInstruments() int { return len(e.books) - 1 }
