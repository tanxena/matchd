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
