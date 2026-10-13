# 012 — Durability: what may leak before fsync, and the three fsync policies

- **Status:** accepted (milestone 4). The group-commit defaults, the batching behaviour of `always`, and full-log replay at startup were chosen with the user.
- **Code:** `internal/sequencer/sequencer.go`, `internal/durable/durable.go`, `internal/publisher/publisher.go` (`Run`, `release`, `withhold`), `internal/server/server.go`

## The rule (PRD 5.4 asks for it to be written down exactly)

> **No byte that depends on a command may leave the process before that command's WAL record has been fsynced.**

That covers acks, rejects, fills, cancel-acks and (from milestone 6) market data, to *any* client, including clients other than the sender. A maker's fill depends on the taker's order, so it waits for the taker's fsync.

**May happen before the fsync:**
- the command is stamped with its seq and time;
- it is written to the OS (page cache);
- under `group`, the engine applies it and computes its events. The book can be ahead of the disk by one batch; nothing observable is.

**May never happen before the fsync:**
- an event leaving the publisher;
- any other client learning the command's effect.

*(Milestone 7)* Metrics and the per-command debug log follow the same rule: commands are counted and logged by the publisher on release, never when the engine executes them ([decision 017](017-metrics-and-clock.md)).

**Not covered:** replies the gateway makes on its own (`LoginAck`, `Heartbeat`, and gateway rejects with seq 0). They don't depend on any sequenced command.

**How the rule is enforced: one gate, the same for all policies.** The publisher holds every event in a FIFO queue and releases it only when `durable.Seq` ≥ the event's `Seq`. Under `always` the sequencer advances `durable.Seq` before the engine even sees the command, so events pass straight through. Under `group` they wait for the syncer. Having a single gate means there is one place to get this right, and one place to test.

## The policies

| policy | order of operations | an ack means | lost on power failure |
|---|---|---|---|
| `always` (default) | stamp → write → **fsync** → apply → emit → release | the command is on disk | nothing acknowledged |
| `group:Nms` | stamp → write → apply → emit and **hold** → fsync (within N ms) → release | the command is on disk | nothing acknowledged; up to one window of unacknowledged, applied commands, which clients never heard about |
| `never` (tests only) | stamp → write → apply → emit → release | nothing about disk | anything still in the OS page cache, **including acknowledged commands** |

A process crash (`kill -9`) loses nothing under any of them: the OS still writes the page cache to disk. Only power loss or an OS crash distinguishes the policies. That's why the milestone 5 crash test can't validate fsync (PRD 5.4); the fsync-gating tests below, and the benchmark, do.

### `always`: batch what's already waiting (chosen with the user)

The sequencer takes every command already in its input channel, up to `MaxBatch` (4,096). It writes them in one `Write`, fsyncs **once**, and only then forwards them to the engine. No command is applied or acknowledged before it is on disk, so the rule holds exactly. But commands that arrived together share an fsync instead of paying for one each.

Strictly one fsync per command was the alternative. It caps throughput at 1 / fsync latency, and fsync measured about 1.5 ms on this laptop, so about 660 commands per second, with no gain in safety. The cost of batching: under load, a command may wait for the fsync of the batch it joined.

Measured: 1,000 commands queued behind a blocked fsync went through in **2 fsyncs**. Sequential round trips (each waiting for its ack) cost exactly **one fsync each** (`TestFsyncCounts`).

### `group:Nms`: 1 ms window, early at 4,096 (Section 10, chosen with the user)

- **Commands are forwarded to the engine as soon as they are written.** The engine never waits for disk; that is the point of group commit (PRD 5.4: "the engine must not stall per batch").
- **A separate syncer goroutine** waits for the sequencer's "dirty" signal, then waits out the **window (default 1 ms)**, then fsyncs once for everything written so far and advances `durable.Seq`.
- **The sequencer kicks the syncer early** once 4,096 commands are written but unsynced, so held events (and memory) don't pile up during a burst.
- **The publisher holds at most 65,536 events.** If that fills, it stops taking events, which back-pressures the engine.
- **On shutdown,** the sequencer stops the syncer and does a final fsync itself.

Effect on latency: an ack waits for the rest of the current window plus one fsync, so about 1 ms + 1.5 ms here. Measured: 500 pipelined orders took **4 fsyncs**.

### `never`

