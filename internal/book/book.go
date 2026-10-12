package book

import "fmt"

// nilIdx is the "no node" value for slab indices. Slot 0 of the slab is
// reserved and never holds an order, so 0 can mean nil (decision 002).
const nilIdx uint32 = 0

// orderNode is one resting order, stored by value in a chunk of the slab
// (cow.go). The orders at a price level form a doubly linked FIFO list
// through prev/next, which are slab indices rather than pointers (decision
// 002). A free slot reuses next to link the free list.
type orderNode struct {
	id      OrderID
	clOrdID ClientOrderID
	client  ClientID
	side    Side
	price   Price
	qty     Qty // remaining quantity; always > 0 while resting
	prev    uint32
	next    uint32
}

// level is all resting orders at one price on one side, in arrival order.
type level struct {
	price Price
	total Qty    // sum of qty over the queue
	count uint32 // number of orders in the queue
	head  uint32 // oldest order: matched first
	tail  uint32 // newest order: new arrivals link here
}

// ladder is one side of the book: its price levels in a slice sorted so that
// the best level is at the end (decision 001).
//
// Levels are ordered by key = price*sign ascending. Bids use sign +1 (highest
// price is best, so it sorts last); asks use sign -1 (lowest price is best, so
// its negation is the largest key and sorts last). That lets both sides share
// one search, insert, and remove implementation.
type ladder struct {
	sign   int64
	levels []*level
}

func (l *ladder) key(p Price) int64 { return int64(p) * l.sign }

