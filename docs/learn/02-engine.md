# 02 — The engine loop: one goroutine, commands in, events out

Milestone 2 wraps the book from milestone 1 in an **engine**: a single goroutine that owns every instrument's book, applies a totally ordered stream of commands, and emits events describing what happened. Everything is still in memory; the network arrives in milestone 3 and the disk in milestone 4.

This milestone also contains this project's first **design reversal**. Proving the PRD's "zero allocations" requirement uncovered Go maps allocating in steady state, so the book's lookups were replaced with a purpose-built hash table. Most of what is worth learning here sits in that story, and in how hard it is to *measure* zero allocations honestly.

---

## 1. Concepts

### 1.1 Why one goroutine owns the whole engine

PRD section 4 calls the single-threaded engine "the central thing you must be able to defend". Here is the defence.

A matching engine is a **state machine**: state (the books), plus a function that takes the current state and one command and produces a new state and some outputs. If the same commands are applied in the same order to the same starting state, you get the same results every time. That property, determinism, gives us three things at once:

- **Recovery.** Write every command to a log before applying it (milestone 4). After a crash, replay the log into an empty engine and you get back exactly the same books, ids and events. This is the *state machine approach* (F. B. Schneider, "Implementing fault-tolerant services using the state machine approach: a tutorial", *ACM Computing Surveys* 22(4), 1990).
- **Testing.** A test can feed a command list and compare the output, with no timing involved.
- **Fairness.** "Who was first" has exactly one answer: their position in the sequence.

Determinism needs a **single, total order** of commands and **no hidden inputs**. Hidden inputs include the wall clock, random numbers, and the order in which Go iterates a map. A single goroutine gives the total order for free. If you let several goroutines touch the books, even with locks, the interleaving depends on the OS scheduler, and the scheduler is a hidden input.

*If you know TypeScript:* this is the Node.js event loop, deliberately. One thread runs your handlers one at a time, so handlers never need locks. The concurrency is in the I/O around them. Our "I/O" (connections, the disk log, fan-out to subscribers) runs in other goroutines that talk to the engine only through channels.

What it costs: one engine uses one core. That is fine here. A matching step takes a few hundred nanoseconds (section 2), so even a single core can process millions of commands per second, more than the network or disk will deliver. Splitting across cores would need a design decision about ordering across instruments, which PRD section 3 rules out for v1.

### 1.2 Commands, events, and the three counters

**Commands** are inputs: `NewOrder`, `Cancel`, `AddInstrument`. **Events** are outputs: `Ack`, `Reject`, `Fill`, `Cancelled`, `BookDelta`, `InstrumentAdded`. One command produces zero or more events, in an order fixed by [decision 006](../decisions/006-event-model.md).

Three counters give everything an identity. Each is **gap-free**, meaning every number is used exactly once, in order:

| counter | assigned by | advances on | used for |
|---|---|---|---|
| `Seq` | sequencer (since milestone 3; written to the WAL with each command since milestone 4) | every command, including rejected ones | ordering; durability gating; replay |
| `OrderID` | engine | accepted new orders only | cancels; routing fills |
| `MatchID` | engine | every match | pairing the taker's and maker's fill events |

**Why gap-free matters.** A consumer that sees `Seq` 41 and then 43 knows for certain that it missed something; no timeout is needed. The engine **panics** if a command arrives out of sequence ([decision 005](../decisions/005-engine-loop.md)). After a gap, the engine's state would no longer be a function of the log, and a process that stops is recoverable in a way a silently diverging one is not.

**Why timestamps arrive on the command.** If the engine called `time.Now()`, replaying the log tomorrow would stamp tomorrow's times on events. The sequencer stamps the time once, writes it into the log with the command, and the engine just copies it (PRD 5.4).

### 1.3 Market data: market-by-price deltas

A market-data subscriber wants to keep its own copy of the book. There are two common ways to describe changes:

- **Market by order (MBO):** one message per order event: added, executed, cancelled. NASDAQ [TotalView-ITCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHspecification.pdf) works this way (*Add Order*, *Order Executed*, *Order Cancel*, *Order Delete*). Subscribers can rebuild the full queue at every price.
- **Market by price (MBP):** one message per price level change: "level 100 on the ask side now holds 37". Smaller, and enough for anyone who doesn't need individual queue positions.

