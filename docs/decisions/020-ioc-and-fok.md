# 020 — IOC and FOK: trade now, or not at all

- **Status:** accepted (milestone 8). Chosen with the user:
  - a FOK that can't fill is a `Reject` (`FOKNoLiquidity`);
  - an IOC's remainder is a `CancelAck` with a new reason, `IOCUnfilled`;
  - both TIFs are limit-only;
  - the wire values are additive;
  - the FOK check walks levels and stops once covered.
- **Code:** `internal/book/book.go` (`Submit`, `canFill`, `fillCheck`), `internal/book/types.go` (`TIF`, `Validate`, `RejectFOKNoLiquidity`), `internal/book/ref/ref.go`, `internal/engine/engine.go` (`newOrder`), `internal/proto/messages.go`, `cmd/engctl`

## Context

PRD roadmap 8: "IOC / FOK (small; FOK needs a liquidity pre-check without mutating the book). Extend differential tests."

- **IOC** (immediate or cancel) trades whatever it can on arrival and cancels the rest. It never rests.
- **FOK** (fill or kill) trades its **whole** quantity on arrival, or nothing.

Both are limit orders: they trade only at their price or better.

## Decisions

### A FOK that can't fill is rejected, as if it never existed (chosen with the user)

If the book can't fill a FOK completely at acceptable prices, `Submit` returns `Reject: FOKNoLiquidity`:
- no `Ack`, no cancel;
- **no order id consumed** (the engine commits an id only for accepted orders, decision 005);
- the book untouched.

The client sees one `Reject`, exactly like an order with a bad price. The alternative, an `Ack` then a cancel, would hand out an order id for an order that never had a chance to trade, and leave a two-event trace for nothing.

**Precedence:** validation, then the duplicate client-order-id check, then the liquidity check. A FOK reusing a live order's client order id is `DuplicateClientOrderID` whatever the book holds. Both books apply the same order, and a scenario test pins it.

### An IOC's remainder is cancelled with its own reason (chosen with the user)

An accepted IOC gets the usual `Ack`, its fills, the deltas, and then, if anything is left, a `CancelAck` with reason **`IOCUnfilled`** (3), carrying the order's price and the cancelled quantity. That's the event sequence OUCH uses for immediate-or-cancel orders ([decision 003](003-integer-domain-and-reject-semantics.md)). It isn't `Expired`, the market-order remainder, so any consumer of the events can tell the two apart.

*Note on the fill log:* the user's reason for a distinct value was "so the fill log distinguishes the two". The fill log ([decision 014](014-fill-log-and-replay-check.md)) records **matches only**, one record per taker–maker pair, and no cancels of any kind. So the distinction lives in the event stream, the wire `CancelAck` and the WAL replay, not in the fill log. Adding cancel records to the fill log would change its format; it's raised as a question at the end of the milestone.

Inside the book, an IOC remainder uses `Result.Expired`, the same field as a market remainder ("unfilled quantity cancelled on arrival"). The engine chooses the reason by order type.

### Limit-only (chosen with the user)

A market order with TIF IOC or FOK is rejected with `InvalidTIF`. A market order already behaves like an IOC with no price limit. "Market FOK" (fill the whole quantity at any price, or nothing) exists on some venues, but it's **out of scope for v1** and listed as a known limitation. Any other TIF on a market order is still ignored, as before, so existing clients are unaffected.

### Wire values: IOC = 3, FOK = 4 (additive; corrected from the approved 2 and 3)

The user approved "IOC = 2, FOK = 3" as I had proposed it. But **2 was already `Day`**, in the book's enum and on the wire, since milestone 1. Reusing it would change the meaning of every `Day` order in existing logs and clients. The additive equivalent the user asked for is **IOC = 3, FOK = 4**:

| enum | new value |
|---|---|
| TIF | `TIFIOC` = 3, `TIFFOK` = 4 |
| cancel reason | `CancelIOCUnfilled` = 3 |
| reject reason | `ReasonFOKNoLiquidity` = 11 (the book's `RejectFOKNoLiquidity`, numbered after the engine's reasons so every existing value keeps its number) |

There's no protocol version bump: no layout changed, and old clients never send the new values. `TestEnumsMatchWire` checks every value and name on both sides. `engctl order --tif gtc|day|ioc|fok`, and `buy QTY PX ioc` in `engctl connect`.

### The FOK check: walk levels from the best, stop once covered (chosen with the user)