Writes but never fsyncs, and advances `durable.Seq` as soon as the write returns. It exists for tests and for the benchmark's baseline row. `cmd/engine` logs a warning when it is selected.

## Failure: fail-stop, never retry

If a write or fsync fails, `durable.Fail(err)` is called:
- `durable.Seq` never advances again;
- the publisher drops every held event and drains the rest of the engine's output without releasing anything (`Withheld` counts them);
- the sequencer stops;
- the server disconnects every client; `cmd/engine` logs the error and exits.

**Why not retry the fsync?** After a failed fsync, Linux may already have marked the dirty pages clean and thrown the data away. A retry then *succeeds* without writing it. That is what PostgreSQL learned in 2018 ([Fsync Errors](https://wiki.postgresql.org/wiki/Fsync_Errors)), and the behaviour is studied across file systems in Rebello et al., *[Can Applications Recover from fsync Failures?](https://www.usenix.org/conference/atc20/presentation/rebello)* (USENIX ATC 2020). The only safe response is to stop, and on restart rebuild from what is actually on disk.

**A consequence clients must understand: "not acknowledged" doesn't mean "didn't happen".** The failed batch was *written* to the OS before its fsync failed. If the data did reach the disk, restart replays those commands, and orders the client never got an ack for may exist and may have traded. This is the same ambiguity as a network timeout. v1 has no order-status query, so a client that saw its connection drop without an ack must treat the order's fate as unknown (decision 009 records the missing status query as a known limitation).

## Startup replays the whole log (chosen with the user)

Milestone 5 adds snapshots, manifests, `replay-check` and the crash harness. Milestone 4 already needed restarts to work, so `server.Start`:
1. opens the WAL;
2. replays every record through `engine.Step` into a fresh engine, discarding the events;
3. continues numbering from the last seq.

The engine's determinism ([decision 005](005-engine-loop.md)) is what makes this give exactly the pre-crash state; `TestRestartReplaysToIdenticalState` checks books, last order id and last seq.

Configured instruments are checked against the log: an existing symbol with a different tick or lot size is a startup error. New ones are written to the log through `Sequencer.LogNow`, which always fsyncs, so instruments get the same ids on every replay.

## How it's tested

A wrapper around every WAL file counts fsyncs and can **hold** them (block until released) or **fail** them (`internal/server/durability_test.go`, `internal/sequencer/sequencer_test.go`):

| test | shows |
|---|---|
| `TestAckGatedOnFsync/always` | with fsync held, the client receives nothing; release it and the ack arrives |
| `TestAckGatedOnFsync/group:1ms` | the same; and while held, the publisher is holding the engine's events |
| `TestFsyncFailureEmitsNothing` (both policies) | a resting maker, and a crossing taker whose batch's fsync fails: **no** ack for the taker and **no** fill for either; the server reports failure and closes both connections; under group, the 4 events computed for the batch are withheld |
| `TestFsyncCounts` | always, sequential: 20 orders = 20 fsyncs; group burst: 500 orders = 4 fsyncs |
| `TestAlwaysForwardsOnlyAfterFsync`, `TestAlwaysBatchesWaitingCommands` | sequencer-level ordering; 1,000 waiting commands, 2 fsyncs |
| `TestGroupBurstAndKick` | with a 1-hour window, the 4,096-style kick still makes data durable |
| `TestHeldUntilDurableThenReleasedInOrder`, `TestFailureWithholdsEverything`, `TestPendingLimitBackpressure` | publisher gate unit tests |

Planted bugs, each caught: advancing durability without fsync under `always`; a publisher that ignores the durable seq; a WAL that marks data synced before the fsync succeeds; a syncer that is never kicked; treating every damaged record as torn.

## Costs

- **Under `group`, the book runs ahead of the disk** by up to one window. A crash loses those applied-but-unacknowledged commands. Nobody was told about them, so nothing visible is lost, but their effects are visible to no one either.
- **Under `always`, everything stops for an fsync.** About 1.5 ms per batch here; the benchmark (milestone 9) measures the throughput difference between policies.
- **The publisher's hold queue** costs memory (up to 65,536 × 96 bytes ≈ 6 MB; 112 bytes, ≈ 7.3 MB, since milestone 6) and adds a queue operation to every event, even under `always`.
- **Fail-stop:** one bad disk write takes the whole engine down. That's correct for an exchange, and an operational burden.
