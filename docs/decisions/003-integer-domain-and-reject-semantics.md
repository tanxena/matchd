# 003 — Integer prices, order identity, and reject semantics

- **Status:** accepted (milestone 1)

> **Superseded in part (2026-10-13, `dc42b11`, `fd338eb`):** quantity and limit price gained upper bounds (`MaxQty`, `MaxPrice`; review F-002), and a client order id became unique per client across every instrument rather than per book (review F-003). Both are described below, in "Upper bounds" and "Across instruments".

- **Code:** `internal/book/types.go`, `internal/book/book.go` (`Submit`, `Cancel`)

These are the book's externally visible rules that the PRD leaves open. The reference book (`internal/book/ref`) implements every one of them independently, and the differential test compares the results command by command.

## Integers only

- `Price` is an `int64` count of ticks.
- `Qty` is an `int64` count of lots.

`Instrument.TickSize` and `LotSize` record what one tick and one lot are worth so that edges such as the CLI can convert. Matching never reads them.

**Why:** floats can't represent most decimal prices exactly. Two orders "at 100.1" could then compare unequal, and price-time priority would depend on rounding. PRD 5.1 forbids floats in the matching path.

**Cost:** clients must convert at the edge. A price that isn't a whole number of ticks can't be expressed at all, which is what we want.

### Upper bounds (review F-002)

| bound | value | rejected with |
|---|---|---|
| `MaxQty` | 2^30 lots (1,073,741,824) | `InvalidQty` |
| `MaxPrice` | 2^53 ticks (a limit price) | `InvalidPrice` |

**Why:**
- **No sum can overflow.** Until the review, any positive `int64` quantity was accepted. Two resting orders at one price, `MaxInt64` and 1, wrapped the level's total to a negative number. A fill-or-kill check then rejected an order the reference book filled, and market data published a negative quantity. A book indexes its orders with `uint32`, so it holds fewer than 2^32 orders. Below 2^30 each, a level's total and a FOK check's running sum over a whole side stay below 2^62.
- **Prices fit a `float64` exactly.** Every price below 2^53 is exactly representable as a `float64`, for any client or tool that decodes one.

**Cost:** an order above either bound is refused. 2^30 lots is a billion lots: an instrument needing more would use a bigger lot. Both books apply the same bounds (`book.MaxQty`, `book.MaxPrice`, values from the spec, not shared logic). `TestQuantityAndPriceBounds` checks the bounds and an exact, positive total for a level of several `MaxQty` orders.

## Who assigns `order_id`

The **caller** assigns it: the engine from milestone 2 on, and the test harness in milestone 1. The book never invents ids. It panics if asked to rest an id that is already resting, because that is a caller bug that would corrupt `byID`.

**Why:** the engine owns sequencing (PRD 5.1). Keeping id assignment out of the book keeps the book a pure function of its inputs, which replay (milestone 5) depends on.

## Validation order

`Order.Validate` checks fields in a fixed order: **side, type, qty, price, tif**. An order with several problems always gets the first reason in that order.

- Zero values of `Side`, `OrderType` and `TIF` are deliberately invalid, so an uninitialised field is rejected rather than read as "buy" or "limit".
- Quantity must be in 1..`MaxQty`.
- Limit orders need `Price` in 1..`MaxPrice` and a TIF of `GTC` or `Day` (since milestone 8 also `IOC` or `FOK`, [decision 020](020-ioc-and-fok.md)).
- Market orders need `Price == 0`. A market order with a price is almost certainly a client mistake, so we reject it instead of guessing. Their TIF is ignored, except that since milestone 8 `IOC` and `FOK` are refused with `InvalidTIF`: they are limit-only, and "market FOK" is a known limitation.

The duplicate-client-order-id check runs only after an order passes validation.

## Duplicate client order id (PRD 5.1)

A new order whose `(client_id, client_order_id)` matches a **resting** order is rejected with `DuplicateClientOrderID`. This applies to market orders too, because the rule says "a `NewOrder`". Once the original order has left the book, by full fill or cancel, the id may be reused. Different clients may use the same client order id.

**Across instruments (review F-003).** PRD 5.1 says unique "per client among live orders": that client's live orders on *every* instrument. Until the review, each book checked only its own orders, so client 7 could rest client order id 5 on two instruments at once.
- **Now:** the engine keeps one `book.ClOrdIDs`, the live (client, client order id) pairs of all its books, and every book shares it (`Book.Share`). A book checks it before accepting an order, adds its resting orders and removes them as they leave. A restored engine rebuilds it from the restored books' orders.
- **The reference book** implements the same rule independently: linked reference books (`ref.Link`) look through each other's orders. `TestDifferentialAcrossInstruments` spreads the command stream over two instruments in both implementations. The engine-level test is `TestClientOrderIDUniqueAcrossInstruments`.
- **This changes reject behaviour:** a `NewOrder` reusing a live client order id on a *different* instrument is now rejected with `DuplicateClientOrderID`, where it used to be accepted.
- **The WAL format is unaffected.** The log records commands, not outcomes, and no existing log is being replayed. A log written before this change would replay with the new rule and could diverge from what was acknowledged then.

## Cancel is scoped to the owning client

`Cancel(client, order_id)` succeeds only if the order is resting **and** belongs to `client`. Every other case returns `UnknownOrder`:

- the order never existed;
- it was already filled;
- it was already cancelled;
- it belongs to another client.

**Why:** without the owner check, any connected client could cancel anyone's orders once the gateway exists (milestone 3). Returning the same `UnknownOrder` for "not yours" as for "doesn't exist" also stops a client from probing which order ids are live.

**Cost:** the cancel command must carry `client_id`. From milestone 3 on, the gateway fills this in from the logged-in session, never from the frame payload.

## Market order on an empty or thin book

A market order is **accepted, never rejected**, for lack of liquidity. It fills what it can, and its remainder is reported in `Result.Expired`. A market order on an empty book is accepted with `Filled = 0` and `Expired = Qty`.

**Why:** "rejected" would wrongly suggest the order was malformed. Exchange protocols report a "trade what you can now" order as accepted, with its unfilled part cancelled. In [NASDAQ OUCH 4.2](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH4.2.pdf), for example, an immediate-or-cancel order that doesn't fully execute gets an *Order Accepted* message, then *Order Executed* for any fills, then *Order Canceled* for the remainder. Our market order behaves the same way, with no price limit.

## Fill-or-kill without liquidity (milestone 8)

A FOK the book can't fill in full at acceptable prices is **rejected** (`FOKNoLiquidity`), after validation and the duplicate client-order-id check. Unlike a market order, it is never accepted and then cancelled: it never existed, and consumes no order id ([decision 020](020-ioc-and-fok.md)). An IOC, like a market order, is accepted, and its unfilled part is cancelled with reason `IOCUnfilled`.

## Day orders

`TIF = Day` is accepted and behaves exactly like `GTC`. There is no trading-session concept in v1, so nothing ever expires a day order. This is a documented known limitation (PRD 5.1, 8.8).

## Self-trade prevention

None in v1 (PRD 5.2). A client's buy can match its own resting sell. This is a known limitation, listed in the README.
