package engine_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

// driver feeds commands to an engine with consecutive sequence numbers and
// deterministic timestamps, the way the sequencer will from milestone 4.
type driver struct {
	t   *testing.T
	e   *engine.Engine
	seq engine.Seq
	out []engine.Event
}

func newDriver(t *testing.T) *driver {
	return &driver{t: t, e: engine.New()}
}

// step applies c (filling in Seq and Time) and returns its events. The slice
// is only valid until the next call.
func (d *driver) step(c engine.Command) []engine.Event {
	d.seq++
	c.Seq = d.seq
	c.Time = engine.Timestamp(1_000_000 * int64(d.seq)) // fake clock: 1 ms per command
	d.out = d.e.Step(&c, d.out[:0])
	return d.out
}

func (d *driver) addInstrument(sym string) engine.InstrumentID {
	d.t.Helper()
	ev := d.step(engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: sym, TickSize: 1, LotSize: 1}})
	if len(ev) != 1 || ev[0].Kind != engine.EvInstrumentAdded {
		d.t.Fatalf("add %s: events %v", sym, fmtEvents(ev))
	}
	return ev[0].Instrument
}

func limit(inst engine.InstrumentID, client book.ClientID, clOrd book.ClientOrderID, side book.Side, px book.Price, qty book.Qty) engine.Command {
	return engine.Command{Kind: engine.CmdNewOrder, Instrument: inst, Client: client, ClOrdID: clOrd,
		Side: side, Type: book.Limit, TIF: book.GTC, Price: px, Qty: qty}
}

func market(inst engine.InstrumentID, client book.ClientID, clOrd book.ClientOrderID, side book.Side, qty book.Qty) engine.Command {
	return engine.Command{Kind: engine.CmdNewOrder, Instrument: inst, Client: client, ClOrdID: clOrd,
		Side: side, Type: book.Market, Qty: qty}
}

func cancel(inst engine.InstrumentID, client book.ClientID, id book.OrderID) engine.Command {
	return engine.Command{Kind: engine.CmdCancel, Instrument: inst, Client: client, OrderID: id}
}

// ev is a compact expected event: only the fields a test names are compared
// (Seq and Time are checked separately for every event).
type ev struct {
	kind   engine.EventKind
	client book.ClientID
	clOrd  book.ClientOrderID
	order  book.OrderID
	side   book.Side
	px     book.Price
	qty    book.Qty
	leaves book.Qty
	match  uint64
	role   engine.Role
	reason book.RejectReason
	cxl    engine.CancelReason
	md     uint64
}

func toEv(e engine.Event) ev {
	return ev{e.Kind, e.Client, e.ClOrdID, e.OrderID, e.Side, e.Price, e.Qty, e.Leaves, e.MatchID, e.Role, e.Reason, e.CancelReason, e.MDSeq}
}

func expect(t *testing.T, got []engine.Event, want ...ev) {
	t.Helper()
	g := make([]ev, len(got))
	for i, e := range got {
		g[i] = toEv(e)
	}
	if !slices.Equal(g, want) {
		t.Fatalf("events:\n%s\nwant:\n%s", fmtEvs(g), fmtEvs(want))
	}
}

func fmtEvs(es []ev) string {
	var sb strings.Builder
	for _, e := range es {
		fmt.Fprintf(&sb, "  %+v\n", e)
	}
	return sb.String()
}

func fmtEvents(es []engine.Event) string {
	var sb strings.Builder
	for _, e := range es {
		fmt.Fprintf(&sb, "  %+v\n", e)
	}
	return sb.String()
}

const (
	buy  = book.Buy
	sell = book.Sell
)

func ack(client book.ClientID, clOrd book.ClientOrderID, id book.OrderID, side book.Side, px book.Price, qty book.Qty) ev {
	return ev{kind: engine.EvAck, client: client, clOrd: clOrd, order: id, side: side, px: px, qty: qty}
}

// delta is a book delta with its md_seq.
func delta(side book.Side, px book.Price, qty book.Qty, md uint64) ev {
	return ev{kind: engine.EvBookDelta, side: side, px: px, qty: qty, md: md}
}

