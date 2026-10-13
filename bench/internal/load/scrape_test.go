package load

import (
	"math"
	"testing"
)

// The slowest fsync between two scrapes is bounded by the lowest bucket
// whose cumulative count grew by every observation made between them.
func TestMaxBucket(t *testing.T) {
	before := map[string]float64{
		"f_count": 10, `f_bucket{le="0.001"}`: 8, `f_bucket{le="0.01"}`: 10, `f_bucket{le="+Inf"}`: 10,
	}
	after := map[string]float64{
		"f_count": 15, `f_bucket{le="0.001"}`: 12, `f_bucket{le="0.01"}`: 15, `f_bucket{le="+Inf"}`: 15,
	}
	if got := maxBucket(before, after, "f"); got != 0.01 { // one of the five new ones was over 1 ms
		t.Fatalf("maxBucket = %v, want 0.01", got)
	}
	if got := maxBucket(after, after, "f"); got != 0 {
		t.Fatalf("no observations: %v", got)
	}
	after2 := map[string]float64{"f_count": 11, `f_bucket{le="0.001"}`: 8, `f_bucket{le="0.01"}`: 10, `f_bucket{le="+Inf"}`: 11}
	if got := maxBucket(before, after2, "f"); !math.IsInf(got, 1) {
		t.Fatalf("beyond the last bound: %v", got)
	}
}
