# 002 — Order storage and cancel lookup: indexed slab, uint32 links, tombstone-free hash index

- **Status:** accepted. Revision 1 (milestone 1) used Go maps for the two lookups. **Revision 2 (milestone 2)** replaced them with the book's own hash index, after measurement showed Go maps allocating in steady state. Both revisions were approved by the user.
- **Code:** `internal/book/book.go` (`orderNode`, `Book.nodes`, `Book.alloc`, `Book.release`, `Book.unlink`, `Book.Cancel`) and `internal/book/index.go` (`index`)
- **Evidence:** `internal/book/mapchurn_test.go`; run `go test ./internal/book -run MapChurn -mapchurn -v`

## Context

Within a price level, orders form a FIFO queue. The queue needs three operations:

- remove the head when it is fully filled;
- append at the tail when an order rests;
- remove from the middle when an order is cancelled, which needs O(1) access to an arbitrary order by `order_id`.

There is also a duplicate check: is `(client_id, client_order_id)` live? (PRD 5.1)

PRD 6 requires zero allocations on the steady-state add, cancel and match paths. Milestone 5 will need to copy the book cheaply for snapshots.

## Options considered: order storage

1. **`map[OrderID]*node` and a pointer-linked doubly linked list per level.** This is the textbook design: O(1) unlink through the node's prev/next pointers.
   - Each new resting order allocates a node unless a separate pool is added.
   - Every node holds pointers, so the garbage collector must trace all resting orders on every cycle; GC cost grows with book depth.
   - Copying the book for a snapshot means rebuilding a pointer graph.

   Rejected.
2. **Indexed slab with `uint32` prev/next indices and a free list** (chosen).
   - All orders live by value in a single `[]orderNode`.
   - Queues are linked by index, not pointer. Index `0` is reserved as nil, so the zero value of a link means "none".
   - Freed slots are chained through their own `next` field into a free list and reused before the slab grows.
3. **A slab indexed directly by `order_id`**, with no lookup table. `order_id` grows forever while slots are recycled, so this would need `order_id mod capacity` plus generation checks. That is more machinery than one hash lookup. Rejected.

## Options considered: the two lookups (order id to slot, `(client, clOrdID)` to slot)

1. **Go's built-in `map`** (revision 1). It is simple and fast. **Rejected in revision 2** on measured evidence; see below.
2. **The book's own open-addressing hash table** (revision 2, chosen). It uses linear probing and **backward-shift deletion**, so it never leaves tombstones, and it **grows only when the live count reaches a new peak**. About 150 lines, tested against a Go map.
3. **Keep Go maps, but pre-size them.** This makes rebuilds rarer but doesn't eliminate them: rebuilds depend on how many deleted markers have built up, not on how full the table is. Rejected.
4. **Keep Go maps and weaken acceptance criterion 3** to "zero allocations except rare map rebuilds". Rejected: it hides an O(n) pause in the matching path.

### Why Go maps were rejected (the evidence)

An order book churns keys forever: every new order id is a key never seen before, and old ones are deleted when orders fill or cancel. The number of live keys stays roughly constant.

Go 1.24+ maps (Swiss tables) mark a deleted slot with a **tombstone** so that later lookups keep probing past it. Tombstones use up the table's free space. When there is none left, an insert triggers a **rehash**, and in the cases measured here the rehash allocates a brand-new table (`runtime.mapassign_fast64 → table.rehash → table.grow → newTable`), even though the live count hasn't changed. That is both a heap allocation and an O(n) copy, on the matching goroutine.

**What we saw:**

1. **The engine's allocation test failed in about 1 run in 3**, with a book of about 400 resting orders. The allocation stack ran through `book.(*Book).rest → mapassign_fast64 → table.rehash → table.grow → newTable`. `testing.AllocsPerRun` reported **0**, because it divides by the number of runs using integer division, so the strict check described in decision 007 was what caught it.
2. **The committed measurement** (`mapchurn_test.go`): 20 independent tables per row, each run through 1,000,000 delete-random-key-then-insert-new-key operations after warm-up, with allocations counted by `testutil.OwnAllocations`:

   | live keys | Go map: allocations | Go map: per million ops | index: allocations | index: table grew |
   |---:|---:|---:|---:|---:|
   | 50 | 0 | 0.00 | 0 | 0 of 20 |
   | 400 | 0 | 0.00 | 0 | 0 of 20 |
   | 5000 | 270 | 13.50 | 0 | 0 of 20 |
   | 50000 | 0 | 0.00 | 0 | 0 of 20 |

   *Go 1.27.0, windows/amd64, Intel i7-8650U. Each Go map gets a random hash seed, so exact counts vary between runs: 261 and 270 have both been seen in the 5,000 row.*
