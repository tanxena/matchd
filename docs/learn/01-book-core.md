# 01 — The book core: price-time matching, and proving it correct

Milestone 1 builds the heart of the engine: a limit order book for one instrument that accepts limit and market orders, matches them by price-time priority, and supports cancels. It also builds the machinery that tells us the book is right: a deliberately naive reference book and a differential test that compares the two on over a million random commands.

Nothing in this milestone touches the network, disk, or concurrency. That is deliberate. Every later milestone feeds commands into this code, so it has to be correct first (PRD section 9).

---

## 1. Concepts

### 1.1 What a limit order book is

An exchange keeps, for each instrument, a list of everyone's standing offers to trade:

- **Bids** are offers to buy: "I will buy up to 10 at 99".
- **Asks** (or offers) are offers to sell: "I will sell up to 7 at 101".

Orders are grouped by price into **price levels**:

```
        ASKS (sellers)                 lowest ask = best ask
   price 102 │ 4
   price 101 │ 7  3                    two orders at 101: 7 arrived first, then 3
   price 100 │ 10                      ← best ask
   ──────────┼──────────  spread = 100 − 99 = 1
   price  99 │ 6  4                    ← best bid
   price  98 │ 3
        BIDS (buyers)                  highest bid = best bid
```

The best bid is always strictly below the best ask. If a buyer were willing to pay 100 and a seller to accept 100, they would already have traded. A matching engine's job is to keep that true: whenever a new order overlaps the other side, it trades immediately.

Everything here is an integer. Prices are **counts of ticks** and quantities are **counts of lots** (decision [003](../decisions/003-integer-domain-and-reject-semantics.md)). If you come from Python or TypeScript, think "store money as integer cents, never as `0.1 + 0.2`". The `float64` value `0.1` isn't exactly 0.1, and two orders "at the same price" could otherwise compare unequal.

### 1.2 Price-time priority

When a new order arrives, which resting orders does it trade with, and in what order? Most equity exchanges use **price-time priority**:

1. **Price first.** A buyer trades with the *cheapest* asks first; a seller trades with the *highest* bids first. The incoming order keeps going to worse prices only while they are still within its limit.
2. **Time second.** Among orders at the *same* price, the one that arrived first trades first: a first-in-first-out (FIFO) queue per level.
3. **The price is the resting order's price.** If you bid 105 and the best ask is 100, you trade at 100. The resting order (the **maker**) set the price; the incoming order (the **taker**, or aggressor) accepted it. You get *price improvement*.
4. **A partial fill doesn't lose its place.** If a resting order for 10 is hit for 4, its remaining 6 stays at the front of the queue. Moving it to the back would punish it for being traded with.

NASDAQ's market-data spec ([TotalView-ITCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHspecification.pdf)) describes the book in exactly these terms. Its *Add Order*, *Order Executed* (with the remaining shares implicit), *Order Cancel* and *Order Delete* messages map onto our rest, fill, partial cancel and full removal. The order-entry side ([OUCH 4.2](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH4.2.pdf)) is where the vocabulary of *accepted*, *executed*, *canceled* and *rejected* comes from.

**Worked example.** The ask side holds 100×10 (order A1, oldest), 100×5 (A2), and 101×7 (A3). A **limit buy for 20 at 101** arrives:

| step | best ask level | maker | trade | maker left | taker left |
|------|----------------|-------|-------|------------|------------|
| 1 | 100 | A1 (oldest at 100) | 10 @ 100 | 0, removed | 10 |
| 2 | 100 | A2 | 5 @ 100 | 0, removed; level 100 is now empty and goes | 5 |
| 3 | 101 | A3 | 5 @ 101 | 2, stays at the front of level 101 | 0 |

The result is `Filled=20, Rested=0`, with three fills. With a limit of **100** instead, the order stops after step 2: 101 is above the limit. Its remaining 5 rests as a new bid at 100. It doesn't cross anything, because level 100 on the ask side is gone. The scenario test "buy sweeps asks from lowest price up, each at the maker's price" checks a variant of this case.

### 1.3 Order types in this milestone

- **Limit:** trade at the limit price or better; whatever is left rests on the book. TIF `GTC` rests until cancelled. `Day` is accepted and behaves like GTC in v1, because there is no trading session to end.
- **Market:** trade at any price until filled or until the other side runs out. Any remainder is **cancelled, never rested**: a market order has no price, so there is nowhere to put it. A market order into an empty book is *accepted* with everything expired, not *rejected* (decision 003).
- **Cancel:** remove your own resting order by its engine-assigned `order_id`. **Cancel is scoped to the owning client**, which extends PRD 5.2. Cancelling someone else's order returns `UnknownOrder`, exactly as if the order didn't exist, so you can't even find out that it does.

