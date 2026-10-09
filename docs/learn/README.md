# Learning docs: start here

This folder is a study guide to the matching engine, written milestone by milestone. Each milestone doc has the same six sections: concepts, code walkthrough, design decisions, what breaks if, Go idioms, and self-check questions.

## Reading order

1. [00-glossary.md](00-glossary.md): skim it first, then come back whenever a term is unfamiliar.
2. [01-book-core.md](01-book-core.md): **milestone 1**. What an order book is, price-time priority, the book's data structures, and how we know it is correct (the reference book and differential testing).
3. [02-engine.md](02-engine.md): **milestone 2**. Why one goroutine owns the engine, commands and events, the three gap-free counters, market-by-price deltas, why the book stopped using Go maps, and how to measure "zero allocations" honestly.
4. [03-gateway.md](03-gateway.md): **milestone 3**. TCP framing, fixed binary layouts, two goroutines per connection, backpressure in both directions, the slow-consumer policy, sessions and identity, and fuzzing.
5. [04-wal.md](04-wal.md): **milestone 4**. Write-ahead logging, what `write` and `fsync` guarantee, torn writes and checksums, group commit, the durability gate, fail-stop on fsync errors, and recovery by replay.
6. *(milestone 5: snapshots, recovery and crash test, not yet written)*
7. *(milestone 6: market data, not yet written)*
8. *(milestone 7: metrics and polish, not yet written)*
9. *(milestone 8: IOC / FOK, not yet written)*
10. *(milestone 9: benchmarks, not yet written)*

Design decisions live in [`../decisions/`](../decisions/). Each milestone doc links to the ones it relies on.

## How an order flows from socket to fill to market-data event

This is the full path the finished engine will have (PRD section 4). **Steps marked ✅ exist today**; the rest are described so you know where each milestone fits.

```
client ──TCP frame──▶ gateway ✅ ──command──▶ sequencer ✅ ──record──▶ WAL (disk) ✅
                                              │                         │ fsync
                                              ▼                         ▼
                                        engine ✅ ──Submit/Cancel──▶ book ✅    durable seq ✅
                                              │                                  │
                                              ▼ events (ack, fills, book deltas) │ gates release
                                          publisher ✅* ◀─────────────────────────┘
                                              ├──▶ client writer ✅ ──TCP──▶ client
                                              └──▶ market-data subscribers

✅* = partly built: the publisher gates on durability and routes to clients,
      but doesn't serve market data yet (M6).
```

1. **Gateway** ✅ *(M3)*: the connection's reader goroutine (`gateway.session.readLoop`) reads a length-prefixed binary frame with `proto.Reader`, decodes it, and checks the session is logged in. It builds an `engine.Command` with the **session's** client id (frames carry none) and puts it on the single shared command channel. It validates the protocol only: field values go to the engine unchecked, so every business reject is sequenced and logged. Walkthrough: [03-gateway.md](03-gateway.md#2-code-walkthrough).
2. **Sequencer** ✅ *(M3, M4)*: takes every command already waiting on the shared channel (up to 4,096), gives each the next global `seq` and the timestamp, and writes the batch to the **write-ahead log** with one `write`. Under `fsync=always` it fsyncs before forwarding the batch to the engine. Under `group:1ms` it forwards at once, and a syncer goroutine fsyncs every 1 ms. Either way the **durable seq** advances once the fsync covers the batch. Walkthrough: [04-wal.md](04-wal.md#2-code-walkthrough).
3. **Engine** ✅ *(M2)*: one goroutine (`engine.Engine.Run`) owns all books. It checks the command's `Seq` is the next one, finds the instrument's book, offers it the next `order_id` (committed only if the book accepts), and calls `Submit` or `Cancel`. Walkthrough: [02-engine.md](02-engine.md#2-code-walkthrough).
4. **Book** ✅ *(M1)*: `book.Book.Submit` validates the order and rejects a duplicate client order id. It then matches against the opposite side by price-time priority, appends one `book.Fill` per match, and rests any limit remainder or expires any market remainder. It returns a `book.Result`. `book.Book.Cancel` removes a resting order owned by the caller. Walkthrough: [01-book-core.md](01-book-core.md#2-code-walkthrough).
5. **Events** ✅ *(M2)*: the engine turns the `Result` and `Fill`s into `engine.Event`s, in a fixed order: `Ack` (or a single `Reject`); for each traded level, a taker/maker `Fill` pair per match then that level's `BookDelta`; then `Cancelled(expired)` for a market remainder, or the `BookDelta` of the level where a limit remainder rested. Each event carries its command's `Seq` and `Time`. `Run` sends them, one by one, on its output channel.
6. **Publisher** ✅* *(M3 routing; M4 durability gate; M6 market data)*: **holds** each event until the durable seq covers it. Nothing leaves the process before its command is on disk (decision 012). It then routes the event by client id to that client's session queue, **never blocking**; a full queue disconnects the client as a slow consumer. From milestone 6 it also sends book deltas, stamped with `md_seq`, to market-data subscribers.
7. **Writer** ✅ *(M3)*: the connection's writer goroutine encodes each event as a wire message (`Ack`, `Fill`, `CancelAck`, `Reject`) and writes it to the socket, flushing when it has nothing more queued.

Today the whole client path runs, durably. Start `engine -data ./data -instrument XYZ:1:1` and use `engctl connect --client 1` to watch an order go socket → gateway → sequencer → WAL → engine → book → publisher (waiting for fsync) → writer → socket. Kill the engine and start it again: it replays `./data/wal` and carries on. `internal/server/server_test.go` and `durability_test.go` drive the same path over loopback in tests.
