package book_test

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/book/ref"
)

// Run the long suite with:   go test ./internal/book -run Differential -long
// Replay a single seed with: go test ./internal/book -run Differential -seed 42 [-cmds 500000]
var (
	longRun = flag.Bool("long", false, "differential test: 1M+ commands across 20 seeds")
	oneSeed = flag.Uint64("seed", 0, "differential test: run only this seed")
	// Named -cmds, not -n: go test consumes -n itself (print commands, do not run).
	numCmds  = flag.Int("cmds", 0, "differential test: commands per seed (0 = suite default)")
	verbose  = flag.Bool("diffstats", false, "differential test: log per-seed coverage stats")
	statsMu  sync.Mutex // guards totals; seeds run as parallel subtests
	totals   coverage
	totalsOK bool
)

const (
	normalSeeds   = 10
	normalPerSeed = 10_000
	longSeeds     = 20
	longPerSeed   = 60_000 // 20 x 60k = 1.2M commands
	numClients    = 5
)

func TestDifferential(t *testing.T) {
	seeds := make([]uint64, 0, longSeeds)
	perSeed := normalPerSeed
	switch {
	case *oneSeed != 0:
		seeds = append(seeds, *oneSeed)
	case *longRun:
		for s := range uint64(longSeeds) {
			seeds = append(seeds, 1000+s)
		}
	default:
		for s := range uint64(normalSeeds) {
			seeds = append(seeds, 1+s)
		}
	}
	if *longRun {
		perSeed = longPerSeed
	}
	if *numCmds > 0 {
		perSeed = *numCmds
	}

	t.Run("seeds", func(t *testing.T) {
		for _, seed := range seeds {
			t.Run(fmt.Sprint(seed), func(t *testing.T) {
				t.Parallel()
				cov := runDifferential(t, seed, perSeed)
				if *verbose {
					t.Logf("seed %d: %s", seed, cov)
				}
				statsMu.Lock()
				totals.add(cov)
				totalsOK = true
				statsMu.Unlock()
			})
		}
	})
	// The parallel subtests have all finished once the group returns.
	if t.Failed() || !totalsOK {
		return
	}
	t.Logf("%d seeds x %d commands: %s", len(seeds), perSeed, totals)
	// The stream must actually exercise every path, or "no divergence" means
	// little. These floors are far below what any realistic seed produces.
	totals.requireCoverage(t, len(seeds)*perSeed)
}

// ---------------------------------------------------------------------------
// Command stream generation

type cmdKind uint8

const (
	cmdSubmit cmdKind = iota
	cmdCancel
)

type command struct {
	kind   cmdKind
	order  book.Order    // cmdSubmit
	reuse  bool          // cmdSubmit: (client, clOrdID) was used by an earlier order
	client book.ClientID // cmdCancel
	id     book.OrderID  // cmdCancel
}

func (c command) String() string {
	if c.kind == cmdCancel {
		return fmt.Sprintf("Cancel{client=%d id=%d}", c.client, c.id)
	}
	o := c.order
	return fmt.Sprintf("New{id=%d client=%d clOrd=%d %s %s tif=%s px=%d qty=%d}",
		o.ID, o.Client, o.ClOrdID, o.Side, o.Type, o.TIF, o.Price, o.Qty)
}

type issued struct {
	id      book.OrderID
	client  book.ClientID
	clOrdID book.ClientOrderID
}

// generator produces a reproducible random command stream from a seed. It
// tracks which orders are live (from the oracle's results) so it can aim
// cancels and duplicate client order ids at live orders, dead orders, and ids
// that never existed.
type generator struct {
	rng       *rand.Rand
	n         int   // commands generated so far
	mid       int64 // drifting mid price, in ticks
	reg       regime
	nextID    book.OrderID
	nextClOrd [numClients + 1]book.ClientOrderID
	deep      int                  // build a deep book first: this many commands of mostly resting orders (0 = no)
	every     int                  // compare full resting state every this many commands (0 or 1 = every command)
	all       []issued             // every order ever submitted (accepted or not)
	live      []issued             // currently resting, per the oracle
	liveIdx   map[book.OrderID]int // id -> index in live
}

