package book_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/book/ref"
)

// orderBook is the behaviour shared by the real book and the reference book.
// Scenario and differential tests are written against it so every rule is
// checked on both implementations.
type orderBook interface {
	Submit(o book.Order, fills []book.Fill) (book.Result, []book.Fill)
	Cancel(client book.ClientID, id book.OrderID) book.CancelResult
	Best(s book.Side) (book.Price, book.Qty, bool)
	Live(id book.OrderID) bool
	Len() int
	Orders(s book.Side, dst []book.RestingOrder) []book.RestingOrder
	CheckInvariants() error
}

var instr = book.Instrument{Symbol: "TEST", TickSize: 1, LotSize: 1}

func implementations(t *testing.T) map[string]func() orderBook {
	t.Helper()
	return map[string]func() orderBook{
		"book": func() orderBook {
			b, err := book.New(instr)
			if err != nil {
				t.Fatal(err)
			}
			return b
		},
		"ref": func() orderBook {
			b, err := ref.New(instr)
			if err != nil {
				t.Fatal(err)
			}
			return b
		},
	}
}

// Order builders. Client order id defaults to the order id so most scenarios
// don't need to think about it.
func buy(id book.OrderID, client book.ClientID, px book.Price, qty book.Qty) book.Order {
	return book.Order{ID: id, Client: client, ClOrdID: book.ClientOrderID(id), Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: px, Qty: qty}
}

func sell(id book.OrderID, client book.ClientID, px book.Price, qty book.Qty) book.Order {
	return book.Order{ID: id, Client: client, ClOrdID: book.ClientOrderID(id), Side: book.Sell, Type: book.Limit, TIF: book.GTC, Price: px, Qty: qty}
}

func mkt(id book.OrderID, client book.ClientID, side book.Side, qty book.Qty) book.Order {
	return book.Order{ID: id, Client: client, ClOrdID: book.ClientOrderID(id), Side: side, Type: book.Market, Qty: qty}
}

func withClOrd(o book.Order, c book.ClientOrderID) book.Order { o.ClOrdID = c; return o }

// fill builds the expected fill for taker t against resting maker m.
func fill(t, m book.Order, px book.Price, qty, makerLeft book.Qty) book.Fill {
	return book.Fill{
		Price: px, Qty: qty, TakerSide: t.Side,
		Taker: t.ID, TakerClient: t.Client,
		Maker: m.ID, MakerClient: m.Client, MakerClOrdID: m.ClOrdID,
		MakerLeft: makerLeft,
	}
}

func resting(o book.Order, qty book.Qty) book.RestingOrder {
	return book.RestingOrder{ID: o.ID, Client: o.Client, ClOrdID: o.ClOrdID, Side: o.Side, Price: o.Price, Qty: qty}
}

// step is one command in a scenario with its expected outcome.
type step struct {
	submit    *book.Order // exactly one of submit / cancel is set
	cancel    *cancelCmd
	want      book.Result
	wantFills []book.Fill
	wantCxl   book.CancelResult
}

type cancelCmd struct {
	client book.ClientID
	id     book.OrderID
}

func sub(o book.Order, want book.Result, fills ...book.Fill) step {
	return step{submit: &o, want: want, wantFills: fills}
}

func cxl(client book.ClientID, id book.OrderID, want book.CancelResult) step {
	return step{cancel: &cancelCmd{client, id}, wantCxl: want}
}

func rested(q book.Qty) book.Result            { return book.Result{Rested: q} }
func filled(q book.Qty) book.Result            { return book.Result{Filled: q} }
func rejected(r book.RejectReason) book.Result { return book.Result{Reject: r} }
func cancelled(q book.Qty) book.CancelResult   { return book.CancelResult{Cancelled: q} }

var unknown = book.CancelResult{Reject: book.RejectUnknownOrder}

type scenario struct {
	name     string
	steps    []step
	wantBids []book.RestingOrder // final state, priority order
	wantAsks []book.RestingOrder
}

