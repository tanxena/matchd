package engine_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

// Randomized property tests for the engine's event stream. The book's
// matching is already proven by the differential test in internal/book; these
// tests prove that the *events* describe what the books did, closely enough
// that a client or a market-data consumer can rebuild the state from events
// alone.

const (
	streamSeeds = 8
	streamCmds  = 5_000
)

type liveOrder struct {
	inst   engine.InstrumentID
	client book.ClientID
	id     book.OrderID
}

// streamGen produces commands for two instruments. Its only state that
// depends on engine output is the list of acked order ids, used to aim
// cancels; it is updated by the test from the events.
type streamGen struct {
	rng   *rand.Rand
	mid   [3]int64 // per instrument (index 1, 2)
	clOrd book.ClientOrderID
	acked []liveOrder
}

func newStreamGen(seed uint64) *streamGen {
	return &streamGen{rng: rand.New(rand.NewPCG(seed, 7)), mid: [3]int64{0, 1000, 500}}
}

func (g *streamGen) next() engine.Command {
	inst := engine.InstrumentID(1 + g.rng.IntN(2))
	g.mid[inst] = min(max(g.mid[inst]+int64(g.rng.IntN(3)-1), 400), 1200)
	client := book.ClientID(1 + g.rng.IntN(4))
	g.clOrd++
	side := book.Buy
	if g.rng.IntN(2) == 0 {
		side = book.Sell
	}
	switch r := g.rng.IntN(100); {
	case r < 55:
		return limit(inst, client, g.clOrd, side, book.Price(g.mid[inst]+int64(g.rng.IntN(11)-5)), book.Qty(1+g.rng.IntN(10)))
	case r < 65:
		return market(inst, client, g.clOrd, side, book.Qty(1+g.rng.IntN(25)))
	case r < 90 && len(g.acked) > 0:
		o := g.acked[g.rng.IntN(len(g.acked))] // may be live or already gone
		c := cancel(o.inst, o.client, o.id)
		switch g.rng.IntN(10) {
		case 0:
			c.Client = client // probably not the owner
		case 1:
			c.Instrument = 3 - o.inst // the other instrument
		}
		return c
	case r < 95:
		return limit(inst, client, g.clOrd, side, 0, 1) // invalid price
	default:
		return limit(engine.InstrumentID(3+g.rng.IntN(3)), client, g.clOrd, side, 100, 1) // unknown instrument
	}
}

type levelKey struct {
	inst engine.InstrumentID
	side book.Side
	px   book.Price
}

// views are the two pictures of the world a consumer can build from events.
type views struct {
	levels map[levelKey]book.Qty              // market data: from BookDelta only
	open   map[book.OrderID]book.RestingOrder // clients: from Ack, Fill, Cancelled
	inst   map[book.OrderID]engine.InstrumentID
}

func newViews() *views {
	return &views{
		levels: make(map[levelKey]book.Qty),
		open:   make(map[book.OrderID]book.RestingOrder),
		inst:   make(map[book.OrderID]engine.InstrumentID),
	}
}

