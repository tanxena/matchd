# 014 — The fill log and replay-check

- **Status:** accepted (milestone 5). Writing a fill log, rather than comparing only books, was chosen with the user.
- **Code:** `internal/snapshot/filllog.go`, `internal/snapshot/check.go`, `internal/publisher/publisher.go` (`FillRecorder`), `cmd/engctl` (`replay-check`)

## Context

PRD 5.4:

> Replay check tool (`engctl replay-check`): runs a WAL through a fresh engine and compares the resulting book and fill log against the engine's own snapshot and fill log.

So the running engine must keep a record of the fills it emitted, and a separate tool must be able to rebuild them independently.

## Decisions

### What the fill log records

One record per **released match**, written by the publisher when it releases the maker's half of a fill pair. The engine emits a taker fill immediately followed by its maker fill, and the publisher releases events in order, so the pair is always adjacent.

The record is a `snapshot.Match`:
- match id;
- the taker's command seq and time;
- instrument, price and quantity;
- the taker's side;
- for each side: order id, client, client order id, and quantity left.

That is everything both parties were told.

### Format: the WAL's record format, gaps allowed

The fill log reuses `internal/wal`, with each record's seq set to the match id. That gives checksums, torn-tail handling, segments and crash recovery for free.

Match ids in the fill log **may have gaps**: matches whose events were never released (the process died first) are simply absent. So the log is opened with `wal.Options{AllowGaps: true}`, which requires ids to be strictly increasing rather than consecutive. A repeated id is still corruption, and a duplicated fill can't be written: `FillLog.Released` refuses a match id that isn't above the last one.

### Not fsynced

The fill log is an **audit trail**, not part of the durability contract. Writing it costs one `write` per release batch. Fsyncing it would put a second fsync on every batch. A power failure may cut its tail, and that is allowed: replay-check treats *missing* matches as fine and *wrong or extra* ones as failures. A write failure is logged once and counted, but isn't fatal.

### What replay-check verifies (`snapshot.ReplayCheck`, `engctl replay-check --data DIR`)

It only **reads** the data directory: the WAL is opened read-only, which reports a torn tail without truncating it. The engine must be stopped.

1. **Choose the base.** If the WAL still starts at seq 1, use an empty engine, which checks all of history. Otherwise use the **older** of the two listed snapshots; the WAL is always kept from there on.
2. **Replay the WAL from the base.** When the replay reaches the **newest** snapshot's seq, its full state must equal that snapshot exactly (`reflect.DeepEqual` on `engine.State`). The newest snapshot is thus rebuilt independently, from older data plus the WAL.
3. **Continue to the end of the WAL,** collecting every match the replay produces.
4. **Every fill-log record newer than the base must equal the replayed match with the same id.** A record the replay never produced means a fill was released whose command isn't in the WAL. That is either an ack that escaped before durability, or a WAL that lost acknowledged data. Both are reported.
5. It also fails if the WAL ends before the newest snapshot.

The result includes the full replayed state at the WAL's end and all replayed matches. The crash harness uses both (decision 015).

### What replay-check does not do

- It doesn't check fill-log records older than its base. The snapshots it verifies cover them, and the fill log's older segments are pruned anyway.
- It can't find matches that are *missing* from the fill log. They are legitimately possible (never released, or a lost tail).
- It doesn't prove fsync semantics; see decision 012 and decision 015.

### The startup cross-check

`server.Start` refuses to start if the fill log reaches a match id higher than the recovered engine's last match id. The fill log may lag the engine, but it must never lead it.

## How it's tested

- **`TestReplayCheckDetectsDamage`:**
  - a snapshot whose content was changed but whose checksum was recomputed fails with "differs from snapshot";
  - a fabricated fill-log record fails with "never produced";
  - a WAL with its newest segment removed fails.
- **`TestSnapshotsWrittenPrunedAndRecovered`:** on a clean data directory, it rebuilt snapshot 902 from base 702 and compared 140 fill-log records with the replay.
- **The crash harness:** it runs `replay-check` after every one of 50+ kills.

## Costs

- **One extra `write` per release batch** on the publisher goroutine, plus an 85-byte record per match on disk. The disk use is pruned along with snapshots.
- **No fsync**, so after a power failure the fill log may be shorter than what clients actually received. That is accepted for an audit trail, and documented.