func newGenerator(seed uint64) *generator {
	return &generator{
		rng:     rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		mid:     1000,
		liveIdx: make(map[book.OrderID]int),
	}
}

// regime shapes where limit orders land. The generator switches regime every
// regimeLen commands so one stream contains both shallow, heavily crossing
// books and deep books with many price levels (which exercise inserting and
// removing levels in the middle of the ladder, not just at the best price).
type regime struct {
	spread  int64 // limit prices land up to this many ticks from mid
	passive int   // percent of limits placed on their own side of mid (non-crossing)
}

const regimeLen = 2000

var regimes = []regime{
	{spread: 10, passive: 50}, // balanced: about half the limits cross
	{spread: 40, passive: 85}, // deep: wide, mostly resting book
	{spread: 3, passive: 30},  // tight: aggressive, crossing-heavy
}

// limitPrice returns a price for a limit on side s under the current regime.
// Passive orders sit 1..spread ticks away from mid on their own side; the rest
// are placed 0..spread ticks through mid toward the other side.
func (g *generator) limitPrice(s book.Side) book.Price {
	off := 1 + g.rng.Int64N(g.reg.spread)
	if g.rng.IntN(100) >= g.reg.passive {
		off = -g.rng.Int64N(g.reg.spread + 1)
	}
	if s == book.Buy {
		return book.Price(g.mid - off)
	}
	return book.Price(g.mid + off)
}

func (g *generator) client() book.ClientID { return book.ClientID(1 + g.rng.IntN(numClients)) }

func (g *generator) side() book.Side {
	if g.rng.IntN(2) == 0 {
		return book.Buy
	}
	return book.Sell
}

func (g *generator) newOrder(client book.ClientID) book.Order {
	g.nextID++
	g.nextClOrd[client]++
	return book.Order{ID: g.nextID, Client: client, ClOrdID: g.nextClOrd[client]}
}

// deepRegime holds a deep book once built: wide and mostly resting.
var deepRegime = regime{spread: 300, passive: 92}

func (g *generator) next() command {
	switch {
	case g.deep > 0:
		g.reg = deepRegime
	case g.n%regimeLen == 0:
		g.reg = regimes[g.rng.IntN(len(regimes))]
	}
	g.n++
	if g.n <= g.deep {
		return g.build()
	}
	// Mid does a bounded random walk so resting orders spread over a band of
	// prices. Bounds keep every price positive.
	g.mid = min(max(g.mid+int64(g.rng.IntN(3)-1), 900), 1100)

	var c command
	switch r := g.rng.IntN(100); {
	case r < 50: // limit, placed per the current regime
		o := g.newOrder(g.client())
		o.Side, o.Type = g.side(), book.Limit
		switch t := g.rng.IntN(100); {
		case t < 70:
			o.TIF = book.GTC
		case t < 78:
			o.TIF = book.Day
		case t < 89:
			o.TIF = book.IOC
		default:
			o.TIF = book.FOK
		}
		o.Price = g.limitPrice(o.Side)
		o.Qty = book.Qty(1 + g.rng.IntN(20))
		switch g.rng.IntN(400) {
		case 0: // edge quantity (review F-002/F-004): a level holding it must not overflow
			o.Qty = book.MaxQty
		case 1: // edge prices: an IOC through the whole book, at 1 or MaxPrice
			o.TIF, o.Price = book.IOC, 1
			if o.Side == book.Buy {
				o.Price = book.MaxPrice
			}
		}
		if o.TIF == book.FOK && g.rng.IntN(3) == 0 {
			// Large enough to need several levels, so both outcomes (and
			// the walk across levels) are common.
			o.Qty = book.Qty(10 + g.rng.IntN(60))
		}
		c = command{kind: cmdSubmit, order: o}
	case r < 58: // market
		o := g.newOrder(g.client())
		o.Side, o.Type = g.side(), book.Market
		o.Qty = book.Qty(1 + g.rng.IntN(40))
		c = command{kind: cmdSubmit, order: o}
	case r < 83: // cancel
		c = g.cancel()
	case r < 93: // reuse an earlier (client, clOrdID): live -> reject, dead -> accept
		c = g.reuseClOrd()
	default: // invalid order
		c = g.invalid()
	}
	if c.kind == cmdSubmit {
		o := c.order
		g.all = append(g.all, issued{o.ID, o.Client, o.ClOrdID})
	}
	return c
}

