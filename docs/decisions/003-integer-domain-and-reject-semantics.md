# 003 — Integer prices, order identity, and reject semantics

- **Status:** accepted (milestone 1)
- **Code:** `internal/book/types.go`, `internal/book/book.go` (`Submit`, `Cancel`)

These are the book's externally visible rules that the PRD leaves open. The reference book (`internal/book/ref`) implements every one of them independently, and the differential test compares the results command by command.

## Integers only

- `Price` is an `int64` count of ticks.
- `Qty` is an `int64` count of lots.

`Instrument.TickSize` and `LotSize` record what one tick and one lot are worth so that edges such as the CLI can convert. Matching never reads them.

**Why:** floats can't represent most decimal prices exactly. Two orders "at 100.1" could then compare unequal, and price-time priority would depend on rounding. PRD 5.1 forbids floats in the matching path.

**Cost:** clients must convert at the edge. A price that isn't a whole number of ticks can't be expressed at all, which is what we want.

## Who assigns `order_id`

The **caller** assigns it: the engine from milestone 2 on, and the test harness in milestone 1. The book never invents ids. It panics if asked to rest an id that is already resting, because that is a caller bug that would corrupt `byID`.

**Why:** the engine owns sequencing (PRD 5.1). Keeping id assignment out of the book keeps the book a pure function of its inputs, which replay (milestone 5) depends on.

## Validation order

`Order.Validate` checks fields in a fixed order: **side, type, qty, price, tif**. An order with several problems always gets the first reason in that order.

- Zero values of `Side`, `OrderType` and `TIF` are deliberately invalid, so an uninitialised field is rejected rather than read as "buy" or "limit".
- Limit orders need `Price > 0` and a TIF of `GTC` or `Day`.
- Market orders need `Price == 0`. A market order with a price is almost certainly a client mistake, so we reject it instead of guessing. Their TIF is ignored.

The duplicate-client-order-id check runs only after an order passes validation.

## Duplicate client order id (PRD 5.1)

A new order whose `(client_id, client_order_id)` matches a **resting** order is rejected with `DuplicateClientOrderID`. This applies to market orders too, because the rule says "a `NewOrder`". Once the original order has left the book, by full fill or cancel, the id may be reused. Different clients may use the same client order id.

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

## Day orders

`TIF = Day` is accepted and behaves exactly like `GTC`. There is no trading-session concept in v1, so nothing ever expires a day order. This is a documented known limitation (PRD 5.1, 8.8).

## Self-trade prevention

None in v1 (PRD 5.2). A client's buy can match its own resting sell. This is a known limitation, to be listed in the README.
