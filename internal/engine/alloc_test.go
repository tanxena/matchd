package engine_test

import (
	"strings"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/testutil"
)

// PRD 6: the engine goroutine must not allocate on the steady-state path for
// limit add, cancel and match.
//
// Each workload below is a *cycle*: a short command sequence that leaves the
// books exactly as it found them (every order it adds is filled or
// cancelled). Running a cycle many times therefore exercises the steady state
// rather than a book that keeps growing, and any allocation is a real one.
//
// Why not testing.AllocsPerRun alone: it returns mallocs/runs using integer
// division, so a slice that doubles now and then (say 3 allocations over
// 10,000 runs) reports 0. So there is also a strict check that finds every
// allocation made during 100,000 cycles and requires there to be none.
//
// The strict check uses testutil.OwnAllocations, which records every
// allocation with its stack and counts only those that pass through this
// module's code. Counting runtime.MemStats.Mallocs instead picks up the Go
// runtime's own background allocations and fails at random; see
// docs/learn/02-engine.md, "What breaks if".

// allocBench is an engine with one instrument and a realistic resting book
// (200 orders per side, away from the prices the cycles trade at) so the maps,
// slab and ladders are populated, not empty.
type allocBench struct {
	e   *engine.Engine
	seq engine.Seq
	out []engine.Event
}

const allocInst engine.InstrumentID = 1

func newAllocBench(tb testing.TB) *allocBench {
	tb.Helper()
	a := &allocBench{e: engine.New()}
	a.do(engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "BENCH", TickSize: 1, LotSize: 1}})
	for i := range 200 {
		px := book.Price(i % 50)
		a.do(limit(allocInst, 9, book.ClientOrderID(1_000_000+i), book.Buy, 800+px, 10))
		a.do(limit(allocInst, 9, book.ClientOrderID(2_000_000+i), book.Sell, 1200+px, 10))
	}
	return a
}

// do applies one command. c is a value parameter: taking its address for Step
// must not make it escape to the heap, or every call would allocate.
func (a *allocBench) do(c engine.Command) {
	a.seq++
	c.Seq = a.seq
	c.Time = engine.Timestamp(a.seq)
	a.out = a.e.Step(&c, a.out[:0])
}

// Workloads. Prices 1000-1010 are inside the spread left by the background book.
var allocCycles = []struct {
	name string
	run  func(a *allocBench)
}{
	{"limit add + cancel", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Buy, 1000, 5)) // rests: new level
		a.do(cancel(allocInst, 1, a.e.LastOrderID()))   // removes it and the level
	}},
	{"add to existing level + cancel", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Buy, 1000, 5))
		first := a.e.LastOrderID()
		a.do(limit(allocInst, 2, 1, book.Buy, 1000, 3)) // joins the tail of 1000
		a.do(cancel(allocInst, 2, a.e.LastOrderID()))
		a.do(cancel(allocInst, 1, first))
	}},
	{"full match", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Sell, 1005, 5)) // rests
		a.do(limit(allocInst, 2, 1, book.Buy, 1005, 5))  // fills it completely
	}},
	{"partial fill + cancel remainder", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Sell, 1005, 10))
		maker := a.e.LastOrderID()
		a.do(limit(allocInst, 2, 1, book.Buy, 1005, 4)) // maker keeps 6, in place
		a.do(cancel(allocInst, 1, maker))
	}},
	{"market sweep of three levels", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Sell, 1001, 2))
		a.do(limit(allocInst, 1, 2, book.Sell, 1002, 2))
		a.do(limit(allocInst, 1, 3, book.Sell, 1003, 2))
		a.do(market(allocInst, 2, 1, book.Buy, 6))
	}},
	{"IOC partial fill, rest cancelled", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Sell, 1005, 3))
		ioc := limit(allocInst, 2, 1, book.Buy, 1005, 5) // fills 3, cancels 2
		ioc.TIF = book.IOC
		a.do(ioc)
	}},
	{"FOK across two levels, then FOK rejected", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Sell, 1004, 3))
		a.do(limit(allocInst, 1, 2, book.Sell, 1005, 3))
		fok := limit(allocInst, 2, 1, book.Buy, 1005, 6) // exactly fills both
		fok.TIF = book.FOK
		a.do(fok)
		fok.ClOrdID, fok.Qty = 2, 1 // nothing left inside the spread: rejected
		a.do(fok)
	}},
	{"rejects", func(a *allocBench) {
		a.do(limit(allocInst, 1, 1, book.Buy, 0, 5)) // invalid price
		a.do(cancel(allocInst, 1, 999_999_999))      // unknown order
		a.do(limit(77, 1, 1, book.Buy, 1000, 5))     // unknown instrument
	}},
}

func TestZeroAllocSteadyState(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation allocates; run without -cover")
	}
	for _, cy := range allocCycles {
		t.Run(cy.name, func(t *testing.T) {
			a := newAllocBench(t)
			for range 10_000 { // warm up: grow scratch buffers to their steady size
				cy.run(a)
			}

			if avg := testing.AllocsPerRun(10_000, func() { cy.run(a) }); avg != 0 {
				t.Errorf("AllocsPerRun = %v per cycle, want 0", avg)
			}

			if sites := testutil.OwnAllocations(100_000, func() { cy.run(a) }); len(sites) > 0 {
				t.Errorf("allocations in matchd code across 100,000 cycles:\n%s", strings.Join(sites, "\n"))
			}
		})
	}
}

// The strict check must be able to fail: allocations made in this module's
// code, too rare for AllocsPerRun's integer division to show, have to be
// reported.
func TestAllocCheckCanFail(t *testing.T) {
	if testing.CoverMode() != "" {
		t.Skip("coverage instrumentation allocates")
	}
	i := 0
	rare := func() {
		i++
		if i%400 == 0 { // 2 allocations in 1,000 runs
			allocSink = make([]byte, 64+i)
		}
	}
	if avg := testing.AllocsPerRun(1000, rare); avg != 0 {
		t.Fatalf("expected AllocsPerRun to miss rare allocations (got %v); the premise of the strict check changed", avg)
	}
	i = 0
	sites := testutil.OwnAllocations(1000, rare)
	if len(sites) == 0 {
		t.Fatal("deliberate allocations were not detected by the strict check")
	}
	t.Logf("detected as expected:\n%s", strings.Join(sites, "\n"))
}

// allocSink is a package-level destination so the compiler can't optimise
// the deliberate allocation away.
var allocSink []byte

func BenchmarkCycles(b *testing.B) {
	for _, cy := range allocCycles {
		b.Run(cy.name, func(b *testing.B) {
			a := newAllocBench(b)
			for range 10_000 {
				cy.run(a)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				cy.run(a)
			}
		})
	}
}