// build is one command of a deep stream's build-up: mostly resting limit
// orders across a wide band, some cancels. 13,000 of them leave more than
// 10,000 orders resting: three slab chunks and a grown order index.
func (g *generator) build() command {
	if g.rng.IntN(100) < 6 {
		return g.cancel()
	}
	o := g.newOrder(g.client())
	o.Side, o.Type, o.TIF = g.side(), book.Limit, book.GTC
	off := 1 + g.rng.Int64N(deepRegime.spread)
	o.Price = book.Price(g.mid + off)
	if o.Side == book.Buy {
		o.Price = book.Price(g.mid - off)
	}
	o.Qty = book.Qty(1 + g.rng.IntN(20))
	g.all = append(g.all, issued{o.ID, o.Client, o.ClOrdID})
	return command{kind: cmdSubmit, order: o}
}

func (g *generator) cancel() command {
	switch r := g.rng.IntN(100); {
	case r < 60 && len(g.live) > 0: // a live order, usually by its owner
		o := g.live[g.rng.IntN(len(g.live))]
		cl := o.client
		if g.rng.IntN(10) == 0 {
			cl = g.client() // may or may not be the owner
		}
		return command{kind: cmdCancel, client: cl, id: o.id}
	case r < 90 && len(g.all) > 0: // any past order: often already dead
		o := g.all[g.rng.IntN(len(g.all))]
		return command{kind: cmdCancel, client: o.client, id: o.id}
	default: // an id that has never been issued
		return command{kind: cmdCancel, client: g.client(), id: g.nextID + 1 + book.OrderID(g.rng.IntN(1000))}
	}
}

func (g *generator) reuseClOrd() command {
	var src issued
	switch {
	case len(g.live) > 0 && g.rng.IntN(2) == 0:
		src = g.live[g.rng.IntN(len(g.live))]
	case len(g.all) > 0:
		src = g.all[g.rng.IntN(len(g.all))]
	default:
		return g.invalid()
	}
	g.nextID++
	o := book.Order{
		ID: g.nextID, Client: src.client, ClOrdID: src.clOrdID,
		Side: g.side(), Type: book.Limit, TIF: book.GTC,
		Qty: book.Qty(1 + g.rng.IntN(20)),
	}
	o.Price = g.limitPrice(o.Side)
	if g.rng.IntN(4) == 0 {
		o.Type, o.TIF, o.Price = book.Market, 0, 0
	}
	return command{kind: cmdSubmit, order: o, reuse: true}
}

func (g *generator) invalid() command {
	o := g.newOrder(g.client())
	o.Side, o.Type, o.TIF = g.side(), book.Limit, book.GTC
	o.Price, o.Qty = book.Price(g.mid), 1
	switch g.rng.IntN(11) {
	case 9: // just above the bounds (decision 003)
		o.Qty = book.MaxQty + 1
	case 10:
		o.Price = book.MaxPrice + 1
	case 0:
		o.Qty = 0
	case 1:
		o.Qty = -book.Qty(1 + g.rng.IntN(5))
	case 2:
		o.Price = 0
	case 3:
		o.Side = 0
	case 4:
		o.Type = 7
	case 5:
		o.TIF = 9
	case 6:
		o.Type, o.TIF = book.Market, 0 // market with a price
	case 7, 8:
		o.Type, o.Price = book.Market, 0 // market IOC or FOK: limit-only TIFs
		o.TIF = book.IOC
		if g.rng.IntN(2) == 0 {
			o.TIF = book.FOK
		}
	}
	return command{kind: cmdSubmit, order: o}
}

