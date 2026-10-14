# matchd

matchd is a limit order matching engine written in Go: the core of an exchange, as one process. Clients connect over TCP with a compact binary protocol, send orders and cancels, and receive acknowledgements, fills and a sequenced market-data feed. Every accepted command is written to a write-ahead log before anything about it is sent, and the engine's state is a pure function of that log, so a crash is recovered by replay to the same book, sequence numbers and fills. It is meant for anyone who wants to see what an exchange core does and what it costs, measured, and for embedding a deterministic matcher in a trading simulator. It is not a production exchange: it has no risk checks, accounts, authentication or replication (see [Known limitations](#known-limitations-and-open-observations)).

## Features

- **Matching:** one book per instrument, by price-time priority. Limit orders (GTC; Day, accepted and treated as GTC; IOC; FOK), market orders, and cancels scoped to the owning client. A client order id is unique among that client's live orders on every instrument.
- **Instruments** are given at startup or added at runtime over HTTP. Prices and quantities are integer ticks and lots, with bounds that keep every sum in the book from overflowing.
- **Write-ahead log** with a CRC32C per record, and three fsync policies (`always`, `group:<window>`, `never`). No answer leaves the process before its command is durable.
- **Snapshots** taken without pausing the engine (copy-on-write), recovery from the newest snapshot plus the WAL, and an independent replay check (`engctl replay-check`).
- **Binary TCP protocol** with a hand-written codec: no reflection, and no allocation on the hot path. Clients that can't keep up are disconnected rather than allowed to stall the engine.
- **Market data:** per-instrument book deltas and trades with a gap-free `md_seq`, snapshot-then-deltas for new subscribers, gap detection, and `Resend` from a ring buffer.
- **Operations:** Prometheus metrics, `/healthz`, a log line per command at debug level, and graceful shutdown.
- **Testing:** differential testing against a deliberately simple reference book, and a crash harness that kills the real binary 50 times and checks every acknowledged command and fill.
- **Benchmarks:** a harness that measures matching-core and end-to-end latency and throughput under a fixed protocol, and generates this README's tables from committed raw results.

## Quick start

Go 1.27 or later; no other dependencies.

```sh
git clone https://github.com/tanxena/matchd && cd matchd
go build -o bin/ ./cmd/...

# terminal 1: the engine, one instrument XYZ (tick 1, lot 1)
bin/engine -data ./data -instrument XYZ:1:1    # TCP 127.0.0.1:9000, HTTP 127.0.0.1:9100

# terminal 2: market data for XYZ (a snapshot, then deltas and trades)
bin/engctl subscribe XYZ

# terminal 3: rest a sell, then cross it with a buy
bin/engctl order --client 7 --instrument XYZ --side sell --type limit --px 101 --qty 5
bin/engctl order --client 8 --instrument XYZ --side buy --type limit --px 101 --qty 2
bin/engctl book XYZ
```

The buy is acknowledged and filled against the resting sell:

```
Ack seq=2 order=1 clordid=… inst=1 sell 5 @ 101
Ack seq=3 order=2 clordid=… inst=1 buy 2 @ 101
Fill seq=3 match=1 order=2 clordid=… taker buy 2 @ 101 leaves=0
```

and the subscriber sees the trade and the level change (`Trade md=… 2 @ 101 (aggressor buy)`, then `BookDelta … sell 101 -> qty 3`). More:

```sh
bin/engctl add-instrument --symbol ABC --tick 5 --lot 100
bin/engctl instruments
curl 127.0.0.1:9100/metrics
bin/engctl connect --client 7                   # interactive; type "help"
```

Ctrl-C shuts the engine down gracefully: clients receive every answer, a snapshot is written, and the next start replays nothing. With the engine stopped, `bin/engctl replay-check --data ./data` rebuilds the newest snapshot independently from the WAL and checks the fill log against it. On Windows the binaries are `bin\engine.exe` and `bin\engctl.exe`.

## Architecture