func TestScenarios(t *testing.T) {
	// Reused orders. Names say side, price, and what they are for.
	a100 := sell(1, 1, 100, 10)
	a100b := sell(2, 2, 100, 5)
	a101 := sell(3, 1, 101, 7)
	b99 := buy(4, 2, 99, 4)
	b99b := buy(5, 3, 99, 6)
	b98 := buy(6, 1, 98, 3)

	scenarios := []scenario{
		{
			name:     "non-crossing limits rest on both sides",
			steps:    []step{sub(a100, rested(10)), sub(b99, rested(4))},
			wantBids: []book.RestingOrder{resting(b99, 4)},
			wantAsks: []book.RestingOrder{resting(a100, 10)},
		},
		{
			name: "buy sweeps asks from lowest price up, each at the maker's price",
			steps: []step{
				sub(a101, rested(7)), sub(a100, rested(10)),
				sub(buy(10, 3, 105, 12), filled(12),
					fill(buy(10, 3, 105, 12), a100, 100, 10, 0),
					fill(buy(10, 3, 105, 12), a101, 101, 2, 5)),
			},
			wantAsks: []book.RestingOrder{resting(a101, 5)},
		},
		{
			name: "sell sweeps bids from highest price down",
			steps: []step{
				sub(b98, rested(3)), sub(b99, rested(4)),
				sub(sell(10, 3, 90, 5), filled(5),
					fill(sell(10, 3, 90, 5), b99, 99, 4, 0),
					fill(sell(10, 3, 90, 5), b98, 98, 1, 2)),
			},
			wantBids: []book.RestingOrder{resting(b98, 2)},
		},
		{
			name: "FIFO within a level: older order fills first",
			steps: []step{
				sub(a100, rested(10)), sub(a100b, rested(5)),
				sub(buy(10, 3, 100, 12), filled(12),
					fill(buy(10, 3, 100, 12), a100, 100, 10, 0),
					fill(buy(10, 3, 100, 12), a100b, 100, 2, 3)),
			},
			wantAsks: []book.RestingOrder{resting(a100b, 3)},
		},
		{
			name: "partial fill keeps queue position",
			steps: []step{
				sub(a100, rested(10)), sub(a100b, rested(5)),
				sub(buy(10, 3, 100, 4), filled(4), fill(buy(10, 3, 100, 4), a100, 100, 4, 6)),
				// a100 is still ahead of a100b despite being touched.
				sub(buy(11, 3, 100, 8), filled(8),
					fill(buy(11, 3, 100, 8), a100, 100, 6, 0),
					fill(buy(11, 3, 100, 8), a100b, 100, 2, 3)),
			},
			wantAsks: []book.RestingOrder{resting(a100b, 3)},
		},
		{
			name: "limit does not trade through its price; remainder rests",
			steps: []step{
				sub(a100, rested(10)), sub(a101, rested(7)),
				sub(buy(10, 3, 100, 15), book.Result{Filled: 10, Rested: 5},
					fill(buy(10, 3, 100, 15), a100, 100, 10, 0)),
			},
			wantBids: []book.RestingOrder{resting(buy(10, 3, 100, 15), 5)},
			wantAsks: []book.RestingOrder{resting(a101, 7)},
		},
		{
			name:  "market order on empty book is accepted and fully expires",
			steps: []step{sub(mkt(10, 1, book.Buy, 5), book.Result{Expired: 5})},
		},
		{
			name: "market order sweeps all levels and never rests",
			steps: []step{
				sub(a100, rested(10)), sub(a101, rested(7)),
				sub(mkt(10, 3, book.Buy, 20), book.Result{Filled: 17, Expired: 3},
					fill(mkt(10, 3, book.Buy, 20), a100, 100, 10, 0),
					fill(mkt(10, 3, book.Buy, 20), a101, 101, 7, 0)),
			},
		},
		{
			name: "market sell partially consumes a level",
			steps: []step{
				sub(b99, rested(4)), sub(b99b, rested(6)),
				sub(mkt(10, 1, book.Sell, 5), filled(5),
					fill(mkt(10, 1, book.Sell, 5), b99, 99, 4, 0),
					fill(mkt(10, 1, book.Sell, 5), b99b, 99, 1, 5)),
			},
			wantBids: []book.RestingOrder{resting(b99b, 5)},
		},
		{
			name: "cancel removes; second cancel and unknown id are rejected",
			steps: []step{
				sub(a100, rested(10)), sub(a100b, rested(5)),
				cxl(1, a100.ID, cancelled(10)),
				cxl(1, a100.ID, unknown),
				cxl(1, 999, unknown),
			},
			wantAsks: []book.RestingOrder{resting(a100b, 5)},
		},
		{
			name: "cancel is scoped to the owning client",
			steps: []step{
				sub(a100, rested(10)),
				cxl(2, a100.ID, unknown), // client 2 does not own it
			},
			wantAsks: []book.RestingOrder{resting(a100, 10)},
		},
		{
			name: "cancel of a partially filled order removes only the remainder",
			steps: []step{
				sub(a100, rested(10)),
				sub(buy(10, 3, 100, 4), filled(4), fill(buy(10, 3, 100, 4), a100, 100, 4, 6)),
				cxl(1, a100.ID, cancelled(6)),
			},
		},
		{
			name: "cancel of a fully filled order is unknown",
			steps: []step{
				sub(a100b, rested(5)),
				sub(buy(10, 3, 100, 5), filled(5), fill(buy(10, 3, 100, 5), a100b, 100, 5, 0)),
				cxl(2, a100b.ID, unknown),
			},
		},
		{
			name: "duplicate client order id among live orders is rejected",
			steps: []step{
				sub(withClOrd(a100, 77), rested(10)),
				sub(withClOrd(buy(10, 1, 90, 1), 77), rejected(book.RejectDuplicateClientOrderID)),
				// Market orders are new orders too.
				sub(withClOrd(mkt(11, 1, book.Buy, 1), 77), rejected(book.RejectDuplicateClientOrderID)),
				// Same clOrdID, different client: fine.
				sub(withClOrd(buy(12, 2, 90, 1), 77), rested(1)),
			},
			wantBids: []book.RestingOrder{resting(withClOrd(buy(12, 2, 90, 1), 77), 1)},
			wantAsks: []book.RestingOrder{resting(withClOrd(a100, 77), 10)},
		},
		{
			name: "client order id is reusable after cancel and after full fill",
			steps: []step{
				sub(withClOrd(a100, 77), rested(10)),
				cxl(1, a100.ID, cancelled(10)),
				sub(withClOrd(sell(10, 1, 100, 2), 77), rested(2)),
				sub(buy(11, 2, 100, 2), filled(2), fill(buy(11, 2, 100, 2), withClOrd(sell(10, 1, 100, 2), 77), 100, 2, 0)),
				sub(withClOrd(sell(12, 1, 105, 1), 77), rested(1)),
			},
			wantAsks: []book.RestingOrder{resting(withClOrd(sell(12, 1, 105, 1), 77), 1)},
		},
		{
			name:     "day TIF is accepted and rests like GTC",
			steps:    []step{sub(book.Order{ID: 1, Client: 1, ClOrdID: 1, Side: book.Buy, Type: book.Limit, TIF: book.Day, Price: 50, Qty: 2}, rested(2))},
			wantBids: []book.RestingOrder{{ID: 1, Client: 1, ClOrdID: 1, Side: book.Buy, Price: 50, Qty: 2}},
		},
		{
			name: "validation rejects, checked in a fixed order",
			steps: []step{
				sub(book.Order{ID: 1, Client: 1, Type: book.Limit, TIF: book.GTC, Price: 1, Qty: 1}, rejected(book.RejectInvalidSide)),
				sub(book.Order{ID: 2, Client: 1, Side: book.Buy, Type: 9, Qty: 1}, rejected(book.RejectInvalidType)),
				sub(book.Order{ID: 3, Client: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 1, Qty: 0}, rejected(book.RejectInvalidQty)),
				sub(book.Order{ID: 4, Client: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 1, Qty: -3}, rejected(book.RejectInvalidQty)),
				sub(book.Order{ID: 5, Client: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 0, Qty: 1}, rejected(book.RejectInvalidPrice)),
				sub(book.Order{ID: 6, Client: 1, Side: book.Buy, Type: book.Limit, TIF: 0, Price: 1, Qty: 1}, rejected(book.RejectInvalidTIF)),
				sub(book.Order{ID: 7, Client: 1, Side: book.Sell, Type: book.Market, Price: 5, Qty: 1}, rejected(book.RejectInvalidPrice)),
				// Side is checked before qty: both wrong -> InvalidSide.
				sub(book.Order{ID: 8, Client: 1, Type: book.Limit, Qty: 0}, rejected(book.RejectInvalidSide)),
			},
		},
		{
			name: "rejected order does not touch the book",
			steps: []step{
				sub(a100, rested(10)),
				sub(buy(10, 3, 0, 5), rejected(book.RejectInvalidPrice)),
			},
			wantAsks: []book.RestingOrder{resting(a100, 10)},
		},
		{
			name: "new order at an existing level joins its tail",
			steps: []step{
				sub(b99, rested(4)), sub(b98, rested(3)), sub(b99b, rested(6)),
			},
			wantBids: []book.RestingOrder{resting(b99, 4), resting(b99b, 6), resting(b98, 3)},
		},
	}

	for implName, newBook := range implementations(t) {
		for _, sc := range scenarios {
			t.Run(implName+"/"+sc.name, func(t *testing.T) {
				runScenario(t, newBook(), sc)
			})
		}
	}
}