// apply folds one command's events into the views and checks per-command
// rules on the event sequence itself. It returns an error describing the
// first violation.
func (v *views) apply(c engine.Command, evs []engine.Event, lastOrderID *book.OrderID, lastMatchID *uint64) error {
	for i := 0; i < len(evs); i++ {
		e := evs[i]
		if e.Seq != c.Seq || e.Time != c.Time {
			return fmt.Errorf("event %d (%v) carries seq=%d time=%d, command has %d/%d", i, e.Kind, e.Seq, e.Time, c.Seq, c.Time)
		}
		switch e.Kind {
		case engine.EvAck:
			if i != 0 {
				return fmt.Errorf("Ack at position %d, must be first", i)
			}
			if e.OrderID != *lastOrderID+1 {
				return fmt.Errorf("Ack order id %d after %d: ids must be gap-free", e.OrderID, *lastOrderID)
			}
			*lastOrderID = e.OrderID
			v.open[e.OrderID] = book.RestingOrder{ID: e.OrderID, Client: e.Client, ClOrdID: e.ClOrdID, Side: e.Side, Price: e.Price, Qty: e.Qty}
			v.inst[e.OrderID] = e.Instrument
		case engine.EvFill:
			if e.Role != engine.RoleTaker || i+1 >= len(evs) {
				return fmt.Errorf("fill at %d is not a taker fill followed by its maker fill", i)
			}
			m := evs[i+1]
			if m.Kind != engine.EvFill || m.Role != engine.RoleMaker || m.MatchID != e.MatchID ||
				m.Price != e.Price || m.Qty != e.Qty || m.Side != e.Side.Opposite() {
				return fmt.Errorf("fill pair mismatch:\n taker %+v\n maker %+v", e, m)
			}
			if e.MatchID != *lastMatchID+1 {
				return fmt.Errorf("match id %d after %d", e.MatchID, *lastMatchID)
			}
			*lastMatchID = e.MatchID
			for _, f := range [2]engine.Event{e, m} {
				o, ok := v.open[f.OrderID]
				if !ok {
					return fmt.Errorf("fill for order %d that is not open", f.OrderID)
				}
				o.Qty -= f.Qty
				if o.Qty != f.Leaves {
					return fmt.Errorf("order %d: leaves %d, but open qty after fill is %d", f.OrderID, f.Leaves, o.Qty)
				}
				if o.Qty == 0 {
					delete(v.open, f.OrderID)
				} else {
					v.open[f.OrderID] = o
				}
			}
			i++ // consumed the maker event
		case engine.EvCancelled:
			o, ok := v.open[e.OrderID]
			if !ok || o.Qty != e.Qty {
				return fmt.Errorf("cancelled %d of order %d, client view has %+v (%v)", e.Qty, e.OrderID, o, ok)
			}
			delete(v.open, e.OrderID)
		case engine.EvBookDelta:
			k := levelKey{e.Instrument, e.Side, e.Price}
			if e.Qty == 0 {
				delete(v.levels, k)
			} else {
				v.levels[k] = e.Qty
			}
		case engine.EvReject:
			if len(evs) != 1 {
				return fmt.Errorf("Reject must be the only event, got %d events", len(evs))
			}
		default:
			return fmt.Errorf("unexpected event kind %v", e.Kind)
		}
	}
	return nil
}

// check compares both views with the engine's actual books.
func (v *views) check(e *engine.Engine) error {
	wantLevels := 0
	wantOpen := 0
	var rest []book.RestingOrder
	for id := engine.InstrumentID(1); int(id) <= e.NumInstruments(); id++ {
		bk := e.Book(id)
		for _, s := range [2]book.Side{book.Buy, book.Sell} {
			rest = bk.Orders(s, rest[:0])
			for j, r := range rest {
				if j == 0 || rest[j-1].Price != r.Price {
					wantLevels++
					if got, want := v.levels[levelKey{id, s, r.Price}], bk.LevelQty(s, r.Price); got != want {
						return fmt.Errorf("market data: inst %d %s %d = %d, book has %d", id, s, r.Price, got, want)
					}
				}
				wantOpen++
				if got := v.open[r.ID]; got != r || v.inst[r.ID] != id {
					return fmt.Errorf("client view of order %d = %+v (inst %d), book has %+v (inst %d)", r.ID, got, v.inst[r.ID], r, id)
				}
			}
		}
	}
	if len(v.levels) != wantLevels {
		return fmt.Errorf("market data has %d levels, books have %d (a level was never removed)", len(v.levels), wantLevels)
	}
	if len(v.open) != wantOpen {
		return fmt.Errorf("client view has %d open orders, books have %d (an order left without an event)", len(v.open), wantOpen)
	}
	return nil
}