Adapted from [PRD section 4](PRD.md#4-architecture):

```
clients (engctl, bench/loadgen, internal/client)
     |  TCP, length-prefixed binary frames
     v
+-------------------------------------------------------------+
| gateway     framing, decode, login, per-connection writer   |
|     |                                                       |
|     v  one ordered command channel                          |
| sequencer   assigns seq + timestamp, appends to WAL, fsync  |
|     |                                                       |
|     v                                                       |
| engine      one goroutine; owns all books; no locks inside  |
|   ├─ book[instrument]  price levels, FIFO queues            |
|   └─ matcher           price-time priority                  |
|     |                                                       |
|     v  events (ack, fill, cancel-ack, reject, book delta)   |
| publisher   holds each event until its command is durable,  |
|             then fans out to clients and market-data subs   |
| snapshot    copy-on-write book snapshots; recovery          |
| metrics     /metrics, /healthz, /instruments on HTTP        |
+-------------------------------------------------------------+
     disk: <data>/{wal/*.log, snapshots/*.snap, manifest.json, fills/*.log}
```

All books belong to one goroutine, which takes commands off a channel one at a time ([decision 005](docs/decisions/005-engine-loop.md)). Nothing else touches a book, so the matching path has no locks, and the order of execution *is* the sequence order: replaying the log reproduces every fill exactly. A command takes 1–2 µs in the engine at the median (tables below) while each WAL fsync takes milliseconds, so the cost is one core for all instruments, and the rest of the pipeline (gateway, sequencer, WAL, publisher) runs on its own goroutines.

How one order travels this path, step by step, is in the [guide's order flow](docs/guide/README.md#how-an-order-flows-from-socket-to-fill-to-market-data-event). The [guide](docs/guide/README.md) explains each part, and the [decision records](docs/decisions/) say why each choice was made and what it cost.

### Wire protocol

Every frame is a 4-byte little-endian length, then a version byte (1) and a type byte, then a fixed-layout payload ([decision 008](docs/decisions/008-wire-protocol.md)).

| message | code | direction | payload bytes |
|---|---|---|---:|
| Login | 0x01 | in | 4 |
| NewOrder | 0x02 | in | 31 |
| Cancel | 0x03 | in | 12 |
| Subscribe | 0x04 | in | 4 |
| Heartbeat | 0x05 | both | 0 |
| Resend | 0x06 | in | 12 |
| LoginAck | 0x81 | out | 4 |
| Ack | 0x82 | out | 53 |
| Reject | 0x83 | out | 37 |
| Fill | 0x84 | out | 70 |
| CancelAck | 0x85 | out | 54 |
| BookDelta | 0x86 | out | 45 |
| Snapshot | 0x87 | out | 16 (then one SnapshotLevel per level) |
| Trade | 0x88 | out | 53 |
| SnapshotLevel | 0x89 | out | 21 |

Prices are integer ticks (a limit price at most 2^53) and quantities integer lots (at most 2^30), so no sum in the book can overflow ([decision 003](docs/decisions/003-integer-domain-and-reject-semantics.md)). Every outbound message carries its command's sequence number and timestamp.

- A connection logs in as one client id, and the gateway attaches that id to every command, so clients can't act for each other.
- A client that sends nothing for 60 s is disconnected.
- A client that can't keep up with its own events is disconnected as a slow consumer, judged by time rather than by a momentary burst ([decision 022](docs/decisions/022-slow-consumers-and-durability-stalls.md)).
- Market-data requests need no login, so each connection's are rate-limited (a burst of 32, then 16 per second), and a Resend is answered with at most 4,096 events ([decision 016](docs/decisions/016-market-data.md)).

### Market data

Per instrument: book deltas (market by price) and trade prints, each with a gap-free `md_seq` assigned by the engine ([decision 016](docs/decisions/016-market-data.md)).
- A subscriber first gets a snapshot of the book, then every delta after it, with no gap or overlap.
- A consumer that sees `md_seq` jump sends `Resend`, answered from a 65,536-event ring per instrument. Beyond the ring, it subscribes again for a fresh snapshot.
- `internal/client.Feed` does all of this. A test joins mid-stream, drops events to induce gaps, and converges to the same book as a subscriber from the start.

### Observability

`/metrics` serves Prometheus text format: commands by type and outcome, fills, matching latency, WAL fsync latency, WAL bytes, snapshot age, connections, slow-consumer disconnects and market-data resends ([decision 017](docs/decisions/017-metrics-and-clock.md)). A stalled disk shows as `matchd_wal_fsync_stalls_total` (fsyncs slower than `-fsync-stall`, default 25 ms) and `matchd_wal_fsync_in_progress_seconds` ([decision 022](docs/decisions/022-slow-consumers-and-durability-stalls.md)). `/debug/latency` serves the matching latency as a log-linear histogram precise to 3.1%, which the benchmark reads ([decision 021](docs/decisions/021-benchmark-harness.md)).

- **Units:** latency histograms are named `_seconds` and labelled in seconds, as Prometheus expects. Their bucket bounds are defined in microseconds (0.25, 0.5, 1, 2, 5 … µs): `le="2.5e-07"` is 0.25 µs.
- **Clock:** latencies are timed with `internal/clock`, not `time.Now`, which on Windows advances only every ~515 µs on this machine. On Windows with an invariant TSC it reads the CPU's timestamp counter, calibrated against QueryPerformanceCounter: 13.5 ns per read in the clock-overhead table. Without one it uses QPC, measured at 78–179 ns per read from Go. On Linux it uses Go's monotonic clock. The source and its cost are logged at startup and exported as `matchd_clock_info` and `matchd_clock_read_cost_seconds`.

## Durability and recovery

```
command ─▶ sequencer ─▶ WAL segment ─▶ fsync ─▶ durable seq ─▶ publisher releases its answers
                         00000000000000000001.log      (always: fsync each batch before the engine sees it;
                         00000000000000004097.log       group:1ms: engine runs ahead, answers wait for the fsync)
                         …

restart:  manifest ─▶ newest snapshot ─▶ WAL records after its seq ─▶ engine (replayed; events discarded)
          (on damage: the older snapshot + the WAL from there)
```

- **The WAL** ([decision 011](docs/decisions/011-wal-format.md)):
  - segments of 64 MiB, named by their first sequence number;
  - each record is a length, a CRC32C, the seq, the timestamp and the encoded command;
  - recovery truncates a torn tail and refuses anything else damaged, including a gap in sequence numbers.
- **Fsync policies** ([decision 012](docs/decisions/012-durability-and-fsync-policies.md)):
  - `always` (default): each batch is fsynced before it reaches the engine;
  - `group:1ms`: commands reach the engine at once, a syncer fsyncs at most every millisecond, and answers are held until their fsync;
  - `never`: tests only.

  **Under `always` and `group`, no answer leaves the process before its command is fsynced to disk**, so a power failure loses nothing acknowledged. **Under `never`, no answer leaves before its command is written to the operating system**, which a power failure can lose. The benchmark measures what each policy costs.
- **Snapshots** ([decision 013](docs/decisions/013-copy-on-write-snapshots.md)): by default every 10 s while commands arrive, or every 1,000,000 commands, and on shutdown. The engine freezes its books between two commands and hands a copy-on-write view to a writer goroutine. The engine carries on, copying only the parts it changes. Two snapshots are kept, and WAL segments covered by both are deleted.
- **Replay check** ([decision 014](docs/decisions/014-fill-log-and-replay-check.md)): `engctl replay-check --data DIR` rebuilds the newest snapshot independently, from the older one plus the WAL, and checks every logged fill against the replay. The fill log is an audit trail, written by the publisher goroutine and never fsynced. That goroutine touches disk in two places only. After each release batch that contains a match, it makes one `write` into the page cache. When the current 64 MiB segment is full, it closes it, creates the next one and writes its 8-byte header; on Linux it then fsyncs the fill-log directory once (on Windows that call is a no-op). Pruning old fill-log segments runs on the snapshot writer's goroutine, not the publisher's.

**What the crash test proves, and what it doesn't** ([decision 015](docs/decisions/015-crash-test.md)):
- **What it does:** it runs the real binary under load and kills it with `SIGKILL` / `TerminateProcess` 50 times (`make test-long`; 5 times in the normal suite).
  - **After each kill** it checks:
    - no acknowledged seq is beyond the WAL's end;
    - every acknowledged command still in the WAL is the command the client sent;
    - no fill was delivered twice;
    - every received fill after replay-check's base is the one replay produces;
    - recovery reaches the same state as replaying the WAL independently.
  - **After the last run** it checks every acknowledged command and every received fill, from all runs, against a replay of the whole history from seq 1. Pruned segments are kept aside for this with `-archive`.
  - **Every fifth run** it leaves a torn record at the WAL's end, which the next start must cut off.
- **What it doesn't:** killing a process leaves the operating system's page cache intact, so data the engine wrote but never fsynced still reaches the disk. **The crash test therefore validates recovery logic (replay, determinism, sequence continuity, and torn tails, which the harness injects since kills alone don't produce them), not power-loss durability under any fsync policy.** It would pass with `fsync=never`, and every third round deliberately runs with `never` to show it. Torn tails at every byte offset are also covered by unit tests (`internal/wal`).
- **How fsync behaviour is validated instead:**
  - a test that wraps the WAL file, holds or fails its fsync, and shows no answer leaves before the fsync, under both `always` and `group`;
  - the throughput difference between the policies, in the benchmark.

### How correctness is checked

- **Differential testing** ([guide 01](docs/guide/01-book-core.md)): the book is compared, result by result, with a deliberately simple reference book on random command streams: 10,000 commands per seed over 10 seeds in the normal suite, and 1.2 million commands over 20 seeds in `make test-long`, with zero divergence. The streams include edge quantities and prices. A deep-book run takes the book past 11,000 resting orders, as deep as the benchmark's, and another spreads the stream over two instruments that share client order ids.
- **Crash recovery:** the kill-and-recover runs above.
- **No allocation** on the engine's steady-state path for limit add, cancel and match, proven by test ([decision 007](docs/decisions/007-allocation-testing.md)).
- **Market data:** a late joiner converges through an induced gap.
- **A subscriber that stops reading** is disconnected, and the engine's latency histogram doesn't move.
- **Planted bugs:** each milestone planted bugs to show its tests fail when they should (listed in each decision record).
- **Race detector and lint:** `go test -race ./...` and `golangci-lint` are clean on Windows (`make ci`). On Linux, every binary and test binary cross-compiles, but the suite has not yet run there (see [Development process](#development-process)).

## Benchmarks

**The headline tables (order mix and fsync policy) run with snapshots off**, so that they show matching and the WAL without the end-to-end stall that snapshots cause ([observation 002](docs/bugs/002-snapshot-runs-stall-the-wal.md)). The engine's default takes a snapshot every 10 s; for that configuration, read the snapshot table's "on" row.

All benchmark tables are Windows 11 on one laptop, the development machine, stated in full below. There are no Linux benchmark numbers. Every number in this section is generated by [`bench/report`](bench/report) from the per-run JSON files in [`bench/results/`](bench/results), written by the harness in [`bench/`](bench/README.md) under the protocol of PRD section 7 ([decision 021](docs/decisions/021-benchmark-harness.md)). None of them is typed by hand.

**Two commits.** The main suite (`bench/results/full`) ran at commit `73e6c8d`, before the review fixes. The Balanced suite (`bench/results/balanced`) was re-run at `449ab7e`, after them; its result files record the commit as `449ab7e…-dirty`, since the working tree had uncommitted changes. The generated protocol line below names only the main suite's commit. The 26 commits between them change code on the measured path, including the book's client-order-id registry (review F-003) and the engine. So the comparison of the two plans is not the power plan alone.

### How to read the tables

- **Two latencies, never mixed.**
  - *Matching-core* is the time inside the engine, from taking a command off its channel to sending its last event.
  - *End-to-end* is the time at the client, from its scheduled send to the first answer, over loopback TCP. It includes the WAL fsync, which every answer waits for.
- **Open loop and closed loop.**
  - The mix, snapshot and Balanced tables send at a fixed 20,000 commands/s, well below saturation. Latency is measured from each command's *scheduled* send time, so a late load generator can't hide its own delay.
  - The fsync table keeps a fixed number of commands in flight (closed loop) to find sustained throughput. Its end-to-end latency is therefore mostly queueing.
- **Core latency at a fixed rate is not the engine's best case.** At 20,000 commands/s the engine idles between commands, so its caches cool and the CPU can clock down. Each table states its load.
- **End-to-end latency is dominated by the disk.** Each answer waits for its WAL fsync, and an fsync on this laptop's SSD takes milliseconds: see the "fsync mean" column of the end-to-end mix table (at 20,000 commands/s) and of the fsync table (at saturation). That is the price of acknowledging nothing before it is durable, not of matching.
- **Snapshots cost the tail, not the engine.** In the snapshot table, core latency is about the same with snapshots on and off: copy-on-write keeps the engine out of it. End-to-end p99.9 is far higher with snapshots on, and 0.89–0.99% of commands in those runs waited more than 50 ms (from the runs' end-to-end histograms; none did with snapshots off). Why is an open question: [observation 002](docs/bugs/002-snapshot-runs-stall-the-wal.md) records a separate reproduction in which WAL fsyncs stalled a few seconds after a snapshot, which fits but is not verified. The committed result files don't record fsync stalls; the load generator records them from `13875f7` on.
- **Balanced versus High performance:** at this load, the Balanced suite's matching core is slower at every quantile: median 2.11 µs against 1.47, p99 18.9 against 15.1, p99.9 30.7 against 26.1. End to end, the two are close, since the fsync dominates: median 3.6 ms against 3.3. The suites ran different commits (above). All 5 Balanced latency runs are clean. Its clock row counts 4 of 5 runs: one clock-probe run was disturbed by Defender (`MsMpEng.exe`) and is left out, listed under the tables.
- **Quantiles are bucket upper bounds:** at most 3.1% above the true value, never below. Cells are the median (min–max) across runs, never the best run.

<!-- bench:begin: generated by bench/report from bench/results; do not edit by hand -->

**Hardware and environment** (stated once; every table below ran on it): Intel(R) Core(TM) i7-8650U CPU @ 1.90GHz, 4 cores / 8 threads, 8 MB L3 shared by logical CPUs 0–7, 15.8 GB RAM; Windows 11 Pro 26H2 (build 26300); go1.27.0; High performance plan on AC (the balanced suite: Balanced plan, power mode best power efficiency on AC); engine pinned to CPUs 0–3 and load generator to 4–7 (no shared physical core, verified from the OS's processor topology; they share the L3); Microsoft Defender exclusion on the benchmark data directory (%TEMP%\matchd-bench) (stated by the operator; the harness can't check it); nothing else running (measured: see below).

**Nothing else running,** as measured: other processes used 0.04 logical CPUs on average per measured window (median across runs), 0.17 at most (36-snapshot-snapshots-off-r4, mostly WindowsPackageManagerServer.exe); a run over 0.35 is excluded as disturbed.

**Pinning** with the process affinity mask (as `start /affinity` sets it), read back after setting.

**Protocol** (PRD 7): a fresh engine per run; 30s warmup discarded, 1m0s measured; each configuration run 5 times, interleaved round by round with the other configurations, with a cooldown between runs. Cells are the **median (min–max) across runs**, never the best run. Commit `73e6c8d5eea7`.

#### Clock overhead

Back-to-back reads of `internal/clock`, the engine's hot-path clock (decision 017), by `bench/clockprobe` pinned to the engine's CPUs: the cost each read adds to a timed span, and what timing zero work measures.

| suite (plan) | source | cost per read (ns) | resolution (ns) | zero-work span p50 (ns) | p99.9 (ns) | max (µs) | reads per run | runs |
|---|---|---|---|---|---|---|---|---|
| balanced (balanced) | tsc | 13.6 (13.5–13.6) | 11 | 13 | 15 | 135 (28.2–246) | 20,000,000 | **n=4** of 5† |
| full (high performance) | tsc | 13.5 (13.3–13.6) | 11 | 13 | 15 (15–18) | 123 (88.1–336) | 20,000,000 | 5 |

#### Matching-core latency by order mix

PRD 7.1(a). Open loop at 20,000 commands/s (all connections), fsync `group:1ms`, **snapshots off**: the engine's default takes one every 10 s, and its end-to-end tail is then the snapshot table's "on" row (observation 002). The load generator aims to hold the book at 10,000 resting orders; how many it actually held at the end of each window is in the "book orders" column: cancel-heavy settles lower, because its cancels alone nearly balance what it adds. Microseconds.

| mix | count per run | p50 | p99 | p99.9 | max | mean | book orders | runs |
|---|---|---|---|---|---|---|---|---|
| resting¹ | 1,200,188 | 1.47 (1.09–1.53) | 15.1 (14.8–15.4) | 26.1 (24.6–26.1) | 221 (180–258) | 2.42 (2.13–2.42) | 10,104 (10,090–10,115) | 5 |
| crossing¹ | 1,200,203 | 1.76 (1.47–1.82) | 9.98 (9.73–10.2) | 17.9 (15.1–17.9) | 193 (172–262) | 2.11 (1.98–2.16) | 8,398 (8,396–8,406) | 5 |
| cancel-heavy¹ | 1,200,188 | 1.76 (1.28–1.79) | 9.21 (8.70–9.47) | 14.3 (13.6–15.1) | 188 (125–352) | 1.97 (1.75–2.04) | 6,816 (6,797–6,828) | 5 |

¹ Timed inside the engine with `internal/clock` (source `tsc`; one read costs 13.5 (13.3–13.6) ns, measured by `clockprobe` on the engine's CPUs (the clock-overhead table)): from taking the command off the engine's input channel to sending its last event. The span includes one clock read and one histogram update (the matching step's own timing) in between. Quantiles and max come from a log-linear histogram (`GET /debug/latency`) and are at most 3.1% above the true value, never below.

#### End-to-end latency by order mix

PRD 7.1(b), the same runs: client send to client receipt of the command's first answer (Ack, Reject or CancelAck) over loopback TCP, measured from each command's *scheduled* send time, so the load generator falling behind counts against it. Microseconds. Every answer waits for its WAL fsync (`group:1ms`). **Snapshots are off here. With the engine's default (one every 10 s) the tail is far worse: see the snapshot table.**

| mix | count per run | p50 | p99 | p99.9 | max | send lag p99 | fsync mean (µs) | engine CPU | loadgen CPU | runs |
|---|---|---|---|---|---|---|---|---|---|---|
| resting | 1,200,006 | 3342 (3211–3342) | 19399 (16515–19923) | 23069 (22544–23069) | 27787 (26739–31982) | 16.9 (15.6–19.5) | 1381 (1354–1655) | 1.23 (1.09–1.28) | 1.17 (1.16–1.19) | 5 |
| crossing | 1,199,994 | 3277 (3146–3342) | 19923 (18350–20447) | 24117 (23593–24642) | 29360 (29360–31982) | 16.1 (13.1–18.4) | 1375 (1338–1582) | 1.23 (1.13–1.28) | 1.17 (1.16–1.19) | 5 |
| cancel-heavy | 1,200,000 | 3473 (2949–3604) | 20447 (17302–20972) | 24117 (22544–24642) | 29884 (28312–31457) | 13.1 (12.0–19.5) | 1338 (1327–1481) | 1.24 (1.09–1.27) | 1.14 (1.14–1.15) | 5 |

CPU columns are logical CPUs busy on average over the window. The load generator spins on the clock to keep its schedule, so it always uses at least one.

#### Throughput and end-to-end latency by fsync policy

PRD 7.2 and 7.6. Closed loop: 4 connections × 64 commands in flight, resting mix, snapshots off. Throughput is commands (orders and cancels) answered per second over the window; end-to-end latency here is at saturation, so it is mostly queueing (in flight ÷ throughput). Matching-core latency doesn't depend on the fsync policy (PRD 7.1), so it isn't given per policy. Latency in microseconds.

| fsync | commands/s | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max | fsyncs/s | commands per fsync | fsync mean (µs) | engine CPU | loadgen CPU | runs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| `always` | 32,411 (29,697–32,854) | 1,944,646 | 8651 (8651–8913) | 14680 (14418–17826) | 23593 (23069–32506) | 35652 (32506–47186) | 268 (243–273) | 121 (120–123) | 3629 (3565–4015) | 1.49 (1.33–1.51) | 0.75 (0.65–0.78) | 5 |
| `group:1ms` | 53,777 (51,653–54,947) | 3,226,613 | 3998 (3932–4325) | 12059 (11796–12059) | 17302 (16515–19399) | 34603 (28836–35652) | 245 (223–249) | 221 (220–231) | 2688 (2586–2807) | 0.77 (0.70–0.82) | 0.28 (0.26–0.32) | 5 |
| `never` | 67,590 (66,597–71,712) | 4,055,398 | 3736 (3539–3801) | 5898 (5505–6029) | 7864 (6816–10486) | 40894 (24117–55575) | – | – | – | 3.78 (3.78–3.80) | 1.95 (1.92–1.98) | 5 |

`never` is for tests only: acknowledged work can be lost on power failure (decision 012). It has no fsync on the commit path, so its fsync columns are blank. The only fsyncs it does are WAL segment rolls, each flushing a 64 MiB segment.

#### Snapshots: every 10 s vs none

PRD 5.4: the copy-on-write snapshot's effect on the tail. Resting mix, open loop at 20,000 commands/s, fsync `group:1ms`, snapshots on and off alternating. Microseconds.

| snapshots | core count per run¹ | core p50 | core p99 | core p99.9 | core max | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max | snapshots per run | runs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| on | 1,200,175 | 1.34 (1.09–1.44) | 15.4 (15.1–15.4) | 26.6 (25.6–27.1) | 213 (143–311) | 1,200,008 | 3277 (3211–3342) | 34603 (31982–49283) | 163578 (155189–184549) | 184549 (176161–213910) | 6 | 5 |
| off | 1,200,181 | 1.12 (1.05–1.63) | 15.1 (15.1–15.6) | 25.6 (24.6–26.6) | 242 (209–1180) | 1,200,005 | 3211 (3146–3342) | 17826 (14680–19923) | 22544 (21496–23069) | 29884 (27787–33554) | 0 | 5 |

¹ Timed inside the engine with `internal/clock` (source `tsc`; one read costs 13.5 (13.3–13.6) ns, measured by `clockprobe` on the engine's CPUs (the clock-overhead table)): from taking the command off the engine's input channel to sending its last event. The span includes one clock read and one histogram update (the matching step's own timing) in between. Quantiles and max come from a log-linear histogram (`GET /debug/latency`) and are at most 3.1% above the true value, never below.

#### The same resting row under the Balanced plan

The mix table's resting row, and the same configuration run under the Balanced plan with the power mode at "best power efficiency": this laptop's own setting at the time, which is not Windows' default (the default power mode is "Balanced"). It ran on its own, after the main suite, since a plan can't be switched mid-suite. Microseconds.

| plan | core count per run¹ | core p50 | core p99 | core p99.9 | core max | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max | clock read (ns) | runs |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| High performance (mix table) | 1,200,188 | 1.47 (1.09–1.53) | 15.1 (14.8–15.4) | 26.1 (24.6–26.1) | 221 (180–258) | 1,200,006 | 3342 (3211–3342) | 19399 (16515–19923) | 23069 (22544–23069) | 27787 (26739–31982) | 13.5 (13.3–13.6) | 5 |
| Balanced, best power efficiency | 1,200,036 | 2.11 (1.38–2.11) | 18.9 (17.9–21.0) | 30.7 (29.2–34.8) | 311 (193–1343) | 1,199,997 | 3604 (3211–3932) | 19923 (13631–19923) | 22544 (20447–23593) | 29360 (26739–29360) | 13.6 (13.5–13.6) | 5 |

† Runs left out of the numbers (failed, or disturbed by the checks in `bench/internal/result/assess.go`):

- `balanced/01-clock-clock-r1`: disturbed: other processes used 1.19 CPUs on average (limit 0.35): MsMpEng.exe 1.15, csrss.exe 0.01, svchost.exe 0.01, svchost.exe 0.01

<details><summary>Exact commands for every row</summary>

Each suite's runs were started by `benchrun`, which pins each process with the same affinity mask `start /affinity` takes. To repeat one run by hand from cmd.exe, start the engine, then the load generator with `-engine-pid` set to the engine's process id (or 0 to skip its CPU time). Each run's data path is shown relative to the parent of `benchrun`'s data root: `matchd-bench\<run>\…`, where the default root is `%TEMP%\matchd-bench`.

Suite **balanced**, started with `bin\benchrun.exe -suite balanced -results bench/results/balanced`:

**balanced / balanced / resting**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\02-balanced-resting-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 20000 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**balanced / clock / clock**

```
set "GOMAXPROCS=4" && start "clockprobe" /affinity 0F bin\clockprobe.exe -reads 20000000 -out matchd-bench\03-clock-clock-r2\clock.json
```

Suite **full**, started with `bin\benchrun.exe -suite full -results bench/results/full`:

**full / clock / clock**

```
set "GOMAXPROCS=4" && start "clockprobe" /affinity 0F bin\clockprobe.exe -reads 20000000 -out matchd-bench\01-clock-clock-r1\clock.json
```

**full / fsync / always**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\02-fsync-always-r1\data -fsync always -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 0 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / fsync / group:1ms**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\03-fsync-group_1ms-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 0 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / fsync / never**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\04-fsync-never-r1\data -fsync never -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 0 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / mix / resting**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\05-mix-resting-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 20000 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / mix / crossing**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\06-mix-crossing-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix crossing -rate 20000 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / mix / cancel-heavy**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\07-mix-cancel-heavy-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix cancel-heavy -rate 20000 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / snapshot / snapshots-on**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\08-snapshot-snapshots-on-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 10s
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 20000 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

**full / snapshot / snapshots-off**

```
set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data matchd-bench\09-snapshot-snapshots-off-r1\data -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 0
set "GOMAXPROCS=4" && start "loadgen" /affinity F0 bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -engine-pid 0 -mix resting -rate 20000 -window 64 -conns 4 -depth 10000 -warmup 30s -duration 1m0s -seed 1 -out run.json
```

</details>

<!-- bench:end -->

## Known limitations and open observations

- **No self-trade prevention.** A client's buy can trade with its own sell.
- **No replication or failover.** It's a single process. Durability is the local disk's, under the fsync policy chosen.
- **Day orders behave as GTC.** There are no trading sessions, so nothing expires them.
- **IOC and FOK are limit-only.** There is no market IOC or FOK.
- **With snapshots on, WAL fsyncs stall and the end-to-end tail suffers** ([observation 002](docs/bugs/002-snapshot-runs-stall-the-wal.md)). The cause on the disk side is not established, and nothing is fixed.
- **A race-detector failure was seen once and never reproduced** ([observation 001](docs/bugs/001-unreproduced-server-race-failure.md)). `make test-race-loop` repeats one package under `-race` to look for it.
- **Linux is untested.** Every binary and test binary cross-compiles for Linux, but the suite has only run on Windows. The Linux-only code paths (directory fsync, the monotonic clock, `SIGKILL` in the crash harness, the TCP reset check) are compiled but not exercised. Benchmark numbers exist for Windows on one laptop only.
- **No market-data heartbeat.** A consumer detects a gap only when a later `md_seq` arrives, so a lost *final* message, or a Resend refused by the rate limit that is waiting for a later message to retry on, stays unnoticed until the instrument next changes. Over TCP single frames aren't lost (a stalled consumer is disconnected instead), so this matters only after a bug or a refused Resend ([decision 016](docs/decisions/016-market-data.md)).
- **No order-status query.** A client that disconnects loses the events sent while it was away; its orders stay on the book.
- **No risk checks, accounts, auctions, stop, iceberg or pegged orders** (PRD non-goals). There is no TLS or authentication: the gateway and the HTTP side are for a trusted local network, and `/instruments` lets anyone with access add instruments. There is no limit on connections either: many connections each making their full allowance of market-data requests could still keep the publisher busy.
- **One engine goroutine for all instruments:** one instrument's work can't use a second core, and one slow command delays every instrument.

## Development process

matchd was built with Claude Code, an AI coding agent, working from [`PRD.md`](PRD.md) with the author, one roadmap milestone at a time. Each milestone produced [decision records](docs/decisions/) for its design choices and a [guide](docs/guide/README.md) document explaining the code. A separate Claude Code session then reviewed the finished code against the PRD's acceptance criteria; its 20 findings, the responses and fixes, and the follow-up are in [`docs/reviews/`](docs/reviews/full-review.md). A GitHub Actions workflow ([`.github/workflows/ci.yml`](.github/workflows/ci.yml)) runs vet, lint, the race suite and the short crash test on `ubuntu-latest`; it has not run yet, because the repository has not yet been pushed. The process is described in [docs/process/build-process.md](docs/process/build-process.md).

### Building and testing

Go 1.27 ([decision 004](docs/decisions/004-toolchain.md)). The race detector needs cgo, so on Windows it needs gcc on `PATH` (for example MSYS2's `C:\msys64\ucrt64\bin`).

| target | does |
|---|---|
| `make build` | `bin/engine`, `bin/engctl` |
| `make test` | the suite with `-race`, including 5 kill-and-recover crash runs |
| `make test-long` | the 1M-command differential test and 50 crash runs |
| `make test-race-loop N=50` | one package (`PKG`, default `./internal/server/`) under `-race` N times, every run's output kept in `bin/race-loop.log` |
| `make lint` | `golangci-lint` |
| `make run` | an engine on `./data` with one instrument |
| `make bench` | every Go benchmark, with allocation counts |
| `make ci` | gofmt, vet (Windows and Linux), lint, Linux cross-compile of every binary and test binary, and the race suite |
| `make bench-smoke` | every benchmark configuration once, briefly (about 1.5–2 min), and a test report in `bin/bench-smoke/report.md`: checks the harness works |
| `make bench-full`, `make bench-balanced` | the benchmark suites, High performance plan (about 75 min) and Balanced (about 10 min); read [`bench/CHECKLIST.md`](bench/CHECKLIST.md) first |
| `make bench-report` | regenerate the benchmark tables above from `bench/results` |

On Windows, run `make` (MSYS2's `C:\msys64\usr\bin\make.exe`) from PowerShell, cmd or an MSYS2 shell, **not Git Bash**, where it starts with an empty environment ([decision 010](docs/decisions/010-windows-gotchas.md) §13).

## Layout

```
cmd/engine/          the server
cmd/engctl/          CLI client: order, cancel, connect, book, subscribe, instruments, add-instrument, replay-check
internal/proto/      wire protocol: frames, encode/decode
internal/gateway/    TCP listener, per-connection reader and writer, sessions
internal/sequencer/  seq and timestamp assignment, WAL append, fsync policies
internal/wal/        segmented log, CRC32C per record, replay, torn-tail handling
internal/engine/     the engine goroutine, events, instrument registry
internal/book/       order book: price levels, FIFO queues, matching; copy-on-write
internal/book/ref/   reference book, used only by tests
internal/publisher/  durability gate, fan-out, market data
internal/snapshot/   snapshots, manifest, fill log, recovery, replay check
internal/server/     wiring, HTTP side (/metrics, /healthz, /instruments), shutdown
internal/client/     client library, including the market-data Feed
internal/clock/      the hot-path clock
internal/metrics/    counters and histograms
internal/crashtest/  the kill-and-recover harness
internal/durable/    the durable-seq signal shared by the sequencer and publisher
internal/fsutil/     fsync and atomic-replace helpers per platform
internal/testutil/   test helpers (allocation checks)
internal/doccheck/   test that keeps the docs' code links accurate
bench/               load generator, clock probe, runner, report generator, results/
docs/decisions/      decision records
docs/guide/          guide to the design and code
docs/bugs/           open observations
docs/reviews/        the code review, responses and follow-up
docs/process/        how the project was built
```

## License

MIT; see [LICENSE](LICENSE).
