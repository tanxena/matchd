# 015 — The crash test: what it checks, and what it can't prove

- **Status:** accepted (milestone 5). "5 runs by default, 50+ with `-crash`" was chosen with the author.
- **Code:** `internal/crashtest/crash_test.go`
- **Run:** `go test ./internal/crashtest` (5 runs), `go test ./internal/crashtest -crash` (50 runs, acceptance 2), `-crashruns N` for more

> **Superseded in part (2026-10-14, `3852a5e`):** the per-run checks below no longer stand alone. The engine runs with `-archive`, and after the last run every acknowledged command and every received fill is checked against a replay of the whole history from seq 1; every fifth run the harness injects a torn WAL tail the binary must cut off (steps 6 and 7, review F-007). The milestone 5 results table predates both.

## Context

PRD 5.4 and acceptance 2: run the engine in a child process, drive random load, kill it with SIGKILL / `TerminateProcess` at random points, restart, run replay-check, and assert:

- every acked command is in the WAL;
- the recovered book equals the pre-crash book for all acked commands;
- no fill was emitted twice;
- sequence numbers continue with no gap or reuse.

At least 50 consecutive runs.

## What one run does

1. Start the **real `engine` binary**, built once by `TestMain` with `go build`, on a data directory shared by all runs. The configuration is:
   - `-listen 127.0.0.1:0`; the harness reads the chosen port from the `gateway listening` log line;
   - the fsync policy cycles **`always` → `group:1ms` → `never`**;
   - snapshots every 200 commands **or 20 ms**, so kills often land while a snapshot is being written;
   - WAL segments alternate between **64 KiB** (odd runs: segment rolls and pruning happen, so kills land during them) and **64 MiB** (even runs: nothing is pruned, so every acknowledged command in the run can be checked against the WAL directly).
2. Three clients send random limit and market orders, and cancels of their own acknowledged orders (sometimes naming the wrong instrument). They record every sequenced answer (Ack, engine Reject, CancelAck) and every fill.
3. After a random **80–400 ms**, `cmd.Process.Kill()` kills the engine. That is `TerminateProcess` on Windows and SIGKILL on Linux: no deferred code, no flush, no graceful anything.
4. Check, with the engine dead:

| PRD wording | what the harness asserts |
|---|---|
| — | **`snapshot.ReplayCheck` passes**: the newest snapshot is rebuilt from the older one (or from seq 1) plus the WAL and compared exactly, and every fill-log record after the base equals the replayed match (decision 014) |
| every acked command is in the WAL | for each answer the clients received: **its seq ≤ the WAL's last seq** (no ack beyond the durable log); if the record is still in the WAL, **it is that command**: same kind, client, and client order id or order id. Records older than the oldest kept snapshot have been pruned; the snapshot that covers them is what replay-check verified. |
| no fill was emitted twice | **no (match id, role) pair is ever received twice**, across *all* runs; and every received fill newer than replay-check's base **equals the replayed match** (price, quantity, order id, client order id, leaves, seq). A received fill for a seq past the WAL's end is a failure. |
| the recovered book equals the pre-crash book for all acked commands | **`snapshot.Recover`** (exactly what the engine runs at startup: latest snapshot + WAL tail) gives **the same full state** as replay-check's independent replay of the WAL from its base. The clients' answers and fills, all consistent with that replay, are the "acked" view. |
| sequence numbers continue with no gap or reuse | the WAL itself refuses gaps and repeats on open (decision 011); and **no seq answered in run k+1 is ≤ the WAL's last seq after run k**: the restarted engine never reuses a number |

5. The next run starts the binary again on the same directory, so the engine's own recovery runs under test every time.
6. **Every fifth run, a torn tail** *(review F-007)*. After the checks, the harness appends three bytes, the start of a record's length field, to the newest WAL segment: what a write cut short by a power failure leaves. The next start must find and cut it off: the harness requires `torn_bytes` > 0 in the binary's `recovered` log line. Kills alone essentially never tear a record (below), so this is how the real binary's torn-tail recovery runs under the harness.
7. **After the last run, the whole history** *(review F-007)*. The engine runs with `-archive`, so segments that snapshots make unnecessary are moved aside instead of deleted, and pruning still happens as usual. The live WAL plus the archive is the complete log from seq 1. It is replayed into an empty engine, and **every acknowledged command and every fill received in any run** is checked against it: the command at its seq, the fill against the match with its id.

