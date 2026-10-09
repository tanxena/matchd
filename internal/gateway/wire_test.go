package gateway

import (
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/proto"
)

// The gateway converts engine enums to wire values by numeric conversion
// (proto.Reason(ev.Reason), uint8(ev.Side), ...). That is only correct while
// both sides use the same numbers. The wire owns its numbering (decision
// 008); this test is what stops either side drifting silently.
func TestEnumsMatchWire(t *testing.T) {
	pairs := []struct {
		name       string
		engine, pr uint8
	}{
		{"buy", uint8(book.Buy), proto.SideBuy},
		{"sell", uint8(book.Sell), proto.SideSell},
		{"limit", uint8(book.Limit), proto.TypeLimit},
		{"market", uint8(book.Market), proto.TypeMarket},
		{"gtc", uint8(book.GTC), proto.TIFGTC},
		{"day", uint8(book.Day), proto.TIFDay},
		{"taker", uint8(engine.RoleTaker), proto.RoleTaker},
		{"maker", uint8(engine.RoleMaker), proto.RoleMaker},
		{"cancel requested", uint8(engine.CancelRequested), proto.CancelRequested},
		{"cancel expired", uint8(engine.CancelExpired), proto.CancelExpired},
	}
	for _, p := range pairs {
		if p.engine != p.pr {
			t.Errorf("%s: engine %d, wire %d", p.name, p.engine, p.pr)
		}
	}
	// Every engine reject reason must exist on the wire with the same number
	// and the same name, and stay below the gateway's range (100+).
	for r := book.RejectInvalidSide; r <= book.RejectDuplicateSymbol; r++ {
		w := proto.Reason(r)
		if w.String() != r.String() {
			t.Errorf("reason %d: engine %q, wire %q", r, r, w)
		}
		if w >= proto.ReasonNotLoggedIn {
			t.Errorf("engine reason %d collides with the gateway's range", r)
		}
	}
	if s := (book.RejectDuplicateSymbol + 1).String(); s[:7] != "reject(" {
		t.Errorf("book has a reason after DuplicateSymbol (%s); add it to the wire and extend this test", s)
	}
}

// Every engine event kind a client can receive encodes to the matching wire
// message, with every field carried across.
func TestAppendEvent(t *testing.T) {
	base := engine.Event{Seq: 7, Time: 99, Instrument: 3, Client: 5, ClOrdID: 11, OrderID: 13, Side: book.Sell, Price: 1001, Qty: 4}
	frame := func(ev engine.Event) (proto.MsgType, []byte) {
		b := appendEvent(nil, &ev)
		return proto.MsgType(b[5]), b[proto.HeaderSize:]
	}

	e := base
	e.Kind = engine.EvAck
	typ, p := frame(e)
	var ack proto.Ack
	if typ != proto.TypeAck || ack.Decode(p) != nil ||
		ack != (proto.Ack{Seq: 7, Time: 99, Instrument: 3, ClOrdID: 11, OrderID: 13, Side: proto.SideSell, Price: 1001, Qty: 4}) {
		t.Errorf("ack: %v %+v", typ, ack)
	}

	e = base
	e.Kind, e.MatchID, e.Role, e.Leaves = engine.EvFill, 21, engine.RoleMaker, 2
	typ, p = frame(e)
	var fill proto.Fill
	if typ != proto.TypeFill || fill.Decode(p) != nil ||
		fill != (proto.Fill{Seq: 7, Time: 99, Instrument: 3, ClOrdID: 11, OrderID: 13, MatchID: 21, Side: proto.SideSell,
			Role: proto.RoleMaker, Price: 1001, Qty: 4, Leaves: 2}) {
		t.Errorf("fill: %v %+v", typ, fill)
	}

	e = base
	e.Kind, e.Reason = engine.EvReject, book.RejectUnknownOrder
	typ, p = frame(e)
	var rej proto.Reject
	if typ != proto.TypeReject || rej.Decode(p) != nil ||
		rej != (proto.Reject{Seq: 7, Time: 99, Instrument: 3, ClOrdID: 11, OrderID: 13, Reason: proto.ReasonUnknownOrder}) {
		t.Errorf("reject: %v %+v", typ, rej)
	}

	e = base
	e.Kind, e.CancelReason = engine.EvCancelled, engine.CancelExpired
	typ, p = frame(e)
	var ca proto.CancelAck
	if typ != proto.TypeCancelAck || ca.Decode(p) != nil ||
		ca != (proto.CancelAck{Seq: 7, Time: 99, Instrument: 3, ClOrdID: 11, OrderID: 13, Side: proto.SideSell,
			Reason: proto.CancelExpired, Price: 1001, Qty: 4}) {
		t.Errorf("cancel ack: %v %+v", typ, ca)
	}

	for _, k := range []engine.EventKind{engine.EvBookDelta, engine.EvInstrumentAdded} {
		e = base
		e.Kind = k
		if b := appendEvent(nil, &e); len(b) != 0 {
			t.Errorf("%v encoded to %d bytes; it must never reach a client session", k, len(b))
		}
	}
}