// observe updates the live set from the oracle's outcome of c.
func (g *generator) observe(c command, res book.Result, fills []book.Fill, cres book.CancelResult) {
	for _, f := range fills {
		if f.MakerLeft == 0 {
			g.removeLive(f.Maker)
		}
	}
	switch c.kind {
	case cmdSubmit:
		if res.Rested > 0 {
			o := c.order
			g.liveIdx[o.ID] = len(g.live)
			g.live = append(g.live, issued{o.ID, o.Client, o.ClOrdID})
		}
	case cmdCancel:
		if cres.Reject == book.RejectNone {
			g.removeLive(c.id)
		}
	}
}

func (g *generator) removeLive(id book.OrderID) {
	i, ok := g.liveIdx[id]
	if !ok {
		return
	}
	last := len(g.live) - 1
	g.live[i] = g.live[last]
	g.liveIdx[g.live[i].id] = i
	g.live = g.live[:last]
	delete(g.liveIdx, id)
}

// ---------------------------------------------------------------------------
// Running and comparing

// runDifferential drives one seeded stream through both books and fails on
// the first command after which they disagree in any observable way.
func runDifferential(t *testing.T, seed uint64, n int) coverage {
	t.Helper()
	real, err := book.New(instr)
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := ref.New(instr)
	if err != nil {
		t.Fatal(err)
	}
	return runPair(t, seed, n, real, oracle)
}

func runPair(t *testing.T, seed uint64, n int, real, oracle orderBook) coverage {
	t.Helper()
	return runPairWith(t, newGenerator(seed), seed, n, real, oracle)
}

