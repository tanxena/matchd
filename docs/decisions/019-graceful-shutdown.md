# 019 — Graceful shutdown: stop, drain, fsync, deliver, snapshot

- **Status:** accepted (milestone 7). Chosen with the author: the WAL is flushed and fsynced before the shutdown snapshot ("the snapshot is an optimisation for restart time, never a durability requirement"), and clients get 5 s by default (`-shutdown-timeout`) to receive their answers.
- **Code:** `internal/server/server.go` (`Close`, `shutdownSnapshot`), `internal/gateway/gateway.go` (`StopReading`, `FinishWriters`), `internal/gateway/session.go` (`lingerClose`), `cmd/engine` (signals)

## Context

PRD 6: "Graceful shutdown: stop accepting, drain the command channel, fsync, snapshot, close."

Until milestone 7, `Close` disconnected every client first and then drained the pipeline. Commands already accepted were executed and logged, but their answers were thrown away with the connections. The clients saw a dropped connection and had to treat those orders' fate as unknown (decision 012).

## Decision: the order of shutdown

`Server.Close`:

1. **Stop accepting.**
   - `/healthz` turns 503.
   - The HTTP listener stops taking requests; in-flight ones, including an admin `POST` waiting for its answer, finish.
   - `gateway.StopReading` closes the TCP listener and stops every session's **reader**, but not the session. It sets a draining flag and wakes each reader with an immediate read deadline. A reader that sees the flag returns without closing anything; it's checked again after the idle deadline is re-armed, so the wake-up can't be lost. `StopReading` returns once no reader can submit another command.
   - The admin path is closed under a lock, too.

   Commands a client had sent that no reader had taken are not executed, and the client gets no answer for them.
2. **Drain.** `close(cmds)`. The sequencer writes and **fsyncs** everything still queued (its normal finish), so **the WAL is durable before anything else happens**. The engine applies it, and the publisher releases every event into the session queues.
3. **Deliver, and snapshot, in parallel.**
   - `gateway.FinishWriters(ShutdownTimeout)`: each session's **writer** sends what's queued, then closes its session gracefully (below). After the timeout, sessions still being sent to are closed by force: a client that doesn't read can't hold shutdown hostage.
   - Meanwhile `shutdownSnapshot` freezes the stopped engine and writes a snapshot with the writer's normal code (`WriteNow`), so the next start replays nothing. It's skipped when snapshots are disabled, when nothing changed since the last one, and **when durability failed**: the engine may then contain commands the WAL never made durable, and a snapshot would resurrect them.
4. **Close** the WAL and the fill log.

`cmd/engine` runs this on SIGINT or SIGTERM, and exits 0. A second signal exits at once. That's safe: everything acknowledged is already in the WAL, and the next start just replays more.

### Why a crash during the snapshot loses nothing

Step 2 made the WAL durable first. The snapshot file is written atomically and only listed in the manifest once complete (decision 013). So a crash at any point during step 3 leaves either:
- the previous snapshot plus a complete WAL, which recovery replays; or
- the new snapshot.

`TestCrashDuringShutdownSnapshotLosesNothing` copies the data directory just before the snapshot is written, which is exactly what a crash there would leave. It runs under group commit, the policy where the engine is ahead of the disk. It then checks that recovering from the copy gives the identical state (by replaying the WAL), and that the clean directory gives it with zero records replayed.

### The lingering close (found by the shutdown test)

Closing a TCP socket that still has **unread input** sends a reset (RST) instead of a normal FIN. A reset makes the *client's* network stack discard data it has received but its program hasn't read yet. During shutdown that's the normal case: the readers have stopped, the client is still pipelining orders into a socket nobody reads, and the answers to its earlier orders are sitting in its receive buffer. A plain close destroyed them:
- the first version of `TestGracefulShutdownDrainsAndSnapshots` failed 3 runs in 8 (the client read 959 acks for 1,002 executed orders);
- `TestShutdownDoesNotResetAClientStillSending`, written to force the case, read **0** of its 300-plus acks without the fix.

So the writer ends a session the way HTTP servers do (`lingerClose`):
1. flush;
2. half-close the connection (`CloseWrite`, sending FIN, "nothing more from us");
3. read and discard whatever the client still sends, until it closes or 1 s passes;
4. only then close.

With no unread input left, the close is a normal close, and the client reads every answer and then EOF.

## Costs

- **Shutdown can take up to `ShutdownTimeout`** (default 5 s, the author's choice), plus up to 1 s of lingering per session in parallel, plus the snapshot write. A deployment's kill timeout must be longer than that, or the second-signal path applies.
- **Commands still in a socket buffer when the readers stop are dropped unanswered.** The alternative, reading until clients go quiet, has no bound.
- **Tests that count snapshots** must opt out of the shutdown snapshot (`noShutdownSnapshot`, test-only); one did.

## Tests

- `TestGracefulShutdownDrainsAndSnapshots`: a pipelining client, with `Close` called mid-stream. The acks it receives exactly equal its orders in the recovered book, and the restart replays 0 WAL records. *Planted:* closing sessions at once (851 acks for 1,004 orders); no lingering close; no shutdown snapshot (1,004 records replayed).
- `TestShutdownDoesNotResetAClientStillSending`: the reset case, deterministically.
- `TestShutdownTimeoutCutsAStuckClient`: a client that never reads, a 300 ms timeout: `Close` returns promptly and logs the cut. *Planted:* the timeout ignored (`Close` took 10 s).
- `TestCrashDuringShutdownSnapshotLosesNothing`: above.
- `TestSignalShutsDownGracefully` (crash harness, the real binary): Ctrl-Break on Windows, SIGTERM on Linux. The engine exits 0, the client's connection is closed, and the restart replays 0 records.