**Why steps 6 and 7 were added:** the review measured what the per-run checks above actually covered. In 50 runs, 30% of acknowledged commands had been pruned before their run's check, and 71% of received fills were at or before replay-check's base, so they were checked only for duplicates. 0 runs had a torn tail. A bug corrupting a fill's content before a snapshot boundary would have passed about 70% of the time. Now, one 50-run measurement checked 447,645 of 447,645 acknowledged commands and 401,510 of 401,510 received fills against the full replay, and recovered 9 of 9 injected torn tails. The per-run checks still run too.

### Why not compare against "the pre-crash book" directly

At the instant of a kill, no one knows the exact pre-crash state. Commands can be in the WAL but not yet acknowledged; the kill can land between the fsync and the ack. Those commands are legitimately replayed after a restart ("unacknowledged doesn't mean didn't happen", decision 012).

So the harness checks what *is* knowable. Every acknowledged command and every delivered fill must agree with the log. Recovery must agree with an independent replay of that log. Replay is deterministic (milestone 2), so agreeing with the replay is agreeing with the history clients observed.

## What the crash test does NOT prove

**Killing a process leaves the operating system's page cache intact.** Data the engine `write`-ed but never fsynced still reaches the disk afterwards. So this test validates **recovery logic**: replay, determinism, torn-tail handling, snapshot plus WAL composition, and sequence continuity. It **does not** validate power-loss durability under any fsync policy.

It would pass with `fsync=never`, and it deliberately runs every third round with `never` to show that.

Fsync behaviour is validated separately, as PRD 5.4 requires:
1. `internal/server/durability_test.go` wraps the WAL file so fsync can be held or failed. It shows that no ack leaves before the fsync, under both `always` and `group`, and that no ack or fill leaves for a batch whose fsync failed.
2. The throughput difference between policies is measured in the benchmark (milestone 9).

**The README must say this plainly**, and must not claim the kill test validates fsync.

Two more gaps:
- **Torn tails rarely happen under process kills.** A `write` that has completed is in the page cache even if the process dies the next instant, so a kill almost never leaves a half-written record. In the 50-run results, 0 runs needed torn-tail recovery. Torn-tail handling is tested where it can be forced: every byte offset in `internal/wal` (`TestTornTailEveryOffset`), plus server-level restarts after a torn tail. *(Since review F-007, the harness also injects a torn tail every fifth run, which the real binary must cut off: step 6.)*
- **Snapshot-file atomicity under power loss** can't be tested here, for the same page-cache reason.

## Results (this laptop, Windows 11)

Three separate 50-run executions during the milestone, each with a different random crash pattern (the seed is logged):

| run | acknowledged commands | checked against WAL | fills received | compared with replay | snapshot rebuilt from WAL | result |
|---|---:|---:|---:|---:|---:|---|
| 1 | 427,038 | 290,841 | 463,210 | 113,498 | 50 of 50 | pass |
| 2 | 260,765 | 201,931 | 286,572 | 116,826 | 50 of 50 | pass |
| 3 (after the manifest fix) | 386,902 | 273,245 | 421,050 | 141,833 | 50 of 50 | pass |

"Checked against WAL" is lower than "acknowledged" because odd runs prune. "Compared with replay" counts fills newer than replay-check's base, which is the older of the two kept snapshots.

## Planted bugs this harness (or its neighbours) catch

- **Snapshot drops the last match id.** The restarted engine refuses to start, because the fill log is ahead of it (decision 014's cross-check).
- **Recovery re-applies records the snapshot already contains.** Recovery fails ("record 603 does not follow the recovered state at seq 902").
- **Pruning with only one snapshot.** Recovery fails once that snapshot is damaged (`TestRecoveryFromWALWhenOnlySnapshotDamaged`).

## Costs

- **About 20–30 s for 50 runs.** That's why the default is 5 runs (about 3 s) on every `go test`.
- **It builds the engine binary in `TestMain`,** so the test needs `go` on the PATH.
- **The kill timing is random,** seeded from the clock and logged. A failure prints its seed, but the timing of the OS scheduler can't be reproduced exactly.
