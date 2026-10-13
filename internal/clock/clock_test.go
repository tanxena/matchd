package clock

import (
	"testing"
	"time"
)

func TestNowIsMonotonicAndFine(t *testing.T) {
	in := Init()
	t.Logf("source %s, TSC %.4f GHz, read cost %v, resolution %v", in.Source, in.TSCHz/1e9, in.ReadCost, in.Resolution)
	prev := Now()
	minStep := int64(1 << 62)
	for range 1_000_000 {
		n := Now()
		if n < prev {
			t.Fatalf("clock went backwards: %d after %d", n, prev)
		}
		if d := n - prev; d > 0 && d < minStep {
			minStep = d
		}
		prev = n
	}
	// The whole point: steps far below time.Now's ~515 us on Windows. The
	// smallest step seen is also bounded below by the cost of a read (about
	// 15 ns normally, ~300 ns under the race detector with the whole suite
	// running), so the bound is 1 us, not the source's resolution.
	if minStep > 1000 {
		t.Fatalf("smallest visible step %d ns: too coarse to time matching", minStep)
	}
}

func TestNowDoesNotAllocate(t *testing.T) {
	Init()
	if n := testing.AllocsPerRun(1000, func() { _ = Now() }); n != 0 {
		t.Fatalf("Now allocates %v times", n)
	}
}

// Elapsed time by Now agrees with Go's clock over a sleep: coarse
// (time.Now's step on Windows is ~0.5 ms), but catches a unit or
// conversion error.
func TestNowMeasuresElapsedTime(t *testing.T) {
	Init()
	a, wa := Now(), time.Now()
	time.Sleep(200 * time.Millisecond)
	b, wb := Now(), time.Now()
	got, want := time.Duration(b-a), wb.Sub(wa)
	if d := got - want; d < -2*time.Millisecond || d > 2*time.Millisecond {
		t.Fatalf("Now measured %v, time.Now %v", got, want)
	}
}

func TestWallNanos(t *testing.T) {
	Init()
	w, n := WallNanos(), time.Now().UnixNano()
	if d := time.Duration(w - n); d < -5*time.Millisecond || d > 5*time.Millisecond {
		t.Fatalf("WallNanos is %v away from time.Now", d)
	}
	if a, b := WallNanos(), WallNanos(); b < a {
		t.Fatalf("WallNanos went backwards: %d then %d", a, b)
	}
}

func BenchmarkNow(b *testing.B) {
	Init()
	for b.Loop() {
		_ = Now()
	}
}

// The read cost Init reports (in the startup log, in /metrics, and in every
// core-latency table's footnote) agrees with an independent measurement
// over a long loop, where time.Now's coarse Windows tick doesn't matter.
// It once read 0 on an idle machine, where time.Now advanced only every
// 15.6 ms and Init's short passes measured nothing.
func TestReadCostIsMeasured(t *testing.T) {
	in := Init()
	var n int64
	var sink int64
	start := time.Now()
	for time.Since(start) < 300*time.Millisecond {
		for range 10_000 {
			sink += Now()
		}
		n += 10_000
	}
	_ = sink
	independent := time.Since(start) / time.Duration(n)
	t.Logf("Init says %v per read; a 300 ms loop says %v", in.ReadCost, independent)
	if in.ReadCost <= 0 || in.ReadCost < independent/3 || in.ReadCost > 3*independent+5*time.Nanosecond {
		t.Fatalf("Init's read cost %v disagrees with %v measured over 300 ms", in.ReadCost, independent)
	}
}
