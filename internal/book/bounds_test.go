package book_test

import (
	"math"
	"testing"

	"github.com/tanxena/matchd/internal/book"
)

// Review F-002: quantities and prices have upper bounds (decision 003), so
// no level total or fill-or-kill sum can overflow int64. Both books.
func TestQuantityAndPriceBounds(t *testing.T) {
	for name, mk := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			b := mk()
			for _, tc := range []struct {
				o    book.Order
				want book.RejectReason
			}{
				{sell(1, 1, 100, book.MaxQty+1), book.RejectInvalidQty},
				{sell(2, 1, 100, math.MaxInt64), book.RejectInvalidQty},
				{sell(3, 1, book.MaxPrice+1, 1), book.RejectInvalidPrice},
				{sell(4, 1, math.MaxInt64, 1), book.RejectInvalidPrice},
				{book.Order{ID: 5, Client: 1, ClOrdID: 5, Side: book.Buy, Type: book.Market, Qty: book.MaxQty + 1}, book.RejectInvalidQty},
			} {
				if res, _ := b.Submit(tc.o, nil); res.Reject != tc.want {
					t.Errorf("%+v: %v, want %v", tc.o, res.Reject, tc.want)
				}
			}

			// At the bounds: accepted, and a level holding several orders of
			// MaxQty keeps an exact, positive total.
			const n = 8
			for i := range n {
				if res, _ := b.Submit(sell(book.OrderID(10+i), 1, book.MaxPrice, book.MaxQty), nil); res.Reject != book.RejectNone {
					t.Fatalf("MaxQty at MaxPrice rejected: %v", res.Reject)
				}
			}
			if px, qty, ok := b.Best(book.Sell); !ok || px != book.MaxPrice || qty != n*book.MaxQty {
				t.Fatalf("best ask %d x %d (ok %v), want %d x %d", px, qty, ok, book.MaxPrice, n*book.MaxQty)
			}
			// The reviewer's case: a FOK against that level fills.
			fok := buy(30, 2, book.MaxPrice, 3)
			fok.TIF = book.FOK
			if res, _ := b.Submit(fok, nil); res.Reject != book.RejectNone || res.Filled != 3 {
				t.Fatalf("FOK for 3 against %d x MaxQty: %+v", n, res)
			}
			if err := b.CheckInvariants(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
