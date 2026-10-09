package book

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// checkIndex verifies the index's structural invariants: count matches the
// occupied slots, and every stored key is reachable by probing from its home
// slot without crossing an empty slot (otherwise get could never find it).
func checkIndex[K comparable](ix *index[K]) error {
	used := 0
	for i, s := range ix.slots {
		if s.val == 0 {
			continue
		}
		used++
		for j := ix.home(s.key); j != uint64(i); j = (j + 1) & ix.mask {
			if ix.slots[j].val == 0 {
				return fmt.Errorf("key %v at slot %d is cut off from its home %d by empty slot %d", s.key, i, ix.home(s.key), j)
			}
		}
	}
	if used != ix.count {
		return fmt.Errorf("count = %d, occupied slots = %d", ix.count, used)
	}
	return nil
}

// Differential test against Go's map. A tiny key space and a table that
// starts at 16 slots force long probe runs, collisions, wrap-around at the end
// of the table, and growth, which are the paths where backward-shift deletion
// can go wrong.
func TestIndexAgainstMap(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			rng := rand.New(rand.NewPCG(seed, 1))
			ix := newIndex[OrderID](0)
			ref := map[OrderID]uint32{}
			keySpace := 8 + rng.IntN(60)
			for op := range 20_000 {
				k := OrderID(rng.IntN(keySpace))
				switch rng.IntN(3) {
				case 0, 1:
					v := uint32(1 + rng.IntN(1000))
					ix.put(k, v)
					ref[k] = v
				case 2:
					_, want := ref[k]
					if got := ix.delete(k); got != want {
						t.Fatalf("op %d: delete(%d) = %v, map says %v", op, k, got, want)
					}
					delete(ref, k)
				}
				if ix.len() != len(ref) {
					t.Fatalf("op %d: len %d, map %d", op, ix.len(), len(ref))
				}
				if err := checkIndex(&ix); err != nil {
					t.Fatalf("op %d: %v", op, err)
				}
				for q := range OrderID(keySpace) {
					got, ok := ix.get(q)
					want, wok := ref[q]
					if ok != wok || got != want {
						t.Fatalf("op %d: get(%d) = (%d,%v), map (%d,%v)", op, q, got, ok, want, wok)
					}
				}
			}
		})
	}
}

func TestIndexStructKey(t *testing.T) {
	ix := newIndex[clientOrderKey](0)
	ix.put(clientOrderKey{1, 7}, 3)
	ix.put(clientOrderKey{2, 7}, 4) // same clOrdID, different client: distinct key
	if v, ok := ix.get(clientOrderKey{1, 7}); !ok || v != 3 {
		t.Fatalf("get {1,7} = %d, %v", v, ok)
	}
	if v, ok := ix.get(clientOrderKey{2, 7}); !ok || v != 4 {
		t.Fatalf("get {2,7} = %d, %v", v, ok)
	}
	if _, ok := ix.get(clientOrderKey{3, 7}); ok {
		t.Fatal("found a key never inserted")
	}
}

func TestIndexRejectsZeroValue(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("put with value 0 did not panic")
		}
	}()
	ix := newIndex[OrderID](0)
	ix.put(1, 0)
}

// The property that motivated the index: under endless churn of new keys at a
// constant live size, the table never grows (grow is its only allocation).
func TestIndexChurnNeverGrows(t *testing.T) {
	for _, live := range []int{7, 400, 5000} {
		ix := newIndex[OrderID](0)
		keys := make([]OrderID, live)
		next := OrderID(1)
		for i := range keys {
			keys[i] = next
			ix.put(next, 1)
			next++
		}
		slots := len(ix.slots)
		rng := rand.New(rand.NewPCG(uint64(live), 2))
		for range 1_000_000 {
			j := rng.IntN(live)
			if !ix.delete(keys[j]) {
				t.Fatalf("live=%d: key %d missing", live, keys[j])
			}
			ix.put(next, 1)
			keys[j] = next
			next++
		}
		if len(ix.slots) != slots {
			t.Errorf("live=%d: table grew from %d to %d slots under constant-size churn", live, slots, len(ix.slots))
		}
		if err := checkIndex(&ix); err != nil {
			t.Fatalf("live=%d: %v", live, err)
		}
	}
}

// The structural checker must be able to fail.
func TestCheckIndexDetectsCorruption(t *testing.T) {
	ix := newIndex[OrderID](0)
	for k := range OrderID(6) {
		ix.put(k+1, 1)
	}
	ix.count++
	if checkIndex(&ix) == nil {
		t.Fatal("wrong count not detected")
	}
	ix.count--
	// Punch a hole in front of an entry that sits past its home slot. With
	// six keys in 16 slots there may be none, so add keys until there is one.
	for k := OrderID(100); ; k++ {
		ix.put(k, 1)
		for i, s := range ix.slots {
			if s.val != 0 && ix.home(s.key) != uint64(i) {
				ix.slots[(uint64(i)-1)&ix.mask] = indexSlot[OrderID]{} // empty the slot before it
				ix.count--
				if checkIndex(&ix) == nil {
					t.Fatal("unreachable entry not detected")
				}
				return
			}
		}
	}
}
