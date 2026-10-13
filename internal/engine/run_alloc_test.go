package engine_test

import (
	"runtime"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/metrics"
)

// Run's per-command path, with latency recording on (two histograms, three
// clock reads), allocates nothing: acceptance 3 covers the goroutine form,
// not only Step. Counted across the whole process with MemStats, since
// AllocsPerRun can't see into another goroutine.
func TestRunRecordsLatencyWithoutAllocating(t *testing.T) {
	e := engine.New()
	var out []engine.Event
	e.Step(&engine.Command{Seq: 1, Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "XYZ", TickSize: 1, LotSize: 1}}, out)
	lat, step := metrics.NewHistogram(metrics.MatchingBuckets), metrics.NewHistogram(metrics.MatchingBuckets)
	e.SetLatency(lat, step)

	in := make(chan engine.Command, 4096)
	events := make(chan engine.Event, 1<<16)
	go e.Run(in, events, nil)
	reached := make(chan struct{}, 1)
	target := make(chan int, 1)
	go func() { // drains events; signals each time a target count is reached
		n, want := 0, <-target
		for range events {
			if n++; n == want {
				reached <- struct{}{}
				want = <-target
			}
		}
	}()

	seq, order := engine.Seq(1), book.OrderID(0)
	cycle := func(n int) { // n rest-then-cancel pairs: 2 commands, 4 events each
		for range n {
			order++
			seq++
			in <- engine.Command{Seq: seq, Kind: engine.CmdNewOrder, Instrument: 1, Client: 1, ClOrdID: book.ClientOrderID(order),
				Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 100, Qty: 1}
			seq++
			in <- engine.Command{Seq: seq, Kind: engine.CmdCancel, Instrument: 1, Client: 1, OrderID: order}
		}
	}
	const warm, measured = 2000, 10000
	target <- 4 * warm
	cycle(warm)
	<-reached

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	target <- 4 * (warm + measured)
	cycle(measured)
	<-reached
	runtime.ReadMemStats(&after)
	close(in)

	allocs := after.Mallocs - before.Mallocs
	if got := lat.Snapshot().Count(); got != 2*(warm+measured) || step.Snapshot().Count() != got {
		t.Fatalf("recorded %d latencies, want %d", got, 2*(warm+measured))
	}
	t.Logf("%d heap allocations over %d commands", allocs, 2*measured)
	// The runtime itself may allocate a little in the background (timers,
	// profiling buckets); anything per command would show as >= 20000.
	if allocs > 20 {
		t.Fatalf("%d allocations over %d commands: Run's recording path allocates", allocs, 2*measured)
	}
}
