package book

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/tanxena/matchd/internal/testutil"
)

// The evidence for decision 002 rev. 2 (replacing Go maps with index). Run:
//
//	go test ./internal/book -run MapChurn -mapchurn -v
//
// It reproduces the table in docs/learn/02-engine.md and
// docs/decisions/002-order-storage-and-cancel.md.
var mapChurn = flag.Bool("mapchurn", false, "run the Go map vs index churn measurement (about a minute)")

const (
	churnInstances = 20        // independent tables per size (each Go map gets its own hash seed)
	churnOps       = 1_000_000 // delete-one + insert-one operations per table, after warm-up
)

// A keyed table under book-like churn: the live count stays constant, every
// inserted key is new (order ids only increase), and deletions hit random
// live keys (orders are cancelled or filled in no particular order).
type churnTable interface {
	insert(OrderID)
	remove(OrderID)
	slots() int // current capacity, -1 if not observable
}

type goMap map[OrderID]uint32

func (m goMap) insert(k OrderID) { m[k] = 1 }
func (m goMap) remove(k OrderID) { delete(m, k) }
func (m goMap) slots() int       { return -1 }

type ourIndex struct{ ix index[OrderID] }

func (t *ourIndex) insert(k OrderID) { t.ix.put(k, 1) }
func (t *ourIndex) remove(k OrderID) { t.ix.delete(k) }
func (t *ourIndex) slots() int       { return len(t.ix.slots) }

type churnResult struct {
	allocs int64 // objects allocated by the churn loop itself, all instances
	grew   int   // instances whose capacity changed during measurement (index only)
}

func measureChurn(live int, newTable func() churnTable) churnResult {
	var r churnResult
	for inst := range churnInstances {
		tbl := newTable()
		keys := make([]OrderID, live)
		next := OrderID(1)
		for i := range keys {
			keys[i] = next
			tbl.insert(next)
			next++
		}
		rng := rand.New(rand.NewPCG(uint64(live), uint64(inst)))
		churn := func() {
			j := rng.IntN(live)
			tbl.remove(keys[j])
			tbl.insert(next)
			keys[j] = next
			next++
		}
		for range 2*live + 50_000 { // warm up past initial growth
			churn()
		}
		before := tbl.slots()
		r.allocs += testutil.CountOwnAllocations(1, func() {
			for range churnOps {
				churn()
			}
		})
		if tbl.slots() != before {
			r.grew++
		}
	}
	return r
}

func TestMapChurnMeasurement(t *testing.T) {
	if !*mapChurn {
		t.Skip("measurement; run with -mapchurn")
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n| live keys | Go map: allocations | Go map: per million ops | index: allocations | index: table grew |\n")
	fmt.Fprintf(&sb, "|---:|---:|---:|---:|---:|\n")
	for _, live := range []int{50, 400, 5_000, 50_000} {
		m := measureChurn(live, func() churnTable { return goMap{} })
		ix := measureChurn(live, func() churnTable { return &ourIndex{newIndex[OrderID](0)} })
		fmt.Fprintf(&sb, "| %d | %d | %.2f | %d | %d of %d |\n", live, m.allocs,
			float64(m.allocs)/float64(churnInstances*churnOps)*1e6, ix.allocs, ix.grew, churnInstances)
		if ix.allocs != 0 || ix.grew != 0 {
			t.Errorf("live=%d: index allocated %d objects / grew in %d instances under constant-size churn", live, ix.allocs, ix.grew)
		}
	}
	fmt.Fprintf(&sb, "\n%d tables per row, %d delete+insert operations each after warm-up; allocations counted with testutil.OwnAllocations.\n",
		churnInstances, churnOps)
	t.Log(sb.String())
}