`canFill` walks the opposite side from its best level, adding each level's **total** (kept on the level already, so it's O(1) per level). It stops at the first level past the order's limit (no), or as soon as the sum reaches the order's quantity (yes). It reads only; the book is untouched. **Its cost is proportional to the levels the order would trade through, not to the depth of the book.** `TestFOKCheckVisitsOnlyLevelsNeeded` shows it on a 10,000-level book: 1 level visited when the best covers the order, 3 for three levels' worth, 10,000 only when the whole side isn't enough.

Since a FOK that passes will trade through exactly those levels anyway, the check at most doubles the walk the match does. A FOK that fails costs the walk and nothing else.

**Rejected: a cumulative-depth structure.** Keeping, per side, a running sum of quantity from the best level outward (for example a Fenwick tree indexed by level position) would answer "how much is available up to price P" in **O(log L)** for L levels. It isn't worth it for v1:
- **Every add, cancel and fill would pay for it.** Any change to a level's total updates O(log L) prefix sums, and inserting or removing a level shifts positions. That's a cost on the hot path of *every* order, paid to speed up one order type.
- **The walk is already cheap where it matters.** A FOK that can fill trades through those same levels immediately afterwards, so the check never changes the order of the work. Only a FOK that fails deep in the book costs more than its match would have.
- **It's more state to snapshot and to keep exactly in step with the ladder,** and more for the differential test to prove.

If FOK orders against very deep books became common, this is the structure to add.

### The reference book uses the same rule, independently implemented

`ref.Book` adds up its resting orders in priority order (order by order, where the real book adds whole levels), stops at the first unacceptable price or once covered, and rejects if short. IOC remainders are `Expired`. Market IOC/FOK is `InvalidTIF`. The differential test runs the **same seeds** as before (1–10; 1000–1019 with `-long`). The generator now sends:
- among limit orders: 11% IOC and 11% FOK, a third of the FOKs sized to need several levels;
- among invalid orders: market orders with IOC or FOK.

Coverage floors require each of these to happen at least once per 1,000 commands: IOC remainders cancelled, IOCs filled in full, FOKs filled in full, FOKs filled across more than one level, and `FOKNoLiquidity` rejects.

## Costs

- A failing FOK deep in a book walks many levels for nothing (above).
- No market FOK, and no minimum-quantity orders (a "fill at least N" order generalises FOK). Both are out of scope.
- One more reject reason and one more cancel reason for every client to handle.

## Tests

- **`TestIOCAndFOK`** (book and reference):
  - IOC: partial fill with the rest cancelled; IOC filled in full; IOC with nothing to trade.
  - FOK: exact fit across three levels; one lot short (rejected, book unchanged); enough quantity but beyond its limit.
  - Precedence of the duplicate check; market IOC/FOK rejected; market GTC still accepted.
- **`TestFOKCheckVisitsOnlyLevelsNeeded`:** the bound.
- **`TestDifferential`:** 10 seeds × 10,000 commands. Among them, 4,539 IOC remainders cancelled, 1,013 IOCs filled in full, 751 FOKs filled (277 across levels), 4,653 `FOKNoLiquidity` rejects, 2,226 `InvalidTIF` rejects. With `-long` (acceptance 1): **1,200,000 commands over 20 seeds, zero divergence**, including 53,585 IOC remainders cancelled, 12,213 IOCs filled in full, 9,761 FOKs filled (3,600 across levels), 56,360 `FOKNoLiquidity` and 27,832 `InvalidTIF` rejects. *(That run was first attempted through `make test-long`, which turned out never to have worked: see the learning doc §5, flag order.)*
- **`TestIOCAndFOKEvents`** (engine): the exact event sequences; the rejected FOK consumes no order id; market IOC → `InvalidTIF`.
- **The event-stream test:** IOC/FOK in the generated stream; no IOC or FOK ever rests; an accepted FOK is never partly cancelled.
- **`TestZeroAllocSteadyState`:** an IOC cycle and a FOK cycle (fill across two levels, then a rejected FOK), 0 allocations.
- **`TestIOCAndFOKOverTheWire`:** over TCP, the `CancelAck(IOCUnfilled)` with price and quantity, `Reject(FOKNoLiquidity)` with no order id, a FOK filled in full, market FOK → `InvalidTIF`, and nothing else sent.
- **Crash harness:** its clients now also send IOC and FOK orders, so WAL replay is checked with them.

**Planted bugs, all caught:**
- a FOK check that needs strictly more than the order (the differential test, seed 4, on an exact fit);
- a FOK check that ignores the limit (the book's own consistency check panics);
- a check that doesn't stop once covered (the bound test);
- an IOC remainder that rests;
- a market IOC accepted;
- the reference book counting orders beyond the limit;
- the engine reporting an IOC remainder as "expired";
- a rejected FOK consuming an order id.