func runScenario(t *testing.T, b orderBook, sc scenario) {
	t.Helper()
	var fills []book.Fill
	for i, st := range sc.steps {
		if st.submit != nil {
			var res book.Result
			res, fills = b.Submit(*st.submit, fills[:0])
			if res != st.want {
				t.Fatalf("step %d submit %+v: result %+v, want %+v", i, *st.submit, res, st.want)
			}
			if !slices.Equal(fills, st.wantFills) {
				t.Fatalf("step %d submit %+v:\n fills %s\n want  %s", i, *st.submit, fmtFills(fills), fmtFills(st.wantFills))
			}
		} else {
			res := b.Cancel(st.cancel.client, st.cancel.id)
			if res != st.wantCxl {
				t.Fatalf("step %d cancel %+v: result %+v, want %+v", i, *st.cancel, res, st.wantCxl)
			}
		}
		if err := b.CheckInvariants(); err != nil {
			t.Fatalf("step %d: invariant: %v", i, err)
		}
	}
	if got := b.Orders(book.Buy, nil); !slices.Equal(got, sc.wantBids) {
		t.Errorf("bids %+v, want %+v", got, sc.wantBids)
	}
	if got := b.Orders(book.Sell, nil); !slices.Equal(got, sc.wantAsks) {
		t.Errorf("asks %+v, want %+v", got, sc.wantAsks)
	}
}

