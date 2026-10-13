# Learning docs: start here

This folder is a study guide to the matching engine, written milestone by milestone. Each milestone doc has the same six sections: concepts, code walkthrough, design decisions, what breaks if, Go idioms, and self-check questions.

## Reading order

1. [00-glossary.md](00-glossary.md): skim it first, then come back whenever a term is unfamiliar.
2. [01-book-core.md](01-book-core.md): **milestone 1**. What an order book is, price-time priority, the book's data structures, and how we know it is correct (the reference book and differential testing).
3. [02-engine.md](02-engine.md): **milestone 2**. Why one goroutine owns the engine, commands and events, the three gap-free counters, market-by-price deltas, why the book stopped using Go maps, and how to measure "zero allocations" honestly.
4. [03-gateway.md](03-gateway.md): **milestone 3**. TCP framing, fixed binary layouts, two goroutines per connection, backpressure in both directions, the slow-consumer policy, sessions and identity, and fuzzing.
5. [04-wal.md](04-wal.md): **milestone 4**. Write-ahead logging, what `write` and `fsync` guarantee, torn writes and checksums, group commit, the durability gate, fail-stop on fsync errors, and recovery by replay.
6. [05-recovery.md](05-recovery.md): **milestone 5**. Snapshots without pausing the engine (copy-on-write over a chunked slab), atomic file replacement and why it isn't atomic on Windows, the manifest and retention, the fill log and replay-check, and a crash harness that kills the real binary more than 50 times.
7. [06-market-data.md](06-market-data.md): **milestone 6**. Market-by-price deltas and trade prints, `md_seq` assigned by the engine, snapshot-then-deltas with no gap or overlap, gap detection, resend from a per-instrument ring, re-snapshot when too old, and the client `Feed` that does it.
8. [07-operations.md](07-operations.md): **milestone 7**. Prometheus metrics and why histograms; what matching latency means exactly; **timing sub-microsecond work on Windows** (why `time.Now` can't, QPC's cost from Go, the invariant TSC and its calibration); the HTTP side and why admin commands go through the log; graceful shutdown and the TCP reset that can destroy answers already sent.
9. [08-ioc-fok.md](08-ioc-fok.md): **milestone 8**. Time in force; IOC and FOK and why traders want them; deciding fill-or-kill with a read-only pre-check bounded by the levels needed; why a rejected FOK never existed; adding enum values without breaking old logs.
10. [09-benchmarks.md](09-benchmarks.md): **milestone 9**. Latency versus throughput and Little's law; closed and open loop; coordinated omission; quantiles and log-linear histograms; why a laptop is a noisy instrument (power plans, hyperthreads, affinity, other programs, interrupt time); interleaving and never the best run. Also the line-by-line walkthrough of the harness: load generator, clock probe, interleaving script, disturbance checks, report generator.

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
                                              ├──▶ fill log ✅ (every released match)
                                              └──▶ market-data subscribers ✅ (snapshot, then deltas and trades by md_seq; resend from a ring)

  in the background: engine ──freeze (copy-on-write)──▶ snapshot writer ✅ ──▶ snapshots + manifest ✅
  at startup:        manifest → newest snapshot → WAL records after it → engine ✅