3. **A finer sweep** (8 maps × 300,000 operations per size) found rebuilds at 5,000, 10,000 and 20,000 live keys, and none at 100–4,500, 6,000–8,000 or 50,000. A separate "newest-key" churn pattern (insert one key, delete it again, which is closer to the engine's add/cancel cycle) found no rebuilds at any size tested, including 400. Yet the engine test hit one at about 400.

**Conclusion:** whether a Go map rebuilds under churn depends on its size, its full insert/delete history and its random seed. It can't be predicted from the live count alone, so it can't be ruled out. The index rules it out by construction: its only allocation is `grow`, which runs only when the live count exceeds half the table. `TestIndexChurnNeverGrows` and the measurement's "table grew" column check exactly that.

## Decision (revision 2)

- **Order storage:** option 2 (indexed slab). Unchanged from revision 1. *Since milestone 5* the slab is split into fixed-size chunks of 4,096 slots, so snapshots can share it copy-on-write ([decision 013](013-copy-on-write-snapshots.md)); slot indices, links and the free list work exactly as described here.
  - `orderNode` holds id, client order id, client, side, price, remaining qty, and `prev`/`next uint32`.
  - `release` zeroes a slot before pushing it on the free list, so a stale index can never read a plausible-looking order.
- **Lookups:** two instances of the generic `index[K comparable]`:
  - `Book.byID index[OrderID]` finds the slot for a cancel.
  - `Book.byClOrd index[clientOrderKey]` enforces the duplicate-client-order-id rule.
  - A slot holding value 0 counts as empty. That works because slab slot 0 is reserved and never stored.
  - The table keeps its load at or below 1/2. It starts with room for 1,024 entries (2,048 slots).
- **Hashing:** `hash/maphash.Comparable` with a **random per-index seed** (`maphash.MakeSeed()`), as the user chose over a fixed hash. The reasons:
  - Client order ids are chosen by clients. With a fixed, public hash function, a hostile client could pick ids that all land in one probe run and make every lookup on that book linear (**hash flooding**).
  - The seed decides only *where* an entry sits in memory. No code ever iterates an index, so iteration order can't leak into output. Lookups return the same answers for any seed. The book's state and events are therefore identical whatever the seed, and WAL replay (milestone 5) stays exact.
  - This is the engine's **one use of randomness**. PRD 5.4 forbids randomness because it would break deterministic replay, and this use can't, for the reasons just given. The differential and determinism tests run with a fresh seed every time and still produce identical results.
  - Measured cost: about 8 ns per hash for an `OrderID` key and about 15 ns for the struct key, with zero allocations.

**Cancel cost:**
- O(1) expected for the index lookup and the unlink;
- O(log L) for a binary search to find the order's level, because a node doesn't store a pointer to its level;
- if the level empties, the O(L − i) slice removal from decision 001.

So cancel is **O(1) for the order plus O(log L) for the level** in the common case. That answers PRD 5.2's "O(1) or O(log n); document which".

## Costs

- **More code that is ours to get right.** About 150 lines for the index. The deletion step is the subtle part: an entry may only move back into a gap if its home slot is not inside that gap, accounting for runs that wrap around the end of the table. Three tests cover it:
  - `TestIndexAgainstMap`: 20 seeds × 20,000 random operations on a tiny table that starts at 16 slots, compared with a Go map, with full structural checks after every operation;
  - `TestIndexChurnNeverGrows`;
  - `TestCheckIndexDetectsCorruption`.

  Four planted bugs in the deletion logic were each caught within 391 operations (see `docs/learn/02-engine.md`).
- **Memory: at most 50% load**, so the table has 2–4 slots per live entry. Each slot is small (16 bytes for an order-id key, 24 for the client-order-id key), so this is cheap at book scale.
- **Growth still allocates**, by doubling the table, when the book reaches a new peak order count. That is unavoidable without a fixed maximum size, and it is not steady state. Snapshots or a configured size hint can pre-grow it later if the benchmark shows it matters.
- **The slab grows the same way.** *(Milestone 1–4 wording; since milestone 5 the slab grows by adding a chunk, and existing chunks never move, so the pointer hazard below no longer exists. Decision 013.)* `append` doubles the backing array when the book reaches a new peak. The pointer `m := &b.nodes[i]` is only valid until the next growth, so `Submit` finishes matching, which never grows the slab, before calling `rest`, which may. That rule is commented in the code and is a "what breaks if" scenario in `docs/learn/01-book-core.md`.
- **2³²−1 resting orders at most per book.** `alloc` panics beyond that.
- **No level pointer in the node.** This keeps `orderNode` pointer-free, so the GC never scans the slab. The price is the O(log L) level search on cancel.
- **Two hash lookups per accepted order:** the duplicate check on arrival, and an insert when it rests. This is the price of the PRD 5.1 rule.