func runPairWith(t *testing.T, g *generator, seed uint64, n int, real, oracle orderBook) coverage {
	t.Helper()
	var (
		cov                 coverage
		realFills, refFills []book.Fill
		realRest, refRest   []book.RestingOrder
	)
	fail := func(i int, c command, format string, args ...any) {
		t.Helper()
		t.Fatalf("DIVERGENCE seed=%d command #%d %s\n%s\nreplay: go test ./internal/book -run Differential -seed %d -cmds %d",
			seed, i, c, fmt.Sprintf(format, args...), seed, i+1)
	}

	// Copy-on-write snapshots (decision 013): at random points freeze the
	// real book, remember what it held, keep trading, and later check the
	// frozen view still shows exactly that. A separate generator drives the
	// freezes so the command stream is the same as without them.
	rb, _ := real.(*book.Book)
	freezeRng := rand.New(rand.NewPCG(seed, 0xc0ffee))
	var (
		frozen     *book.Frozen
		frozenWant [2][]book.RestingOrder
		frozenAt   int
	)
	checkFrozen := func(i int, c command) {
		t.Helper()
		for si, s := range [2]book.Side{book.Buy, book.Sell} {
			var got []book.RestingOrder
			frozen.Each(s, func(r book.RestingOrder) { got = append(got, r) })
			if !slices.Equal(got, frozenWant[si]) {
				fail(i, c, "frozen view (taken after command #%d) changed on the %s side:\n got  %+v\n want %+v",
					frozenAt, s, got, frozenWant[si])
			}
		}
	}
	for i := range n {
		c := g.next()
		// Reset per command so a cancel leaves both fill slices empty.
		realFills, refFills = realFills[:0], refFills[:0]
		var (
			realRes, refRes book.Result
			realCxl, refCxl book.CancelResult
		)
		switch c.kind {
		case cmdSubmit:
			realRes, realFills = real.Submit(c.order, realFills)
			refRes, refFills = oracle.Submit(c.order, refFills)
			if realRes != refRes {
				fail(i, c, "result: book %+v\n        ref  %+v", realRes, refRes)
			}
			if !slices.Equal(realFills, refFills) {
				fail(i, c, "fills: book %s\n       ref  %s", fmtFills(realFills), fmtFills(refFills))
			}
			cov.submit(c, refRes, refFills)
		case cmdCancel:
			realCxl = real.Cancel(c.client, c.id)
			refCxl = oracle.Cancel(c.client, c.id)
			if realCxl != refCxl {
				fail(i, c, "cancel: book %+v\n        ref  %+v", realCxl, refCxl)
			}
			cov.cancel(refCxl)
		}

		// Identical resting state after every command, order by order in
		// priority order (this checks time priority, not just level totals).
		// A deep book is compared every g.every commands and at the end:
		// results and fills are still compared on every command, and a
		// divergence in resting state persists until the next comparison.
		full := g.every <= 1 || i%g.every == 0 || i == n-1
		for _, s := range [2]book.Side{book.Buy, book.Sell} {
			if !full {
				break
			}
			realRest = real.Orders(s, realRest[:0])
			refRest = oracle.Orders(s, refRest[:0])
			if !slices.Equal(realRest, refRest) {
				fail(i, c, "%s side differs:\n book %+v\n ref  %+v", s, realRest, refRest)
			}
			rp, rq, rok := real.Best(s)
			op, oq, ook := oracle.Best(s)
			if rp != op || rq != oq || rok != ook {
				fail(i, c, "best %s: book %d x %d (%v), ref %d x %d (%v)", s, rp, rq, rok, op, oq, ook)
			}
			// LevelQty (used by the engine for book deltas) must equal the
			// oracle's per-order quantities summed by price, for every level.
			if rb, ok := real.(*book.Book); ok {
				for j := 0; j < len(refRest); {
					px := refRest[j].Price
					var want book.Qty
					for ; j < len(refRest) && refRest[j].Price == px; j++ {
						want += refRest[j].Qty
					}
					if got := rb.LevelQty(s, px); got != want {
						fail(i, c, "LevelQty(%s, %d) = %d, ref level sums to %d", s, px, got, want)
					}
				}
			}
		}
		if real.Len() != oracle.Len() {
			fail(i, c, "len: book %d, ref %d", real.Len(), oracle.Len())
		}

		// Structural invariants on both (O(orders), so with the full
		// comparison).
		if full {
			if err := real.CheckInvariants(); err != nil {
				fail(i, c, "book invariant: %v", err)
			}
			if err := oracle.CheckInvariants(); err != nil {
				fail(i, c, "ref invariant: %v", err)
			}
		}
		// Orders that just left the book must be gone from both.
		for _, f := range refFills {
			if f.MakerLeft == 0 && (real.Live(f.Maker) || oracle.Live(f.Maker)) {
				fail(i, c, "filled maker %d still live", f.Maker)
			}
		}
		if c.kind == cmdCancel && refCxl.Reject == book.RejectNone && (real.Live(c.id) || oracle.Live(c.id)) {
			fail(i, c, "cancelled order %d still live", c.id)
		}

		cov.depth = max(cov.depth, oracle.Len())
		g.observe(c, refRes, refFills, refCxl)

		if rb != nil {
			switch {
			case frozen == nil && freezeRng.IntN(300) == 0:
				frozen, frozenAt = rb.Freeze(), i
				frozenWant[0] = rb.Orders(book.Buy, nil)
				frozenWant[1] = rb.Orders(book.Sell, nil)
				cov.freezes++
			case frozen != nil && freezeRng.IntN(150) == 0:
				checkFrozen(i, c)
				rb.Thaw(frozen)
				frozen = nil
				if err := rb.CheckInvariants(); err != nil {
					fail(i, c, "after thaw: %v", err)
				}
			}
		}
	}
	if frozen != nil {
		checkFrozen(n-1, command{})
		rb.Thaw(frozen)
	}
	if rb != nil {
		cov.cowCopies += int(rb.CowCopies())
	}
	return cov
}

// ---------------------------------------------------------------------------
// Coverage accounting: proof that the stream exercised each path.

