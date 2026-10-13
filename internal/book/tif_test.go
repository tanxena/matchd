package book_test

import (
	"testing"

	"github.com/tanxena/matchd/internal/book"
)

// IOC and FOK (decision 020), on both implementations.

func withTIF(o book.Order, tif book.TIF) book.Order { o.TIF = tif; return o }

func TestIOCAndFOK(t *testing.T) {
	for name, mk := range implementations(t) {
		t.Run(name, func(t *testing.T) {
			// Asks: 101 x 5, 102 x 5, 103 x 5.
			setup := func(t *testing.T) orderBook {
				b := mk()
				for i, px := range []book.Price{101, 102, 103} {
					if res, _ := b.Submit(sell(book.OrderID(i+1), 1, px, 5), nil); res.Rested != 5 {
						t.Fatalf("setup: %+v", res)
					}
				}
				return b
			}

			// IOC: trades what it can at its limit, the rest is cancelled
			// on arrival, nothing rests.
			b := setup(t)
			res, fills := b.Submit(withTIF(buy(10, 2, 102, 12), book.IOC), nil)
			if res != (book.Result{Filled: 10, Expired: 2}) || len(fills) != 2 || b.Live(10) {
				t.Fatalf("IOC: %+v, %d fills, live %v", res, len(fills), b.Live(10))
			}
			// An IOC that fills completely has nothing to cancel.
			if res, _ := b.Submit(withTIF(buy(11, 2, 103, 5), book.IOC), nil); res != (book.Result{Filled: 5}) {
				t.Fatalf("IOC filled in full: %+v", res)
			}
			// An IOC that can't trade at all is cancelled whole.
			if res, fills := b.Submit(withTIF(buy(12, 2, 100, 3), book.IOC), nil); res != (book.Result{Expired: 3}) || len(fills) != 0 {
				t.Fatalf("IOC with nothing to trade: %+v", res)
			}

			// FOK that fits exactly across levels: fills in full.
			b = setup(t)
			res, fills = b.Submit(withTIF(buy(20, 2, 103, 15), book.FOK), nil)
			if res != (book.Result{Filled: 15}) || len(fills) != 3 || b.Len() != 0 {
				t.Fatalf("FOK exact: %+v, %d fills, %d left", res, len(fills), b.Len())
			}
			// FOK one lot short: rejected, and the book is untouched.
			b = setup(t)
			before := b.Orders(book.Sell, nil)
			res, fills = b.Submit(withTIF(buy(21, 2, 103, 16), book.FOK), nil)
			if res != (book.Result{Reject: book.RejectFOKNoLiquidity}) || len(fills) != 0 {
				t.Fatalf("FOK short: %+v, %d fills", res, len(fills))
			}
			// FOK with enough quantity, but not within its limit.
			res, _ = b.Submit(withTIF(buy(22, 2, 102, 11), book.FOK), nil)
			if res.Reject != book.RejectFOKNoLiquidity {
				t.Fatalf("FOK beyond its limit: %+v", res)
			}
			if after := b.Orders(book.Sell, nil); len(after) != len(before) || after[0] != before[0] {
				t.Fatalf("a rejected FOK changed the book: %+v", after)
			}
			// The client order id check comes first: a FOK reusing a live
			// order's id is a duplicate, whatever the liquidity.
			b.Submit(buy(30, 3, 90, 1), nil) // rests, clOrdID 30
			dup := withTIF(buy(31, 3, 103, 99), book.FOK)
			dup.ClOrdID = 30
			if res, _ := b.Submit(dup, nil); res.Reject != book.RejectDuplicateClientOrderID {
				t.Fatalf("FOK duplicate clOrdID: %+v", res)
			}

			// IOC and FOK are limit-only.
			for _, tif := range []book.TIF{book.IOC, book.FOK} {
				if res, _ := b.Submit(withTIF(mkt(40, 2, book.Buy, 1), tif), nil); res.Reject != book.RejectInvalidTIF {
					t.Fatalf("market %s: %+v", tif, res)
				}
			}
			// A market order's other TIF values are still ignored.
			if res, _ := b.Submit(withTIF(mkt(41, 2, book.Buy, 1), book.GTC), nil); res.Reject != book.RejectNone {
				t.Fatalf("market gtc: %+v", res)
			}
			if err := b.CheckInvariants(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
