# 08 — IOC and FOK: orders that refuse to wait

Until now every limit order did the same thing with its unfilled part: it **rested**, waiting on the book for someone to trade with it. Milestone 8 adds two orders that refuse to wait:
- **IOC** (immediate or cancel) takes what it can now and cancels the rest;
- **FOK** (fill or kill) takes *everything* now, or nothing at all.

FOK is the interesting one: it must decide "can I fill completely?" **before touching the book**, because once matching starts, makers have been changed and fills reported.

---

## 1. Concepts

### 1.1 Time in force

An order's **time in force (TIF)** says how long it may wait. On this engine:

| TIF | unfilled part | on the wire |
|---|---|---|
| GTC (good till cancelled) | rests until filled or cancelled | 1 |
| Day | rests; treated as GTC in v1, since there are no trading sessions (decision 003) | 2 |
| **IOC** (immediate or cancel) | cancelled at once: never rests | **3** |
| **FOK** (fill or kill) | there is none: the order fills in full, or is rejected untouched | **4** |

IOC and FOK are **limit** orders here. They trade only at their price or better. A market order already behaves like an IOC with no price limit, so a market order marked IOC or FOK is rejected (`InvalidTIF`); "market FOK" is out of scope for v1. NASDAQ's [OUCH 4.2](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH4.2.pdf) is a good reference for the vocabulary: its "time in force" field has an immediate-or-cancel value, reported as accepted, executed, then cancelled.

### 1.2 Why traders want them

- **IOC: take liquidity without leaving a trace.** A trader who wants up to 500 at ≤ 101 *now*, but doesn't want a stale order sitting on the book afterwards (to be filled later, when the market has moved against them), sends an IOC. Whatever it can't get immediately is gone.
- **FOK: atomicity.** A trader hedging, or doing one leg of an arbitrage, needs exactly 500. Getting 300 is worse than getting nothing: they'd hold an unhedged position. FOK guarantees all or nothing.

### 1.3 Checking before touching

Matching **changes the book as it goes**: each match reduces a maker's quantity, may remove a maker and a whole level, and appends a fill. If a FOK started matching and only discovered at the third level that it would fall short, undoing that would mean restoring makers' quantities *and their queue positions*, and recalling the fills. And under copy-on-write snapshots (milestone 5), it would mean undoing chunk copies too.

So FOK is decided by a **read-only pre-check**:
1. Walk the opposite side from the best level, adding each level's total quantity (which the level already keeps).
2. Stop at the first level beyond the order's limit, meaning not enough: reject.
3. Or stop as soon as the sum reaches the order's quantity, meaning enough: then match normally. It's guaranteed to fill in full.

**The cost is the number of levels the order needs**, not the depth of the book. A FOK for 5 lots against a 10,000-level book that has 5 at the best price looks at one level.

**Could it be faster?** A **prefix-sum structure** (for example a Fenwick tree) over the levels would answer "how much is there up to price P?" in O(log L). But every add, cancel and fill would then have to update it, slowing *every* order to speed up one type. A FOK that can fill trades through those same levels right after the check anyway, so the walk never changes the order of the work. Not worth it in v1 ([decision 020](../decisions/020-ioc-and-fok.md)).

### 1.4 "It never existed"

A FOK that can't fill is **rejected**, exactly like an order with a bad price:
- one `Reject` event (`FOKNoLiquidity`);
- no `Ack`, no order id consumed, no cancel, no market data.

The alternative, accept and then cancel, would give out an order id and emit two events for an order that never had a chance to trade. An **IOC**, by contrast, *is* accepted and *may* trade. So it gets `Ack`, its fills, and a `CancelAck` for the remainder, with its own reason (`IOCUnfilled`), so consumers can tell it from a market order's expired remainder.

### 1.5 Adding enum values without breaking anything

The new TIFs are 3 and 4, not 2 and 3. **2 is `Day`**, and has been in every log and on the wire since milestone 1. A WAL is a permanent record: if 2 started meaning IOC, replaying an old log would turn every resting `Day` order into an IOC that vanishes on arrival. That's a silent, deterministic, wrong recovery. New values are only ever **appended**. The same goes for the reject reason: `FOKNoLiquidity` is 11, after the engine's reasons, even though it's a book reason.

---

## 2. Code walkthrough

### Types: `internal/book/types.go`

