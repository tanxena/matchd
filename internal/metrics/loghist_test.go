package metrics

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
)

// Every value lands in a bucket whose bounds contain it, buckets tile the
// number line without gaps or overlaps, and no bucket is wider than 1/32
// of its smallest value.
func TestLogBucketsTileAndBound(t *testing.T) {
	var prevHi int64 = -1
	for i := range logBuckets {
		lo, hi := logBounds(i)
		if lo != prevHi+1 {
			t.Fatalf("bucket %d starts at %d, previous ended at %d", i, lo, prevHi)
		}
		if hi < lo {
			t.Fatalf("bucket %d: hi %d < lo %d", i, hi, lo)
		}
		if lo >= 2*logSub && float64(hi-lo+1) > float64(lo)/logSub {
			t.Fatalf("bucket %d [%d,%d] is wider than 1/32 of %d", i, lo, hi, lo)
		}
		if logIndex(lo) != i || logIndex(hi) != i {
			t.Fatalf("bucket %d [%d,%d]: index(lo)=%d index(hi)=%d", i, lo, hi, logIndex(lo), logIndex(hi))
		}
		prevHi = hi
	}
	if _, hi := logBounds(logBuckets - 1); hi != 1<<logMaxBits-1 {
		t.Fatalf("last bucket ends at %d, want 2^40-1", hi)
	}
	// Out of range: clamped, never a panic.
	if logIndex(-5) != 0 || logIndex(1<<62) != logBuckets-1 {
		t.Fatal("out-of-range values not clamped")
	}
}

// Quantiles from the histogram are never below the exact quantile and at
// most 3.125% above it, on a wide, skewed distribution.
func TestLogQuantilesWithinBound(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	h := NewLogHistogram()
	vals := make([]int64, 200_000)
	for i := range vals {
		// Log-uniform from 1 ns to about 1 ms, like latencies.
		v := int64(1) << rng.IntN(20)
		v += rng.Int64N(v)
		vals[i] = v
		h.Observe(v)
	}
	slices.Sort(vals)
	s := h.Snapshot()
	if s.Count() != uint64(len(vals)) {
		t.Fatalf("count %d, want %d", s.Count(), len(vals))
	}
	for _, q := range []float64{0, 0.001, 0.5, 0.9, 0.99, 0.999, 0.9999, 1} {
		rank := int(float64(len(vals)) * q)
		if float64(rank) < float64(len(vals))*q {
			rank++
		}
		exact := vals[max(rank, 1)-1]
		got := s.QuantileNS(q)
		if got < exact || float64(got-exact) > float64(exact)/logSub {
			t.Errorf("q=%v: got %d, exact %d (allowed up to +%.3f%%)", q, got, exact, 100.0/logSub)
		}
	}
	if s.MaxNS() < vals[len(vals)-1] {
		t.Errorf("max %d below the true max %d", s.MaxNS(), vals[len(vals)-1])
	}
	var sum int64
	for _, v := range vals {
		sum += v
	}
	if s.MeanNS() != float64(sum)/float64(len(vals)) {
		t.Errorf("mean %v, want exact %v", s.MeanNS(), float64(sum)/float64(len(vals)))
	}
}

func TestLogSnapshotArithmetic(t *testing.T) {
	h := NewLogHistogram()
	for _, v := range []int64{10, 20, 1000} {
		h.Observe(v)
	}
	before := h.Snapshot()
	for _, v := range []int64{5000, 7000} {
		h.Observe(v)
	}
	window := h.Snapshot().Sub(before)
	if window.Count() != 2 || window.SumNS != 12000 {
		t.Fatalf("window: count %d sum %d, want 2 and 12000", window.Count(), window.SumNS)
	}
	if q := window.QuantileNS(0.5); q < 5000 || q > 5000+5000/logSub {
		t.Fatalf("window p50 %d, want ~5000", q)
	}
	both := window.Add(before)
	if both.Count() != 5 || both.SumNS != h.Snapshot().SumNS {
		t.Fatalf("add: count %d sum %d", both.Count(), both.SumNS)
	}
	var empty LogSnapshot
	if empty.QuantileNS(0.5) != 0 || empty.MaxNS() != 0 || empty.MeanNS() != 0 {
		t.Fatal("empty snapshot should report zeros")
	}
	var nilH *LogHistogram
	nilH.Observe(1) // no panic
	if nilH.Snapshot().Count() != 0 {
		t.Fatal("nil histogram has observations")
	}
}

// The JSON form round-trips: it's what /debug/latency serves and the
// benchmark reads.
func TestLogSnapshotJSON(t *testing.T) {
	h := NewLogHistogram()
	h.Observe(300)
	h.Observe(900_000)
	b, err := json.Marshal(h.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	var back LogSnapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.SubBits != 5 || back.Count() != 2 || back.SumNS != 900_300 || back.MaxNS() != h.Snapshot().MaxNS() {
		t.Fatalf("round trip: %+v", back)
	}
}

func TestLogObserveDoesNotAllocate(t *testing.T) {
	h := NewLogHistogram()
	if n := testing.AllocsPerRun(1000, func() { h.Observe(12345) }); n != 0 {
		t.Fatalf("Observe allocates %v times", n)
	}
}

// Recording from several goroutines while snapshotting: run with -race.
func TestLogConcurrent(t *testing.T) {
	h := NewLogHistogram()
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10_000 {
				h.Observe(int64(g*1000 + i))
			}
		}()
	}
	for range 10 {
		_ = h.Snapshot()
	}
	wg.Wait()
	if n := h.Snapshot().Count(); n != 40_000 {
		t.Fatalf("count %d, want 40000", n)
	}
}

func BenchmarkLogObserve(b *testing.B) {
	h := NewLogHistogram()
	for i := 0; b.Loop(); i++ {
		h.Observe(int64(200 + i&1023))
	}
}
