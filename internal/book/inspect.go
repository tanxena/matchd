package book

import "fmt"

// Best returns the best price on side s and the total quantity resting there.
// ok is false if the side is empty.
func (b *Book) Best(s Side) (px Price, qty Qty, ok bool) {
	lv := b.ladderFor(s).best()
	if lv == nil {
		return 0, 0, false
	}
	return lv.price, lv.total, true
}

// LevelQty returns the total quantity resting at price px on side s, or 0 if
// there is no level there. O(log L). The engine calls it after a command to
// report each touched level's new size as a book delta.
func (b *Book) LevelQty(s Side, px Price) Qty {
	lad := b.ladderFor(s)
	if i, found := lad.search(px); found {
		return lad.levels[i].total
	}
	return 0
}

// Live reports whether order id is resting on the book.
func (b *Book) Live(id OrderID) bool {
	_, ok := b.byID.get(id)
	return ok
}

// Len returns the number of resting orders.
func (b *Book) Len() int { return b.byID.len() }

// Orders appends every resting order on side s to dst in priority order (best
// level first, oldest first within a level) and returns the extended slice.
func (b *Book) Orders(s Side, dst []RestingOrder) []RestingOrder {
	lad := b.ladderFor(s)
	for i := len(lad.levels) - 1; i >= 0; i-- {
		for j := lad.levels[i].head; j != nilIdx; j = b.nodes[j].next {
			n := &b.nodes[j]
			dst = append(dst, RestingOrder{
				ID: n.id, Client: n.client, ClOrdID: n.clOrdID,
				Side: n.side, Price: n.price, Qty: n.qty,
			})
		}
	}
	return dst
}

// CheckInvariants walks the whole book and returns the first structural
// inconsistency it finds, or nil. It is O(orders) and allocates; it exists
// for tests, which call it after every command.
//
// Checked:
//   - the book is not crossed: best bid < best ask when both sides exist
//   - each side's levels are strictly ordered with the best last, and none is empty
//   - each level's total equals the sum of its orders' qty, and its count matches
//   - each queue's prev/next links are consistent with head and tail
//   - every order has qty > 0 and the side and price of the level holding it
//   - no order appears twice, and the id and client-order-id indexes contain
//     exactly the resting orders (so cancelled or filled ids are gone)
//   - every slab slot is either resting or on the free list, never both
func (b *Book) CheckInvariants() error {
	if bp, _, ok := b.Best(Buy); ok {
		if ap, _, ok := b.Best(Sell); ok && bp >= ap {
			return fmt.Errorf("crossed book: best bid %d >= best ask %d", bp, ap)
		}
	}

	seen := make(map[uint32]bool, b.byID.len())
	for _, side := range [2]Side{Buy, Sell} {
		lad := b.ladderFor(side)
		for i, lv := range lad.levels {
			if lv == nil {
				return fmt.Errorf("%s level %d is nil", side, i)
			}
			if i > 0 && lad.key(lad.levels[i-1].price) >= lad.key(lv.price) {
				return fmt.Errorf("%s levels out of order at %d: %d then %d", side, i, lad.levels[i-1].price, lv.price)
			}
			if lv.head == nilIdx || lv.count == 0 {
				return fmt.Errorf("%s level %d is empty but still in the ladder", side, lv.price)
			}
			var sum Qty
			var count uint32
			prev := nilIdx
			for j := lv.head; j != nilIdx; j = b.nodes[j].next {
				if int(j) >= len(b.nodes) {
					return fmt.Errorf("%s level %d links to slot %d beyond slab", side, lv.price, j)
				}
				if seen[j] {
					return fmt.Errorf("slot %d appears twice (cycle or shared node)", j)
				}
				seen[j] = true
				n := &b.nodes[j]
				if n.prev != prev {
					return fmt.Errorf("order %d prev=%d, want %d", n.id, n.prev, prev)
				}
				if n.qty <= 0 {
					return fmt.Errorf("order %d resting with qty %d", n.id, n.qty)
				}
				if n.side != side || n.price != lv.price {
					return fmt.Errorf("order %d (%s @%d) in %s level %d", n.id, n.side, n.price, side, lv.price)
				}
				if got, ok := b.byID.get(n.id); !ok || got != j {
					return fmt.Errorf("order %d: byID has (%d,%v), want slot %d", n.id, got, ok, j)
				}
				if got, ok := b.byClOrd.get(clientOrderKey{n.client, n.clOrdID}); !ok || got != j {
					return fmt.Errorf("order %d: byClOrd has (%d,%v), want slot %d", n.id, got, ok, j)
				}
				sum += n.qty
				count++
				prev = j
			}
			if lv.tail != prev {
				return fmt.Errorf("%s level %d tail=%d, want %d", side, lv.price, lv.tail, prev)
			}
			if sum != lv.total {
				return fmt.Errorf("%s level %d total=%d, sum of orders=%d", side, lv.price, lv.total, sum)
			}
			if count != lv.count {
				return fmt.Errorf("%s level %d count=%d, walked %d", side, lv.price, lv.count, count)
			}
		}
	}

	if len(seen) != b.byID.len() {
		return fmt.Errorf("byID has %d entries, %d orders on the book", b.byID.len(), len(seen))
	}
	if len(seen) != b.byClOrd.len() {
		return fmt.Errorf("byClOrd has %d entries, %d orders on the book", b.byClOrd.len(), len(seen))
	}

	free := 0
	for j := b.freeNode; j != nilIdx; j = b.nodes[j].next {
		if seen[j] {
			return fmt.Errorf("free slot %d is also resting, or the free list has a cycle", j)
		}
		seen[j] = true
		free++
	}
	if live := b.byID.len(); live+free != len(b.nodes)-1 {
		return fmt.Errorf("slab leak: %d live + %d free != %d slots", live, free, len(b.nodes)-1)
	}
	return nil
}