### 1.4 Order identity

There are two ids:

- **`order_id`** is assigned by the engine (the caller of the book) and is unique forever. Cancels use it.
- **`client_order_id`** is the client's own label. PRD 5.1: it must be unique **among that client's live orders**. While your order with client order id 77 is resting, another new order with 77 is rejected (`DuplicateClientOrderID`). Once the first one has been filled or cancelled, 77 is free again. Two different clients may both use 77.

### 1.5 Why a second, "dumb" implementation: differential testing

How do you know a matching engine is correct? Hand-written examples (unit tests) only check the situations you thought of. The bugs that matter in an order book live in combinations: a partial fill, then a cancel of the next order at that level, then a sweep that empties the level just as a new order arrives at the same price.

**Differential testing** (McKeeman, "Differential Testing for Software", *Digital Technical Journal* 10(1), 1998) attacks this from a different angle:

1. Write a second implementation that is **obviously correct** and makes no attempt to be fast. Ours is `internal/book/ref`: two sorted slices of orders and linear scans. You can check it against section 1.2 by reading it.
2. Generate long random command streams.
3. Feed each command to both books and **fail on the first observable difference**: results, fills, the full resting state order by order, the best price and quantity, and the order count.

Every clever optimisation in the real book (free lists, index links, the sign trick, binary search) is then checked against the naive version on millions of situations nobody wrote by hand.

Three things make this trustworthy rather than theatrical:

- **Seeds.** The generator is a seeded pseudo-random generator ([`math/rand/v2` PCG](https://pkg.go.dev/math/rand/v2#PCG); the algorithm is described at [pcg-random.org](https://www.pcg-random.org/paper.html)). The same seed always gives the same stream. A failure prints its seed and command number, plus a command that replays exactly that prefix.
- **Coverage floors.** "Zero divergences" means nothing if the generator never produced a cancel of a partially filled order. The harness counts each kind of event and **fails if any falls below 0.1% of commands**.
- **Mutation checks.** We plant a one-line bug in the real book and confirm the harness catches it (section 4). A test you have never seen fail is a test you can't trust.

**Invariants** are a second safety net. They are properties that must hold after *every* command, whatever the inputs: the book isn't crossed, each level's total equals the sum of its orders, no order appears twice, and filled or cancelled ids are gone (PRD 5.3). Both books check their own invariants after every command in the harness.

### 1.6 Two data-structure ideas you'll meet

- **Sorted slice with the hot end at the back.** Removing the last element of a slice costs nothing; removing the first means shifting everything. We put the **best** price level at the *end* of each side's slice, so the most frequent operation, consuming the best level, is free ([decision 001](../decisions/001-price-level-storage.md)).
- **Slab plus free list plus index links.** Instead of allocating a separate heap object per order and linking them with pointers, every order lives by value in one big slice (`[]orderNode`). The queue at a level is linked by **indices** into that slice. Freed slots go on a free list and are reused before the slice grows. In steady state there are no allocations and no pointers for the garbage collector to trace ([decision 002](../decisions/002-order-storage-and-cancel.md); background in the [Go GC guide](https://go.dev/doc/gc-guide)).

  *Revised in milestone 5:* the slab is now a list of fixed-size **chunks** of 4,096 orders, so snapshots can share them copy-on-write ([decision 013](../decisions/013-copy-on-write-snapshots.md), [05-recovery.md](05-recovery.md)). Everything in this section still holds: slots, index links, free list, no per-order allocation.

---

## 2. Code walkthrough

### `internal/book/types.go`: the vocabulary

- **`Price`, `Qty`, `OrderID`, `ClientID`, `ClientOrderID`** ([types.go:18–33](../../internal/book/types.go#L18)). Each is a distinct named integer type, so the compiler refuses to mix them: a `Qty` can't be passed where a `Price` is expected.
- **`Side`, `OrderType`, `TIF`** ([types.go:38–104](../../internal/book/types.go#L38)). Small enums whose **zero value is invalid** (`Buy = 1`, `Sell = 2`), so an order with a forgotten field is rejected instead of quietly becoming a buy. Each has a `String()` method for readable test output. `Side.Opposite()` gives the side an order trades against.
- **`RejectReason`** ([types.go:116](../../internal/book/types.go#L116)). `RejectNone` (0) means accepted. The book's reasons are `InvalidSide`, `InvalidType`, `InvalidQty`, `InvalidPrice`, `InvalidTIF`, `DuplicateClientOrderID` and `UnknownOrder`. Since milestone 2 the enum also holds three engine-level reasons that a `Book` never returns (`UnknownInstrument`, `InvalidInstrument`, `DuplicateSymbol`), so the engine, wire protocol and books share one list.
- **`Order`** ([types.go:171](../../internal/book/types.go#L171)). The new-order command.
- **`Result`** ([types.go:185](../../internal/book/types.go#L185)). The outcome of `Submit`: `Reject`, `Filled`, `Rested`, `Expired`. For an accepted order, Filled + Rested + Expired = Qty.
- **`CancelResult`** ([types.go:195](../../internal/book/types.go#L195)). `Reject` and `Cancelled` (the quantity removed). Since milestone 2 it also echoes the removed order's `Side`, `Price` and `ClOrdID`, so the engine can build the cancel acknowledgement and the book delta without a second lookup.
- **`Fill`** ([types.go:205](../../internal/book/types.go#L205)). One match. It records the price (always the maker's), the quantity, the taker's side, id and client, and the maker's id, client and client order id. **`MakerLeft`** is the maker's quantity after this fill, so 0 means the maker left the book. Milestone 2 uses it to emit the right book delta without asking the book again.
- **`RestingOrder`** ([types.go:219](../../internal/book/types.go#L219)). A read-only view of an order on the book. The two books are compared using it.
- **`Instrument` and `Instrument.Validate`** ([types.go:255](../../internal/book/types.go#L255)). Symbol, tick size and lot size. The book checks them once at construction and never uses them in matching.
- **`Order.Validate`** ([types.go:255](../../internal/book/types.go#L255)). Field checks in a fixed order (side, type, qty, price, tif), so an order with several problems always gets the same reason. Limit orders need price > 0 and TIF GTC or Day; market orders need price == 0. *(Since review F-002, quantity is also bounded by `MaxQty` = 2^30 and a limit price by `MaxPrice` = 2^53, so no level total can overflow; decision 003.)*

### `internal/book/book.go`: the real book

**Storage types.**

- **`nilIdx`** ([book.go:7](../../internal/book/book.go#L7)). The index 0, meaning "no node". Slot 0 of the slab is reserved so that a zeroed link reads as nil.
- **`orderNode`** ([book.go:13](../../internal/book/book.go#L13)). One resting order, stored by value. It holds id, client order id, client, side, price and remaining qty, plus `prev`/`next uint32` links within its level's queue. It contains **no pointers**.
- **`level`** ([book.go:25](../../internal/book/book.go#L25)). One price on one side. `total` is the summed qty, `count` the number of orders, and `head`/`tail` the oldest and newest slots in the queue.
- **`ladder`** ([book.go:40](../../internal/book/book.go#L40)). One side: `levels []*level`, sorted so the **best level is last**. `sign` is +1 for bids and −1 for asks.
  - `key(p)` = `p × sign`, so both sides are sorted ascending by key, and for both sides the best price has the largest key.
  - `search(p)` ([book.go:50](../../internal/book/book.go#L50)) is a lower-bound binary search. It returns where `p` is, or where it would be inserted.
  - `best()` returns the last element, or nil.
  - `insertAt(i, lv)` grows the slice by one, shifts the levels from `i` upward by one with `copy`, and stores `lv` at `i`.
  - `removeAt(i)` shifts the levels above `i` down by one and **sets the vacated last slot to nil**, so the slice doesn't keep the removed level reachable.
- **`clientOrderKey`** ([book.go:92](../../internal/book/book.go#L92)). The `(client, clOrdID)` pair, used as the key of the duplicate-check index.

**`Book`** ([book.go:98](../../internal/book/book.go#L98)) holds:

- `bids`, `asks`;
- the order slab: `chunks` (fixed-size blocks of 4,096 slots since milestone 5), `nslots` and the free-list head `freeNode`;
- `freeLevels`, a stack of reusable `*level`;
- `byID`, an `index[OrderID]` from order id to slot (for cancel);
- `byClOrd`, an `index[clientOrderKey]` from `(client, clOrdID)` to slot (for the duplicate check).

*Revised in milestone 2:* these two were Go maps in milestone 1. They are now the book's own open-addressing hash table (`internal/book/index.go`), because Go maps were measured allocating in steady state. See [02-engine.md](02-engine.md) and [decision 002, revision 2](../decisions/002-order-storage-and-cancel.md).

**`New`** ([book.go:129](../../internal/book/book.go#L129)) validates the instrument, sets the ladder signs, and creates the first chunk, with slot 0 reserved.

**`crosses(s, limit, resting)`** ([book.go:156](../../internal/book/book.go#L156)) says whether a level is tradable for a limit taker on side `s`. For a buy, that means resting ≤ limit; for a sell, resting ≥ limit. **Equality crosses**: a bid at 100 trades with an ask at 100.

**`Submit(o, fills)`** ([book.go:171](../../internal/book/book.go#L171)) is the main path:

1. `o.Validate()`. If it fails, return the reject with no fills and the book untouched.
2. Look up `(client, clOrdID)` in `byClOrd`. If the pair is live, reject with `DuplicateClientOrderID`.
3. **The match loop.** While quantity remains, take the opposite ladder's best level. Stop if there is none, or if this is a limit order and the level doesn't cross. Inner loop, while quantity remains and the level has orders:
   - take the **head** (oldest);
   - trade `min(remaining, maker.qty)`;
   - reduce the maker, the level total and the taker's remainder;
   - append a `Fill` priced at `lv.price`;
   - if the maker reached zero, `unlink` it and `release` its slot.

   A partially filled maker isn't touched further: it stays at the head with less quantity. When a level empties, it is removed from the end of the ladder (it was the best level, the last element) and its struct goes on the free list.
4. Build the `Result`. If everything filled, return. A market order's leftover goes into `Expired`. A limit order's leftover is passed to `rest` and goes into `Rested`.

`m := b.nodeW(mi)` returns a writable pointer to the maker. Since milestone 5 it is always safe to hold across the loop: chunks never move when the slab grows, and `nodeW` copies a shared chunk (during a snapshot) at most once, before handing out the pointer. In milestone 1 the slab was one slice, and this pointer was only safe because nothing in the loop grew it; see "What breaks if" #5.

**`rest(o, qty)`** ([book.go:272](../../internal/book/book.go#L272)):

- Panics if `o.ID` is already resting. That is a caller bug: the engine guarantees unique ids, and continuing would corrupt `byID`.
- Finds or creates the level, inserting it into the ladder if it is new.
- `alloc`s a slot, fills it in, and links it at the **tail**, which is what gives time priority.
- Updates `count` and `total`, then records the order in `byID` and `byClOrd`.

**`Cancel(client, id)`** ([book.go:318](../../internal/book/book.go#L318)):

- Looks up `id` in `byID`. If it is missing **or owned by a different client**, returns `UnknownOrder`.
- Otherwise copies the node (the slot is about to be cleared) and binary-searches for its level.
- Unlinks the order, subtracts its quantity from the level and releases the slot. If the level is now empty, removes it from the ladder wherever it sits.
- Returns the cancelled quantity.

Cost: O(1) for the order plus O(log L) to find the level (decision 002).

**`unlink(lv, i)`** ([book.go:342](../../internal/book/book.go#L342)) is the standard doubly linked list removal: point the neighbours at each other, fixing up `head`/`tail` when `i` was at an end, then decrement `count`. It deliberately does **not** change `total`; the caller knows how much quantity left.

**`alloc`** ([cow.go:78](../../internal/book/cow.go#L78)) and **`release`** ([book.go:359](../../internal/book/book.go#L359)) manage the slab.

- `alloc` pops a slot from the free list, or hands out the next one, adding a chunk when the last is full. It panics at 2³²−1 slots.
- `release` deletes the order from both indexes, **zeroes** the slot (so a stale index reads as an obviously empty order), and pushes it on the free list through `next`.

**`newLevel` and `freeLevel`** ([book.go:367, 378](../../internal/book/book.go#L367)) do the same for level structs.

### `internal/book/inspect.go`: read-only views

- **`Best(s)`** returns the price and total quantity of the best level.
- **`Live(id)`** is an index lookup; **`Len()`** is the index's count.
- **`LevelQty(s, px)`** *(added in milestone 2)* returns the total resting at one price, or 0. It is a binary search, and the engine uses it to report each touched level's new size.
- **`Orders(s, dst)`** appends every resting order on side `s` in priority order: levels from last to first (best first), and within a level from head to tail.
- **`CheckInvariants()`** ([inspect.go:64](../../internal/book/inspect.go#L64)) walks everything and returns the first inconsistency it finds. It checks:
  - the book isn't crossed;
  - levels are strictly sorted, none is empty, and no entry is nil;
  - each queue's `prev` links match a forward walk, and `tail` is the last node;
  - every order has qty > 0 and the side and price of the level holding it;
  - each level's `total` and `count` match its orders;
  - both indexes point at exactly the resting orders and nothing else, which proves cancelled and filled ids are gone;
  - every slab slot is either resting or on the free list, never both and never neither (no leaks).

  It allocates and is O(n); only tests call it.

### `internal/book/ref/ref.go`: the oracle

- **`Book`** ([ref.go:22](../../internal/book/ref/ref.go#L22)) is just `bids` and `asks` as `[]book.RestingOrder`, each **in priority order, best first**.
- **`validate`** is the oracle's *own* copy of the validation rules. It doesn't call `book.Order.Validate`, so a validation bug in the real book can't hide by also being in the oracle.
- **`Submit`** ([ref.go:94](../../internal/book/ref/ref.go#L94)):
  - scans both slices for a live `(client, clOrdID)`;
  - repeatedly trades with element `[0]` of the opposite slice, stopping when a buy meets an ask above its limit or a sell meets a bid below its limit;
  - deletes makers that reach zero;
  - rests the remainder by inserting it **before the first order with a strictly worse price**, which puts it after every order at an equal price. That is time priority, expressed in one loop.
- **`Cancel`** is a linear scan for the id, with the same owner rule.
- **`Best`**, **`Live`**, **`Orders`** and **`CheckInvariants`** are equally plain.

### Tests

- **`internal/book/book_internal_test.go`** is white-box and inside the package, so it can see unexported fields.
  - `TestLadderOrderingBestLast` pins down the sign trick.
  - `TestSlabAndLevelReuse` runs ten add/cancel cycles and asserts that neither the slab nor the level pool grows.
  - `TestFilledMakerReleasesSlot` checks that a full fill frees the slot, not just a cancel.
  - `TestCheckInvariantsDetectsCorruption` deliberately corrupts nine different fields and asserts that the checker notices each. An invariant checker that can't fail is as useless as a test that can't.
- **`internal/book/scenario_test.go`** contains 19 table-driven scenarios, one per rule in sections 1.2–1.4 (FIFO, a partial fill keeping its place, no trading through the limit, a market order on an empty book, cancel ownership, client order id reuse, validation order, ...). **Every scenario runs against both books** through the `orderBook` interface. The expected fills are written out by hand, so a scenario checks the reference book against the spec, independently of the real book.
- **`internal/book/ref/ref_test.go`** covers the oracle's own self-checks.
  - `TestCheckInvariantsDetectsCorruption` corrupts the reference book five ways (crossed, out of order, zero qty, wrong side, duplicate id) and requires each to be detected.
  - `TestRestJoinsBackOfEqualPrice` pins down the one loop that gives the reference book its time priority.
- **`internal/book/diff_test.go`**: the differential harness.
  - **`generator`** ([diff_test.go:120](../../internal/book/diff_test.go#L120)) produces commands from a seed. The mix is 50% limit, 8% market, 25% cancel, 10% reuse of an earlier client order id, and 7% invalid orders.
    - Limit prices come from `limitPrice` under a **regime** that changes every 2,000 commands: balanced (spread 10, half crossing), deep (spread 40, 85% passive, which builds books hundreds of orders deep), or tight (spread 3, mostly crossing).
    - Cancels target a live order (sometimes from the wrong client), any past order (usually already dead), or an id never issued.
    - `observe` keeps the generator's live set in step with the **oracle's** results.
  - **`runPair`** ([diff_test.go:357](../../internal/book/diff_test.go#L357)) runs each command through both books. It compares result, fills, resting state (both sides, order by order), best price and quantity, and length. It then runs both invariant checkers and asserts that every maker that just left, and every just-cancelled id, is gone from both books. On the first difference it calls `fail`, which prints the seed, the command number, the command, both sides of the difference, and a replay command line.
  - **`coverage`** counts fills, multi-fill orders, partial-then-rest, market expiries, successful cancels, successful reuse of a dead client order id, and every reject reason. **`requireCoverage`** fails the test if any count is below 0.1% of commands.
  - **Flags.** `-long` runs 20 seeds × 60k = 1.2M commands. `-seed N` runs one seed, and `-cmds K` limits it to K commands. `-diffstats` prints per-seed coverage.

**Measured on this machine** (`go test ./internal/book -run Differential -v`):

| run | commands | fills | max resting orders | divergences | wall time |
|-----|----------|-------|--------------------|-------------|-----------|
| normal (10 seeds × 10k) | 100,000 | 42,990 | 760 | 0 | ~2.4 s |
| `-long` (20 seeds × 60k) | 1,200,000 | 535,585 | 836 | 0 | ~28 s |

---

## 3. Design decisions

- [001: Price level storage](../decisions/001-price-level-storage.md). A sorted slice of `*level` per side with the best level last, and a free list of level structs. A red-black tree, a skip list and a price-indexed array were rejected, and the record says why. The cost: O(L) shifts when inserting or removing a level far from the best.
- [002: Order storage and cancel](../decisions/002-order-storage-and-cancel.md). An indexed slab with `uint32` prev/next links, a free list, and an id-to-slot lookup (a Go map in milestone 1; the book's own tombstone-free hash index since milestone 2, revision 2 of the record). The pointer-linked list was rejected. Cancel is O(1) for the order plus O(log L) for the level search.
- [003: Integer domain and reject semantics](../decisions/003-integer-domain-and-reject-semantics.md). Integer ticks and lots; the caller assigns `order_id`; the validation order; the duplicate rule applies to market orders too; **cancel is scoped to the owning client**; a market order on an empty book is accepted and expired; Day is treated as GTC; there is no self-trade prevention.
- [004: Toolchain](../decisions/004-toolchain.md). Go 1.27.0, zero dependencies, LF line endings through `.gitattributes`, and the race detector on Windows. It needs cgo and a C compiler; MSYS2 gcc is installed, so run `CGO_ENABLED=1 go test -race ./...`.

---

## 4. What breaks if

Each of these was either run for real against the harness (the "caught" results are actual output) or is a hazard the code is specifically shaped to avoid.

1. **Matching takes the newest order at a level instead of the oldest.** Change `mi := lv.head` to `mi := lv.tail` in `Submit`. Price priority still holds; only time priority breaks. Nothing crashes, the book stays internally consistent, and every invariant passes. But the wrong client gets filled: someone who queued first is skipped in favour of someone who arrived later. On a real exchange that is a fairness and regulatory failure, not a crash. **Caught:** seed 9, command #52. The real book filled makers 35, 34, 31 (newest first); the reference filled 31 alone. Seed 8 diverged at command #9. This is exactly why the harness compares *fills and resting order sequences*, not just level totals. Totals were identical.

2. **Fills are priced at the taker's limit instead of the maker's price.** Change `Price: lv.price` to `Price: o.Price`. A buyer bidding 105 against an ask at 100 now pays 105, so the price improvement is stolen. Market orders, whose `Price` is 0, would print trades at 0. **Caught:** seed 1, command #8, a market sell whose fills showed price 0.

3. **Crossing uses `<` instead of `<=`.** A buy at 100 no longer trades with an ask at 100. The order rests at 100 alongside the ask: a **locked book**, which no exchange may show. **Caught:** seed 5, command #15. The invariant checker would also have caught it, since best bid ≥ best ask is reported as crossed.

4. **Cancel skips the ownership check.** Any client can cancel anyone's resting orders by guessing ids. They are small, sequential integers, so that is easy. Once the gateway exists, this is a remote "wipe the book" for any connected client. **Caught:** seed 9, command #31, `Cancel{client=3 id=15}` succeeded in the real book and was rejected by the reference.

5. **Holding a pointer into the slab across a slab growth.** `m := &b.nodes[mi]` points into the slab's current backing array. If something inside the match loop called `alloc()` and the `append` had to grow the slab, Go would copy the slab to a new, larger array. `m` would still point into the *old* array, and every later write through `m` (reducing the maker's qty) would silently go to memory that is no longer the book. The book would then show a maker with its old quantity and fill it twice. **Avoided by construction:** `Submit` finishes the whole match loop, which never allocates a slot, before calling `rest`, which may. The comment on that line states the rule. If milestone 8's FOK pre-check or any future change adds an allocation inside the loop, the differential test is what would catch the double fill.

   *Since milestone 5 this hazard is gone by construction:* the slab is a list of chunks that never move, so growing it can't invalidate a pointer. Writes go through `nodeW` for a different reason, copy-on-write during snapshots ([05-recovery.md](05-recovery.md#4-what-breaks-if), item 1).

---

## 5. Go idioms introduced

**Named (defined) types.** `type Price int64` creates a new type whose underlying representation is `int64`, but which the compiler treats as distinct. You can't pass a `Qty` where a `Price` is expected without an explicit conversion (`book.Price(x)`). This is Go's cheap answer to TypeScript's branded types, with zero runtime cost. See [Type definitions](https://go.dev/ref/spec#Type_definitions) in the spec.

**Typed constants and the zero value.** Every Go variable starts at its type's *zero value*: 0, `""`, nil, or a struct of zero values. There is no `undefined`. We number `Buy = 1, Sell = 2` so that the zero `Side` is *invalid* and gets rejected. A struct literal that forgets `Side:` can't accidentally become a buy. See [The zero value](https://go.dev/ref/spec#The_zero_value).

**`String()` methods (`fmt.Stringer`).** Any type with a `String() string` method is printed with it by `fmt` (`%v`, `%s`). That is why test failures read `buy limit tif=gtc` instead of `1 1 1`. Go interfaces are satisfied implicitly: there is no `implements` keyword.

**Value receivers vs pointer receivers.** `func (s Side) Opposite()` gets a *copy* of `s`; `func (b *Book) Submit(...)` gets a pointer and can modify the book. The rule of thumb: use a pointer receiver if the method mutates the value or the value is large. `Instrument.Validate` uses a value receiver; `Order.Validate` takes `*Order` to avoid copying a 48-byte struct on every order.

**The comma-ok idiom.** Looking up a missing key in a Go map, `v, ok := m[k]`, returns the zero value plus `ok == false`, instead of throwing or returning `undefined`. The book's own index follows the same convention (`idx, ok := b.byID.get(id)`), so it reads like a map. The same shape appears in type assertions and channel receives later on. `if _, dup := b.byClOrd.get(k); dup { ... }` combines it with an `if` initialiser, so the variable's scope is just that `if`.

**Structs as keys.** A struct whose fields are all comparable is itself comparable with `==`, so it can be a map key, or here the `K` of the generic `index[K comparable]`. Both `==` and `maphash.Comparable` work on the whole struct, so no string concatenation like `` `${client}:${clOrdID}` `` is needed. See [Comparison operators](https://go.dev/ref/spec#Comparison_operators) and [Map types](https://go.dev/ref/spec#Map_types).

**Append to a caller-supplied buffer.** `Submit(o, fills []Fill) (Result, []Fill)` appends to `fills` and returns the possibly-reallocated slice. The caller passes `buf[:0]`, which keeps the backing array but sets the length to zero, and stores the result back. After a few calls the buffer is big enough and matching never allocates again. The same pattern is used by `strconv.AppendInt` and `Orders(s, dst)`. It works because a slice is a small `(pointer, len, cap)` header, not the data itself ([Go slices: usage and internals](https://go.dev/blog/slices-intro)).

**Pointers into slices, and `append` moving the data.** `&b.nodes[i]` is a real pointer into the slice's backing array. `append` beyond capacity allocates a *new* array and copies, and old pointers keep pointing at the old one. Python lists and JS arrays hide this; Go doesn't. See "What breaks if" #5. (The book itself avoids this since milestone 5 by storing orders in fixed chunks.)

**`copy` for insert and remove, and clearing the vacated slot.** `insertAt` does `append(s, nil)`, then `copy(s[i+1:], s[i:])`, then `s[i] = x`. `removeAt` does the reverse, then sets the now-unused last element to nil. Without that, the backing array would keep a pointer to the removed level and the GC couldn't reclaim what it references. The `slices` package (`slices.Insert`, `slices.Delete`, which the reference book uses) does the same thing; the real book writes it out to keep control of the hot path.

**`min` / `max` builtins and range over an integer.** `min(remaining, m.qty)` is a builtin since Go 1.21. `for i := range n` loops `i = 0..n-1` since Go 1.22.

**panic vs error.** Functions return `error` for things a caller can reasonably cause and handle, such as a bad instrument in `New`. `panic` is for "this program is broken": a duplicate resting order id, or a resting order whose level is missing. Continuing after either would corrupt state silently. Matching never panics on client input; client mistakes become `RejectReason`s.

**`internal/` packages.** Code under `internal/` can only be imported by code rooted at the parent of `internal`, here this module. It is the compiler-enforced version of "private to this project". See the [go command docs](https://pkg.go.dev/cmd/go#hdr-Internal_packages).

**Internal vs external test packages.** `book_internal_test.go` declares `package book` and can see unexported fields such as `b.chunks`. `scenario_test.go` and `diff_test.go` declare `package book_test`: same directory, but compiled as a separate package that sees only exported names, like a real user would. That external package can import `ref`, which itself imports `book`, without an import cycle.

**Table-driven tests and subtests.** A slice of cases plus a loop that calls `t.Run(name, func(t *testing.T) {...})` for each is idiomatic Go testing. Each case is a named subtest you can run alone with `-run 'TestScenarios/book/FIFO'`. `t.Helper()` makes failure line numbers point at the caller. `t.Fatalf` stops the current subtest; `t.Errorf` records a failure and continues. `t.Parallel()` lets the seed subtests run concurrently. See [package testing](https://pkg.go.dev/testing).

**Custom test flags.** A test file can declare package-level `flag.Bool(...)` variables, and `go test ./pkg -long` passes the flag through. One catch, found the hard way in this milestone: flags that `go test` itself understands are consumed by `go test`. We originally named the commands-per-seed flag `-n`, but `go test -n` means "print the build commands and don't run anything", so the replay command silently did nothing. It is now `-cmds`. See [Testing flags](https://pkg.go.dev/cmd/go#hdr-Testing_flags).

**Seeded randomness with `math/rand/v2`.** `rand.New(rand.NewPCG(seed1, seed2))` creates an independent generator with its own state. Unlike the package-level functions, it is reproducible. Each seed subtest owns its own generator, so parallel subtests don't interfere.

**Comparing values: `==` and `slices.Equal`.** Structs whose fields are all comparable can be compared with `==` (`res != st.want`). Slices can't, because `==` on slices is a compile error. `slices.Equal` compares them element by element with `==`.

**A mutex guarding test state.** The parallel seed subtests all add their coverage into one shared `totals`. The `statsMu sync.Mutex` declared next to it guards it: lock, add, unlock. Without the lock, two subtests could interleave their read-modify-write and lose counts. This is the only shared mutable state in milestone 1.

---

## 6. Self-check questions

1. An incoming **sell limit at 99 for 10** meets this bid side: 100×4 (order X), 99×3 (Y, oldest at 99), 99×5 (Z), 98×8 (W). List the fills in order, with price and quantity, and say what is left on the book afterwards.
2. Why does `Submit` match against `levels[len-1]` rather than `levels[0]`, and what would it cost to keep the best level at index 0 instead?
3. `Cancel` returns `UnknownOrder` both for an id that never existed and for an id owned by another client. Why not return a more helpful `NotYourOrder`?
4. The differential harness compares `Orders(side)` order by order after every command, not just `Best()` and level totals. Name a bug that the totals-only comparison would miss.
5. Why is `CheckInvariants` itself tested by deliberately corrupting a book, and why do the differential tests have coverage floors? What do these two have in common?

<details>
<summary>Answers</summary>

1. The sell trades with the best (highest) bids first, while the bid ≥ 99.
   - X: 4 @ 100. X is removed and level 100 empties. 6 left to sell.
   - Y: 3 @ 99. Y is the oldest at 99 and is removed. 3 left.
   - Z: 3 @ 99. Z goes from 5 to 2 and keeps its place at the front of 99. 0 left.

   Result: `Filled=10`, three fills, nothing rests. The book afterwards holds bids 99×2 (Z) and 98×8 (W). The first fill is at **100**, not 99: makers set the price.

2. Removing the best level is the most frequent structural change, since every sweep consumes it. Removing the *last* element of a slice just shrinks its length: O(1), no copying. With the best at index 0, every consumed best level would shift the entire remaining slice down by one, O(L) on the hottest path. New levels near the best would also be inserted at the front, which shifts everything. Putting the best at the end makes the common case cheap and moves the O(L) cost to deep inserts and removals, which are rare ([decision 001](../decisions/001-price-level-storage.md)).

3. A distinct answer would leak information. Order ids are small sequential integers, so a client could probe ids and learn which other participants' orders are live. With one answer for both cases, a client can learn nothing about orders it doesn't own ([decision 003](../decisions/003-integer-domain-and-reject-semantics.md)).

4. Any bug that puts orders in the wrong place in a queue without changing quantities. Take `rest` linking a new order at the level's **head** instead of its tail. At the moment of the bug no fill happens, and best price, best quantity and every level total are identical in both books, so a totals-only harness sees nothing. The wrong order is only revealed when that level is next traded, which may be thousands of commands later, and the divergence report would then point at an innocent command. Comparing `Orders(side)` order by order flags the exact command that rested the order in the wrong place. A partial fill that re-queues the maker at the tail behaves the same way.

5. Both guard against **a check that can't fail**. Invariants that never fire might be checking nothing (an off-by-one walk, a condition that is always true). Corrupting nine specific fields and requiring each to be detected proves the checker looks at them. Likewise, "zero divergences" is only meaningful if the generator actually produced the situations where bugs live. The coverage floors fail the run if, for example, no market order ever partially filled or no dead client order id was ever reused. The mutation checks in section 4 are the third instance of the same idea, applied to the whole harness.

</details>
