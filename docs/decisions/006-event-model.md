# 006 — The event model: one flat struct, a fixed order per command

- **Status:** accepted (milestone 2)
- **Code:** `internal/engine/types.go` (`Command`, `Event`, `EventKind`), `internal/engine/engine.go` (`newOrder`, `cancel`)

## Context

Each command produces events for two audiences:

- **The client(s) involved:** an ack or reject, fills, and cancel confirmations.
- **Market-data subscribers:** how each price level changed.

Milestone 3 will encode events onto the wire, milestone 4 will hold them until their command is durable, and milestone 6 will sequence the market-data ones. The representation must not allocate (PRD 6), and the order of events must be fixed so that clients and the replay check can rely on it.

## Decision: one flat, pointer-free struct

`Event` is a single 96-byte struct (112 bytes since milestone 6, which added `MDSeq`). Its `Kind` field says which fields are meaningful:

- `EvAck`, `EvReject`, `EvFill`, `EvCancelled`, `EvBookDelta`, `EvInstrumentAdded`.
- Unused fields are zero.
- Every event carries the `Seq` and `Time` of its command.
- *(Since milestone 7)* every event carries `Cmd`, its command's kind, so a rejected cancel can be told from a rejected order (metrics, admin), and every `BookDelta` carries `Orders`, the instrument's resting order count after the command (for `GET /instruments`, never sent on the wire). Both sit in what was padding: `Event` stays 112 bytes. `InstrumentAdded` echoes the command's `ClOrdID` ([decision 018](018-http-admin-and-instruments.md)).

The alternative was a Go interface with one concrete type per event (`type Ack struct{...}`, `type Fill struct{...}`). It was rejected because:

- **Allocation.** Putting a struct into an interface value usually copies it to the heap. A flat struct can be appended to a reused `[]Event` and sent on a channel with no allocation at all.
- **GC cost.** A slice of pointer-free structs is one block of memory that the GC never needs to scan.
- **Simple copying.** Events can be passed by value, compared with `==` in tests, and written into ring buffers (milestone 6) without aliasing.

**Cost:** a 96-byte (now 112-byte) event carries fields that its kind doesn't use. For a reject, most of it is zeros. This is the same trade NASDAQ's binary protocols make, with fixed layouts per message type. Here the in-memory struct is shared, while the wire format (milestone 3) will encode only the fields each kind needs.

`Command` is flat for the same reasons (104 bytes).

## Decision: the event order for each command

**A new order that is rejected:** exactly one `Reject`, and the book is unchanged.

**A new order that is accepted:**

```
Ack                                   order id assigned; echoes side, price, qty
for each opposite-side level traded, best price first:
    Fill(taker), Fill(maker)          one pair per resting order matched, oldest first
    ...
    BookDelta(level)                  the level's new total; 0 = level removed
Cancelled(expired)                    if a market order had an unfilled remainder
Cancelled(ioc-unfilled)               if an IOC limit order had one (milestone 8)
BookDelta(own level)                  if a limit remainder rested
```

**A cancel:** `Cancelled(requested)`, which echoes the order's client order id, side, price and removed quantity, then `BookDelta` for its level. Or a single `Reject`.

**AddInstrument:** `InstrumentAdded` or `Reject`.

**Why this order:**
- **Ack first.** A client must learn its order id before any fill that refers to it.
- **Taker fill before maker fill.** This is fixed so consumers can rely on it and pair the two by `MatchID`.
- **A level's delta comes right after that level's fills.** A market-data consumer sees the trades, then the level's new state, before moving to the next price. It never sees a level shrink before the trades that shrank it.
- **The level's final size, not one delta per fill.** One delta per touched level, carrying the level's size after the whole command, is fewer messages and can't be misapplied by a consumer that drops intermediate states. The delta is computed with `Book.LevelQty` after matching has finished, which is correct because a command's matching is complete before any of its events are built.
- **An expired market remainder is a `Cancelled` event with reason `CancelExpired`**, not a reject. The order was accepted and partly filled, and the client needs to know the rest won't execute (decision 003).

## Fill fields

- `Qty` is the matched quantity.
- `Leaves` is that party's open quantity *after* this fill: for the taker, still to be matched; for the maker, still resting.
- `Role` is taker or maker.
- `MatchID` pairs the two halves of one match.
- `Price` is always the maker's price.

A client can reconcile entirely from events. `TestEventStreamRebuildsState` checks this: it rebuilds every order's open quantity from `Ack`, `Fill` and `Cancelled` alone, and compares it with the books after every command.

## Costs

- **Two fill events per match** instead of one shared "trade" event. *(Since milestone 6 a third, public `EvTrade` print follows each fill pair, for market data, and deltas and trades carry a per-instrument `md_seq`; [decision 016](016-market-data.md).)* That is simpler routing (each event has exactly one recipient client), at the cost of twice the fill events.
- **Deltas carry absolute level sizes, not changes.** A consumer that misses one delta and then sees the next is still correct for that level. Gap *detection* is still needed, and comes from `md_seq` (milestone 6; a trade print, unlike a delta, is lost for good if missed, which is why the Feed resends rather than waiting for the next delta).
