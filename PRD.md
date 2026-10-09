# PRD — Order Matching Engine (Go)

## 0. How to work on this project

You are pair-programming with me, not building this for me. Rules:

- Work in **small increments**. Finish one roadmap milestone, stop, and explain what you built and why before moving on. Do not build ahead.
- For every non-trivial design decision (book data structure, WAL format, sequencing, locking, protocol framing), write `docs/decisions/NNN-title.md`: options considered, choice, why, what it costs.
- Prefer the **standard library**. Justify each dependency in a decision record. No gRPC, no protobuf; the wire protocol is ours.
- No magic. Every goroutine, mutex, channel, and atomic has a comment saying what it protects or coordinates.
- Every package gets tests. Anything touching shared state runs under `go test -race`. Anything touching disk gets a crash/recovery test. The matching core gets differential tests against the reference book (see 5.3).
- **Git is local only.** `git init` if needed, Go `.gitignore`, commit after each milestone with conventional messages. No remote, no push until I say so.
- **Benchmarks:** you write the harness under `bench/` and walk me through it line by line before I run it. All README numbers come from committed files in `bench/results/`. The benchmark rules in section 7 are requirements, not suggestions.
- When you finish a milestone, give me 3 to 5 questions I should be able to answer about the code.

### 0.1 Learning documentation (mandatory)

This repo is also my study resource. Maintain `docs/learn/` as you go.

For every milestone, write `docs/learn/NN-<milestone>.md`, in this order:

1. **Concepts** — the ideas this milestone depends on (price-time priority, what a limit order book is, why a WAL plus deterministic replay gives exact recovery, what `fsync` guarantees, how TCP framing works, what sequence numbers buy a market-data consumer), from first principles for someone who knows Python/TypeScript but is new to Go and to exchange systems. Link primary sources where they exist (Go docs, the relevant RFCs, exchange protocol specs such as NASDAQ ITCH/OUCH for terminology), not blog posts.
2. **Code walkthrough** — file by file, function by function, real paths and names.
3. **Design decisions** — summary linking to `docs/decisions/`.
4. **What breaks if** — 3 to 5 concrete failure scenarios (ack before fsync, replay with a non-deterministic clock, cancel racing a fill, a market-data gap) and exactly what goes wrong.
5. **Go idioms introduced** — one paragraph each, first time used.
6. **Self-check questions** — answers in a collapsed `<details>` block.

Also maintain `docs/learn/00-glossary.md` and `docs/learn/README.md` (reading order, and "how an order flows from socket to fill to market-data event"). Stale learning docs = failing milestone.

## 1. Summary

A single-instrument-per-book, multi-book limit order matching engine in Go. Clients connect over TCP with a compact binary protocol, submit limit, market, cancel (and optionally IOC/FOK) orders, receive acknowledgements and fills, and can subscribe to a sequenced market-data feed. Every accepted command is written to a write-ahead log before it is acknowledged; the engine's state is a pure function of the log, so a crash at any point is recovered by replay to the exact same book, sequence numbers and fills. Correctness is verified by differential testing against a deliberately simple reference book. Performance is reported as absolute p50/p99/p99.9 matching latency and throughput under a strict benchmark protocol.

Target user: anyone who needs to know what an exchange core actually does, or a developer embedding a deterministic matcher in a trading simulator.

## 2. Goals

1. **Correct:** fills identical to the reference book on millions of randomised order streams, including cancels and market orders.
2. **Deterministic and durable:** `kill -9` at any instant; on restart the book, next sequence number and all fills are identical to a no-crash run of the same input. No acknowledged command is lost.
3. **Fast:** report absolute matching-core latency (order in → fills out, excluding network) at p50/p99/p99.9 and sustained orders/sec on the laptop, under the benchmark protocol in section 7. No target number is promised in advance; the number is what it is, measured properly.
4. **Observable:** sequenced market-data feed with gap detection; metrics endpoint; per-command structured logs at debug level.
5. **Explainable:** every design choice recorded; every number reproducible.

## 3. Non-goals

- Multiple instances, replication, failover. Single process.
- Risk checks, margin, positions, accounts, balances. Orders carry a client id and nothing else about the client.
- Auctions, opening/closing crosses, stop orders, iceberg orders, pegged orders.
- Persistence of market-data snapshots for late joiners beyond a simple "snapshot + resume from sequence N".
- A UI. A tiny CLI client is enough.
- TLS, authentication.

## 4. Architecture