// trade is the public print of match `match`; side is the aggressor's.
func trade(side book.Side, px book.Price, qty book.Qty, match, md uint64) ev {
	return ev{kind: engine.EvTrade, side: side, px: px, qty: qty, match: match, md: md}
}

func fillT(client book.ClientID, clOrd book.ClientOrderID, id book.OrderID, side book.Side, px book.Price, qty, leaves book.Qty, match uint64) ev {
	return ev{kind: engine.EvFill, client: client, clOrd: clOrd, order: id, side: side, px: px, qty: qty, leaves: leaves, match: match, role: engine.RoleTaker}
}

func fillM(client book.ClientID, clOrd book.ClientOrderID, id book.OrderID, side book.Side, px book.Price, qty, leaves book.Qty, match uint64) ev {
	return ev{kind: engine.EvFill, client: client, clOrd: clOrd, order: id, side: side, px: px, qty: qty, leaves: leaves, match: match, role: engine.RoleMaker}
}

func TestNewOrderEventSequence(t *testing.T) {
	d := newDriver(t)
	x := d.addInstrument("XYZ")

	// Resting limit: Ack then a delta for its own level. Market-data events
	// (deltas, trades) are numbered per instrument from md_seq 1.
	expect(t, d.step(limit(x, 1, 11, sell, 100, 5)), ack(1, 11, 1, sell, 100, 5), delta(sell, 100, 5, 1))
	expect(t, d.step(limit(x, 2, 21, sell, 100, 3)), ack(2, 21, 2, sell, 100, 3), delta(sell, 100, 8, 2))
	expect(t, d.step(limit(x, 1, 12, sell, 101, 4)), ack(1, 12, 3, sell, 101, 4), delta(sell, 101, 4, 3))

	// Sweep: per match the fill pair then its trade print; per level, then
	// that level's delta.
	expect(t, d.step(limit(x, 3, 31, buy, 101, 10)),
		ack(3, 31, 4, buy, 101, 10),
		fillT(3, 31, 4, buy, 100, 5, 5, 1), fillM(1, 11, 1, sell, 100, 5, 0, 1), trade(buy, 100, 5, 1, 4),
		fillT(3, 31, 4, buy, 100, 3, 2, 2), fillM(2, 21, 2, sell, 100, 3, 0, 2), trade(buy, 100, 3, 2, 5),
		delta(sell, 100, 0, 6),
		fillT(3, 31, 4, buy, 101, 2, 0, 3), fillM(1, 12, 3, sell, 101, 2, 2, 3), trade(buy, 101, 2, 3, 7),
		delta(sell, 101, 2, 8),
	)

	// Market order larger than the book: fills, trade, delta, then the remainder expires.
	expect(t, d.step(market(x, 3, 32, buy, 5)),
		ack(3, 32, 5, buy, 0, 5),
		fillT(3, 32, 5, buy, 101, 2, 3, 4), fillM(1, 12, 3, sell, 101, 2, 0, 4), trade(buy, 101, 2, 4, 9),
		delta(sell, 101, 0, 10),
		ev{kind: engine.EvCancelled, client: 3, clOrd: 32, order: 5, side: buy, qty: 3, cxl: engine.CancelExpired},
	)

	// Partial fill then rest: the taker's last fill leaves 4, which rests.
	d.step(limit(x, 1, 13, sell, 102, 6)) // md 11
	expect(t, d.step(limit(x, 2, 22, buy, 102, 10)),
		ack(2, 22, 7, buy, 102, 10),
		fillT(2, 22, 7, buy, 102, 6, 4, 5), fillM(1, 13, 6, sell, 102, 6, 0, 5), trade(buy, 102, 6, 5, 12),
		delta(sell, 102, 0, 13),
		delta(buy, 102, 4, 14),
	)
}