func fmtFills(fs []book.Fill) string {
	if len(fs) == 0 {
		return "[]"
	}
	s := ""
	for _, f := range fs {
		s += fmt.Sprintf("\n   %+v", f)
	}
	return s
}

func TestBestAggregatesLevel(t *testing.T) {
	for name, newBook := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			b := newBook()
			if _, _, ok := b.Best(book.Buy); ok {
				t.Fatal("empty book reports a best bid")
			}
			b.Submit(buy(1, 1, 99, 4), nil)
			b.Submit(buy(2, 2, 99, 6), nil)
			b.Submit(buy(3, 1, 98, 1), nil)
			if px, qty, ok := b.Best(book.Buy); !ok || px != 99 || qty != 10 {
				t.Fatalf("best bid = %d x %d (%v), want 99 x 10", px, qty, ok)
			}
		})
	}
}

func TestNewRejectsBadInstrument(t *testing.T) {
	bad := []book.Instrument{
		{Symbol: "", TickSize: 1, LotSize: 1},
		{Symbol: "X", TickSize: 0, LotSize: 1},
		{Symbol: "X", TickSize: 1, LotSize: -1},
	}
	for _, in := range bad {
		if _, err := book.New(in); err == nil {
			t.Errorf("book.New(%+v) accepted a bad instrument", in)
		}
		if _, err := ref.New(in); err == nil {
			t.Errorf("ref.New(%+v) accepted a bad instrument", in)
		}
	}
}