// search returns the index of the first level whose key is >= key(p) and
// whether that level's price is exactly p. If not found, the index is where a
// level for p should be inserted.
func (l *ladder) search(p Price) (int, bool) {
	k := l.key(p)
	lo, hi := 0, len(l.levels)
	for lo < hi {
		mid := int(uint(lo+hi) >> 1)
		if l.key(l.levels[mid].price) < k {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo, lo < len(l.levels) && l.levels[lo].price == p
}

// best returns the best level, or nil if the side is empty.
func (l *ladder) best() *level {
	if n := len(l.levels); n > 0 {
		return l.levels[n-1]
	}
	return nil
}

// insertAt inserts lv at index i, shifting the levels at i and above (the
// better-priced ones) up by one. Inserts near the best price shift little.
func (l *ladder) insertAt(i int, lv *level) {
	l.levels = append(l.levels, nil)
	copy(l.levels[i+1:], l.levels[i:])
	l.levels[i] = lv
}

// removeAt removes and returns the level at index i. Removing the best level
// (the last element) moves nothing.
func (l *ladder) removeAt(i int) *level {
	lv := l.levels[i]
	copy(l.levels[i:], l.levels[i+1:])
	l.levels[len(l.levels)-1] = nil // drop the stale pointer so it is not retained
	l.levels = l.levels[:len(l.levels)-1]
	return lv
}

// clientOrderKey identifies a live order by its client's own id, for the
// DuplicateClientOrderID rule.
type clientOrderKey struct {
	client  ClientID
	clOrdID ClientOrderID
}

// Book is a limit order book for one instrument.
type Book struct {
	instr Instrument
	bids  ladder
	asks  ladder

	// The order slab, in fixed-size chunks so snapshots can share them
	// (cow.go, decision 013). Slot 0 is reserved (nilIdx). nslots counts the
	// slots handed out so far. Freed slots are chained through
	// orderNode.next starting at freeNode and reused before the slab grows,
	// so steady-state trading does not allocate.
	chunks   []*chunk
	nslots   uint32
	freeNode uint32

	// Copy-on-write state (cow.go). epoch is bumped by Freeze; a chunk whose
	// epoch is older is shared with a snapshot and is copied before its first
	// write. spare holds chunks for those copies. Only the owning goroutine
	// touches these.
	epoch     uint32
	frozen    bool
	spare     []*chunk
	cowCopies uint64

	// freeLevels holds level structs whose price level emptied, for reuse.
	freeLevels []*level

	byID    index[OrderID]        // live order id -> slab index (cancel lookup)
	byClOrd index[clientOrderKey] // live (client, clOrdID) -> slab index (duplicate check)
}

// New returns an empty book for instr.
func New(instr Instrument) (*Book, error) {
	if err := instr.Validate(); err != nil {
		return nil, err
	}
	return &Book{
		instr:   instr,
		bids:    ladder{sign: +1},
		asks:    ladder{sign: -1},
		chunks:  []*chunk{new(chunk)},
		nslots:  1, // slot 0 reserved
		byID:    newIndex[OrderID](1024),
		byClOrd: newIndex[clientOrderKey](1024),
	}, nil
}

// Instrument returns the instrument this book trades.
func (b *Book) Instrument() Instrument { return b.instr }

func (b *Book) ladderFor(s Side) *ladder {
	if s == Buy {
		return &b.bids
	}
	return &b.asks
}

// crosses reports whether an incoming order on side s with limit price limit
// can trade against a resting level at price resting.
func crosses(s Side, limit, resting Price) bool {
	if s == Buy {
		return resting <= limit
	}
	return resting >= limit
}

// Submit processes a new order. Fills are appended to fills, which is returned
// (the append-to-caller-buffer idiom: the caller reuses one slice across calls,
// so matching does not allocate once the buffer has grown).
//
// Matching walks the opposite side from its best level while the order still
// has quantity and (for a limit order) the level's price crosses the limit.
// Within a level, makers are matched oldest first. Every fill trades at the
// maker's price.
func (b *Book) Submit(o Order, fills []Fill) (Result, []Fill) {
	if r := o.Validate(); r != RejectNone {
		return Result{Reject: r}, fills
	}
	if _, dup := b.byClOrd.get(clientOrderKey{o.Client, o.ClOrdID}); dup {
		return Result{Reject: RejectDuplicateClientOrderID}, fills
	}

	remaining := o.Qty
	opp := b.ladderFor(o.Side.Opposite())
	for remaining > 0 {
		lv := opp.best()
		if lv == nil {
			break
		}
		if o.Type == Limit && !crosses(o.Side, o.Price, lv.price) {
			break
		}
		for remaining > 0 && lv.head != nilIdx {
			mi := lv.head
			// Writable pointer to the maker. It stays valid for this whole
			// loop: chunks never move, and nodeW copies a shared chunk at
			// most once per snapshot, before handing out the pointer.
			m := b.nodeW(mi)
			q := min(remaining, m.qty)
			m.qty -= q
			lv.total -= q
			remaining -= q
			fills = append(fills, Fill{
				Price:        lv.price,
				Qty:          q,
				TakerSide:    o.Side,
				Taker:        o.ID,
				TakerClient:  o.Client,
				Maker:        m.id,
				MakerClient:  m.client,
				MakerClOrdID: m.clOrdID,
				MakerLeft:    m.qty,
			})
			if m.qty == 0 {
				b.unlink(lv, mi)
				b.release(mi)
			}
			// A partially filled maker stays at the head: same node, same
			// queue position, smaller qty.
		}
		if lv.head == nilIdx {
			opp.removeAt(len(opp.levels) - 1) // lv was the best level: the last element
			b.freeLevel(lv)
		}
	}

	res := Result{Filled: o.Qty - remaining}
	if remaining == 0 {
		return res, fills
	}
	if o.Type == Market {
		res.Expired = remaining
		return res, fills
	}
	b.rest(&o, remaining)
	res.Rested = remaining
	return res, fills
}

// rest places the unfilled remainder of a limit order at the tail of its
// price level, creating the level if needed.
func (b *Book) rest(o *Order, qty Qty) {
	if _, exists := b.byID.get(o.ID); exists {
		// The engine assigns unique ids; a repeat is a caller bug that would
		// corrupt the index, so fail loudly rather than continue.
		panic(fmt.Sprintf("book: order id %d already resting", o.ID))
	}
	lad := b.ladderFor(o.Side)
	i, found := lad.search(o.Price)
	var lv *level
	if found {
		lv = lad.levels[i]
	} else {
		lv = b.newLevel(o.Price)
		lad.insertAt(i, lv)
	}

	idx := b.alloc()
	*b.nodeW(idx) = orderNode{
		id:      o.ID,
		clOrdID: o.ClOrdID,
		client:  o.Client,
		side:    o.Side,
		price:   o.Price,
		qty:     qty,
		prev:    lv.tail,
		next:    nilIdx,
	}
	if lv.tail != nilIdx {
		b.nodeW(lv.tail).next = idx
	} else {
		lv.head = idx
	}
	lv.tail = idx
	lv.count++
	lv.total += qty

	b.byID.put(o.ID, idx)
	b.byClOrd.put(clientOrderKey{o.Client, o.ClOrdID}, idx)
}

// Cancel removes a resting order. The order must exist and belong to client;
// otherwise the result is RejectUnknownOrder, so a client cannot learn whether
// another client's order id is live (decision 003).
//
// Cost: O(1) map lookup and unlink, plus an O(log L) search for the level and,
// if the level empties, an O(L) shift to remove it (decision 002).
func (b *Book) Cancel(client ClientID, id OrderID) CancelResult {
	idx, ok := b.byID.get(id)
	if !ok || b.node(idx).client != client {
		return CancelResult{Reject: RejectUnknownOrder}
	}
	n := *b.node(idx) // copy: release below clears the slot
	lad := b.ladderFor(n.side)
	i, found := lad.search(n.price)
	if !found {
		panic(fmt.Sprintf("book: resting order %d has no level at price %d", id, n.price))
	}
	lv := lad.levels[i]
	b.unlink(lv, idx)
	lv.total -= n.qty
	b.release(idx)
	if lv.head == nilIdx {
		lad.removeAt(i)
		b.freeLevel(lv)
	}
	return CancelResult{Side: n.side, Price: n.price, ClOrdID: n.clOrdID, Cancelled: n.qty}
}

// unlink removes node i from lv's queue. It does not touch lv.total; callers
// adjust it because they know how much quantity is leaving.
func (b *Book) unlink(lv *level, i uint32) {
	prev, next := b.node(i).prev, b.node(i).next // read before any write copies a chunk
	if prev != nilIdx {
		b.nodeW(prev).next = next
	} else {
		lv.head = next
	}
	if next != nilIdx {
		b.nodeW(next).prev = prev
	} else {
		lv.tail = prev
	}
	lv.count--
}

// release drops slot i from the indexes and pushes it on the free list. The
// slot is zeroed so a stale index can never read a plausible-looking order.
func (b *Book) release(i uint32) {
	n := b.nodeW(i)
	b.byID.delete(n.id)
	b.byClOrd.delete(clientOrderKey{n.client, n.clOrdID})
	*n = orderNode{next: b.freeNode}
	b.freeNode = i
}

func (b *Book) newLevel(p Price) *level {
	if k := len(b.freeLevels); k > 0 {
		lv := b.freeLevels[k-1]
		b.freeLevels[k-1] = nil
		b.freeLevels = b.freeLevels[:k-1]
		*lv = level{price: p}
		return lv
	}
	return &level{price: p}
}

func (b *Book) freeLevel(lv *level) {
	*lv = level{}
	b.freeLevels = append(b.freeLevels, lv)
}