// IOC and FOK (decision 020): an IOC's unfilled rest is a CancelAck with
// reason IOCUnfilled (not "expired", a market order's); a FOK that can't
// fill in full is a lone Reject that consumes no order id.
func TestIOCAndFOKEvents(t *testing.T) {
	d := newDriver(t)
	x := d.addInstrument("XYZ")
	d.step(limit(x, 1, 11, sell, 100, 5)) // order 1, md 1
	d.step(limit(x, 1, 12, sell, 101, 5)) // order 2, md 2

	ioc := limit(x, 2, 21, buy, 100, 8)
	ioc.TIF = book.IOC
	expect(t, d.step(ioc),
		ack(2, 21, 3, buy, 100, 8),
		fillT(2, 21, 3, buy, 100, 5, 3, 1), fillM(1, 11, 1, sell, 100, 5, 0, 1), trade(buy, 100, 5, 1, 3),
		delta(sell, 100, 0, 4),
		ev{kind: engine.EvCancelled, client: 2, clOrd: 21, order: 3, side: buy, px: 100, qty: 3, cxl: engine.CancelIOCUnfilled},
	)

	fok := limit(x, 2, 22, buy, 101, 6) // only 5 at or below 101
	fok.TIF = book.FOK
	expect(t, d.step(fok), ev{kind: engine.EvReject, client: 2, clOrd: 22, reason: book.RejectFOKNoLiquidity})
	fok = limit(x, 2, 23, buy, 101, 5) // exactly what's there
	fok.TIF = book.FOK
	expect(t, d.step(fok),
		ack(2, 23, 4, buy, 101, 5), // id 4: the rejected FOK consumed none
		fillT(2, 23, 4, buy, 101, 5, 0, 2), fillM(1, 12, 2, sell, 101, 5, 0, 2), trade(buy, 101, 5, 2, 5),
		delta(sell, 101, 0, 6),
	)

	mktIOC := market(x, 2, 24, buy, 1)
	mktIOC.TIF = book.IOC // limit-only
	expect(t, d.step(mktIOC), ev{kind: engine.EvReject, client: 2, clOrd: 24, reason: book.RejectInvalidTIF})
}

func TestCancelEvents(t *testing.T) {
	d := newDriver(t)
	x := d.addInstrument("XYZ")
	d.step(limit(x, 1, 11, buy, 99, 5))
	d.step(limit(x, 2, 21, buy, 99, 2))

	// Wrong owner: rejected exactly like an unknown id; nothing changes.
	expect(t, d.step(cancel(x, 2, 1)), ev{kind: engine.EvReject, client: 2, order: 1, reason: book.RejectUnknownOrder})
	// Owner: Cancelled (echoing clOrdID, side, price, qty) then the level's new size.
	expect(t, d.step(cancel(x, 1, 1)),
		ev{kind: engine.EvCancelled, client: 1, clOrd: 11, order: 1, side: buy, px: 99, qty: 5, cxl: engine.CancelRequested},
		delta(buy, 99, 2, 3),
	)
	expect(t, d.step(cancel(x, 1, 1)), ev{kind: engine.EvReject, client: 1, order: 1, reason: book.RejectUnknownOrder})
	expect(t, d.step(cancel(x, 2, 2)),
		ev{kind: engine.EvCancelled, client: 2, clOrd: 21, order: 2, side: buy, px: 99, qty: 2, cxl: engine.CancelRequested},
		delta(buy, 99, 0, 4),
	)
}

