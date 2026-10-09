// Package testutil holds helpers shared by tests in several packages. Nothing
// outside _test.go files imports it.
package testutil

import (
	"fmt"
	"runtime"
	"strings"
)

// OwnAllocations runs fn runs times with every allocation recorded by the
// memory profiler, and returns one line per allocation site whose call stack
// passes through this module's code, with the number of objects allocated
// there.
//
// Why not just count runtime.MemStats.Mallocs before and after: Mallocs is
// process-wide. The Go runtime's own background goroutines (the memory
// scavenger's timer, GC mark workers, creating an OS thread) allocate a few
// objects at unpredictable moments, so a Mallocs-based "must be zero" check
// fails at random. The profiler records each allocation with its stack, which
// lets us count only allocations our code caused (docs/learn/02-engine.md).
//
// OwnAllocations sets runtime.MemProfileRate, which is process-wide, so it
// must not run while other tests run in parallel.
func OwnAllocations(runs int, fn func()) []string {
	var sites []string
	for stack, n := range ownAllocationCounts(runs, fn) {
		sites = append(sites, fmt.Sprintf("  %d x %s", n, stack))
	}
	return sites
}

// CountOwnAllocations is OwnAllocations reduced to a total object count.
func CountOwnAllocations(runs int, fn func()) int64 {
	var total int64
	for _, n := range ownAllocationCounts(runs, fn) {
		total += n
	}
	return total
}

// ownAllocationCounts does the measurement for both helpers above: objects
// allocated during the runs, per call stack, for stacks owned by this module.
func ownAllocationCounts(runs int, fn func()) map[string]int64 {
	old := runtime.MemProfileRate
	runtime.MemProfileRate = 1 // record every allocation from here on
	defer func() { runtime.MemProfileRate = old }()

	// The profile is only published at the end of a GC cycle. Two cycles
	// before each snapshot make sure everything allocated so far is in it.
	runtime.GC()
	runtime.GC()
	before := profileSnapshot()
	for range runs {
		fn()
	}
	runtime.GC()
	runtime.GC()
	after := profileSnapshot()

	counts := make(map[string]int64)
	for stack, n := range after {
		if d := n - before[stack]; d > 0 && ownedByModule(stack) {
			counts[stack] = d
		}
	}
	return counts
}

// ownedByModule attributes an allocation to the innermost frame of this
// module on its stack (frames are listed innermost first). The allocation
// counts if that frame is real code (book, engine, a test's workload); it is
// ignored if there is no such frame (pure runtime work) or if the frame is
// in this helper itself (its runtime.GC calls and profile snapshots).
//
// Two simpler rules both fail, which is how this one was arrived at:
// "ignore any stack mentioning testutil" ignores everything, because fn is
// always called from ownAllocationCounts (caught by TestAllocCheckCanFail); and
// "ignore stacks through profileSnapshot" still counts the semaphore that
// this helper's own runtime.GC call occasionally allocates (about 1 run
// in 150).
//
// The helper is identified by its two measuring functions, not by package:
// a test inside this package (allocs_test.go) runs its workload from a
// closure in package testutil, and that workload must still be counted.
func ownedByModule(stack string) bool {
	for _, frame := range strings.Split(stack, " <- ") {
		if strings.HasPrefix(frame, "github.com/tanxena/matchd/") {
			return !strings.HasPrefix(frame, helperPrefix+"ownAllocationCounts:") &&
				!strings.HasPrefix(frame, helperPrefix+"profileSnapshot:")
		}
	}
	return false
}

const helperPrefix = "github.com/tanxena/matchd/internal/testutil."

// profileSnapshot returns cumulative allocation counts per call stack.
func profileSnapshot() map[string]int64 {
	n, _ := runtime.MemProfile(nil, true)
	recs := make([]runtime.MemProfileRecord, n+64) // headroom: records may be added between calls
	n, _ = runtime.MemProfile(recs, true)
	out := make(map[string]int64, n)
	for _, r := range recs[:n] {
		var sb strings.Builder
		frames := runtime.CallersFrames(r.Stack())
		for {
			f, more := frames.Next()
			fmt.Fprintf(&sb, "%s:%d", f.Function, f.Line)
			if !more {
				break
			}
			sb.WriteString(" <- ")
		}
		out[sb.String()] += r.AllocObjects
	}
	return out
}
