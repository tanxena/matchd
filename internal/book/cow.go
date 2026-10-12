package book

import (
	"fmt"
	"math"
)

// Chunked order storage with copy-on-write snapshots (decision 013).
//
// Orders live in fixed-size chunks rather than one big slice. Chunks are
// heap objects whose addresses never change, so a snapshot can share them:
// Freeze hands a snapshot goroutine the current chunk list and a value copy
// of every price level, then bumps the book's epoch. From then on, the first
// write to an order in a chunk whose epoch is older than the book's copies
// that chunk first (nodeW), so the snapshot keeps seeing the frozen bytes
// while the engine keeps trading without pausing. Thaw ends the snapshot:
// chunks nobody copied become plain live chunks again, and frozen originals
// that were replaced go to a spare pool for the next copy.
//
// Rule: every read of an order goes through node, every write through nodeW.
// A write through node would mutate a chunk a snapshot may be reading.

const (
	chunkShift = 12
	chunkSize  = 1 << chunkShift // orders per chunk (4,096 x 48 bytes = 192 KiB)
	chunkMask  = chunkSize - 1
)

// chunk is a block of order slots. epoch is the snapshot epoch the chunk
// belongs to; only the book's owning goroutine reads or writes it.
type chunk struct {
	nodes [chunkSize]orderNode
	epoch uint32
}

// node returns slot i for reading.
func (b *Book) node(i uint32) *orderNode {
	return &b.chunks[i>>chunkShift].nodes[i&chunkMask]
}

// nodeW returns slot i for writing, first copying its chunk if a snapshot
// may still be reading it.
func (b *Book) nodeW(i uint32) *orderNode {
	c := b.chunks[i>>chunkShift]
	if c.epoch != b.epoch {
		c = b.copyChunk(i >> chunkShift)
	}
	return &c.nodes[i&chunkMask]
}

// copyChunk replaces chunk k with a private copy at the current epoch. The
// original stays untouched for the snapshot that shares it.
func (b *Book) copyChunk(k uint32) *chunk {
	nc := b.takeChunk()
	nc.nodes = b.chunks[k].nodes // a 192 KiB array copy
	b.chunks[k] = nc
	b.cowCopies++
	return nc
}

// takeChunk returns a chunk at the current epoch, from the spare pool if
// possible. Its slots hold stale data; every slot is fully written before it
// is first used (alloc hands out slots, rest overwrites them).
func (b *Book) takeChunk() *chunk {
	var c *chunk
	if k := len(b.spare); k > 0 {
		c = b.spare[k-1]
		b.spare[k-1] = nil
		b.spare = b.spare[:k-1]
	} else {
		c = new(chunk)
	}
	c.epoch = b.epoch
	return c
}

// alloc returns a free slot, reusing a freed one if possible.
func (b *Book) alloc() uint32 {
	if i := b.freeNode; i != nilIdx {
		b.freeNode = b.node(i).next
		return i
	}
	if b.nslots == math.MaxUint32 {
		panic("book: order slab exhausted (2^32-1 resting orders)")
	}
	if int(b.nslots) == len(b.chunks)*chunkSize {
		b.chunks = append(b.chunks, b.takeChunk())
	}
	i := b.nslots
	b.nslots++
	return i
}

// level values captured by Freeze.
type frozenLevel struct {
	price Price
	head  uint32
}

// Frozen is a point-in-time view of a book. It may be read from another
// goroutine while the book's owner keeps trading, until the owner calls
// Thaw with it.
type Frozen struct {
	instr  Instrument
	sides  [2][]frozenLevel // [0] bids, [1] asks; best level first
	chunks []*chunk
}