func TestRegistry(t *testing.T) {
	d := newDriver(t)
	a := d.addInstrument("AAA")
	b := d.addInstrument("BBB")
	if a != 1 || b != 2 {
		t.Fatalf("ids = %d, %d; want 1, 2 (registration order)", a, b)
	}
	if id, ok := d.e.Lookup("BBB"); !ok || id != b {
		t.Fatalf("Lookup(BBB) = %d, %v", id, ok)
	}
	expect(t, d.step(engine.Command{Kind: engine.CmdAddInstrument, Client: 9, Spec: book.Instrument{Symbol: "AAA", TickSize: 1, LotSize: 1}}),
		ev{kind: engine.EvReject, client: 9, reason: book.RejectDuplicateSymbol})
	expect(t, d.step(engine.Command{Kind: engine.CmdAddInstrument, Client: 9, Spec: book.Instrument{Symbol: "CCC", TickSize: 0, LotSize: 1}}),
		ev{kind: engine.EvReject, client: 9, reason: book.RejectInvalidInstrument})
	if d.e.NumInstruments() != 2 {
		t.Fatalf("NumInstruments = %d after rejected adds, want 2", d.e.NumInstruments())
	}
	// The admin endpoint (milestone 7) matches its answer by client and
	// request id, so InstrumentAdded echoes them, and every event names its
	// command's kind.
	added := d.step(engine.Command{Kind: engine.CmdAddInstrument, Client: 9, ClOrdID: 77, Spec: book.Instrument{Symbol: "DDD", TickSize: 1, LotSize: 1}})
	if len(added) != 1 || added[0].Kind != engine.EvInstrumentAdded || added[0].Client != 9 || added[0].ClOrdID != 77 ||
		added[0].Instrument != 3 || added[0].Cmd != engine.CmdAddInstrument {
		t.Fatalf("add DDD: %+v", added)
	}

	// Orders and cancels for ids that don't exist.
	for _, bad := range []engine.InstrumentID{0, 4, 1 << 31} {
		expect(t, d.step(limit(bad, 1, 1, buy, 10, 1)), ev{kind: engine.EvReject, client: 1, clOrd: 1, reason: book.RejectUnknownInstrument})
		expect(t, d.step(cancel(bad, 1, 1)), ev{kind: engine.EvReject, client: 1, order: 1, reason: book.RejectUnknownInstrument})
	}

	// Books are independent: a buy on BBB doesn't see a sell on AAA, and
	// each instrument numbers its own market data from 1.
	d.step(limit(a, 1, 1, sell, 100, 5))
	expect(t, d.step(limit(b, 2, 1, buy, 100, 5)), ack(2, 1, 2, buy, 100, 5), delta(buy, 100, 5, 1))
}

func TestOrderIDsAreGapFreeAcrossRejects(t *testing.T) {
	d := newDriver(t)
	x := d.addInstrument("XYZ")
	var ids []book.OrderID
	for i := range 6 {
		c := limit(x, 1, book.ClientOrderID(i+1), buy, 50, 1)
		if i%2 == 1 {
			c.Qty = 0 // invalid: rejected, must not consume an id
		}
		for _, e := range d.step(c) {
			if e.Kind == engine.EvAck {
				ids = append(ids, e.OrderID)
			}
		}
	}
	if want := []book.OrderID{1, 2, 3}; !slices.Equal(ids, want) {
		t.Fatalf("acked ids %v, want %v", ids, want)
	}
	if d.e.LastOrderID() != 3 {
		t.Fatalf("LastOrderID = %d, want 3", d.e.LastOrderID())
	}
}

func TestEventsCarryCommandSeqAndTime(t *testing.T) {
	d := newDriver(t)
	x := d.addInstrument("XYZ")
	d.step(limit(x, 1, 1, sell, 100, 5))
	evs := d.step(market(x, 2, 1, buy, 9)) // Ack, 2 fills, trade, delta, expired
	if len(evs) != 6 {
		t.Fatalf("got %d events", len(evs))
	}
	for _, e := range evs {
		if e.Seq != 3 || e.Time != 3_000_000 {
			t.Fatalf("event %v has seq=%d time=%d, want 3 and 3000000", e.Kind, e.Seq, e.Time)
		}
	}
	if d.e.LastSeq() != 3 {
		t.Fatalf("LastSeq = %d", d.e.LastSeq())
	}
}

func TestStepPanicsOnSeqGapOrRepeat(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  engine.Seq
	}{{"gap", 3}, {"repeat", 1}, {"zero", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			e := engine.New()
			first := engine.Command{Seq: 1, Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "X", TickSize: 1, LotSize: 1}}
			e.Step(&first, nil)
			defer func() {
				if recover() == nil {
					t.Fatalf("seq %d after 1 did not panic", tc.seq)
				}
				if e.LastSeq() != 1 {
					t.Fatalf("LastSeq moved to %d on a rejected seq", e.LastSeq())
				}
			}()
			c := limit(1, 1, 1, buy, 10, 1)
			c.Seq = tc.seq
			e.Step(&c, nil)
		})
	}
}

func TestStepPanicsOnUnknownKind(t *testing.T) {
	e := engine.New()
	defer func() {
		if recover() == nil {
			t.Fatal("unknown command kind did not panic")
		}
	}()
	e.Step(&engine.Command{Seq: 1, Kind: 99}, nil)
}
