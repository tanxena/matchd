package book

import "hash/maphash"

// index is an open-addressing hash table from K to a slab slot (uint32). It
// replaces Go's built-in map for the book's two lookups (decision 002, rev. 2).
//
// Why not a Go map: an order book churns keys forever (every order id is new;
// old ones are deleted). Go's map marks deleted slots with tombstones, and when
// enough accumulate it rebuilds the table into newly allocated memory even
// though the number of live keys hasn't changed. That is a steady-state
// allocation plus an O(n) pause on the matching path. The measurement is in
// mapchurn_test.go and docs/guide/02-engine.md.
//
// How this table avoids it:
//   - Linear probing: a key lives at hash&mask or the next free slot after it.
//   - Deletion by backward shift: when a key is removed, later keys in the
//     same probe run are moved back to close the gap, so there are no
//     tombstones and no reason ever to rebuild.
//   - Growth only on a new peak: the table doubles when it would exceed half
//     full, and never shrinks. Once the book has reached its peak size, no
//     operation allocates.
//
// A slot with val == 0 is empty. That works because slab slot 0 (nilIdx) is
// reserved and never stored.
//
// Hashing uses hash/maphash with a random per-table seed, so clients can't
// choose client order ids that all collide (hash flooding). The seed affects
// only where entries sit in memory. Nothing iterates an index, so it can never
// change the book's output, and replay stays exact (decision 002, rev. 2).
type index[K comparable] struct {
	seed  maphash.Seed
	slots []indexSlot[K]
	mask  uint64 // len(slots)-1; len is a power of two
	count int
}

type indexSlot[K comparable] struct {
	key K
	val uint32 // 0 = empty
}

const indexMinSlots = 16

func newIndex[K comparable](hint int) index[K] {
	n := indexMinSlots
	for n < 2*hint { // keep load at or below 1/2
		n *= 2
	}
	return index[K]{
		seed:  maphash.MakeSeed(),
		slots: make([]indexSlot[K], n),
		mask:  uint64(n - 1),
	}
}

func (ix *index[K]) home(k K) uint64 { return maphash.Comparable(ix.seed, k) & ix.mask }

// get returns the value stored for k.
func (ix *index[K]) get(k K) (uint32, bool) {
	for i := ix.home(k); ; i = (i + 1) & ix.mask {
		s := &ix.slots[i]
		if s.val == 0 {
			return 0, false
		}
		if s.key == k {
			return s.val, true
		}
	}
}

// put stores v (which must not be 0) for k, replacing any existing value.
func (ix *index[K]) put(k K, v uint32) {
	if v == 0 {
		panic("book: index value 0 is reserved for empty slots")
	}
	if 2*(ix.count+1) > len(ix.slots) {
		ix.grow()
	}
	for i := ix.home(k); ; i = (i + 1) & ix.mask {
		s := &ix.slots[i]
		if s.val == 0 {
			*s = indexSlot[K]{key: k, val: v}
			ix.count++
			return
		}
		if s.key == k {
			s.val = v
			return
		}
	}
}

// delete removes k and reports whether it was present.
func (ix *index[K]) delete(k K) bool {
	i := ix.home(k)
	for {
		s := &ix.slots[i]
		if s.val == 0 {
			return false
		}
		if s.key == k {
			break
		}
		i = (i + 1) & ix.mask
	}
	// Backward shift: walk the rest of the probe run after the hole at i. An
	// entry at j may move into the hole only if its home is not in (i, j]
	// cyclically. If its home were in that range, moving it to i would put it
	// before its own home, where get could never find it.
	for j := (i + 1) & ix.mask; ix.slots[j].val != 0; j = (j + 1) & ix.mask {
		h := ix.home(ix.slots[j].key)
		var homeInGap bool // is h in the cyclic range (i, j]?
		if i < j {
			homeInGap = i < h && h <= j
		} else { // the run wraps around the end of the table
			homeInGap = i < h || h <= j
		}
		if !homeInGap {
			ix.slots[i] = ix.slots[j]
			i = j
		}
	}
	ix.slots[i] = indexSlot[K]{}
	ix.count--
	return true
}

// grow doubles the table and reinserts every entry. It is the only place an
// index allocates.
func (ix *index[K]) grow() {
	old := ix.slots
	ix.slots = make([]indexSlot[K], 2*len(old))
	ix.mask = uint64(len(ix.slots) - 1)
	ix.count = 0
	for _, s := range old {
		if s.val != 0 {
			ix.put(s.key, s.val)
		}
	}
}

func (ix *index[K]) len() int { return ix.count }
