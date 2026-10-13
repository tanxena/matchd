// Package load is the benchmark's load generator (PRD 7, decision 021): it
// drives an engine over TCP with one of three order mixes, either keeping
// a fixed number of commands in flight (closed loop, for throughput) or
// sending at a fixed rate (open loop, for latency), and measures what
// comes back. cmd bench/loadgen is its command line.
package load

import (
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync/atomic"
)

// The price grid. Every order is on instrument 1 with tick 1 and lot 1;
// the mid never moves.
const (
	Mid  = 1_000_000 // ticks
	Band = 10        // passive orders rest 1..Band ticks from mid
)

// Kind is what the generator chose to send.
type Kind uint8

const (
	// Passive: a GTC limit 1..Band ticks away from mid on its own side
	// (a buy below mid, a sell above). It never crosses, so it always rests.
	Passive Kind = iota + 1
	// Crossing: an IOC limit priced Band ticks through mid (a buy at
	// Mid+Band), so it can trade against every resting level on the other
	// side, best first. IOC so that it never rests: the book only ever
	// holds passive orders, and the mid stays put.
	Crossing
	// Cancel: cancel one of the connection's own resting orders, at random.
	Cancel
	// CapCancel: a cancel sent instead of a passive order because the
	// connection already has LiveCap resting orders.
	CapCancel
)

