package load

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/tanxena/matchd/internal/book"
)

// Each mix sends what it says: the fractions of passive, crossing and
// cancel commands, prices on the right side of mid, quantities in range.
func TestMixFractionsAndPrices(t *testing.T) {
	for name, mix := range Mixes {
		t.Run(name, func(t *testing.T) {
			g := NewGenerator(mix, 7, 0, 1<<30, nil)
			live := NewLive(0, nil)
			for i := range 1000 { // plenty to cancel
				live.Add(uint64(1_000_000 + i))
			}
			const n = 200_000
			var counts [CapCancel + 1]int
			next := uint64(1)
			cancelled := map[uint64]bool{}
			for range n {
				o, _ := g.Next(live)
				counts[o.Kind]++
				switch o.Kind {
				case Passive:
					qtyIn(t, o.Qty, mix.PassiveQty)
					if d := o.Price - Mid; o.Buy && (d > -1 || d < -Band) || !o.Buy && (d < 1 || d > Band) {
						t.Fatalf("passive buy=%v at %d: not 1..%d ticks on its own side of %d", o.Buy, o.Price, Band, Mid)
					}
					live.Add(next) // as if acked
					next++
				case Crossing:
					qtyIn(t, o.Qty, mix.CrossQty)
					if want := int64(Mid - Band); o.Buy && o.Price != Mid+Band || !o.Buy && o.Price != want {
						t.Fatalf("crossing buy=%v at %d, want mid -/+ %d", o.Buy, o.Price, Band)
					}
				case Cancel:
					if cancelled[o.OrderID] {
						t.Fatalf("order %d cancelled twice", o.OrderID)
					}
					cancelled[o.OrderID] = true
				default:
					t.Fatalf("unexpected %v", o.Kind)
				}
			}
			for _, c := range []struct {
				kind Kind
				want float64
			}{{Passive, mix.Passive}, {Crossing, mix.Crossing}, {Cancel, mix.Cancel}} {
				got := float64(counts[c.kind]) / n
				if math.Abs(got-c.want) > 0.005 {
					t.Errorf("%v: %.4f of commands, want %.2f", c.kind, got, c.want)
				}
			}
		})
	}
}

func qtyIn(t *testing.T, q int64, r [2]int64) {
	t.Helper()
	if q < r[0] || q > r[1] {
		t.Fatalf("qty %d outside %v", q, r)
	}
}

func TestCapAndNoTarget(t *testing.T) {
	mix := Mix{Passive: 0.5, Cancel: 0.5, PassiveQty: [2]int64{1, 1}}
	g := NewGenerator(mix, 1, 0, 3, nil)
	live := NewLive(0, nil)
	sawNoTarget, sawCap := false, false
	next := uint64(0)
	for range 1000 {
		before := live.Len()
		o, noTarget := g.Next(live)
		switch o.Kind {
		case Passive:
			next++
			live.Add(next)
			if noTarget {
				sawNoTarget = true
				if before != 0 {
					t.Fatalf("noTarget with %d resting", before)
				}
			}
		case CapCancel:
			sawCap = true
			if before < 3 {
				t.Fatalf("cap cancel with only %d resting (cap 3)", before)
			}
		case Cancel:
			if before == 0 {
				t.Fatal("cancel with nothing resting")
			}
		}
		if live.Len() > 3 {
			t.Fatalf("%d resting, cap is 3", live.Len())
		}
	}
	if !sawNoTarget || !sawCap {
		t.Fatalf("noTarget seen %v, cap cancel seen %v", sawNoTarget, sawCap)
	}
}

func TestLive(t *testing.T) {
	d := NewDepth(0)
	l := NewLive(0, d)
	for i := range uint64(10) {
		l.Add(i)
	}
	l.Add(3) // duplicate: ignored
	if l.Len() != 10 || !l.Remove(3) || l.Remove(3) || l.Len() != 9 || d.Orders() != 9 {
		t.Fatal("add/remove")
	}
	rng := rand.New(rand.NewPCG(1, 1))
	seen := map[uint64]bool{}
	for {
		id, ok := l.TakeRandom(rng)
		if !ok {
			break
		}
		if seen[id] || id == 3 {
			t.Fatalf("took %d twice or after removal", id)
		}
		seen[id] = true
	}
	if len(seen) != 9 || l.Len() != 0 || d.Orders() != 0 {
		t.Fatalf("took %d, %d left", len(seen), l.Len())
	}
}

// Against the real book, with four connections sending each mix, the depth
// control holds the book near its target, no cap cancel is ever needed,
// and no passive order trades on arrival.
func TestMixesHoldTheBookNearTarget(t *testing.T) {
	const conns, perConn, target = 4, 100_000, 10_000
	for name, mix := range Mixes {
		t.Run(name, func(t *testing.T) {
			b, err := book.New(book.Instrument{Symbol: "BENCH", TickSize: 1, LotSize: 1})
			if err != nil {
				t.Fatal(err)
			}
			depth := NewDepth(target)
			gens := make([]*Generator, conns)
			lives := make([]*Live, conns)
			for i := range gens {
				gens[i], lives[i] = NewGenerator(mix, 42, i, target, depth), NewLive(0, depth)
			}
			owner := map[book.OrderID]int{}
			var id book.OrderID
			var fills []book.Fill
			maxLen, capCancels, minLate := 0, 0, 1<<30
			for i := range perConn {
				for c := range conns {
					o, _ := gens[c].Next(lives[c])
					switch o.Kind {
					case Cancel, CapCancel:
						if o.Kind == CapCancel {
							capCancels++
						}
						b.Cancel(book.ClientID(c+1), book.OrderID(o.OrderID))
						continue
					}
					id++
					side, tif := book.Sell, book.GTC
					if o.Buy {
						side = book.Buy
					}
					if o.Kind == Crossing {
						tif = book.IOC
					}
					var res book.Result
					res, fills = b.Submit(book.Order{ID: id, Client: book.ClientID(c + 1), ClOrdID: book.ClientOrderID(id),
						Side: side, Type: book.Limit, TIF: tif, Price: book.Price(o.Price), Qty: book.Qty(o.Qty)}, fills[:0])
					if res.Reject != book.RejectNone {
						t.Fatalf("%v rejected: %v", o.Kind, res.Reject)
					}
					if o.Kind == Passive && res.Filled != 0 {
						t.Fatalf("a passive order traded %d on arrival", res.Filled)
					}
					if o.Kind == Passive {
						owner[id] = c
						lives[c].Add(uint64(id))
					}
					for _, f := range fills {
						if f.MakerLeft == 0 {
							lives[owner[f.Maker]].Remove(uint64(f.Maker))
							delete(owner, f.Maker)
						}
					}
				}
				if b.Len() != int(depth.Orders()) {
					t.Fatalf("book holds %d orders, the generators count %d", b.Len(), depth.Orders())
				}
				maxLen = max(maxLen, b.Len())
				if i > perConn/2 { // after filling up
					minLate = min(minLate, b.Len())
				}
			}
			t.Logf("%s: %d commands, book peaked at %d orders, at least %d in the second half, ended at %d, %d cap cancels",
				name, conns*perConn, maxLen, minLate, b.Len(), capCancels)
			if capCancels > 0 {
				t.Errorf("%d cap cancels: the mix doesn't balance", capCancels)
			}
			if maxLen > 2*target || minLate < target/2 {
				t.Errorf("book ranged %d..%d orders in the second half; target %d", minLate, maxLen, target)
			}
			if err := b.CheckInvariants(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