Our `BookDelta` events are **market by price** and carry the level's **new absolute size**, not the change, so a delta is idempotent. The engine emits one per touched level per command, after that level's fills. The proof that this is enough to rebuild the book is `TestEventStreamRebuildsState`, described in section 2.

### 1.4 Why allocations matter on the matching path

Allocating memory in Go is cheap, but not free. Every heap allocation costs something to make, and adds work for the garbage collector, which runs alongside the program and can briefly slow it down ([A Guide to the Go Garbage Collector](https://go.dev/doc/gc-guide)). For a matching engine, the concern isn't the average: it is the **tail**, the p99.9 latency (PRD 7). One allocation in ten thousand that triggers extra GC work shows up exactly there.

So PRD 6 asks for **zero allocations in steady state**. "Steady state" means a book that isn't growing. Reaching a new peak size may allocate once (a slice doubles); adding and removing orders at a constant depth must not.

### 1.5 Hash tables, tombstones, and why the book stopped using Go maps

A hash table stores keys in an array. A key's hash picks its **home slot**, and **open addressing** means that if the home slot is taken, you probe onward (here: the next slot, then the next: **linear probing**) until you find a free one. Lookups follow the same path and stop at the first empty slot.

Deletion is the hard part. You can't simply empty a slot, because a key further along the same probe path would become unreachable: lookups stop at the first empty slot. There are two standard fixes:

- **Tombstones.** Mark the slot "deleted, keep probing". Simple, but tombstones fill the table. Eventually the table must be **rebuilt** to clear them, and a rebuild usually allocates a new array and re-inserts everything: O(n), on whichever unlucky insert triggers it. Go's maps use a far more sophisticated design ([Swiss tables, Go 1.24](https://go.dev/blog/swisstable)), but they also mark deletions this way.
- **Backward-shift deletion.** After emptying the slot, walk the rest of the probe run, and move each entry back into the hole if (and only if) the hole lies between that entry's home and its current position. The table never contains markers, so it never needs rebuilding. This is what `internal/book/index.go` does.

**Hash flooding.** If an attacker knows your hash function, they can choose keys that all hash to the same slot, and every lookup becomes a linear scan. Client order ids are chosen by clients, so the index uses a **seeded** hash (`hash/maphash`) with a random seed per book. The seed changes where entries sit in memory but never what the book outputs, so replay stays exact ([decision 002, revision 2](../decisions/002-order-storage-and-cancel.md)).

**The evidence that triggered the change.** The engine's zero-allocation test caught a Go map rebuilding inside `book.rest` in about 1 run in 3. To show this wasn't a fluke of the engine test, `internal/book/mapchurn_test.go` measures it directly. It runs 20 independent tables per size through 1,000,000 operations each; one operation deletes a random live key and inserts a brand-new one, as order ids do in a book:

```
go test ./internal/book -run MapChurn -mapchurn -v
```

| live keys | Go map: allocations | Go map: per million ops | index: allocations | index: table grew |
|---:|---:|---:|---:|---:|
| 50 | 0 | 0.00 | 0 | 0 of 20 |
| 400 | 0 | 0.00 | 0 | 0 of 20 |
| 5000 | 270 | 13.50 | 0 | 0 of 20 |
| 50000 | 0 | 0.00 | 0 | 0 of 20 |

*Go 1.27.0, windows/amd64, Intel i7-8650U. Allocations counted with `testutil.OwnAllocations` (section 1.6). Go maps get a random hash seed each, so the 5,000 row varies between runs (261 and 270 both seen).*

Read this table carefully. It does **not** say "Go maps allocate at 5,000 keys". A finer sweep found rebuilds at 5,000, 10,000 and 20,000 live keys and none at sizes in between. A different churn pattern (insert a key, then delete that same key) found none at any size, yet the engine test hit one at about 400 orders. **Whether a Go map rebuilds depends on its size, its whole history and its random seed.** That unpredictability is the argument. You can't promise zero allocations on top of a structure whose allocations you can't predict. The index allocates only in `grow`, which runs only when the live count passes half the table, so under constant-size churn it *cannot* allocate. Its columns in the table are zero by construction, and the test checks it.

### 1.6 Measuring "zero" honestly

Zero is a hard number to measure. It took four attempts, recorded in [decision 007](../decisions/007-allocation-testing.md):

1. **`testing.AllocsPerRun`** computes `mallocs / runs` with integer division. Three allocations over 10,000 runs reads as **0**. It can't see rare allocations, and rare allocations are exactly what structures that occasionally rebuild produce. This is how the Go map problem hid at first.
2. **Counting `runtime.MemStats.Mallocs`** over 100,000 cycles, requiring 0. It failed at random, because `Mallocs` counts the whole process, including the Go runtime's own background goroutines (memory scavenger, GC workers, OS thread creation).
3. **Profiling every allocation with its stack** (`runtime.MemProfileRate = 1`), and counting stacks that pass through our code. This is what found the map rebuild. The first filter was too broad and silently excluded everything; the second was slightly too narrow.
4. **Attributing each allocation to the innermost frame of our module on its stack** is what we use. Book or engine code counts; a test's workload counts; the measuring helper's own work and pure runtime work don't.

The general lesson: **a test of "nothing happened" needs its own test showing it can see something happen.** `TestAllocCheckCanFail` plants two allocations in 1,000 runs and requires the check to report them. That test is what caught attempt 3's first filter excluding everything.

### 1.7 Goroutines and channels in one page

- A **goroutine** is a function running concurrently, started with `go f()`. Goroutines are cheap: a few KB each, scheduled by the Go runtime onto OS threads.
- A **channel** (`chan T`) is a typed queue that goroutines use to hand values to each other. `ch <- v` sends, and blocks if the channel is full; `v := <-ch` receives, and blocks if it is empty. `make(chan T, 64)` gives a buffer of 64. `close(ch)` says "no more values". `for v := range ch` receives until the channel is closed and drained.
- The Go motto is "do not communicate by sharing memory; instead, share memory by communicating" ([Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency)). The engine is a direct application: only the engine goroutine touches the books, and the rest of the program sends it commands and receives events.
- The **race detector** (`go test -race`, [docs](https://go.dev/doc/articles/race_detector)) watches memory accesses at run time and reports two goroutines touching the same memory without synchronisation. `TestRunMatchesStep` runs the engine on its own goroutine and is run under `-race`.

---

## 2. Code walkthrough

### `internal/engine/types.go`: commands and events

- **`InstrumentID`, `Seq`, `Timestamp`** ([types.go:22–31](../../internal/engine/types.go#L22)). Named integer types, as in milestone 1. Instrument id 0 is never valid.
- **`CommandKind`, `Command`** ([types.go:34, 58](../../internal/engine/types.go#L58)). One flat struct (104 bytes) for every command kind; fields a kind doesn't use stay zero. `Client` is set by the gateway from the logged-in session (milestone 3), never taken from the frame payload, which is what makes owner-scoped cancel secure.
- **`EventKind`** ([types.go:81](../../internal/engine/types.go#L81)). Each constant's comment states its contract, for example "`EvAck`: always the first event for an accepted order".
- **`Role`, `CancelReason`** ([types.go:123, 131](../../internal/engine/types.go#L123)). Taker or maker; requested or expired.
- **`Event`** ([types.go:145](../../internal/engine/types.go#L145)). One flat, pointer-free 96-byte struct. `Qty` means a different thing per kind: order qty for an ack, matched qty for a fill, qty removed for a cancel, the level's new total for a delta. The comment on the field says so. `Leaves` is a party's open quantity after a fill.

### `internal/engine/engine.go`: the engine

- **`Engine`** ([engine.go:15](../../internal/engine/engine.go#L15)) holds:
  - `books []*book.Book`, indexed by `InstrumentID`, with slot 0 unused;
  - `symbols map[string]InstrumentID`, which is only touched by `AddInstrument`, so its rare allocations don't matter;
  - the three counters `lastSeq`, `lastOrderID`, `lastMatchID`;
  - `fills`, a scratch buffer reused across commands.

  The doc comment states the ownership rule: one goroutine, no locks.
- **`Step`** ([engine.go:41](../../internal/engine/engine.go#L41)) checks `c.Seq == lastSeq+1` (panicking otherwise), records it, and dispatches on `Kind`. It panics on an unknown kind.
- **`Run`** ([engine.go:76](../../internal/engine/engine.go#L76)) is the goroutine body:

  ```
  defer close(out)
  for each command on in:
      buf = Step(&command, buf[:0])
      send every event in buf on out
  ```

  When `in` is closed, the loop ends, the deferred `close(out)` runs, and the consumer's `range` loop ends. That is how shutdown propagates (draining is PRD 6's graceful shutdown, milestone 7).

  *Since milestone 5,* `Run` also takes snapshot hooks and selects on two more channels (a snapshot-trigger tick, and snapshots coming back to be thawed). It freezes the engine between commands when a trigger fires ([05-recovery.md](05-recovery.md#2-code-walkthrough)).
- **`newOrder`** ([engine.go:161](../../internal/engine/engine.go#L161)):
  1. Look up the book. If it doesn't exist, reject with `UnknownInstrument`.
  2. Build a `book.Order` with id `lastOrderID+1` and call `Submit` with the scratch fills buffer.
  3. If the book rejects it, emit a `Reject`. The id was never committed.
  4. Otherwise commit `lastOrderID`, then emit the `Ack`.
  5. For each fill: bump `lastMatchID`, reduce the taker's `leaves`, and emit the taker event then the maker event. Fills arrive grouped by price, best first, so after the **last fill at a price** (the next fill has a different price, or there isn't one), emit `BookDelta` with `bk.LevelQty(opp, price)`, the level's final size, because matching has already finished.
  6. Emit `Cancelled(expired)` for a market order's remainder, and a `BookDelta` for the taker's own level if a limit remainder rested.
- **`cancel`** ([engine.go:226](../../internal/engine/engine.go#L226)) calls `book.Cancel` and emits `Cancelled(requested)`, echoing the client order id, side and price returned in `CancelResult`, then the level's delta. Or it emits a `Reject`.
- **`addInstrument`** ([engine.go:249](../../internal/engine/engine.go#L249)) validates the spec, rejects duplicate symbols, creates the book, and appends it, so its id is `len(books)`.
- **Accessors:** `LastSeq`, `LastOrderID`, `Lookup`, `Book`, `NumInstruments`. They are for tests now, and for snapshots in milestone 5.

### `internal/book/index.go`: the hash index (new)

- **`index[K comparable]`** ([index.go:31](../../internal/book/index.go#L31)) holds a `maphash.Seed`, a power-of-two `slots` array, a `mask` (`len-1`, so `hash & mask` is a fast modulo), and `count`. A slot with `val == 0` is empty, which works because slab slot 0 is never used.
- **`newIndex(hint)`** ([index.go:45](../../internal/book/index.go#L45)) sizes the table to at least `2 × hint` slots, minimum 16, so the load starts at or below 1/2.
- **`get`** ([index.go:60](../../internal/book/index.go#L60)) probes from `home(k)` until it finds the key or an empty slot.
- **`put`** ([index.go:73](../../internal/book/index.go#L73)) first grows if adding one more entry would exceed half the table, then probes to an empty slot or the existing key.
- **`delete`** ([index.go:95](../../internal/book/index.go#L95)) finds the key, then **backward-shifts**. For each following entry `j` in the run, with home `h`, it asks whether `h` lies in the cyclic range `(i, j]`, where `i` is the current hole. If not, the entry may move back: copy it into the hole, and its old slot becomes the new hole. The `i < j` and `i > j` branches handle runs that wrap past the end of the array. It finishes by emptying the final hole.
- **`grow`** ([index.go:131](../../internal/book/index.go#L131)) doubles the table and re-inserts everything. It is the **only** allocation in the type.

### Book changes for the engine

- **`Book.byID`, `Book.byClOrd`** are now `index[OrderID]` and `index[clientOrderKey]`, created with room for 1,024 orders.
- **`Book.LevelQty(side, px)`** ([inspect.go:18](../../internal/book/inspect.go#L18)) is a binary search for the level, returning its total or 0. The differential harness now checks it against the reference book's per-order state for every level after every command.
- **`CancelResult`** now echoes `Side`, `Price` and `ClOrdID`, and the reference book returns the same. Because the differential harness compares `CancelResult` with `==`, those fields are checked on every cancel too.
- **`RejectReason`** gained `UnknownInstrument`, `InvalidInstrument` and `DuplicateSymbol`, which only the engine returns.

### `internal/testutil/allocs.go`: counting our own allocations (new)

- **`OwnAllocations(runs, fn)`** ([allocs.go:25](../../internal/testutil/allocs.go#L25)) and **`CountOwnAllocations`** are thin wrappers over **`ownAllocationCounts`** ([allocs.go:44](../../internal/testutil/allocs.go#L44)). That function:
  1. sets `runtime.MemProfileRate = 1`;
  2. runs GC twice, because profile data is only published after a GC cycle;
  3. takes a snapshot;
  4. runs `fn` `runs` times;
  5. runs GC twice and snapshots again;
  6. keeps the stacks whose counts grew and which **`ownedByModule`** ([allocs.go:86](../../internal/testutil/allocs.go#L86)) attributes to module code.
- **`ownedByModule`** scans the stack from the innermost frame outward and stops at the first frame in `github.com/tanxena/matchd/`. If that frame is one of the helper's two measuring functions, the allocation is the helper's own and is ignored; otherwise it counts.

### Tests

- **`internal/engine/engine_test.go`**: exact event sequences, written out field by field.
  - `TestNewOrderEventSequence` (resting order, a sweep over two levels, a market order with an expired remainder, partial fill then rest).
  - `TestCancelEvents` (wrong owner, success, repeat).
  - `TestRegistry` (ids, duplicates, invalid specs, unknown instruments, books are independent).
  - `TestOrderIDsAreGapFreeAcrossRejects`.
  - `TestEventsCarryCommandSeqAndTime`.
  - The panic tests for sequence gaps, repeats and unknown kinds.

  A small `driver` plays the sequencer, assigning `Seq` and a fake clock.
- **`internal/engine/stream_test.go`**: randomized properties.
  - `streamGen` produces commands for two instruments. The mix is limits, markets, cancels (including the wrong owner and the wrong instrument), invalid orders and unknown instruments.
  - **`views.apply`** ([stream_test.go:99](../../internal/engine/stream_test.go#L99)) folds each command's events into two pictures built **only from events**: a market-data view, from deltas, and a client view of every order's open quantity, from acks, fills and cancels. Along the way it checks the per-command rules: Ack first, gap-free order ids, fills in taker/maker pairs with consecutive match ids, `Leaves` consistent with the running quantity, and a `Reject` alone.
  - **`views.check`** ([stream_test.go:170](../../internal/engine/stream_test.go#L170)) compares both views with the real books after every command, level by level and order by order, including counts, so a level that is never removed or an order that leaves without an event is caught.
  - `TestEventStreamRebuildsState` runs 8 seeds × 5,000 commands and requires every event kind to occur at least 1% as often as commands.
  - `TestDeterministic` runs a seed twice and requires identical event logs.
  - `TestRunMatchesStep` sends the same commands through `Run` on its own goroutine and requires an identical event stream.
- **`internal/engine/alloc_test.go`**: the zero-allocation proof.
  - Six workloads, each a cycle that leaves the books unchanged, run against a book with 200 background orders per side.
  - `TestZeroAllocSteadyState` requires both `AllocsPerRun == 0` and no attributed allocations over 100,000 cycles.
  - `TestAllocCheckCanFail` proves the check can see rare allocations that `AllocsPerRun` misses.
  - `BenchmarkCycles` times the workloads.
- **`internal/book/index_test.go`**:
  - `TestIndexAgainstMap`: 20 seeds × 20,000 random operations on a table that starts at 16 slots, compared with a Go map, with `checkIndex` after every operation. `checkIndex` verifies the count, and that every key can be reached from its home without crossing an empty slot.
  - `TestIndexChurnNeverGrows`: 1,000,000 churn operations at three sizes; the table must not grow.
  - `TestCheckIndexDetectsCorruption`.
  - Struct-key and zero-value tests.
- **`internal/book/mapchurn_test.go`**: the measurement behind decision 002 revision 2. It is behind the `-mapchurn` flag because it takes about 20 s.

### The evidence that the tests can fail

**Engine.** Six one-line bugs were planted in `engine.go`. Each was caught by the hand-written sequence tests, **and** separately by the randomized property test alone, which matters more because it doesn't rely on anyone predicting the bug:

| planted bug | randomized test caught it at |
|---|---|
| no delta when a level empties | seed 1, command #6 (market data still shows the level) |
| maker's fill reports the taker's side | seed 1, command #6 (fill pair mismatch) |
| taker's `Leaves` never decremented | seed 1, command #6 |
| a rejected order consumes an order id | seed 1, command #24 (Ack id gap) |
| no event for an expired market remainder | seed 1, command #0 (client view keeps an order the book doesn't have) |
| cancel's delta reports the wrong side | seed 1, command #17 |

**Index.** Four bugs in backward-shift deletion: always shifting, never shifting, ignoring wrap-around, and an off-by-one in the range test. They were caught at operations 26, 39, 391 and 15 respectively. A first attempt reported two of them as "not caught". They had in fact failed to compile, so the script now reports a build failure explicitly instead of silently counting it as a pass.

**Allocation check.** Removing the reused fills buffer from `newOrder` (one line) made exactly the three matching workloads fail, with about 100,000 to 300,000 attributed allocations each. Add, cancel and reject workloads still passed.

### Measurements

**Zero allocations** (`go test ./internal/engine -run Alloc -v`). Every workload, 100,000 cycles after 10,000 warm-up cycles, against 200 resting orders per side:

| workload (one cycle) | commands | `AllocsPerRun` | attributed allocations |
|---|---:|---:|---:|
| limit add + cancel | 2 | 0 | 0 |
| add to existing level + cancel | 4 | 0 | 0 |
| full match | 2 | 0 | 0 |
| partial fill + cancel remainder | 3 | 0 | 0 |
| market sweep of three levels | 4 | 0 | 0 |
| rejects (invalid, unknown order, unknown instrument) | 3 | 0 | 0 |

The test passed 400 consecutive runs (300, then 100 after the final change).

**Cycle cost** (`go test ./internal/engine -run '^$' -bench BenchmarkCycles -benchtime 2000000x -count 5`). These are microbenchmarks for intuition, not the PRD section 7 protocol: no warm-up period, no CPU pinning, no load generator. They must not be quoted in the README:

| workload (one cycle) | commands | median ns/cycle | min–max over 5 runs | B/op | allocs/op |
|---|---:|---:|---:|---:|---:|
| limit add + cancel | 2 | 281.5 | 276.4–301.9 | 0 | 0 |
| add to existing level + cancel | 4 | 523.6 | 514.2–554.9 | 0 | 0 |
| full match | 2 | 289.8 | 288.3–296.4 | 0 | 0 |
| partial fill + cancel remainder | 3 | 368.5 | 357.7–376.8 | 0 | 0 |
| market sweep of three levels | 4 | 841.6 | 817.9–852.3 | 0 | 0 |
| rejects | 3 | 86.9 | 85.1–92.1 | 0 | 0 |

*Go 1.27.0, windows/amd64, Intel Core i7-8650U @ 1.90 GHz (laptop, default power plan, other applications open).* Roughly 100–200 ns per command, including event construction.

---

## 3. Design decisions

- [002 (revision 2): Order storage and cancel](../decisions/002-order-storage-and-cancel.md). The slab is unchanged; the Go maps are replaced by a tombstone-free open-addressing index with a random per-book `maphash` seed. The record includes the measurement table and why the seed doesn't break determinism.
- [005: The engine loop](../decisions/005-engine-loop.md). One goroutine, no locks; `Step` is the logic and `Run` the goroutine; events are sent one by one, by value; backpressure stops at the publisher; sequence gaps panic; rejected commands consume a `Seq`; order ids are gap-free; instruments are created by commands; cancels name their instrument.
- [006: The event model](../decisions/006-event-model.md). One flat 96-byte `Event`; the exact event order per command; one delta per touched level, carrying its final size; an expired market remainder is a `Cancelled` event, not a reject.
- [007: Allocation testing](../decisions/007-allocation-testing.md). The four attempts, why each failed, and the attribution rule.

---

## 4. What breaks if

1. **A Go map rebuilds on the matching path.** *Real, found in this milestone.* Once in a while (about 13 times per million operations at 5,000 live keys in our measurement, never at some other sizes), an insert copies the whole table into a new allocation. Matching results stay correct, so every correctness test passes. But that one command takes O(n) longer and leaves garbage for the GC, which is exactly what the p99.9 latency row in milestone 9 would expose. `AllocsPerRun` reported 0 because of its integer division. Only counting individual allocations by stack revealed it. *Fixed by the index (decision 002 revision 2).*

2. **A level that empties gets no `BookDelta`.** A market-data subscriber keeps showing that price level forever, with its last nonzero size. Its book is now crossed: it shows an ask at 100 while the real book's best ask is 101, and a new bid at 100 appears to sit through it. Any client trading from that view is wrong. *Planted and caught*: seed 1, command #6.

3. **A rejected order consumes an order id.** The accepted orders' ids gain holes (1, 3, 5, ...). Nothing breaks immediately, but a client or auditor can no longer distinguish "an id skipped by a reject" from "an event I missed", and recovery loses a cheap consistency check (the number of acks should equal the last order id). *Planted and caught*: seed 1, command #24.

4. **A command is applied with a sequence gap instead of panicking.** Suppose the sequencer skips `Seq` 1,000 because of a bug, and the engine accepts 1,001. Everything works until a crash. Replay then finds no record 1,000 in the log, so replay-check (milestone 5) can't tell whether a record is missing, and acknowledged state may have been built on a command that never reached the disk. A panic instead stops the process at the first inconsistency, when it is still diagnosable. `TestStepPanicsOnSeqGapOrRepeat` covers gaps, repeats and zero.

5. **The publisher blocks on a slow client.** `Run` sends each event on `out`, and that send blocks if nobody receives. If the publisher waited on a slow client's socket before receiving the next event, one client with a bad connection would stop matching for **every** instrument and every client. That is why decision 005 assigns the slow-consumer policy (bounded buffer, disconnect) to the publisher. *Since milestone 3:* this was planted as a bug in the real gateway and observed. With a blocking delivery, a healthy client got no responses for 5 s, and even shutdown hung ([03-gateway.md](03-gateway.md#4-what-breaks-if), item 1). The other half of acceptance 5, that the engine's latency histogram doesn't move, is checked once the histogram exists (milestone 7).

---

## 5. Go idioms introduced

**Goroutines: `go f()`.** Starts `f` running concurrently and returns at once. There is no handle or join: you coordinate through channels, or through `sync` primitives. In `TestRunMatchesStep`, `go engine.New().Run(in, out)` starts the engine goroutine, and a second goroutine feeds it commands.

**Channels and directional channel types.** `make(chan engine.Command, 64)` creates a buffered channel. `Run(in <-chan Command, out chan<- Event, snap *SnapshotHooks)` uses **directional** types: inside `Run`, `in` can only be received from and `out` only sent to. The compiler enforces the data flow, the way a read-only type would in TypeScript.

**`close` and `range` over a channel.** `close(out)` signals "no more values". `for e := range out` receives until the channel is closed **and** empty, then exits. That is how the end of the command stream propagates through the pipeline with no extra "done" message. Only the sender closes a channel. Sending on a closed channel panics.

**`defer`.** `defer close(out)` runs when the function returns, by any path, including a panic. It is Go's `finally`, attached to the function rather than to a block. Deferred calls run in last-in, first-out order.

**`recover` in tests.** `defer func() { if recover() == nil { t.Fatal(...) } }()` turns "this call must panic" into a test. `recover` only works inside a deferred function, and stops the panic from unwinding further.

**Generics: `index[K comparable]`.** A type parameter list in square brackets. `comparable` is the built-in constraint for "supports `==`", which is what a hash key needs. `newIndex[OrderID](1024)` gives the type argument explicitly. One implementation then serves both the `OrderID` key and the struct key, with full type checking and no `interface{}` boxing. See [Type parameters](https://go.dev/ref/spec#Type_parameter_declarations) in the spec.

**`hash/maphash`.** The standard library's seeded hash: `maphash.MakeSeed()` is random, and `maphash.Comparable(seed, key)` hashes any comparable value. It is the same family of hash Go's own maps use. See [package maphash](https://pkg.go.dev/hash/maphash).

**Variadic `append`.** `append(out, ev1, ev2)` appends several values at once. `append(log, evs...)` appends a whole slice; the `...` spreads it, like JavaScript's spread syntax.

**Type assertion with comma-ok.** In the differential harness, `rb, ok := real.(*book.Book)` asks whether the interface value holds a `*book.Book`. `ok` is false instead of panicking if not. It is the same comma-ok shape as a map lookup.

**`testing.B.Loop`.** `for b.Loop() { ... }` (Go 1.24+) is the modern benchmark loop. It runs the body as many times as needed, and keeps the compiler from optimising the body away. `b.ReportAllocs()` adds the B/op and allocs/op columns.

**`runtime.MemProfileRate` and `runtime.MemProfile`.** The memory profiler normally samples about one allocation per 512 KB. Setting the rate to 1 records every allocation with its call stack. `runtime.CallersFrames` turns the recorded program counters into function names and line numbers. This is the machinery under `go test -memprofile`, used directly ([package runtime](https://pkg.go.dev/runtime#MemProfile)).

**A package-level "sink" variable.** `allocSink = make([]byte, 64+i)` assigns to a package-level variable so the compiler can't prove the allocation is unused and remove it. A deliberately allocating test without a sink might allocate nothing.

**A shared test-helper package.** `internal/testutil` is an ordinary package that only test files import. Test helpers can't be imported from another package's `_test.go` files, so shared helpers live in a normal package. `internal/` keeps it private to this module.

---

## 6. Self-check questions

1. A sell market order for 10 arrives. The bid side has 100×3 (order A), 100×4 (order B), and 99×6 (order C). Write out the complete event sequence the engine emits, with each event's kind, side, price, qty and `Leaves`, and the match ids, assuming the last match id used was 41.
2. Why does the engine panic on a sequence gap instead of rejecting the command, or logging and carrying on?
3. The index uses a random hash seed, but PRD 5.4 forbids randomness in the engine. Why doesn't this seed break deterministic replay? Name one change to the code that *would* make it break replay.
4. `testing.AllocsPerRun` reported 0 while the Go map was allocating. Explain exactly why, and why a `MemStats.Mallocs` count was also unsuitable.
5. `BookDelta` carries the level's new total rather than the change ("−4"). Give one advantage of that for a consumer, and say what it still doesn't protect against.

<details>
<summary>Answers</summary>

1. The taker sells into the best (highest) bids, oldest first.
   - `Ack` (sell, qty 10, a new order id).
   - `Fill` taker: sell, price 100, qty 3, leaves 7, match 42. Then `Fill` maker A: buy, price 100, qty 3, leaves 0, match 42.
   - `Fill` taker: price 100, qty 4, leaves 3, match 43. Then `Fill` maker B: buy, price 100, qty 4, leaves 0, match 43.
   - `BookDelta` buy 100, qty 0 (the level is gone).
   - `Fill` taker: price 99, qty 3, leaves 0, match 44. Then `Fill` maker C: buy, price 99, qty 3, leaves 3, match 44.
   - `BookDelta` buy 99, qty 3.

   There is no `Cancelled`, because the order filled completely and nothing expired. If it had been for 15, it would end with `Cancelled(expired)` qty 2 after the 99 delta: 3 + 4 + 6 = 13 filled.

2. A gap means the command stream (the log) and the engine no longer agree. Rejecting would mean the engine and the log record different histories. Logging and continuing would produce a state that replay can't reproduce, because replay would apply exactly the logged commands. Either way, the core guarantee, that state is a pure function of the log, is gone, silently. Stopping keeps the log as the single source of truth: fix the bug, restart, replay.

3. The seed only decides where each key sits in the slot array. Every operation on the index (get, put, delete, len) returns the same answer for any seed, and nothing ever iterates the slots, so the order of entries in memory can never reach the output. The book's state and every event are identical whatever the seed. **What would break it:** iterating the index (for example, to build a snapshot or list orders) and emitting in slot order. Different seeds would then give different output orders. Any future iteration must sort, or walk the price levels instead.

4. `AllocsPerRun` returns `mallocs / runs` with integer division. The map rebuild allocated a handful of objects over tens of thousands of runs, which is less than one per run, so it truncated to 0. `MemStats.Mallocs` counts allocations from the whole process, and the Go runtime's own background goroutines (scavenger, GC workers, thread creation) allocate a few objects at random moments. A must-be-zero count failed at random, unrelated to our code. Only attributing each allocation to a call stack separates "our code allocated" from "the runtime allocated".

5. **Advantage:** a delta is idempotent and self-contained. Applying it twice, or after missing the previous delta for that same level, still leaves that level correct. A "−4" applied twice, or after a missed "+2", is permanently wrong. **What it doesn't protect against:** missing the only delta for a level. If the delta saying a level went to 0 is lost, the consumer shows a stale level forever, and nothing in the deltas themselves reveals the loss. That needs **gap detection** with a per-instrument market-data sequence number (`md_seq`) and a resend or re-snapshot (milestone 6).

</details>