func (k Kind) String() string {
	switch k {
	case Passive:
		return "passive"
	case Crossing:
		return "crossing"
	case Cancel:
		return "cancel"
	case CapCancel:
		return "cap-cancel"
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// Mix is an order mix: the fraction of commands of each kind, and the
// quantity ranges (inclusive, in lots).
type Mix struct {
	Name                      string
	Passive, Crossing, Cancel float64
	PassiveQty, CrossQty      [2]int64
}

// Mixes are PRD 7.6's three order mixes.
//
// The fractions are fixed; what keeps the book's size steady is the
// crossing orders' quantity. Each mix's CrossQty range is chosen so that,
// on average, crossing orders take out about what passive orders (minus
// cancels) put in, per side, per command:
//
//	resting:      in 0.45 x 5.5 = 2.48 lots; out 0.05 x 50.5 = 2.53
//	crossing:     in 0.25 x 5.5 = 1.38;      out 0.25 x 6.5  = 1.63
//	cancel-heavy: in 0.25 x 5.5 = 1.38;      out 0.20 x ~5 (cancels) + 0.05 x 8 = 1.40
//
// Balanced flows alone make the size a random walk, which drifts. So each
// crossing quantity is also scaled by Depth: below the target number of
// resting orders, crossing orders are smaller and the book fills; above
// it, larger and it drains. Every mix then runs against a book of about
// the same size (the resting-order count at the start and end of each
// window is recorded, so a book that didn't hold steady would show).
var Mixes = map[string]Mix{
	// "Mostly resting": 90% limit orders away from mid.
	"resting": {Name: "resting", Passive: 0.90, Crossing: 0.10, PassiveQty: [2]int64{1, 10}, CrossQty: [2]int64{1, 100}},
	// "Mostly crossing": 50% through mid.
	"crossing": {Name: "crossing", Passive: 0.50, Crossing: 0.50, PassiveQty: [2]int64{1, 10}, CrossQty: [2]int64{1, 12}},
	// "Cancel-heavy": 40% cancels.
	"cancel-heavy": {Name: "cancel-heavy", Passive: 0.50, Crossing: 0.10, Cancel: 0.40, PassiveQty: [2]int64{1, 10}, CrossQty: [2]int64{1, 15}},
}

// MixNames lists the mixes, sorted.
func MixNames() string {
	var n []string
	for k := range Mixes {
		n = append(n, k)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}

// Order is one generated command.
type Order struct {
	Kind    Kind
	Buy     bool
	Price   int64
	Qty     int64
	OrderID uint64 // Cancel and CapCancel: the order to cancel
}

// Depth counts resting orders across every connection's Live set, and
// holds the size the book should stay at.
type Depth struct {
	n      atomic.Int64 // resting orders; Live sets add and subtract
	target int64
}

// NewDepth returns a counter aiming at target resting orders.
func NewDepth(target int) *Depth { return &Depth{target: int64(target)} }

// Orders is the current number of resting orders.
func (d *Depth) Orders() int64 { return d.n.Load() }

// Scale is the factor applied to crossing quantities: the book's size over
// the target, clamped to [1/4, 4]. 1 with no target (or no Depth).
func (d *Depth) Scale() float64 {
	if d == nil || d.target <= 0 {
		return 1
	}
	return min(max(float64(d.n.Load())/float64(d.target), 0.25), 4)
}

// Generator chooses commands for one connection. It isn't safe for
// concurrent use; the connection's mutex guards it with the Live set.
type Generator struct {
	mix     Mix
	rng     *rand.Rand
	liveCap int
	depth   *Depth // nil: crossing quantities unscaled
}

// NewGenerator returns a generator for one connection: deterministic for a
// given seed and connection index (what it cancels, and how large its
// crossing orders are, also depend on which orders are still resting,
// which depends on timing).
func NewGenerator(mix Mix, seed uint64, conn int, liveCap int, depth *Depth) *Generator {
	return &Generator{mix: mix, rng: rand.New(rand.NewPCG(seed, uint64(conn)+1)), liveCap: liveCap, depth: depth}
}

func (g *Generator) qty(r [2]int64) int64 { return r[0] + g.rng.Int64N(r[1]-r[0]+1) }

// Next chooses the next command. A cancel takes its target out of live at
// once, so no order is cancelled twice.
func (g *Generator) Next(live *Live) (o Order, noTarget bool) {
	r := g.rng.Float64()
	o.Buy = g.rng.IntN(2) == 0
	switch {
	case r < g.mix.Cancel:
		if id, ok := live.TakeRandom(g.rng); ok {
			return Order{Kind: Cancel, OrderID: id}, false
		}
		noTarget = true // nothing resting: send a passive order instead
	case r < g.mix.Cancel+g.mix.Crossing:
		q := max(int64(math.Round(float64(g.qty(g.mix.CrossQty))*g.depth.Scale())), 1)
		o.Kind, o.Qty, o.Price = Crossing, q, Mid-Band
		if o.Buy {
			o.Price = Mid + Band
		}
		return o, false
	}
	if live.Len() >= g.liveCap {
		id, _ := live.TakeRandom(g.rng)
		return Order{Kind: CapCancel, OrderID: id}, noTarget
	}
	k := 1 + g.rng.Int64N(Band)
	o.Kind, o.Qty, o.Price = Passive, g.qty(g.mix.PassiveQty), Mid+k
	if o.Buy {
		o.Price = Mid - k
	}
	return o, noTarget
}

// Live is the set of a connection's resting orders: added on Ack, removed
// when fully filled or chosen for a cancel. O(1) add, remove and random
// pick (a slice plus an index map, removal by swapping with the last).
type Live struct {
	ids   []uint64
	pos   map[uint64]int
	depth *Depth // nil, or the count shared by every connection
}

// NewLive returns an empty set with room for n orders, counted in depth
// (which may be nil).
func NewLive(n int, depth *Depth) *Live {
	return &Live{ids: make([]uint64, 0, n), pos: make(map[uint64]int, n), depth: depth}
}

// Len is the number of resting orders.
func (l *Live) Len() int { return len(l.ids) }

// Add adds an order.
func (l *Live) Add(id uint64) {
	if _, ok := l.pos[id]; ok {
		return
	}
	l.pos[id] = len(l.ids)
	l.ids = append(l.ids, id)
	if l.depth != nil {
		l.depth.n.Add(1)
	}
}

// Remove removes an order if present.
func (l *Live) Remove(id uint64) bool {
	i, ok := l.pos[id]
	if !ok {
		return false
	}
	last := l.ids[len(l.ids)-1]
	l.ids[i] = last
	l.pos[last] = i
	l.ids = l.ids[:len(l.ids)-1]
	delete(l.pos, id)
	if l.depth != nil {
		l.depth.n.Add(-1)
	}
	return true
}

// TakeRandom removes and returns a random order, or false if empty.
func (l *Live) TakeRandom(rng *rand.Rand) (uint64, bool) {
	if len(l.ids) == 0 {
		return 0, false
	}
	id := l.ids[rng.IntN(len(l.ids))]
	l.Remove(id)
	return id, true
}
