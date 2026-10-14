# 001 — Price level storage: sorted slice, best level last

- **Status:** accepted (milestone 1)
- **Code:** `internal/book/book.go` (`ladder`, `level`, `Book.newLevel`, `Book.freeLevel`)

## Context

Each side of the book holds price levels. The access pattern is lopsided:

- **Best level**: read on every incoming order, and removed whenever it is consumed. This is the hottest path.
- **Inserting a new level**: happens when a limit order rests at a price that has no orders yet. In real markets most new levels appear at or near the best price.
- **Removing a level from deep in the book**: happens when the last order at a far price is cancelled. This is rarer.
- **Finding the level for a given price**: needed to rest an order at an existing level, and to cancel. Moderately hot.
- **The number of levels L**: tens to low thousands per side, not millions.

## Options considered

1. **Sorted slice of `*level`, best price at the end** (chosen).
   - Best level: `levels[len-1]`, O(1).
   - Remove best: shrink the slice by one, O(1), no copying.
   - Find a price: binary search, O(log L).
   - Insert a new level: binary search, then `copy` the better-priced levels up one slot. That costs O(k), where k is how many levels are better than the new one. Inserts near the best price make k small.
   - Remove a deep level: O(L − i) copy.
   - The slice is contiguous, so a binary search touches a few cache lines.
2. **Red-black tree (or another balanced BST).** Insert, remove and find are all O(log L) in the worst case, which is better on paper for deep inserts and removals. But:
   - every step is a pointer dereference to a separately allocated node, so cache behaviour is poor;
   - getting the best level needs either a cached min/max pointer kept up to date across rotations, or an O(log L) walk;
   - it needs node pooling to meet the zero-allocation requirement (PRD 6);
   - rebalancing code is 200+ lines where subtle bugs hide, and in an engine whose core claim is "obviously correct" that is a real cost.

   Rejected: it optimises the rare case (deep inserts) at the expense of the common one (best-level access) and of simplicity.
3. **Skip list.** O(log L) expected time, simpler than a red-black tree, and a well-known structure for order books. But:
   - level heights are chosen at random, and PRD 5.4 forbids randomness in the engine. A fixed-seed generator would technically be deterministic, but then the book's shape depends on generator state that would have to be snapshotted and replayed, which complicates milestone 5;
   - it is still pointer-chasing, with several pointers per node.

   Rejected for the determinism complication and the pointer chasing.
4. **Price-indexed array (one slot per tick in a fixed band around the mid).** Every operation is O(1), and it is common in production engines for instruments with a bounded price range. Rejected for v1:
   - it needs a configured price band per instrument and a policy for prices outside it;
   - finding the next best level after the best empties means scanning empty ticks.

   It is a reasonable upgrade later if profiling shows level lookup matters.

## Decision

Option 1. Each `ladder` stores `levels []*level` sorted so the **best level is last**.

Both sides share one implementation through a sign trick: levels are sorted ascending by `key = price × sign`.

- Bids use `sign = +1`, so the highest price sorts last.
- Asks use `sign = −1`, so the lowest price has the largest key and also sorts last.

`search` is a hand-written lower-bound binary search. It doesn't use `sort.Search` because a plain loop keeps the hot path free of closures and is easy to read.

Emptied `level` structs go onto `Book.freeLevels` and are reused by `newLevel`. Steady-state trading therefore doesn't allocate level structs. The slice's backing array also stops growing once it has reached the book's peak depth.

## Costs

- **Worst case: inserting or removing a level far from the best costs O(L).** With L = 1,000 levels that is a `memmove` of up to 8 KB of pointers. That is fast in absolute terms, but it is a tail-latency source. The milestone 9 benchmark doesn't exercise it: its passive orders rest 1–10 ticks from the mid, so a side holds at most 10 levels ([decision 021](021-benchmark-harness.md)). If a deeper workload shows the cost, the price-indexed array (option 4) is the next step.
- **The slice holds pointers to levels** rather than levels by value. That costs one indirection, but means a `*level` stays valid while the slice shifts around it. Matching relies on this: it keeps `lv` across the inner loop.
- **The sign trick is a small piece of cleverness.** `TestLadderOrderingBestLast` pins it down, and the differential test exercises it on both sides.
