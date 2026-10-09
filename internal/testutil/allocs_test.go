package testutil

import "testing"

func TestOwnedByModule(t *testing.T) {
	cases := []struct {
		name  string
		stack string
		want  bool
	}{
		{"engine code", "runtime.mallocgc:1 <- runtime.growslice:2 <- github.com/tanxena/matchd/internal/engine.(*Engine).newOrder:3 <- github.com/tanxena/matchd/internal/testutil.ownAllocationCounts:4", true},
		{"book via map growth", "runtime.mallocgc:1 <- internal/runtime/maps.newTable:2 <- github.com/tanxena/matchd/internal/book.(*Book).rest:3 <- github.com/tanxena/matchd/internal/engine.(*Engine).Step:4", true},
		{"test workload", "runtime.makeslice:1 <- github.com/tanxena/matchd/internal/engine_test.TestX.func1:2 <- github.com/tanxena/matchd/internal/testutil.ownAllocationCounts:3", true},
		{"workload closure in package testutil", "runtime.makeslice:1 <- github.com/tanxena/matchd/internal/testutil.TestCountOwnAllocations.func2:2 <- github.com/tanxena/matchd/internal/testutil.ownAllocationCounts:3", true},
		{"helper's own GC", "runtime.acquireSudog:1 <- runtime.GC:2 <- github.com/tanxena/matchd/internal/testutil.ownAllocationCounts:3 <- github.com/tanxena/matchd/internal/engine_test.TestX:4", false},
		{"helper's snapshot", "runtime.makeslice:1 <- github.com/tanxena/matchd/internal/testutil.profileSnapshot:2", false},
		{"pure runtime", "runtime.mallocgc:1 <- runtime.bgscavenge:2 <- runtime.goexit:3", false},
	}
	for _, c := range cases {
		if got := ownedByModule(c.stack); got != c.want {
			t.Errorf("%s: ownedByModule = %v, want %v", c.name, got, c.want)
		}
	}
}

var sink []byte

func TestCountOwnAllocations(t *testing.T) {
	if n := CountOwnAllocations(1000, func() {}); n != 0 {
		t.Errorf("empty function: %d allocations, want 0", n)
	}
	i := 0
	n := CountOwnAllocations(1000, func() {
		i++
		if i%250 == 0 {
			sink = make([]byte, 100+i) // 4 allocations in 1,000 runs
		}
	})
	if n != 4 {
		t.Errorf("counted %d allocations, want exactly 4", n)
	}
}