type coverage struct {
	commands, fills, multiFill int
	rested, partialRest        int
	marketExpired              int
	iocUnfilled, iocFilled     int // IOC orders with a remainder cancelled; fully filled ones
	fokFilled, fokMultiLevel   int // FOK orders filled in full; ...across more than one level
	cancelsOK                  int
	rejects                    [book.RejectFOKNoLiquidity + 1]int
	reusedClOrdOK              int // accepted orders whose (client, clOrdID) had been used before
	depth                      int // max resting orders seen
	freezes, cowCopies         int // snapshots taken mid-stream; chunks copied because of them
	edgeQty, edgePrice         int // accepted orders of MaxQty; IOCs at price 1 or MaxPrice
}

func (c *coverage) submit(cmd command, r book.Result, fills []book.Fill) {
	c.commands++
	c.rejects[r.Reject]++
	if r.Reject == book.RejectNone {
		if cmd.order.Qty == book.MaxQty {
			c.edgeQty++
		}
		if cmd.order.Type == book.Limit && (cmd.order.Price == 1 || cmd.order.Price == book.MaxPrice) {
			c.edgePrice++
		}
	}
	c.fills += len(fills)
	if len(fills) > 1 {
		c.multiFill++
	}
	if r.Rested > 0 {
		c.rested++
		if r.Filled > 0 {
			c.partialRest++
		}
	}
	if r.Expired > 0 && cmd.order.Type == book.Market {
		c.marketExpired++
	}
	if r.Reject == book.RejectNone {
		switch cmd.order.TIF {
		case book.IOC:
			if cmd.order.Type == book.Limit && r.Expired > 0 {
				c.iocUnfilled++
			} else if r.Filled == cmd.order.Qty {
				c.iocFilled++
			}
		case book.FOK:
			if r.Filled == cmd.order.Qty && r.Rested == 0 && r.Expired == 0 {
				c.fokFilled++
				if len(fills) > 0 && fills[0].Price != fills[len(fills)-1].Price {
					c.fokMultiLevel++
				}
			}
		}
	}
	if cmd.reuse && r.Reject == book.RejectNone {
		c.reusedClOrdOK++
	}
}

func (c *coverage) cancel(r book.CancelResult) {
	c.commands++
	if r.Reject == book.RejectNone {
		c.cancelsOK++
	} else {
		c.rejects[r.Reject]++
	}
}

func (c *coverage) add(o coverage) {
	c.commands += o.commands
	c.fills += o.fills
	c.multiFill += o.multiFill
	c.rested += o.rested
	c.partialRest += o.partialRest
	c.marketExpired += o.marketExpired
	c.iocUnfilled += o.iocUnfilled
	c.iocFilled += o.iocFilled
	c.fokFilled += o.fokFilled
	c.fokMultiLevel += o.fokMultiLevel
	c.cancelsOK += o.cancelsOK
	c.reusedClOrdOK += o.reusedClOrdOK
	c.freezes += o.freezes
	c.cowCopies += o.cowCopies
	c.edgeQty += o.edgeQty
	c.edgePrice += o.edgePrice
	for i := range c.rejects {
		c.rejects[i] += o.rejects[i]
	}
	c.depth = max(c.depth, o.depth)
}

func (c coverage) String() string {
	s := fmt.Sprintf("commands=%d fills=%d multi-fill-orders=%d rested=%d partial-then-rest=%d market-expired=%d ioc-unfilled=%d ioc-filled=%d fok-filled=%d fok-multi-level=%d cancels-ok=%d clordid-reused=%d max-depth=%d snapshots=%d cow-copies=%d max-qty-orders=%d edge-price-orders=%d rejects:",
		c.commands, c.fills, c.multiFill, c.rested, c.partialRest, c.marketExpired, c.iocUnfilled, c.iocFilled, c.fokFilled, c.fokMultiLevel,
		c.cancelsOK, c.reusedClOrdOK, c.depth, c.freezes, c.cowCopies, c.edgeQty, c.edgePrice)
	for _, r := range bookRejects {
		s += fmt.Sprintf(" %s=%d", r, c.rejects[r])
	}
	return s
}

