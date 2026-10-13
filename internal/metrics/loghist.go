package metrics

import (
	"math"
	"math/bits"
	"sync/atomic"
)

// LogHistogram counts nanosecond observations into log-linear buckets, the
// layout HDR histograms use: every power of two is split into 32 equal
// sub-buckets, so a bucket is never wider than 1/32 (3.125%) of the values
// it holds, at any scale. Values below 64 ns each get their own bucket.
//
// It exists for the benchmark (milestone 9, decision 017's amendment): the
// fixed /metrics buckets can only say a p50 is "between 0.5 and 1 us", and
// can't give a maximum at all. This one gives every quantile to within
// 3.1%, rounded up.
//
// Recording is O(1) (a bit length and two shifts, no search), lock-free and
// allocation-free; the counts are about 9 KB. Values of 2^40 ns (about 18
// minutes) and more land in the last bucket.
type LogHistogram struct {
	counts [logBuckets]atomic.Uint64
	sum    atomic.Int64 // nanoseconds
}

const (
	logSubBits = 5                                      // 32 sub-buckets per power of two
	logSub     = 1 << logSubBits                        // 32
	logMaxBits = 40                                     // values up to 2^40 ns have exact buckets
	logBuckets = (logMaxBits - logSubBits + 1) * logSub // 1152
)

// NewLogHistogram returns an empty histogram.
func NewLogHistogram() *LogHistogram { return &LogHistogram{} }

// logIndex is the bucket for v nanoseconds. Below 2*logSub every value has
// its own bucket. Above, with v in [2^(e-1), 2^e), the value is shifted
// right by e-1-logSubBits so that 32 <= v>>shift < 64, and each shift step
// takes 32 more buckets.
func logIndex(v int64) int {
	if v < 2*logSub {
		if v < 0 {
			return 0
		}
		return int(v)
	}
	shift := bits.Len64(uint64(v)) - logSubBits - 1
	i := shift*logSub + int(uint64(v)>>shift)
	if i >= logBuckets {
		return logBuckets - 1
	}
	return i
}

// logBounds returns the smallest and largest value bucket i holds.
func logBounds(i int) (lo, hi int64) {
	if i < 2*logSub {
		return int64(i), int64(i)
	}
	shift := i/logSub - 1
	sub := int64(i - shift*logSub) // in [32, 64)
	return sub << shift, (sub+1)<<shift - 1
}

// Observe records one observation of ns nanoseconds (negative counts as 0).
func (h *LogHistogram) Observe(ns int64) {
	if h == nil {
		return
	}
	h.counts[logIndex(ns)].Add(1)
	h.sum.Add(max(ns, 0))
}

// LogSnapshot is a LogHistogram's counts at one moment. It is what
// GET /debug/latency serves (as JSON) and what the benchmark subtracts.
type LogSnapshot struct {
	SubBits int      `json:"sub_bits"` // always 5; lets a reader check the layout
	Counts  []uint64 `json:"counts"`   // per bucket, not cumulative
	SumNS   int64    `json:"sum_ns"`
}

// Snapshot reads the histogram. Like Histogram.Snapshot, it doesn't stop
// writers: counts recorded during the read may or may not be included.
func (h *LogHistogram) Snapshot() LogSnapshot {
	s := LogSnapshot{SubBits: logSubBits, Counts: make([]uint64, logBuckets)}
	if h == nil {
		return s
	}
	for i := range h.counts {
		s.Counts[i] = h.counts[i].Load()
	}
	s.SumNS = h.sum.Load()
	return s
}

// Sub returns the observations made between earlier and s (the same
// histogram read twice).
func (s LogSnapshot) Sub(earlier LogSnapshot) LogSnapshot {
	out := LogSnapshot{SubBits: s.SubBits, Counts: make([]uint64, len(s.Counts)), SumNS: s.SumNS - earlier.SumNS}
	for i, c := range s.Counts {
		out.Counts[i] = c
		if i < len(earlier.Counts) {
			out.Counts[i] -= earlier.Counts[i]
		}
	}
	return out
}

// Add returns the observations of s and o together (two histograms of the
// same kind, e.g. one per connection).
func (s LogSnapshot) Add(o LogSnapshot) LogSnapshot {
	n := max(len(s.Counts), len(o.Counts))
	out := LogSnapshot{SubBits: logSubBits, Counts: make([]uint64, n), SumNS: s.SumNS + o.SumNS}
	for i := range n {
		if i < len(s.Counts) {
			out.Counts[i] += s.Counts[i]
		}
		if i < len(o.Counts) {
			out.Counts[i] += o.Counts[i]
		}
	}
	return out
}

// Count is the number of observations.
func (s LogSnapshot) Count() uint64 {
	var n uint64
	for _, c := range s.Counts {
		n += c
	}
	return n
}

// MeanNS is the exact mean, from the sum (0 if empty).
func (s LogSnapshot) MeanNS() float64 {
	n := s.Count()
	if n == 0 {
		return 0
	}
	return float64(s.SumNS) / float64(n)
}

// QuantileNS returns quantile q (0..1) of the observations, as the upper
// bound of the bucket holding it: the true value is at most this, and at
// least this minus 3.125%. The rank is ceil(q*count), so 0.5 of 4 values is
// the 2nd smallest; q=1 gives the maximum's bucket. 0 if empty.
func (s LogSnapshot) QuantileNS(q float64) int64 {
	total := s.Count()
	if total == 0 {
		return 0
	}
	rank := max(uint64(math.Ceil(q*float64(total))), 1)
	var seen uint64
	for i, c := range s.Counts {
		seen += c
		if seen >= rank {
			_, hi := logBounds(i)
			return hi
		}
	}
	_, hi := logBounds(len(s.Counts) - 1)
	return hi
}

// MaxNS is the upper bound of the highest non-empty bucket: the maximum,
// to within 3.125%, rounded up. 0 if empty.
func (s LogSnapshot) MaxNS() int64 { return s.QuantileNS(1) }