```
clients (CLI, bench loadgen, SDK)
     |  TCP, binary frames
     v
+------------------------------------------------------------+
| gateway     framing, decode, validate, per-conn writer     |
|     |                                                      |
|     v  single ordered command channel                      |
| sequencer   assigns seq, appends to WAL, fsync policy      |
|     |                                                      |
|     v                                                      |
| engine      one goroutine; owns all books; no locks inside |
|   ├─ book[instrument]  bids/asks, price levels, FIFO queues|
|   └─ matcher           price-time priority                 |
|     |                                                      |
|     v  events (ack, fill, cancel-ack, reject, book delta)  |
| publisher   fan-out to client writers + market-data subs   |
| snapshot    periodic book + seq snapshot; recovery         |
| metrics     /metrics, /healthz on a side HTTP listener     |
+------------------------------------------------------------+
     disk: <data>/{wal/*.log, snapshots/*.snap, manifest.json}
```

The engine is single-threaded by design: one goroutine consumes a totally ordered command stream and emits events. This is the standard architecture for matching engines and is the central thing you must be able to defend. Concurrency lives at the edges (connections, WAL writer, publisher), not in the book.

Module `github.com/<me>/<name>`:

```
cmd/engine/          server binary
cmd/engctl/          CLI client: connect, submit, cancel, subscribe, replay-check
internal/proto/      wire protocol: frame types, encode/decode, versioning
internal/gateway/    TCP listener, per-connection reader/writer goroutines, backpressure
internal/sequencer/  seq assignment, WAL append, fsync policy, ack gating
internal/wal/        segmented append-only log, CRC32C per record, replay, torn-tail handling
internal/book/       order book: price levels, FIFO queues, add/cancel/match
internal/book/ref/   reference book: deliberately simple sorted-slice implementation, used only in tests
internal/engine/     command loop, event emission, instrument registry
internal/publisher/  event fan-out, market-data sequencing, subscriber ring buffers, slow-consumer policy
internal/snapshot/   snapshot write/load, manifest, recovery orchestration
internal/metrics/    counters, histograms (HDR-style), /metrics
bench/               loadgen (yours, walked through), scripts, results/
docs/decisions/  docs/learn/
```

## 5. Functional requirements

### 5.1 Instruments and orders
- Instruments created via admin command or config: symbol, tick size, lot size. Prices and quantities are integers in ticks and lots; no floats anywhere in the matching path.
- Order: `client_id`, `client_order_id`, `instrument`, `side`, `type ∈ {limit, market, ioc, fok}` (ioc/fok in milestone 8), `price` (limit only), `qty`, `tif` for limit ∈ {gtc, day} (day is accepted and treated as gtc in v1; document).
- Engine assigns a monotonic `order_id` per accepted order and a global `seq` per accepted command.
- `client_order_id` is unique **per client among live orders**. A `NewOrder` whose `(client_id, client_order_id)` matches a currently resting order is rejected with `DuplicateClientOrderID`. Once that order is fully filled or cancelled, the id may be reused. The reference book implements the identical rule; the differential test asserts identical rejects.

### 5.2 Matching semantics
- Price-time priority. Incoming buy matches against asks from lowest price up while ask price ≤ limit; within a price level, FIFO by arrival. Symmetric for sells.
- Partial fills: the resting order's remaining qty is reduced in place, keeping its queue position.
- Market order: matches until filled or book side empty; unfilled remainder is cancelled (never rests).
- Limit order remainder rests at its price.
- Cancel: by `order_id`; removes from the level in O(1) or O(log n); document which and why.
- Self-trade prevention: **not** in v1; document as a known limitation.
- Every match emits a fill event to both sides with price = resting order's price, and a book-delta event for market data.

### 5.3 Reference book and differential testing
- `internal/book/ref` is a brute-force book: two sorted slices, linear scans, obviously correct, no optimisation.
- A differential test harness generates random command streams (seeded, so failures reproduce): mixes of limits at random prices around a drifting mid, markets, cancels of live and already-dead orders, duplicate client order ids. It runs the stream through both books and asserts identical fill sequences, identical resting state after every command, and identical rejects.
- Run sizes: 10k commands per seed in the normal test suite; a `-long` flag runs 1M+ commands across many seeds. Any divergence prints the seed and the first diverging command.
- Property tests on invariants after every command: best bid < best ask or one side empty; every level's qty equals the sum of its orders; no order appears twice; cancelled ids are gone.