// Freeze captures the book's current contents for a snapshot. Its cost is
// O(price levels + chunks): it copies level heads and chunk pointers, never
// orders. The book must not already be frozen.
func (b *Book) Freeze() *Frozen {
	if b.frozen {
		panic("book: Freeze while a snapshot is still in progress")
	}
	f := &Frozen{instr: b.instr, chunks: append([]*chunk(nil), b.chunks...)}
	for s, lad := range [2]*ladder{&b.bids, &b.asks} {
		lv := make([]frozenLevel, 0, len(lad.levels))
		for i := len(lad.levels) - 1; i >= 0; i-- { // ladder is best-last; view is best-first
			lv = append(lv, frozenLevel{price: lad.levels[i].price, head: lad.levels[i].head})
		}
		f.sides[s] = lv
	}
	// Make sure copying every chunk during this snapshot can't allocate:
	// trading while frozen must stay allocation-free (PRD 6).
	for len(b.spare) < len(b.chunks) {
		b.spare = append(b.spare, new(chunk))
	}
	b.epoch++ // every existing chunk is now shared with f
	b.frozen = true
	return f
}

// Thaw ends the snapshot that f belongs to. Call it from the book's owning
// goroutine once nothing reads f any more.
func (b *Book) Thaw(f *Frozen) {
	if !b.frozen {
		panic("book: Thaw without Freeze")
	}
	for k, c := range f.chunks {
		if k < len(b.chunks) && b.chunks[k] == c {
			c.epoch = b.epoch // never written during the snapshot: just unshare it
			continue
		}
		b.spare = append(b.spare, c) // replaced by a copy; the original is free now
	}
	f.chunks = nil // the view is dead
	b.frozen = false
}

// Instrument returns the frozen book's instrument.
func (f *Frozen) Instrument() Instrument { return f.instr }

// Each calls fn for every resting order on side s in priority order (best
// level first, oldest first within a level), as of Freeze.
func (f *Frozen) Each(s Side, fn func(RestingOrder)) {
	sideIdx := 0
	if s == Sell {
		sideIdx = 1
	}
	for _, lv := range f.sides[sideIdx] {
		for j := lv.head; j != nilIdx; {
			n := &f.chunks[j>>chunkShift].nodes[j&chunkMask]
			fn(RestingOrder{ID: n.id, Client: n.client, ClOrdID: n.clOrdID, Side: n.side, Price: n.price, Qty: n.qty})
			j = n.next
		}
	}
}

// CowCopies returns how many chunks have been copied by copy-on-write since
// the book was created (a measure of snapshot cost, for tests and metrics).
func (b *Book) CowCopies() uint64 { return b.cowCopies }

// Load builds a book holding exactly the given resting orders. bids and asks
// must each be in priority order (as Frozen.Each produces them); that order
// becomes the queue order. Order ids and client order ids must be unique.
func Load(instr Instrument, bids, asks []RestingOrder) (*Book, error) {
	b, err := New(instr)
	if err != nil {
		return nil, err
	}
	for si, side := range [2][]RestingOrder{bids, asks} {
		want := Buy
		if si == 1 {
			want = Sell
		}
		for i, r := range side {
			if r.Qty <= 0 || r.Price <= 0 || r.Side != want {
				return nil, fmt.Errorf("book: load %s: invalid order %+v", instr.Symbol, r)
			}
			if i > 0 && b.ladderFor(r.Side).key(r.Price) > b.ladderFor(r.Side).key(side[i-1].Price) {
				return nil, fmt.Errorf("book: load %s: orders not in priority order at %+v", instr.Symbol, r)
			}
			if _, dup := b.byID.get(r.ID); dup {
				return nil, fmt.Errorf("book: load %s: duplicate order id %d", instr.Symbol, r.ID)
			}
			if _, dup := b.byClOrd.get(clientOrderKey{r.Client, r.ClOrdID}); dup {
				return nil, fmt.Errorf("book: load %s: duplicate client order id %d/%d", instr.Symbol, r.Client, r.ClOrdID)
			}
			b.rest(&Order{ID: r.ID, Client: r.Client, ClOrdID: r.ClOrdID, Side: r.Side, Price: r.Price}, r.Qty)
		}
	}
	if err := b.CheckInvariants(); err != nil {
		return nil, fmt.Errorf("book: load %s: %w", instr.Symbol, err)
	}
	return b, nil
}
