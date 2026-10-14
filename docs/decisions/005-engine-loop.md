# 005 — The engine loop: one goroutine, Step plus Run, ids and sequence rules

- **Status:** accepted (milestone 2)

> **Superseded in part (2026-10-12, `08e1425`):** `Run` now takes a third argument, `snap *SnapshotHooks`, and takes copy-on-write snapshots between commands ([decision 013](013-copy-on-write-snapshots.md)); with that and the latency recording added in milestone 7 ([decision 017](017-metrics-and-clock.md)) it is no longer about ten lines. The loop below is still its core.

- **Code:** `internal/engine/engine.go` (`Engine`, `Step`, `Run`, `newOrder`, `cancel`, `addInstrument`)

## Context

PRD section 4: "one goroutine consumes a totally ordered command stream and emits events... Concurrency lives at the edges, not in the book." This is the central design of the whole system. This record covers how the engine is structured, and the identity and sequencing rules it enforces.

## Decisions

### One goroutine owns everything, with no locks

`Engine` holds every book, the instrument registry, and the id counters. Only one goroutine ever touches it: the one running `Run`, or a test calling `Step`.

The alternatives were one goroutine per book, or a lock per book. They were rejected for these reasons:

- **Determinism.** With one goroutine and one input order, the output order is fully determined. With several goroutines, the order in which events from different books reach the publisher would depend on scheduling. Replay could then produce the same books but a differently interleaved event stream, and the crash test's "identical to a no-crash run" (PRD goal 2) would be harder to state and to prove.
- **Throughput is not lock-bound.** A matching step is a few hundred nanoseconds (see `docs/guide/02-engine.md`). An uncontended lock costs tens of nanoseconds, and a contended one far more. One core running without locks is the standard design, and it is fast enough that the network and disk, not matching, set the limits.
- **Simplicity.** Every invariant in `internal/book` assumes a single owner. Adding locks would mean re-proving all of them under interleaving.

**Cost:** a single book can use only one core. Spreading instruments across several engine goroutines would be a later, explicit change; PRD section 3 excludes it from v1.

### Step is a function; Run is a loop around it

`Step(c *Command, out []Event) []Event` contains the engine's entire behaviour, and it has no goroutines or channels. `Run(in <-chan Command, out chan<- Event)` is about ten lines:

```
for each command on in:   events = Step(command, events[:0]); send each event on out
when in is closed:        close out
```

**Why the split:**
- Nearly all tests call `Step` directly and inspect its output, with no timing or goroutines involved.
- `TestRunMatchesStep` then proves the goroutine form emits exactly what `Step` emits, under `-race`.
- WAL replay (milestone 5) calls `Step` directly too (`internal/snapshot/recover.go`).

### Events are sent one at a time, by value

`Run` sends each `Event` (96 bytes at the time, 112 since milestone 6; no pointers) on a `chan Event`. The alternative was sending one slice per command, which would need a pool of event buffers and a protocol for returning them. Otherwise the publisher could read a buffer while the engine overwrites it.

Sending by value can't share memory, and it doesn't allocate. The cost is one channel operation per event rather than per command. It is not measured on its own: it is part of the engine's per-command latency, which is read after the sends ([decision 017](017-metrics-and-clock.md)).

### Backpressure stops at the publisher

A send on `out` blocks when the publisher falls behind, and that stalls the engine. This is deliberate: the engine has no way to drop events without breaking the client or market-data contract.

It makes the publisher responsible for never blocking on a slow client: a per-client bounded buffer, and disconnecting a client that can't keep up (PRD 5.5; milestones 3 and 6; since [decision 022](022-slow-consumers-and-durability-stalls.md) judged by time rather than by a full queue). A slow client must never be able to slow the engine.

### Sequence numbers come in on commands; gaps panic

The sequencer (milestone 4) assigns `Seq` and `Time` before writing the command to the WAL. `Step` requires `c.Seq == lastSeq + 1`, and **panics** otherwise.

A gap or a repeat means the sequencer or the replay logic is broken. If the engine applied such a command anyway, its state would no longer be a function of the log, which defeats the whole durability design. A process that stops is recoverable; one that silently diverges from its log is not.

**Rejected commands still consume a sequence number.** The sequencer logs every command before the engine decides on it. That has to be so: the engine's decision depends on book state, which only the engine knows, so rejects must be replayed too, to reproduce the same reject events.

### Order ids are gap-free; only accepted orders consume one

`newOrder` offers the book `lastOrderID + 1`, and commits the counter only if the book accepts the order. As a result, the `n`th accepted order always has id `n`.

**Why it matters:** a client can never see an id skipped because someone else's order was rejected, and recovery can check `lastOrderID` against the number of acks in the log.

**Cost:** a rejected `NewOrder` was briefly offered an id that is then reused for the next order. That is invisible, because the reject event carries no order id.

### Match ids

Every fill pair gets `lastMatchID + 1`, shared by the taker's event and the maker's event, so a consumer can join the two halves of a trade.

### Instruments are created by commands

`AddInstrument` goes through the same command stream as orders. It is sequenced, logged and replayed, so instruments get the same ids on replay. The HTTP admin endpoint (milestone 7) turns `POST /instruments` into a command under a reserved client id, never calling the registry directly ([decision 018](018-http-admin-and-instruments.md)). Since milestone 7, `Run` also records each command's latency, read from `internal/clock`; the engine never calls `time.Now` ([decision 017](017-metrics-and-clock.md)).

Ids start at 1, in registration order. Id 0 means "none", as with slab slots. Duplicate symbols and invalid tick or lot sizes are rejected.

### Cancel names its instrument

The `Cancel` command carries an instrument id alongside the order id. Without it, the engine would need a global order-id-to-instrument table: another lookup on the hot path, and another structure to keep consistent. Clients always know which instrument their order is on, because the ack tells them.

A cancel sent to the wrong instrument gets `UnknownOrder`, the same answer as for an id that doesn't exist (decision 003).

### Fail-stop on impossible states

`Step` also panics on an unknown command kind. The gateway only builds known kinds, and the WAL checksums every record, so an unknown kind is a bug, not client input. Client mistakes always become `Reject` events; only "this program is broken" panics.

## Costs, summarised

- One core per engine.
- The publisher must never block, so the hard work of the slow-consumer policy moves there.
- Panics are reserved for cases that would otherwise corrupt state. A bug in the sequencer stops the process instead of being survived.
