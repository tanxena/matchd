package ref

import (
	"testing"

	"github.com/tanxena/matchd/internal/book"
)

// The reference book's behaviour is tested against hand-written expectations
// in internal/book/scenario_test.go, which runs every scenario on both books.
// This file checks the one piece that suite can't reach: that the oracle's
// own invariant checker detects corruption. An oracle whose self-check can't
// fail would quietly accept a broken reference.

func setup(t *testing.T) *Book {
	t.Helper()
	b, err := New(book.Instrument{Symbol: "TEST", TickSize: 1, LotSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	orders := []book.Order{
		{ID: 1, Client: 1, ClOrdID: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 5},
		{ID: 2, Client: 1, ClOrdID: 2, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 99, Qty: 3},
		{ID: 3, Client: 2, ClOrdID: 1, Side: book.Sell, Type: book.Limit, TIF: book.GTC, Price: 105, Qty: 2},
	}
	for _, o := range orders {
		if r, _ := b.Submit(o, nil); r.Reject != book.RejectNone {
			t.Fatalf("setup order %d rejected: %v", o.ID, r.Reject)
		}
	}
	if err := b.CheckInvariants(); err != nil {
		t.Fatalf("clean book failed invariants: %v", err)
	}
	return b
}

func TestCheckInvariantsDetectsCorruption(t *testing.T) {
	corruptions := map[string]func(b *Book){
		"crossed":      func(b *Book) { b.asks[0].Price = 100 },
		"out of order": func(b *Book) { b.bids[0].Price, b.bids[1].Price = 99, 100 },
		"zero qty":     func(b *Book) { b.bids[1].Qty = 0 },
		"wrong side":   func(b *Book) { b.asks[0].Side = book.Buy },
		"duplicate id": func(b *Book) { b.asks[0].ID = 1 },
	}
	for name, corrupt := range corruptions {
		t.Run(name, func(t *testing.T) {
			b := setup(t)
			corrupt(b)
			if err := b.CheckInvariants(); err == nil {
				t.Fatalf("corruption %q not detected", name)
			} else {
				t.Logf("detected: %v", err)
			}
		})
	}
}

// Rests must go behind every order at an equal price: the one-loop
// expression of time priority that the whole oracle depends on.
func TestRestJoinsBackOfEqualPrice(t *testing.T) {
	b := setup(t)
	o := book.Order{ID: 4, Client: 3, ClOrdID: 1, Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}
	b.Submit(o, nil)
	var ids []book.OrderID
	for _, r := range b.bids {
		ids = append(ids, r.ID)
	}
	want := []book.OrderID{1, 4, 2} // 100 (old), 100 (new), 99
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("bid order = %v, want %v", ids, want)
	}
}
