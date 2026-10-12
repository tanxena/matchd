package book

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/tanxena/matchd/internal/testutil"
)

// White-box tests for copy-on-write snapshots (cow.go, decision 013). The
// differential harness also freezes and thaws mid-stream, but its books stay
// within one chunk; these build books several chunks deep.

// bigBook rests n orders on each side across 50 price levels, ids from 1.
func bigBook(t *testing.T, n int) *Book {
	t.Helper()
	b := mustNew(t)
	for i := range n {
		id := OrderID(2*i + 1)
		b.Submit(Order{ID: id, Client: ClientID(1 + i%7), ClOrdID: ClientOrderID(id), Side: Buy, Type: Limit, TIF: GTC,
			Price: Price(900 + i%50), Qty: Qty(1 + i%9)}, nil)
		b.Submit(Order{ID: id + 1, Client: ClientID(1 + i%7), ClOrdID: ClientOrderID(id + 1), Side: Sell, Type: Limit, TIF: GTC,
			Price: Price(1000 + i%50), Qty: Qty(1 + i%9)}, nil)
	}
	return b
}

func frozenOrders(f *Frozen) [2][]RestingOrder {
	var out [2][]RestingOrder
	f.Each(Buy, func(r RestingOrder) { out[0] = append(out[0], r) })
	f.Each(Sell, func(r RestingOrder) { out[1] = append(out[1], r) })
	return out
}

// churn applies random cancels, new orders and crossing orders.
func churn(t *testing.T, b *Book, rng *rand.Rand, nextID *OrderID, ops int) {
	t.Helper()
	var fills []Fill
	for range ops {
		*nextID++
		switch rng.IntN(3) {
		case 0: // cancel a random id (often already gone)
			id := OrderID(1 + rng.IntN(int(*nextID)))
			if b.Live(id) {
				b.Cancel(b.node(b.mustSlot(t, id)).client, id)
			}
		case 1: // rest somewhere in the book
			side := Buy
			px := Price(900 + rng.IntN(50))
			if rng.IntN(2) == 0 {
				side, px = Sell, Price(1000+rng.IntN(50))
			}
			b.Submit(Order{ID: *nextID, Client: 9, ClOrdID: ClientOrderID(*nextID), Side: side, Type: Limit, TIF: GTC, Price: px, Qty: 3}, fills[:0])
		case 2: // cross a few levels
			side := Buy
			if rng.IntN(2) == 0 {
				side = Sell
			}
			_, fills = b.Submit(Order{ID: *nextID, Client: 9, ClOrdID: ClientOrderID(*nextID), Side: side, Type: Market, Qty: Qty(1 + rng.IntN(60))}, fills[:0])
		}
	}
}

func (b *Book) mustSlot(t *testing.T, id OrderID) uint32 {
	t.Helper()
	i, ok := b.byID.get(id)
	if !ok {
		t.Fatalf("order %d not live", id)
	}
	return i
}

func TestFrozenViewSurvivesHeavyTradingAcrossChunks(t *testing.T) {
	b := bigBook(t, 10_000) // 20,000 orders: 5 chunks
	if len(b.chunks) < 5 {
		t.Fatalf("only %d chunks", len(b.chunks))
	}
	rng := rand.New(rand.NewPCG(1, 1))
	nextID := OrderID(20_000)
	for cycle := range 3 {
		f := b.Freeze()
		want := [2][]RestingOrder{b.Orders(Buy, nil), b.Orders(Sell, nil)}
		copiesBefore := b.CowCopies()
		churn(t, b, rng, &nextID, 20_000)
		if err := b.CheckInvariants(); err != nil {
			t.Fatalf("cycle %d, while frozen: %v", cycle, err)
		}
		got := frozenOrders(f)
		if !slices.Equal(got[0], want[0]) || !slices.Equal(got[1], want[1]) {
			t.Fatalf("cycle %d: frozen view changed (%d/%d bids, %d/%d asks)", cycle, len(got[0]), len(want[0]), len(got[1]), len(want[1]))
		}
		copies := b.CowCopies() - copiesBefore
		if copies == 0 || copies > uint64(len(f.chunks)) {
			t.Fatalf("cycle %d: %d chunk copies for %d shared chunks; want 1..%d (each at most once)", cycle, copies, len(f.chunks), len(f.chunks))
		}
		shared := len(f.chunks)
		spareBefore := len(b.spare)
		b.Thaw(f)
		if len(b.spare) != spareBefore+int(copies) {
			t.Fatalf("cycle %d: thaw returned %d chunks to the pool, want %d", cycle, len(b.spare)-spareBefore, copies)
		}
		if err := b.CheckInvariants(); err != nil {
			t.Fatalf("cycle %d, after thaw: %v", cycle, err)
		}
		t.Logf("cycle %d: %d of %d chunks copied", cycle, copies, shared)
	}
}

// Writes outside a snapshot never copy.
func TestNoCopiesWithoutSnapshot(t *testing.T) {
	b := bigBook(t, 5_000)
	rng := rand.New(rand.NewPCG(2, 2))
	nextID := OrderID(10_000)
	f := b.Freeze()
	churn(t, b, rng, &nextID, 1000)
	b.Thaw(f)
	before := b.CowCopies()
	churn(t, b, rng, &nextID, 20_000)
	if n := b.CowCopies() - before; n != 0 {
		t.Fatalf("%d chunk copies with no snapshot in progress", n)
	}
}