// runStream generates n commands for seed, applies them via Step, verifies
// both views after every command, and returns the full event log.
func runStream(t *testing.T, seed uint64, n int) []engine.Event {
	t.Helper()
	d := newDriver(t)
	d.addInstrument("AAA")
	d.addInstrument("BBB")
	g := newStreamGen(seed)
	v := newViews()
	var log []engine.Event
	var lastOrderID book.OrderID
	var lastMatchID uint64
	for i := range n {
		c := g.next()
		evs := d.step(c)
		c.Seq, c.Time = d.seq, evs[0].Time // the driver filled these in
		if err := v.apply(c, evs, &lastOrderID, &lastMatchID); err != nil {
			t.Fatalf("seed %d command #%d %+v:\n%v\nevents:\n%s", seed, i, c, err, fmtEvents(evs))
		}
		if err := v.check(d.e); err != nil {
			t.Fatalf("seed %d command #%d %+v:\n%v\nevents:\n%s", seed, i, c, err, fmtEvents(evs))
		}
		for _, e := range evs {
			if e.Kind == engine.EvAck {
				g.acked = append(g.acked, liveOrder{e.Instrument, e.Client, e.OrderID})
			}
		}
		log = append(log, evs...)
	}
	return log
}

func TestEventStreamRebuildsState(t *testing.T) {
	for seed := range uint64(streamSeeds) {
		t.Run(fmt.Sprint(seed+1), func(t *testing.T) {
			t.Parallel()
			log := runStream(t, seed+1, streamCmds)
			var kinds [engine.EvInstrumentAdded + 1]int
			for _, e := range log {
				kinds[e.Kind]++
			}
			// Every event kind must actually occur, or the checks above
			// passed vacuously.
			for k := engine.EvAck; k <= engine.EvBookDelta; k++ {
				if kinds[k] < streamCmds/100 {
					t.Errorf("only %d %v events in %d commands", kinds[k], k, streamCmds)
				}
			}
		})
	}
}

// Same commands in, same events out: the engine has no hidden input.
func TestDeterministic(t *testing.T) {
	a := runStream(t, 42, streamCmds)
	b := runStream(t, 42, streamCmds)
	if !slices.Equal(a, b) {
		for i := range min(len(a), len(b)) {
			if a[i] != b[i] {
				t.Fatalf("runs differ at event %d:\n %+v\n %+v", i, a[i], b[i])
			}
		}
		t.Fatalf("runs differ in length: %d vs %d", len(a), len(b))
	}
}

// Run (the goroutine form) must emit exactly what Step emits, in order, and
// close its output when the input closes. Run with -race.
func TestRunMatchesStep(t *testing.T) {
	// Record the command stream once via Step, with the events it produced.
	d := newDriver(t)
	var cmds []engine.Command
	record := func(c engine.Command) []engine.Event {
		evs := d.step(c)
		c.Seq, c.Time = d.seq, evs[0].Time
		cmds = append(cmds, c)
		return evs
	}
	var want []engine.Event
	want = append(want, record(engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "AAA", TickSize: 1, LotSize: 1}})...)
	want = append(want, record(engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "BBB", TickSize: 1, LotSize: 1}})...)
	g := newStreamGen(7)
	for range 3_000 {
		evs := record(g.next())
		for _, e := range evs {
			if e.Kind == engine.EvAck {
				g.acked = append(g.acked, liveOrder{e.Instrument, e.Client, e.OrderID})
			}
		}
		want = append(want, evs...)
	}

	// Replay the identical commands through Run on its own goroutine.
	in := make(chan engine.Command, 64) // producer -> engine goroutine
	out := make(chan engine.Event, 64)  // engine goroutine -> this test
	go engine.New().Run(in, out)
	go func() { // producer: sends every command, then closes in
		for _, c := range cmds {
			in <- c
		}
		close(in)
	}()
	var got []engine.Event
	for e := range out { // ends when Run closes out after draining in
		got = append(got, e)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Run emitted %d events, Step %d; first difference at %d", len(got), len(want), firstDiff(got, want))
	}
}

func firstDiff(a, b []engine.Event) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}
