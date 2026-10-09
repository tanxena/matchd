// Package ref is a deliberately simple reference order book used only as a
// test oracle for package book.
//
// Every resting order lives in one of two slices kept in priority order (best
// price first, oldest first within a price). Everything is a linear scan. There
// are no indexes, no free lists and no linked lists, so there is very little
// that can be subtly wrong. It is never benchmarked (PRD section 7.7).
//
// It shares only the plain data types from package book. It does not call any
// book logic, including Order.Validate, so a bug in book cannot leak into the
// oracle.
package ref

import (
	"fmt"
	"slices"

	"github.com/tanxena/matchd/internal/book"
)

// Book is the reference order book.
type Book struct {
	instr book.Instrument
	bids  []book.RestingOrder // highest price first; FIFO within a price
	asks  []book.RestingOrder // lowest price first; FIFO within a price
}

// New returns an empty reference book.
func New(instr book.Instrument) (*Book, error) {
	if err := instr.Validate(); err != nil {
		return nil, err
	}
	return &Book{instr: instr}, nil
}

func (b *Book) side(s book.Side) *[]book.RestingOrder {
	if s == book.Buy {
		return &b.bids
	}
	return &b.asks
}

// better reports whether price a has strictly higher priority than price b
// on side s.
func better(s book.Side, a, b book.Price) bool {
	if s == book.Buy {
		return a > b
	}
	return a < b
}

func validate(o book.Order) book.RejectReason {
	if o.Side != book.Buy && o.Side != book.Sell {
		return book.RejectInvalidSide
	}
	if o.Type != book.Limit && o.Type != book.Market {
		return book.RejectInvalidType
	}
	if o.Qty <= 0 {
		return book.RejectInvalidQty
	}
	if o.Type == book.Limit {
		if o.Price <= 0 {
			return book.RejectInvalidPrice
		}
		if o.TIF != book.GTC && o.TIF != book.Day {
			return book.RejectInvalidTIF
		}
	} else if o.Price != 0 {
		return book.RejectInvalidPrice
	}
	return book.RejectNone
}

// Submit processes a new order, appending fills to fills.
func (b *Book) Submit(o book.Order, fills []book.Fill) (book.Result, []book.Fill) {
	if r := validate(o); r != book.RejectNone {
		return book.Result{Reject: r}, fills
	}
	for _, list := range [][]book.RestingOrder{b.bids, b.asks} {
		for _, r := range list {
			if r.Client == o.Client && r.ClOrdID == o.ClOrdID {
				return book.Result{Reject: book.RejectDuplicateClientOrderID}, fills
			}
		}
	}

	remaining := o.Qty
	opp := b.side(o.Side.Opposite())
	for remaining > 0 && len(*opp) > 0 {
		m := &(*opp)[0] // the single highest-priority resting order
		if o.Type == book.Limit {
			if o.Side == book.Buy && m.Price > o.Price {
				break // cheapest ask is above what the buyer will pay
			}
			if o.Side == book.Sell && m.Price < o.Price {
				break // highest bid is below what the seller will accept
			}
		}
		q := min(remaining, m.Qty)
		m.Qty -= q
		remaining -= q
		fills = append(fills, book.Fill{
			Price:        m.Price,
			Qty:          q,
			TakerSide:    o.Side,
			Taker:        o.ID,
			TakerClient:  o.Client,
			Maker:        m.ID,
			MakerClient:  m.Client,
			MakerClOrdID: m.ClOrdID,
			MakerLeft:    m.Qty,
		})
		if m.Qty == 0 {
			*opp = slices.Delete(*opp, 0, 1)
		}
	}

	res := book.Result{Filled: o.Qty - remaining}
	if remaining == 0 {
		return res, fills
	}
	if o.Type == book.Market {
		res.Expired = remaining
		return res, fills
	}

	// Rest after every order whose price is at least as good: that puts the
	// new order behind all earlier orders at its own price (time priority).
	own := b.side(o.Side)
	pos := len(*own)
	for i, r := range *own {
		if better(o.Side, o.Price, r.Price) {
			pos = i
			break
		}
	}
	*own = slices.Insert(*own, pos, book.RestingOrder{
		ID: o.ID, Client: o.Client, ClOrdID: o.ClOrdID,
		Side: o.Side, Price: o.Price, Qty: remaining,
	})
	res.Rested = remaining
	return res, fills
}

// Cancel removes order id if it is resting and owned by client.
func (b *Book) Cancel(client book.ClientID, id book.OrderID) book.CancelResult {
	for _, s := range []book.Side{book.Buy, book.Sell} {
		list := b.side(s)
		for i, r := range *list {
			if r.ID == id {
				if r.Client != client {
					return book.CancelResult{Reject: book.RejectUnknownOrder}
				}
				*list = slices.Delete(*list, i, i+1)
				return book.CancelResult{Side: r.Side, Price: r.Price, ClOrdID: r.ClOrdID, Cancelled: r.Qty}
			}
		}
	}
	return book.CancelResult{Reject: book.RejectUnknownOrder}
}

// Best returns the best price on side s and the total quantity at it.
func (b *Book) Best(s book.Side) (px book.Price, qty book.Qty, ok bool) {
	list := *b.side(s)
	if len(list) == 0 {
		return 0, 0, false
	}
	px = list[0].Price
	for _, r := range list {
		if r.Price != px {
			break
		}
		qty += r.Qty
	}
	return px, qty, true
}

// Live reports whether order id is resting.
func (b *Book) Live(id book.OrderID) bool {
	for _, list := range [][]book.RestingOrder{b.bids, b.asks} {
		for _, r := range list {
			if r.ID == id {
				return true
			}
		}
	}
	return false
}

// Len returns the number of resting orders.
func (b *Book) Len() int { return len(b.bids) + len(b.asks) }

// Orders appends side s's resting orders to dst in priority order.
func (b *Book) Orders(s book.Side, dst []book.RestingOrder) []book.RestingOrder {
	return append(dst, *b.side(s)...)
}

// CheckInvariants verifies the reference book's own structure.
func (b *Book) CheckInvariants() error {
	if bp, _, ok := b.Best(book.Buy); ok {
		if ap, _, ok := b.Best(book.Sell); ok && bp >= ap {
			return fmt.Errorf("ref: crossed book: best bid %d >= best ask %d", bp, ap)
		}
	}
	ids := make(map[book.OrderID]bool)
	for _, s := range []book.Side{book.Buy, book.Sell} {
		list := *b.side(s)
		for i, r := range list {
			if r.Side != s {
				return fmt.Errorf("ref: order %d on wrong side", r.ID)
			}
			if r.Qty <= 0 {
				return fmt.Errorf("ref: order %d resting with qty %d", r.ID, r.Qty)
			}
			if i > 0 && better(s, r.Price, list[i-1].Price) {
				return fmt.Errorf("ref: %s orders out of price order at %d", s, i)
			}
			if ids[r.ID] {
				return fmt.Errorf("ref: order %d appears twice", r.ID)
			}
			ids[r.ID] = true
		}
	}
	return nil
}