### 5.4 Durability and recovery
- Order of operations per command under `fsync=always`: decode → validate → assign seq → append to WAL → fsync → apply to book → emit events → ack. **No outbound event (ack, fill, cancel-ack, book delta) leaves the process before the command it results from is durable.**
- Under `fsync=group:Nms` the engine must not stall per batch. Design: append to WAL → apply to book immediately → place all resulting events in a pending buffer tagged with the command's seq → a WAL writer fsyncs the batch every N ms (or when the buffer is full) → the publisher releases all pending events with seq ≤ the last durable seq. The book is therefore ahead of disk by at most one batch window; nothing observable by any client is. Write this as a decision record with the exact rule for what may and may not leak before durability, and test it: a test that fails the fsync must show that no ack or fill was emitted for the affected batch.
- WAL record: length, CRC32C, seq, timestamp (from a deterministic source, see below), command bytes. Segmented files; a segment is deletable once a snapshot's seq exceeds its last seq.
- fsync policy: `always` (default), `group:Nms` (batch commits, acks released together after one fsync), `never` (tests only). Document exactly what each loses on power failure, and measure the throughput difference in the benchmark.
- Determinism: the engine must not read the wall clock. Timestamps come from the sequencer and are stored in the WAL record, so replay sees the same values. Any randomness is forbidden in the engine.
- Snapshotting a single-threaded engine: the engine goroutine must not pause to serialise the book, because that pause lands in the latency histogram the benchmark measures. Required design: on a snapshot trigger the engine records the current seq and hands a **copy-on-write view** of the book to a snapshot goroutine (immutable price-level slabs are shared; levels mutated after the trigger are copied first), which serialises off-thread and writes atomically. Record in a decision record the cost of the copy-on-write path and measure its effect on p99.9 in the benchmark by running one configuration with snapshots every 10 s. If copy-on-write proves too complex in the time available, the fallback is a stop-the-world copy with the pause **measured and reported** as a separate row, never hidden.
- Recovery: load manifest → load latest snapshot → replay WAL records with seq > snapshot seq → resume. Torn tail (bad CRC at end of last segment) is truncated and logged; a bad CRC elsewhere is fatal.
- Replay check tool (`engctl replay-check`): runs a WAL through a fresh engine and compares the resulting book and fill log against the engine's own snapshot and fill log. Used in the crash test.
- **What the crash test does and does not prove.** Killing the process leaves the OS page cache intact, so unflushed writes still reach disk. The crash test therefore validates recovery logic (replay, determinism, torn-tail handling, seq continuity), **not** power-loss durability of any fsync policy. It would pass with `fsync=never`. The README must say this explicitly and must not claim the kill test validates fsync. Fsync behaviour is validated separately by (a) a test using an fsync-counting file wrapper that asserts the ack is gated on the fsync call under `always` and on the batch fsync under `group`, and (b) the measured throughput difference between policies in the benchmark.
- Crash test: harness runs the engine in a child process, drives random load, kills it with SIGKILL / `TerminateProcess` at random points, restarts, runs `replay-check`, and asserts: every acked command is in the WAL, the recovered book equals the pre-crash book for all acked commands, no fill was emitted twice, sequence numbers continue with no gap or reuse. ≥ 50 consecutive runs in the test.

### 5.5 Wire protocol
- Length-prefixed binary frames over TCP. Fixed little-endian layout per message type; a 1-byte version in the frame header. Messages: `Login`, `NewOrder`, `Cancel`, `Subscribe`, `Heartbeat` inbound; `Ack`, `Reject(reason)`, `Fill`, `CancelAck`, `BookDelta`, `Snapshot`, `Heartbeat` outbound.
- Encode/decode with no reflection and no allocation on the hot path for `NewOrder`, `Ack`, `Fill`. Prove with `go test -bench` allocation counts.
- Per-connection reader goroutine (decode → command channel) and writer goroutine (event channel → socket). Writer uses a bounded buffer; a client that cannot keep up is disconnected with a logged reason (slow-consumer policy), never allowed to stall the engine.
- A small side HTTP listener for `/metrics`, `/healthz`, and an admin `POST /instruments` (JSON). Nothing on the HTTP side touches the order path.

### 5.6 Market data
- Every book-changing event is published with a market-data `md_seq` per instrument. Subscribers receive deltas (price level add/update/remove, trade). A new subscriber receives a `Snapshot` (full book, current `md_seq`) then deltas from there.
- Consumers detect gaps by `md_seq`; the protocol includes a `Resend(from_seq)` request served from the publisher's ring buffer (bounded; beyond it, the client must re-snapshot).
- Document the gap-and-recover flow and test it by dropping frames in a test client.

