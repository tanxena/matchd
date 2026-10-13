package book_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/book/ref"
)

// Review F-003: PRD 5.1 makes a client order id unique per client among
// its live orders on every instrument, not per book. Two instruments, in
// both implementations: the real books share one book.ClOrdIDs, the
// reference books are linked (ref.Link) and look through each other's
// orders. The usual command stream is spread over the two at random, and
// every result, fill and resting order must agree.
func TestDifferentialAcrossInstruments(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			runLinked(t, seed, 10_000)
		})
	}
}

func runLinked(t *testing.T, seed uint64, n int) {
	t.Helper()
	shared := book.NewClOrdIDs()
	var reals [2]*book.Book
	var refs [2]*ref.Book
	for i := range 2 {
		in := book.Instrument{Symbol: fmt.Sprintf("I%d", i), TickSize: 1, LotSize: 1}
		r, err := book.New(in)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Share(shared); err != nil {
			t.Fatal(err)
		}
		o, err := ref.New(in)
		if err != nil {
			t.Fatal(err)
		}
		reals[i], refs[i] = r, o
	}
	ref.Link(refs[0], refs[1])

	g := newGenerator(seed)
	route := rand.New(rand.NewPCG(seed, 0x11d))
	var realFills, refFills []book.Fill
	var realRest, refRest []book.RestingOrder
	crossDup := 0 // duplicates whose live twin is on the other instrument
	for i := range n {
		c := g.next()
		k := route.IntN(2)
		realFills, refFills = realFills[:0], refFills[:0]
		var refRes book.Result
		var refCxl book.CancelResult
		fail := func(format string, args ...any) {
			t.Helper()
			t.Fatalf("seed %d command #%d %s on instrument %d: %s", seed, i, c, k, fmt.Sprintf(format, args...))
		}
		switch c.kind {
		case cmdSubmit:
			var realRes book.Result
			realRes, realFills = reals[k].Submit(c.order, realFills)
			refRes, refFills = refs[k].Submit(c.order, refFills)
			if realRes != refRes || !slices.Equal(realFills, refFills) {
				fail("book %+v %s, ref %+v %s", realRes, fmtFills(realFills), refRes, fmtFills(refFills))
			}
			if refRes.Reject == book.RejectDuplicateClientOrderID && !hasClOrd(refs[k], c.order.Client, c.order.ClOrdID) {
				crossDup++
			}
		case cmdCancel:
			realCxl := reals[k].Cancel(c.client, c.id)
			refCxl = refs[k].Cancel(c.client, c.id)
			if realCxl != refCxl {
				fail("cancel: book %+v, ref %+v", realCxl, refCxl)
			}
		}
		g.observe(c, refRes, refFills, refCxl)

		for j := range 2 {
			for _, s := range [2]book.Side{book.Buy, book.Sell} {
				realRest, refRest = reals[j].Orders(s, realRest[:0]), refs[j].Orders(s, refRest[:0])
				if !slices.Equal(realRest, refRest) {
					fail("instrument %d %s side: book %+v, ref %+v", j, s, realRest, refRest)
				}
			}
			if err := reals[j].CheckInvariants(); err != nil {
				fail("book %d: %v", j, err)
			}
		}
		if got, want := shared.Len(), reals[0].Len()+reals[1].Len(); got != want {
			fail("shared client order ids hold %d, the books %d orders", got, want)
		}
	}
	if crossDup == 0 {
		t.Fatalf("seed %d: no duplicate across instruments was ever sent: the test didn't test the rule", seed)
	}
}

func hasClOrd(b *ref.Book, c book.ClientID, id book.ClientOrderID) bool {
	for _, s := range [2]book.Side{book.Buy, book.Sell} {
		for _, r := range b.Orders(s, nil) {
			if r.Client == c && r.ClOrdID == id {
				return true
			}
		}
	}
	return false
}

// Share refuses a book whose orders clash with the group's.
func TestShareRefusesADuplicate(t *testing.T) {
	shared := book.NewClOrdIDs()
	a, _ := book.New(book.Instrument{Symbol: "A", TickSize: 1, LotSize: 1})
	b, _ := book.New(book.Instrument{Symbol: "B", TickSize: 1, LotSize: 1})
	a.Submit(sell(1, 7, 100, 1), nil)
	b.Submit(sell(2, 7, 100, 1), nil) // same client 7, same ClOrdID 2? no: sell() uses id as ClOrdID
	if err := a.Share(shared); err != nil {
		t.Fatal(err)
	}
	if err := b.Share(shared); err != nil {
		t.Fatalf("distinct client order ids refused: %v", err)
	}
	c, _ := book.New(book.Instrument{Symbol: "C", TickSize: 1, LotSize: 1})
	c.Submit(sell(1, 7, 100, 1), nil) // client 7's client order id 1, live on A
	if err := c.Share(shared); err == nil {
		t.Fatal("a book duplicating a live client order id joined the group")
	}
	if res, _ := b.Submit(buy(3, 7, 90, 1), nil); res.Reject != book.RejectNone {
		t.Fatalf("a fresh client order id on another book: %v", res.Reject)
	}
	o := buy(9, 7, 90, 1)
	o.ClOrdID = 1 // live on A
	if res, _ := b.Submit(o, nil); res.Reject != book.RejectDuplicateClientOrderID {
		t.Fatalf("client 7 reused client order id 1 (live on A) on B: %v", res.Reject)
	}
}