- **`TIF`** ([types.go:85](../../internal/book/types.go#L85)) gains `IOC` (3) and `FOK` (4). `RejectFOKNoLiquidity` is appended after `RejectDuplicateSymbol`.
- **`Order.Validate`** ([types.go:292](../../internal/book/types.go#L292)): a limit order's TIF may be GTC, Day, IOC or FOK. A market order with IOC or FOK is `InvalidTIF`, checked after its price, keeping the fixed order side, type, qty, price, tif. Any other TIF on a market order is still ignored.
- **`Result`** ([types.go:185](../../internal/book/types.go#L185)): `Expired` now means "cancelled on arrival", for a market order's remainder or an IOC's.

### The book: `internal/book/book.go`

- **`Submit`** ([book.go:189](../../internal/book/book.go#L189)): after validation and the duplicate check, `if o.TIF == FOK && !b.canFill(&o)` returns the reject before anything is touched. After matching, `o.Type == Market || o.TIF == IOC` puts the remainder in `Expired` instead of resting it. A FOK with a remainder at that point would mean the check was wrong, so it **panics** rather than rest part of an all-or-nothing order.
- **`canFill`** ([book.go:265](../../internal/book/book.go#L265)) and **`fillCheck`** ([book.go:272](../../internal/book/book.go#L272)): the walk from §1.3. The ladder keeps the best level **last** (decision 001), so it iterates from the end. `fillCheck` also returns how many levels it visited, which the bound test checks; `canFill` discards it.

### The reference book: `internal/book/ref/ref.go`

- **`acceptable`** ([ref.go:95](../../internal/book/ref/ref.go#L95)): "would this order trade at this price?", written separately from the real book's `crosses`.
- **`Submit`** ([ref.go:107](../../internal/book/ref/ref.go#L107)): for a FOK, adds up resting orders in priority order (order by order, not level by level), stopping at an unacceptable price or once covered. Same rule, different code: that's what lets the differential test catch a mistake in either.

### The engine: `internal/engine/engine.go`

- **`newOrder`** ([engine.go:235](../../internal/engine/engine.go#L235)): unchanged for FOK. A book reject is an engine `Reject`, and the order id is only committed on acceptance, so a rejected FOK uses no id. For an expired remainder it picks the reason: `CancelExpired` for a market order, `CancelIOCUnfilled` for a limit (IOC). The cancel event now carries the order's price (0 for a market order, as before).

### The wire and the CLI

- `proto`: `TIFIOC`/`TIFFOK` (3/4), `CancelIOCUnfilled` (3), `ReasonFOKNoLiquidity` (11). The client prints `ioc-unfilled`.
- `engctl order --tif ioc|fok`; in `engctl connect`, `buy 10 101 ioc`. **`parseTIF`** ([main.go:181](../../cmd/engctl/main.go#L181)) parses both. A market order sent with `--tif ioc` passes it through, so the operator sees the engine's reject.

### Tests and results

| test | shows |
|---|---|
| `TestIOCAndFOK` (book and ref) | IOC partial / full / nothing to trade; FOK exact across 3 levels; FOK 1 lot short (rejected, book unchanged); FOK beyond its limit; duplicate check first; market IOC/FOK rejected; market GTC accepted |
| **`TestFOKCheckVisitsOnlyLevelsNeeded`** ([book_internal_test.go:181](../../internal/book/book_internal_test.go#L181)) | on 10,000 levels: 1, 3 and 10 levels visited when that's what's needed; 2 when the second is past the limit; all 10,000 only when the side isn't enough |
| `TestDifferential`, same seeds | 100,000 commands: 4,539 IOC remainders cancelled, 1,013 IOCs filled in full, 751 FOKs filled (277 across levels), 4,653 `FOKNoLiquidity`, 2,226 `InvalidTIF`; no divergence. Since the generator changes of review F-004 the same seeds give 4,463, 1,166, 862 (324), 4,628 and 1,934. The long run (20 seeds × 60,000) is in decision 020 |
| **`TestIOCAndFOKEvents`** ([engine_test.go:177](../../internal/engine/engine_test.go#L177)) | exact event sequences; a rejected FOK uses no order id |
| event-stream test | IOC/FOK in the random stream; none ever rests; no accepted FOK is partly cancelled |
| `TestZeroAllocSteadyState` | IOC cycle and FOK cycle: 0 allocations (~1.0 µs and ~2.1 µs per cycle on this laptop at the time) |
| **`TestIOCAndFOKOverTheWire`** ([server_test.go:557](../../internal/server/server_test.go#L557)) | the real messages over TCP |
| crash harness | clients now send IOC and FOK; WAL replay still exact |

**A measurement note.** At this milestone the "full match" benchmark cycle measured 700–1,100 ns; milestone 2's guide document recorded about 290 ns for the same cycle. Building milestone 2's own code on the same day gave 830–880 ns, so this was the machine's state (power scheme, thermal), not a regression. It is why milestone 9's benchmark protocol records the power mode, interleaves runs and reports medians.

---

## 3. Design decisions

- [020: IOC and FOK](../decisions/020-ioc-and-fok.md). Reject versus cancel; the distinct cancel reason (and the fill log, which only records matches); limit-only; the wire values, corrected from 2/3 to 3/4; the bounded walk and why a prefix-sum structure isn't worth it; the reference book's independent implementation; tests and planted bugs.

---

## 4. What breaks if

1. **The FOK check ignores the order's limit price.** The check sums liquidity at any price and says "enough"; matching then stops at the limit with part unfilled. A naive book would rest or cancel that part, and the trader who needed all-or-nothing now holds half a hedge. Ours detects the impossible state and panics (*planted*: `FOK order 18 passed the liquidity check but left 14 unfilled`), because a crash is recoverable and a silently broken guarantee isn't.

2. **FOK is done by matching and rolling back.** Even if makers' quantities were restored, their **queue positions** would have to be too. A partly filled maker stays at the head; a fully filled one was removed and would have to be put back at the front, not the tail. Any fill or market-data event emitted before the rollback would report a trade that never happened. The read-only pre-check avoids all of it.

3. **An IOC's remainder rests.** The client sent "up to 8, now", got 5, and believes it's done. But 3 lots sit on the book, filling later at a price the client no longer wants, perhaps minutes later, against news it didn't see. *Planted and caught* by the differential test.

4. **A rejected FOK consumes an order id.** Order ids stop being gap-free: the n-th accepted order no longer has id n, and any check of `lastOrderID` against the acks in the log (decision 005) breaks. *Planted and caught.*

5. **IOC took the value 2.** Every `Day` order in an existing WAL replays as an IOC: instead of resting, its remainder is cancelled on arrival. Replay is still deterministic, so nothing complains, but the recovered book is missing every resting day order. Additive enum values make this impossible.

---

## 5. Go idioms introduced

**`go test` flag order.** `go test ./internal/book/ -run TestDifferential -long` works; `go test -run TestDifferential -long ./internal/book/` does **not**. `go test` stops parsing its own flags at the first one it doesn't recognise (`-long`, the test binary's flag) and hands *everything* after it to the test binary, package path included, then tests the current directory instead. The Makefile's `test-long` target had exactly this bug and never ran; it was found this milestone.

**Typed constants as array lengths.** `rejects [book.RejectFOKNoLiquidity + 1]int` sizes an array by an enum's last value. Array lengths must be constants, and a typed constant like `RejectReason(11)` is fine. When the enum grows, the array has to be resized, and an index past it panics, so the coverage table's size is tied to the enum it counts.

**A test-only second return value.** `canFill(o) bool` is what the book needs; `fillCheck(o) (bool, int)` also returns the levels visited. The production path calls `canFill`, which discards the count, at no cost; the white-box test calls `fillCheck` to prove the bound. That's cheaper and more honest than a timing-based test.

**Panic as an assertion.** `if o.TIF == FOK { panic(...) }` after matching documents an invariant the pre-check guarantees ("a FOK has no remainder here"), and turns its violation into a crash, never a silently half-filled FOK. The engine's own policy: panic only on "this program is broken", never on client input (decision 005).

---

## 6. Review questions

1. A FOK buy for 12 at limit 102 arrives; asks are 101 × 5, 102 × 5, 103 × 5. What happens, which events are emitted, and is an order id consumed? And for 10 at 102?
2. Why is FOK decided by a read-only pass instead of matching and undoing?
3. What is the worst-case cost of the FOK check, and what would a prefix-sum structure change? Why wasn't it used?
4. An IOC buy for 8 at 100 meets 5 offered at 100. List the events in order. How does a consumer tell this cancel from a market order's?
5. Why are the new TIF values 3 and 4 rather than 2 and 3, and what would have broken?

<details>
<summary>Answers</summary>

1. **12 at 102:** the check adds 5 (101) and 5 (102) = 10, then reaches 103, beyond the limit, so not enough. The result is one `Reject(FOKNoLiquidity)`: no `Ack`, no fills, no market data, and no order id consumed (the next accepted order gets the id this one would have had). The book is untouched. **10 at 102:** after two levels the sum reaches 10, so it's accepted: `Ack`, then two matches (each a fill pair and a trade print), and the deltas for 101 and 102 (both now 0). No cancel, because a passing FOK fills in full.

2. Matching mutates the book as it goes: makers' quantities shrink, fully filled makers and empty levels are removed, fills are appended, and under copy-on-write, chunks are copied. Undoing that means restoring quantities *and queue positions* (a removed maker would have to go back to the front of its level) and recalling fills, all error-prone. Events already emitted couldn't be recalled at all. A read-only pass costs at most one extra walk over the levels the order trades through anyway, and touches nothing.

3. The check visits levels from the best until the sum covers the order or a level is past the limit. Its worst case is O(levels needed), and O(L), the whole side, only when the side can't cover the order within its limit. A prefix-sum structure (such as a Fenwick tree over levels) would make "how much up to price P?" O(log L), but every add, cancel and fill would have to update it, putting a cost on every order to speed up one type. A passing FOK trades through the same levels right after the check, so the walk never changes the order of its work; only a failing FOK deep in a book costs more than its match would have.

4. `Ack` (order id assigned, qty 8); `Fill` for the taker (5 at 100, leaves 3); `Fill` for the maker (leaves 0); `Trade` (5 at 100, aggressor buy); `BookDelta` (sell 100 → 0); then `CancelAck` (reason `IOCUnfilled`, qty 3, price 100). A market order's remainder carries reason `Expired` (and price 0). The two reasons are distinct values, on the wire and in the events.

5. 2 already meant `Day`, in the book's enum, on the wire and in every WAL record since milestone 1. Reassigning it would make an old log replay every `Day` order as an IOC, cancelling its unfilled part on arrival instead of resting it. Replay would be deterministic and wrong: the recovered book would silently lack every resting day order. Enum values are only ever appended. (The first proposal was 2 and 3, made without checking; it was caught before any code was written.)

</details>