✅* = the publisher is complete for v1: durability gate, client routing and market data.
```

1. **Gateway** ✅ *(M3)*: the connection's reader goroutine (`gateway.session.readLoop`) reads a length-prefixed binary frame with `proto.Reader`, decodes it, and checks the session is logged in. It builds an `engine.Command` with the **session's** client id (frames carry none) and puts it on the single shared command channel. It validates the protocol only: field values go to the engine unchecked, so every business reject is sequenced and logged. Walkthrough: [03-gateway.md](03-gateway.md#2-code-walkthrough).
2. **Sequencer** ✅ *(M3, M4)*: takes every command already waiting on the shared channel (up to 4,096), gives each the next global `seq` and the timestamp, and writes the batch to the **write-ahead log** with one `write`. Under `fsync=always` it fsyncs before forwarding the batch to the engine. Under `group:1ms` it forwards at once, and a syncer goroutine fsyncs every 1 ms. Either way the **durable seq** advances once the fsync covers the batch. Walkthrough: [04-wal.md](04-wal.md#2-code-walkthrough).
3. **Engine** ✅ *(M2)*: one goroutine (`engine.Engine.Run`) owns all books. It checks the command's `Seq` is the next one, finds the instrument's book, offers it the next `order_id` (committed only if the book accepts), and calls `Submit` or `Cancel`. Walkthrough: [02-engine.md](02-engine.md#2-code-walkthrough).
4. **Book** ✅ *(M1)*: `book.Book.Submit` validates the order and rejects a duplicate client order id. A fill-or-kill order is first checked against the opposite side without touching it, and rejected if it can't fill in full (M8). It then matches against the opposite side by price-time priority, appends one `book.Fill` per match, and rests any limit remainder, or cancels it on arrival for a market or IOC order (M8). It returns a `book.Result`. `book.Book.Cancel` removes a resting order owned by the caller. Walkthrough: [01-book-core.md](01-book-core.md#2-code-walkthrough).
5. **Events** ✅ *(M2)*: the engine turns the `Result` and `Fill`s into `engine.Event`s, in a fixed order: `Ack` (or a single `Reject`); for each traded level, a taker/maker `Fill` pair per match then that level's `BookDelta`; then `Cancelled(expired)` for a market remainder, or the `BookDelta` of the level where a limit remainder rested. Each event carries its command's `Seq` and `Time`. `Run` sends them, one by one, on its output channel.
6. **Publisher** ✅ *(M3 routing; M4 durability gate; M6 market data)*: **holds** each event until the durable seq covers it. Nothing leaves the process before its command is on disk (decision 012). It then routes the event by client id to that client's session queue, **never blocking**; a full queue disconnects the client as a slow consumer. Book deltas and trades (each with the instrument's `md_seq`) update the publisher's own price-level view and ring buffer, and go to every subscriber of that instrument. A new subscriber first gets a snapshot from that view ([06-market-data.md](06-market-data.md)).
7. **Writer** ✅ *(M3)*: the connection's writer goroutine encodes each event as a wire message (`Ack`, `Fill`, `CancelAck`, `Reject`) and writes it to the socket, flushing when it has nothing more queued.

**Measuring the path (M9):** the benchmark drives this path from a separate, pinned process (`bench/loadgen`) over loopback TCP. It reads the engine's matching-core latency (step 3, `Run`) from `GET /debug/latency`, and times end-to-end latency at the client, from step 1's send to step 7's first answer, which includes the WAL fsync in step 2. See [09-benchmarks.md](09-benchmarks.md).

**Around the path (M7):** the engine times every command with `internal/clock`. The WAL times every fsync. The publisher counts commands and fills as it **releases** them, so `/metrics` and the per-command debug log never show anything not yet durable. The side HTTP listener serves `/metrics`, `/healthz` and `/instruments`; `POST /instruments` enters the path at step 2 as a sequenced command under a reserved client id, and its answer comes back through step 6. On shutdown the path is drained in order: readers stop, the sequencer fsyncs what's queued, the engine and publisher finish, writers deliver every answer, then a snapshot is written ([07-operations.md](07-operations.md)).

Today the whole client path runs, durably. Start `engine -data ./data -instrument XYZ:1:1` and use `engctl connect --client 1` to watch an order go socket → gateway → sequencer → WAL → engine → book → publisher (waiting for fsync) → writer → socket. Kill the engine and start it again: it loads its newest snapshot, replays the WAL after it, and carries on. Stop it with Ctrl-C instead and it drains, answers every client, snapshots, and the next start replays nothing. Watch it with `curl 127.0.0.1:9100/metrics`, and add instruments with `engctl add-instrument --symbol ABC --tick 1 --lot 1`. `engctl replay-check --data ./data` (with the engine stopped) independently rebuilds the newest snapshot from the WAL and checks the fill log against it. `internal/server/server_test.go` and `durability_test.go` drive the same path over loopback in tests.