### 5.7 CLI
`engctl`: `connect`, `order --side buy --type limit --px 100 --qty 5`, `cancel --id N`, `book <symbol>` (prints top N levels from a fresh snapshot), `subscribe <symbol>` (streams deltas), `replay-check --data <dir>`.

### 5.8 Observability
- `/metrics` in Prometheus text format: commands total by type and outcome, fills total, matching latency histogram (engine in → events emitted), WAL fsync latency histogram, WAL bytes, snapshot age, connections, slow-consumer disconnects, md gap resends.
- Structured logs via `slog`. Info: lifecycle, connections, snapshots, recovery stats. Debug: one line per command.

## 6. Non-functional requirements

- Go 1.22+ (use what's installed; record version). `go vet`, `golangci-lint`, `go test -race ./...` clean.
- The engine goroutine must not allocate on the steady-state path for limit add, cancel, and match. Show with `testing.AllocsPerRun` and a benchmark table in the learning doc.
- Graceful shutdown: stop accepting, drain the command channel, fsync, snapshot, close.
- No Docker. `Makefile` with `build`, `test`, `test-long`, `lint`, `run`, `bench`.

### 6.1 Platform
Developed on Windows 11; must also build and pass tests on Linux. Handle `fsync` semantics (`FlushFileBuffers` on Windows), atomic rename for snapshots, and process-kill in the crash harness on both platforms. Record Windows-specific gotchas in a decision record.

## 7. Benchmark protocol (requirements)

These are hard requirements on the harness and on what the README may claim.

1. **What is measured, exactly.** Two numbers, separately: (a) *matching-core latency*: timestamp taken when the engine goroutine dequeues a command, to when it has emitted all resulting events to the publisher channel, measured in-process with a monotonic clock; (b) *end-to-end latency*: client send to client receipt of ack/fill over loopback TCP. Every reported latency is labelled p50, p99, p99.9, and max, with the count. No unlabelled "overhead".
   - **Clock overhead.** A resting-limit add can take well under a microsecond, so two clock reads per command are not negligible, and Windows timer resolution is coarse at the low end. The harness must measure and report the cost and resolution of the clock it uses (a tight loop of back-to-back reads, reported as its own row), and core-latency tables carry that figure in a footnote. If per-command timing proves to dominate, switch to batch timing (time N commands, divide) for the p50 column and say so; p99/p99.9 stay per-command.
   - **Fsync policy does not affect matching-core latency** by definition (the WAL is outside the measured span). The fsync matrix applies to end-to-end latency and throughput rows only. Core-latency tables are per order mix, not per fsync policy.
2. **Throughput** is sustained orders/sec over the measurement window, with the CPU usage of the engine process and of the load generator recorded alongside, so saturation is visible in the data.
3. **Load generator** is a separate process, pinned to different cores from the engine where the OS allows (`start /affinity` on Windows, `taskset` on Linux). Its own CPU usage and any send-side queueing are recorded in the result file.
4. **Warmup and duration.** Each run: ≥ 30 s warmup discarded, ≥ 60 s measured, so the CPU is at thermal steady state. The result file records the actual durations.
5. **Repetition and interleaving.** Every configuration is run ≥ 5 times. Runs are interleaved across the configurations being compared in the same table, with a cooldown between runs: for the fsync table, cycle `always, group, never, always, group, never, …`; for the order-mix table, cycle `resting, crossing, cancel-heavy, …`; for the snapshot row, alternate `snapshots-on, snapshots-off`. The README reports median and min–max across repetitions, never the best run.
6. **Configurations.** At minimum: fsync `always` vs `group:1ms` vs `never`; order mix "mostly resting" (90% limit away from mid) vs "mostly crossing" (50% at or through mid) vs "cancel-heavy" (40% cancels). Each as its own rows.
7. **No strawman.** The reference book is the correctness oracle and is **never** benchmarked for the README. The README does not cite figures from other engines as comparisons. The headline is this engine's absolute numbers under this protocol.
8. **Reproducibility.** Raw results as JSON in `bench/results/`, one file per run; the README table is generated from those files by a committed script; each row carries the exact command; the hardware (CPU, cores, RAM, OS, Go version, power mode, "nothing else running") is stated once at the top.

## 8. Acceptance criteria

1. Differential test: 1M+ commands across ≥ 20 seeds, zero divergence from the reference book. Normal suite: 10k per seed, ≥ 10 seeds.
2. Crash test: ≥ 50 consecutive runs, zero lost acked commands, zero duplicate fills, recovered book identical, sequence continuous.
3. Zero allocations on the engine's steady-state path for limit add, cancel, match (proven by test).
4. A new market-data subscriber can join mid-stream, receive a snapshot, detect an induced gap, resend, and converge to the same book as a from-start subscriber (test).
5. Slow-consumer test: a subscriber that stops reading is disconnected and the engine's latency histogram does not move.
6. Benchmark tables per section 7, with the generated-from-results script committed.
7. `go test -race ./...` and lint clean; Linux and Windows both pass.
8. README: architecture, the single-threaded-engine rationale, wire protocol summary, WAL/recovery diagram, quick start, benchmark tables, known limitations (no STP, no replication, day orders as GTC).

## 9. Roadmap (build in this order; stop after each)

1. **Skeleton + book core**: module, config, `internal/book` with price levels and FIFO queues, add/cancel/match for limit and market; `internal/book/ref`; differential harness with seeded streams; invariant checks. This is the heart; get it right before anything else.
2. **Engine loop + events**: single goroutine, instrument registry, event types, in-memory only. Allocation tests.
3. **Wire protocol + gateway**: framing, encode/decode with allocation benchmarks, TCP listener, reader/writer goroutines, backpressure, slow-consumer policy. `engctl connect/order/cancel`. End-to-end over loopback.
4. **WAL + sequencer**: record format, CRC, segments, fsync policies, ack gating, deterministic timestamps. Tests for torn tail and corruption.
5. **Snapshot + recovery + crash harness**: manifest, atomic snapshot, replay, `replay-check`, the SIGKILL test. Acceptance 2.
6. **Market data**: md_seq, snapshot-then-deltas, ring buffer, resend, gap test. `engctl book/subscribe`.
7. **Metrics + polish**: `/metrics`, `/healthz`, admin endpoint, graceful shutdown, Makefile, Linux CI-style check.
8. **IOC / FOK** (small; FOK needs a liquidity pre-check without mutating the book). Extend differential tests.
9. **Benchmarks + write-up**: loadgen per section 7, interleaving script, results, generator script for the README table, README.

If milestone 1's differential test is not clean after two days, stop and tell me; everything else depends on it.

**Scope note.** This is larger than the LLM Gateway. If the subscription window is at risk, the cut order is: milestone 8 (IOC/FOK) first; then market-data resend (keep snapshot-then-deltas and gap *detection*, drop `Resend`); then the Linux pass (keep Windows only, state it). Never cut: differential testing, WAL + recovery + crash test, the benchmark protocol.

## 10. Open questions to raise with me at the relevant milestone

- Price level storage: sorted slice of levels, skip list, or red-black tree? Which is most defensible for the access pattern (best-level hot, inserts near best)?
- Order lookup for cancel: map from order_id to a linked-list node, or an indexed slab?
- Group commit window default (1 ms?) and how acks are released.
- Should the matching-core latency histogram be HDR-style with fixed buckets, or sampled?
- md ring buffer size and whether it's per instrument.

## 11. Post-build review process

### 11.1 Tagging
- When milestone 9 is complete and all tests pass, tag `v0.1-unreviewed`. After all review findings are resolved, tag `v0.1-reviewed`. Create neither tag until told.

### 11.2 Responding to a review
A separate reviewer session writes `docs/reviews/full-review.md` with numbered findings (`F-001`, …). I will strike some out. When told to respond:
1. Read the whole file. Do not act on struck findings.
2. Under each remaining finding append: `- **Builder response:** Accepted — fixed in <sha>: <one line>` or `Rejected — <technical reason citing code, test, or PRD section>`. "Works as intended" is not a reason.
3. Fix accepted findings Critical → Should-fix → Nit, one commit per finding or tight group, message `fix(review): F-00N <title>`.
4. If a fix changes behaviour covered by a learning doc or decision record, update it in the same commit. If a finding shows a doc or README claim was wrong, correct the claim even if the code stands, and say so.
5. Run `go build ./...`, `go vet ./...`, `go test -race ./...`, lint. Report results. Stop. Do not tag.
After the reviewer's follow-up round I will rule on disputed findings; apply rulings the same way, then wait for the instruction to tag.

### 11.3 Interview follow-ups
An interviewer session writes `docs/interviews/<date>.md` (gitignored) with ranked gaps. When asked to expand a learning doc on a topic: expand only that section; add at least one new concrete example or "what breaks if" scenario; add two new self-check questions to that doc's `<details>` block; commit as `docs(learn): expand <file> on <topic>`.
