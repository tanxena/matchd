# 007 — Proving "zero allocations": what to count and how

- **Status:** accepted (milestone 2)

> **Superseded in part (2026-10-13, `7da7c30`):** `TestZeroAllocSteadyState` now runs eight workloads; milestone 8 added an IOC cycle and a FOK cycle ([decision 020](020-ioc-and-fok.md)). The method is unchanged.

- **Code:** `internal/testutil/allocs.go` (`OwnAllocations`, `CountOwnAllocations`, `ownedByModule`), `internal/engine/alloc_test.go`

## Context

PRD 6 and acceptance criterion 3 require zero allocations on the engine's steady-state path for limit add, cancel and match, "proven by test". Getting a trustworthy test took four attempts. Each one failed in an instructive way, so the reasoning is recorded here.

## What we tried

1. **`testing.AllocsPerRun` alone** (the tool PRD 6 names). It returns `mallocs / runs` using **integer division**. A slice that doubles three times over 10,000 runs reports **0**. It cannot detect rare allocations, and rare allocations are exactly what a growing structure produces. Kept as a first-line check, but not trusted on its own. `TestAllocCheckCanFail` asserts this blind spot explicitly, so if a future Go version changes the behaviour, we'll find out.
2. **Count `runtime.MemStats.Mallocs` before and after 100,000 cycles, and require 0.** It failed at random, on whichever workload happened to be running, with 1 or 2 allocations. Profiling every allocation showed them all inside the Go runtime's own background goroutines: the memory scavenger's timer heap, GC mark workers, and creating an OS thread. `Mallocs` is process-wide, so it can't tell those apart from ours. Rejected.
3. **Profile every allocation (`runtime.MemProfileRate = 1`), and count stacks that pass through this module.** This found a **real** allocation, a Go map rebuild in `book.rest`, which led to decision 002 revision 2. But the filter was too broad. A first version excluded every stack mentioning `internal/testutil`, which excluded *everything*, because the workload is called from the helper; `TestAllocCheckCanFail` caught that. A second version excluded only the snapshot helper, and still failed about 1 run in 150 because the helper's own `runtime.GC()` call occasionally allocates a semaphore waiter.
4. **Attribute each allocation to the innermost frame of this module on its stack** (chosen). If that frame is one of the helper's two measuring functions, the allocation is the helper's own and is ignored. Otherwise it counts, whether that frame is book code, engine code, or a test's workload closure. Allocations with no module frame at all are runtime background work, and are ignored.

## Decision

`testutil.OwnAllocations(runs, fn)` returns one line per allocation site attributed to module code, with counts. `TestZeroAllocSteadyState` runs six workloads, each a **cycle that leaves the books as it found them**, for 10,000 warm-up cycles and then 100,000 measured cycles. It requires both `AllocsPerRun == 0` and no attributed allocations.

The cycles return the book to its starting state on purpose. Steady state means a book that isn't growing, so any allocation during a cycle is a real one, not a structure reaching a new peak.

## Evidence the check works both ways

- **It doesn't fail on noise:** 0 failures in 300 consecutive runs, then 0 in 100 more after the final changes.
- **It catches real allocations:** removing the reused fills buffer from `Engine.newOrder` (one line) made exactly the three matching workloads fail, with about 100,000 to 300,000 attributed allocations each. Add, cancel and reject workloads still passed.
- **Its own blind spots are tested:** `TestAllocCheckCanFail` (2 allocations in 1,000 runs must be detected), and `TestCountOwnAllocations` / `TestOwnedByModule` in `internal/testutil`, including a workload closure that lives inside package `testutil`.

## Costs

- `MemProfileRate` is process-wide, so these tests must not run in parallel with other tests in the same binary. None of the allocation tests call `t.Parallel`.
- Profiling every allocation makes the measured loops slower. That doesn't matter, because they measure counts, not time. The benchmarks (`BenchmarkCycles`) run without it.
- Coverage instrumentation allocates, so the tests skip under `-cover`.
- The attribution depends on frame names (`github.com/tanxena/matchd/...`). Renaming the module means updating `ownedByModule`; `TestOwnedByModule` would fail first.