func TestFreezeTwicePanics(t *testing.T) {
	b := mustNew(t)
	b.Freeze()
	defer func() {
		if recover() == nil {
			t.Fatal("second Freeze did not panic")
		}
	}()
	b.Freeze()
}

// Load from a frozen view gives a book that behaves exactly like the
// original: same results and fills for the same follow-up commands.
func TestLoadBehavesIdentically(t *testing.T) {
	orig := bigBook(t, 3_000)
	rng := rand.New(rand.NewPCG(3, 3))
	nextID := OrderID(6_000)
	churn(t, orig, rng, &nextID, 3_000) // partial fills, gaps, mixed queues

	f := orig.Freeze()
	snap := frozenOrders(f)
	loaded, err := Load(orig.Instrument(), snap[0], snap[1])
	orig.Thaw(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Side{Buy, Sell} {
		if !slices.Equal(loaded.Orders(s, nil), orig.Orders(s, nil)) {
			t.Fatalf("%s side differs after load", s)
		}
	}
	// Same commands to both: every result and fill must match.
	var fa, fb []Fill
	for i := range 5_000 {
		nextID++
		o := Order{ID: nextID, Client: ClientID(1 + rng.IntN(7)), ClOrdID: ClientOrderID(nextID), Side: Buy, Type: Limit, TIF: GTC,
			Price: Price(940 + rng.IntN(80)), Qty: Qty(1 + rng.IntN(20))}
		if rng.IntN(2) == 0 {
			o.Side = Sell
		}
		var ra, rb2 Result
		ra, fa = orig.Submit(o, fa[:0])
		rb2, fb = loaded.Submit(o, fb[:0])
		if ra != rb2 || !slices.Equal(fa, fb) {
			t.Fatalf("command %d: original %+v %v, loaded %+v %v", i, ra, fa, rb2, fb)
		}
		if rng.IntN(4) == 0 {
			id := OrderID(1 + rng.IntN(int(nextID)))
			if orig.Live(id) {
				cl := orig.node(orig.mustSlot(t, id)).client
				if orig.Cancel(cl, id) != loaded.Cancel(cl, id) {
					t.Fatalf("cancel %d differs", id)
				}
			}
		}
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	ok := RestingOrder{ID: 1, Client: 1, ClOrdID: 1, Side: Buy, Price: 100, Qty: 5}
	cases := map[string][2][]RestingOrder{
		"wrong side":         {{{ID: 1, Client: 1, ClOrdID: 1, Side: Sell, Price: 100, Qty: 5}}, nil},
		"zero qty":           {{{ID: 1, Client: 1, ClOrdID: 1, Side: Buy, Price: 100, Qty: 0}}, nil},
		"not priority order": {{{ID: 1, Client: 1, ClOrdID: 1, Side: Buy, Price: 99, Qty: 1}, {ID: 2, Client: 1, ClOrdID: 2, Side: Buy, Price: 100, Qty: 1}}, nil},
		"duplicate id":       {{ok, {ID: 1, Client: 1, ClOrdID: 2, Side: Buy, Price: 100, Qty: 1}}, nil},
		"duplicate clordid":  {{ok, {ID: 2, Client: 1, ClOrdID: 1, Side: Buy, Price: 100, Qty: 1}}, nil},
		"crossed":            {{ok}, {{ID: 2, Client: 2, ClOrdID: 1, Side: Sell, Price: 100, Qty: 1}}},
	}
	for name, c := range cases {
		if _, err := Load(testInstr, c[0], c[1]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// PRD 6: trading must not allocate while a snapshot is in progress either.
// Freeze pre-fills the spare pool, so copy-on-write takes chunks from it.
func TestZeroAllocWhileFrozen(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation allocates")
	}
	b := bigBook(t, 10_000)
	var fills []Fill
	id := OrderID(100_000)
	cycle := func() {
		id++
		b.Submit(Order{ID: id, Client: 1, ClOrdID: ClientOrderID(id), Side: Sell, Type: Limit, TIF: GTC, Price: 960, Qty: 5}, nil)
		id++
		_, fills = b.Submit(Order{ID: id, Client: 2, ClOrdID: ClientOrderID(id), Side: Buy, Type: Limit, TIF: GTC, Price: 960, Qty: 5}, fills[:0])
		id++
		b.Submit(Order{ID: id, Client: 3, ClOrdID: ClientOrderID(id), Side: Buy, Type: Limit, TIF: GTC, Price: 950, Qty: 5}, nil)
		b.Cancel(3, id)
	}
	for range 100 { // warm up scratch buffers
		cycle()
	}
	f := b.Freeze()
	defer b.Thaw(f)
	if sites := testutil.OwnAllocations(10_000, cycle); len(sites) > 0 {
		t.Fatalf("allocations while frozen:\n%s", strings.Join(sites, "\n"))
	}
	if b.CowCopies() == 0 {
		t.Fatal("no chunk was copied; the test didn't exercise copy-on-write")
	}
}
