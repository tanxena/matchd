# Glossary

Terms used in this repo's code and docs. Each entry names the milestone where the term first matters. Terms marked *(later)* are defined now so earlier docs can refer to them.

## Market structure

- **Instrument / symbol** (M1): the thing being traded, such as a stock. Each instrument has its own independent book.
- **Limit order book (LOB)** (M1): the list of all resting buy and sell orders for one instrument, grouped by price. Its two halves are the *bid* side (buyers) and the *ask* or *offer* side (sellers).
- **Bid / ask** (M1): a bid is a resting buy order; an ask is a resting sell order. The **best bid** is the highest bid price; the **best ask** is the lowest ask price.
- **Top of book** (M1): the best bid and best ask, with the quantity at each.
- **Spread** (M1): best ask − best bid. It is always positive in a healthy book.
- **Mid** (M1): the price halfway between best bid and best ask. The differential generator uses a drifting "mid" to choose where to place orders.
- **Crossed book / locked book** (M1): crossed means best bid > best ask; locked means best bid = best ask. A matching engine can never be in either state after processing a command, because any overlap would already have traded. `CheckInvariants` asserts best bid < best ask.
- **Price level** (M1): all resting orders on one side at one exact price. In the code this is `level`, with a FIFO queue of `orderNode`s.
- **Depth** (M1): how many levels or orders are resting. "Max depth" in the harness's coverage line is the most resting orders seen at once.
- **Tick / tick size** (M1): the smallest allowed price increment. Prices in this engine are *counts of ticks* (`book.Price`).
- **Lot / lot size** (M1): the smallest tradable quantity unit. Quantities are *counts of lots* (`book.Qty`).

## Orders

- **Limit order** (M1): "buy (or sell) up to Q at price P or better". Whatever doesn't trade immediately **rests** on the book at P.
- **Market order** (M1): "buy (or sell) Q at whatever price is available". It never rests; any unfilled remainder is cancelled (`Result.Expired`).
- **IOC (immediate or cancel)** *(later, M8)*: a limit order that never rests; its unfilled remainder is cancelled.
- **FOK (fill or kill)** *(later, M8)*: an order that fills completely right away or does nothing at all.
- **Time in force (TIF)** (M1): how long an order may rest. **GTC** (good till cancelled) rests until filled or cancelled. **Day** would expire at the end of the session; in v1 it is treated as GTC (decision 003).
- **Order id** (M1): the engine's unique number for an accepted order (`book.OrderID`). The caller assigns it, never the book.
- **Client order id (ClOrdID)** (M1): the client's own label for an order. It must be unique among that client's *live* orders (PRD 5.1).
- **Cancel** (M1): removing a resting order. Here it is scoped to the owning client (decision 003).
- **Reject** (M1): refusing a command, with a reason (`book.RejectReason`). A rejected command doesn't change the book.

## Matching

- **Price-time priority** (M1): the matching rule. Better price first; at equal prices, earlier arrival first (FIFO).
- **FIFO queue** (M1): first in, first out. The order of orders within a level.
- **Taker / aggressor** (M1): the incoming order that trades against the book.
- **Maker / resting order** (M1): the order already on the book that the taker trades against.
- **Fill / execution / trade** (M1): one match between a taker and one maker, for some quantity at the **maker's** price (`book.Fill`).
- **Partial fill** (M1): a fill for less than an order's remaining quantity. A partially filled maker keeps its place in the queue.
- **Sweep / walking the book** (M1): a taker consuming several levels in a row, from the best price outward.
- **Self-trade** (M1): a client's order matching that client's own resting order. Not prevented in v1 (PRD 5.2).

## Testing

- **Reference book / oracle** (M1): `internal/book/ref`, a deliberately simple implementation whose output is taken as correct.
- **Differential testing** (M1): running the same inputs through two implementations and failing on any difference (W. M. McKeeman, "Differential Testing for Software", *Digital Technical Journal* 10(1), 1998).
- **Seed** (M1): the starting value of a pseudo-random generator. The same seed always produces the same command stream, so any failure can be replayed.
- **Invariant** (M1): a property that must hold after every command, for example "level total equals the sum of its orders".
- **Mutation check** (M1): deliberately introducing a small bug to confirm that a test catches it. A test that can't fail proves nothing.
- **Coverage floor** (M1): a minimum count for each kind of event in the harness. It fails the test if the generator stops exercising a path.

## Data structures

- **Slab** (M1): one big slice holding all records of a kind, addressed by index (`Book.nodes`).
- **Free list** (M1): a linked list of unused slab slots or structs, reused before allocating new ones.
- **Intrusive linked list** (M1): a list whose links (`prev`, `next`) are fields of the elements themselves, rather than separate list nodes.

## Later milestones

- **Sequence number (seq)** *(later, M4)*: a global, gap-free counter assigned to every accepted command.
- **WAL (write-ahead log)** *(later, M4)*: an append-only file where every command is recorded before its effects become visible.
- **fsync** *(later, M4)*: the system call that forces written data from the OS cache onto stable storage.
- **Snapshot** *(later, M5)*: a saved copy of the full book state at some seq.
- **md_seq** *(later, M6)*: the per-instrument market-data sequence number subscribers use to detect gaps.
- **ITCH / OUCH** (reference): NASDAQ's market-data (ITCH) and order-entry (OUCH) protocols. They are cited for terminology:
  - [TotalView-ITCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHspecification.pdf)
  - [OUCH 4.2](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH4.2.pdf)
  - [OUCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH5.0.pdf)