// bookRejects are the reasons a Book itself returns (the engine-level ones
// in between never come from a book).
var bookRejects = []book.RejectReason{
	book.RejectInvalidSide, book.RejectInvalidType, book.RejectInvalidQty, book.RejectInvalidPrice,
	book.RejectInvalidTIF, book.RejectDuplicateClientOrderID, book.RejectUnknownOrder, book.RejectFOKNoLiquidity,
}

func (c coverage) requireCoverage(t *testing.T, n int) {
	t.Helper()
	floor := n / 1000 // 0.1% of commands
	checks := []struct {
		name string
		got  int
	}{
		{"fills", c.fills},
		{"orders with multiple fills", c.multiFill},
		{"orders resting", c.rested},
		{"partial fill then rest", c.partialRest},
		{"market remainders expired", c.marketExpired},
		{"IOC remainders cancelled", c.iocUnfilled},
		{"IOC orders filled in full", c.iocFilled},
		{"FOK orders filled in full", c.fokFilled},
		{"FOK orders filled across levels", c.fokMultiLevel},
		{"successful cancels", c.cancelsOK},
		{"client order id reused after its order died", c.reusedClOrdOK},
	}
	// Edge values (review F-004): about 1 in 800 commands each, so a floor
	// of 1 in 4,000.
	for _, e := range []struct {
		name string
		got  int
	}{{"orders of MaxQty", c.edgeQty}, {"IOCs at price 1 or MaxPrice", c.edgePrice}} {
		if e.got < n/4000 {
			t.Errorf("coverage: %s = %d, want >= %d", e.name, e.got, n/4000)
		}
	}
	for _, r := range bookRejects {
		checks = append(checks, struct {
			name string
			got  int
		}{r.String() + " rejects", c.rejects[r]})
	}
	for _, ch := range checks {
		if ch.got < floor {
			t.Errorf("coverage: %s = %d, want >= %d; the generator is not exercising this path", ch.name, ch.got, floor)
		}
	}
	// Copy-on-write snapshots happen about once per 450 commands; each must
	// have caused at least one chunk copy, or frozen views were never at risk.
	if c.freezes < n/2000 || c.cowCopies < c.freezes/2 {
		t.Errorf("coverage: %d mid-stream snapshots and %d chunk copies in %d commands; copy-on-write isn't being exercised",
			c.freezes, c.cowCopies, n)
	}
}

// Review F-004: the stream above never holds more than a few hundred orders,
// so the slab never leaves its first chunk and the indexes never grow under
// the oracle. This one builds a book of more than 10,000 resting orders
// (three or more chunks, an order index grown past its initial 1,024
// slots), keeps trading and freezing on it, and requires that it got there.
func TestDifferentialDeepBook(t *testing.T) {
	seeds := []uint64{301}
	if *longRun {
		seeds = []uint64{301, 302, 303}
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			real, err := book.New(instr)
			if err != nil {
				t.Fatal(err)
			}
			oracle, err := ref.New(instr)
			if err != nil {
				t.Fatal(err)
			}
			g := newGenerator(seed)
			g.deep, g.every = 13_000, 50
			cov := runPairWith(t, g, seed, 20_000, real, oracle)
			chunks, slots := real.Footprint()
			t.Logf("seed %d: max depth %d, %d chunks, %d index slots, %d snapshots, %d chunk copies", seed, cov.depth, chunks, slots, cov.freezes, cov.cowCopies)
			if cov.depth < 10_000 || chunks < 3 || slots <= 1024 {
				t.Fatalf("deep book not reached: max depth %d, %d chunks, %d index slots", cov.depth, chunks, slots)
			}
			// A freeze of a multi-chunk book copies more than one chunk.
			if cov.freezes == 0 || cov.cowCopies <= cov.freezes {
				t.Fatalf("%d snapshots, %d chunk copies: copy-on-write never copied more than one chunk per snapshot", cov.freezes, cov.cowCopies)
			}
		})
	}
}
