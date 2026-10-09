# Learning docs: start here

This folder is a study guide to the matching engine, written milestone by milestone. Each milestone doc has the same six sections: concepts, code walkthrough, design decisions, what breaks if, Go idioms, and self-check questions.

## Reading order

1. [00-glossary.md](00-glossary.md): skim it first, then come back whenever a term is unfamiliar.
2. [01-book-core.md](01-book-core.md): **milestone 1**. What an order book is, price-time priority, the book's data structures, and how we know it is correct (the reference book and differential testing).
3. [02-engine.md](02-engine.md): **milestone 2**. Why one goroutine owns the engine, commands and events, the three gap-free counters, market-by-price deltas, why the book stopped using Go maps, and how to measure "zero allocations" honestly.
4. *(milestone 3: wire protocol and gateway, not yet written)*
5. *(milestone 4: WAL and sequencer, not yet written)*
6. *(milestone 5: snapshots, recovery and crash test, not yet written)*
7. *(milestone 6: market data, not yet written)*
8. *(milestone 7: metrics and polish, not yet written)*
9. *(milestone 8: IOC / FOK, not yet written)*
10. *(milestone 9: benchmarks, not yet written)*

Design decisions live in [`../decisions/`](../decisions/). Each milestone doc links to the ones it relies on.

## How an order flows from socket to fill to market-data event

This is the full path the finished engine will have (PRD section 4). **Steps marked ✅ exist today**; the rest are described so you know where each milestone fits.

```
client ──TCP frame──▶ gateway ──command──▶ sequencer ──record──▶ WAL (disk)
                                              │
                                              ▼
                                        engine ✅ ──Submit/Cancel──▶ book ✅
                                              │
                                              ▼ events (ack, fills, book deltas)
                                          publisher ──▶ client writer ──TCP──▶ client
                                              └──────▶ market-data subscribers
```

1. **Gateway** *(M3)*: a per-connection reader goroutine reads a length-prefixed binary frame, decodes it into a `NewOrder` or `Cancel`, validates it, and puts it on the single ordered command channel. The gateway also stamps the logged-in client id onto the command, which is why `book.Cancel` takes a client id (decision 003).
2. **Sequencer** *(M4)*: gives the command the next global `seq` and a deterministic timestamp, appends it to the write-ahead log, and (under `fsync=always`) waits for the data to be durable.
3. **Engine** ✅ *(M2)*: one goroutine (`engine.Engine.Run`) owns all books. It checks the command's `Seq` is the next one, finds the instrument's book, offers it the next `order_id` (committed only if the book accepts), and calls `Submit` or `Cancel`. Walkthrough: [02-engine.md](02-engine.md#2-code-walkthrough).
4. **Book** ✅ *(M1)*: `book.Book.Submit` validates the order and rejects a duplicate client order id. It then matches against the opposite side by price-time priority, appends one `book.Fill` per match, and rests any limit remainder or expires any market remainder. It returns a `book.Result`. `book.Book.Cancel` removes a resting order owned by the caller. Walkthrough: [01-book-core.md](01-book-core.md#2-code-walkthrough).
5. **Events** ✅ *(M2)*: the engine turns the `Result` and `Fill`s into `engine.Event`s, in a fixed order: `Ack` (or a single `Reject`); for each traded level, a taker/maker `Fill` pair per match then that level's `BookDelta`; then `Cancelled(expired)` for a market remainder, or the `BookDelta` of the level where a limit remainder rested. Each event carries its command's `Seq` and `Time`. `Run` sends them, one by one, on its output channel.
6. **Publisher** *(M3–M6, not yet built)*: holds events until their command is durable, then sends acks and fills to the owning connections' writer goroutines, and sends book deltas (each stamped with the instrument's `md_seq`) to market-data subscribers.

Today you can follow steps 3–5 in code: `engine.Step` in `internal/engine/engine.go` calls `book.Submit` in `internal/book/book.go` and builds the events. `internal/engine/stream_test.go` rebuilds market data and every client's orders from those events alone, and `internal/book/diff_test.go` checks the book against `internal/book/ref/ref.go`.
