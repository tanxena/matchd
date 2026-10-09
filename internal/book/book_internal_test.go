package book

import "testing"

// White-box tests for the storage structures. Behavioural tests (matching
// rules, rejects) live in scenario_test.go and diff_test.go and run against
// the reference book too.

var testInstr = Instrument{Symbol: "TEST", TickSize: 1, LotSize: 1}

func mustNew(t *testing.T) *Book {
	t.Helper()
	b, err := New(testInstr)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func ladderPrices(l *ladder) []Price {
	out := make([]Price, len(l.levels))
	for i, lv := range l.levels {
		out[i] = lv.price
	}
	return out
}

func TestLadderOrderingBestLast(t *testing.T) {
	b := mustNew(t)
	id := OrderID(0)
	for _, px := range []Price{100, 98, 102, 99, 101} {
		id++
		b.Submit(Order{ID: id, Client: 1, ClOrdID: ClientOrderID(id), Side: Buy, Type: Limit, TIF: GTC, Price: px, Qty: 1}, nil)
		id++
		b.Submit(Order{ID: id, Client: 1, ClOrdID: ClientOrderID(id), Side: Sell, Type: Limit, TIF: GTC, Price: px + 10, Qty: 1}, nil)
	}
	wantBids := []Price{98, 99, 100, 101, 102}   // ascending: highest (best) last
	wantAsks := []Price{112, 111, 110, 109, 108} // descending: lowest (best) last
	if got := ladderPrices(&b.bids); !equalPrices(got, wantBids) {
		t.Errorf("bid ladder = %v, want %v", got, wantBids)
	}
	if got := ladderPrices(&b.asks); !equalPrices(got, wantAsks) {
		t.Errorf("ask ladder = %v, want %v", got, wantAsks)
	}
	if err := b.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

func equalPrices(a, b []Price) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// After the book has grown once, add/cancel cycles must reuse slab slots and
// level structs instead of growing.
func TestSlabAndLevelReuse(t *testing.T) {
	b := mustNew(t)
	const n = 64
	cycle := func(base OrderID) {
		for i := range OrderID(n) {
			px := Price(100 + i%8)
			b.Submit(Order{ID: base + i, Client: 1, ClOrdID: ClientOrderID(i), Side: Buy, Type: Limit, TIF: GTC, Price: px, Qty: 5}, nil)
		}
		for i := range OrderID(n) {
			if r := b.Cancel(1, base+i); r.Reject != RejectNone {
				t.Fatalf("cancel %d: %v", base+i, r.Reject)
			}
		}
	}
	cycle(1)
	slots, levels := len(b.nodes), len(b.freeLevels)
	for k := range OrderID(10) {
		cycle(1000 * (k + 2))
		if err := b.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
	}
	if len(b.nodes) != slots {
		t.Errorf("slab grew from %d to %d slots across reuse cycles", slots, len(b.nodes))
	}
	if len(b.freeLevels) != levels {
		t.Errorf("free level list went from %d to %d", levels, len(b.freeLevels))
	}
	if b.Len() != 0 {
		t.Errorf("book has %d orders, want 0", b.Len())
	}
}

func TestLevelQty(t *testing.T) {
	b := mustNew(t)
	b.Submit(Order{ID: 1, Client: 1, ClOrdID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 4}, nil)
	b.Submit(Order{ID: 2, Client: 1, ClOrdID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 6}, nil)
	b.Submit(Order{ID: 3, Client: 1, ClOrdID: 3, Side: Buy, Type: Limit, TIF: GTC, Price: 98, Qty: 1}, nil)
	cases := []struct {
		side Side
		px   Price
		want Qty
	}{
		{Buy, 100, 10},
		{Buy, 98, 1},
		{Buy, 99, 0},   // gap between levels
		{Buy, 101, 0},  // above best
		{Buy, 50, 0},   // below worst
		{Sell, 100, 0}, // other side
	}
	for _, c := range cases {
		if got := b.LevelQty(c.side, c.px); got != c.want {
			t.Errorf("LevelQty(%s, %d) = %d, want %d", c.side, c.px, got, c.want)
		}
	}
	b.Cancel(1, 3)
	if got := b.LevelQty(Buy, 98); got != 0 {
		t.Errorf("LevelQty after emptying level = %d, want 0", got)
	}
}

// Fully filled makers must release their slots too, not just cancels.
func TestFilledMakerReleasesSlot(t *testing.T) {
	b := mustNew(t)
	b.Submit(Order{ID: 1, Client: 1, ClOrdID: 1, Side: Sell, Type: Limit, TIF: GTC, Price: 100, Qty: 3}, nil)
	b.Submit(Order{ID: 2, Client: 2, ClOrdID: 1, Side: Buy, Type: Market, Qty: 3}, nil)
	if b.Live(1) || b.Len() != 0 {
		t.Fatalf("maker still live after full fill")
	}
	if b.freeNode != 1 {
		t.Errorf("freeNode = %d, want slot 1 back on the free list", b.freeNode)
	}
	if err := b.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
}

// CheckInvariants must actually detect corruption, or every test that relies
// on it proves nothing.
func TestCheckInvariantsDetectsCorruption(t *testing.T) {
	setup := func() *Book {
		b := mustNew(t)
		b.Submit(Order{ID: 1, Client: 1, ClOrdID: 1, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 5}, nil)
		b.Submit(Order{ID: 2, Client: 1, ClOrdID: 2, Side: Buy, Type: Limit, TIF: GTC, Price: 100, Qty: 7}, nil)
		b.Submit(Order{ID: 3, Client: 1, ClOrdID: 3, Side: Sell, Type: Limit, TIF: GTC, Price: 105, Qty: 2}, nil)
		if err := b.CheckInvariants(); err != nil {
			t.Fatalf("clean book failed invariants: %v", err)
		}
		return b
	}
	corruptions := map[string]func(b *Book){
		"level total":     func(b *Book) { b.bids.levels[0].total++ },
		"level count":     func(b *Book) { b.bids.levels[0].count++ },
		"crossed":         func(b *Book) { b.asks.levels[0].price = 100; b.nodes[3].price = 100 },
		"zero qty order":  func(b *Book) { b.nodes[1].qty = 0; b.bids.levels[0].total -= 5 },
		"stale byID":      func(b *Book) { b.byID.put(99, 1) },
		"missing byClOrd": func(b *Book) { b.byClOrd.delete(clientOrderKey{1, 2}) },
		"broken prev":     func(b *Book) { b.nodes[2].prev = nilIdx },
		"tail wrong":      func(b *Book) { b.bids.levels[0].tail = 1 },
		"leaked slot":     func(b *Book) { b.nodes = append(b.nodes, orderNode{}) },
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			b := setup()
			corrupt(b)
			if err := b.CheckInvariants(); err == nil {
				t.Fatalf("corruption %q not detected", name)
			} else {
				t.Logf("detected: %v", err)
			}
		})
	}
}
