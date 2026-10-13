# 04 — The write-ahead log: nothing is acknowledged until it is on disk

Milestone 4 makes the engine **durable**:
- every command is written to a **write-ahead log** (WAL) before anyone hears about its effects;
- the server can be killed at any moment and restarted with exactly the same state.

It adds:
- the log itself (`internal/wal`);
- a binary encoding for commands (`internal/engine/codec.go`);
- three **fsync policies** in the sequencer;
- a shared **durability tracker** (`internal/durable`);
- a **gate** in the publisher that holds every event until its command is safely on disk;
- recovery by **replay** at startup.

The core question this milestone answers is: *when is it safe to tell a client "done"?*

---

## 1. Concepts

### 1.1 Why a log, and why "write-ahead"

The engine's state, its books, lives in memory and vanishes when the process dies. The cheapest way to make it survive is not to save the books. Instead, save **the inputs** that produced them. The engine is deterministic ([02-engine.md](02-engine.md#11-why-one-goroutine-owns-the-whole-engine)), so feeding the same commands, in the same order, into a fresh engine rebuilds exactly the same books, order ids and sequence numbers. That is **replay**.

"Write-*ahead*" means the record of a change is written *before* the change is allowed to have consequences. If it were the other way round, a crash in between would leave an acknowledged change with no record, and replay could never reproduce it. The idea is old and central to databases (C. Mohan et al., "ARIES: A Transaction Recovery Method...", *ACM TODS* 17(1), 1992). Ours is the simplest form: a log of commands, no undo needed, because a command is never half-applied.

### 1.2 What `write` and `fsync` actually guarantee

When a program calls `write`, the bytes usually go into the operating system's **page cache** (memory), not to the disk. The OS writes them out later. That's fast, but if the **machine** loses power before then, they're gone. If only the **process** dies (a crash, `kill -9`), they're safe: the OS still owns them and writes them eventually.

**`fsync`** ([POSIX](https://pubs.opengroup.org/onlinepubs/9799919799/functions/fsync.html), [Linux fsync(2)](https://man7.org/linux/man-pages/man2/fsync.2.html)) blocks until the file's data has reached stable storage. On Windows, Go implements it with [`FlushFileBuffers`](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers). It is the only way to know that data would survive a power cut, and it is **slow**. On this laptop:

| operation (one 36-byte record) | median | range over 5 runs |
|---|---:|---:|
| write, no fsync (`BenchmarkAppendFlush`) | 3.7 µs | 3.4–4.8 µs |
| write + fsync (`BenchmarkAppendSync`) | 1.51 ms | 1.37–1.62 ms |

That's a factor of about 400. Everything about the fsync policies follows from this number.

Two subtleties:
- **A new file's name** lives in its directory, and on Linux that needs its own fsync of the directory. We do it when creating a segment. Windows offers no way to do this from Go ([decision 010](../decisions/010-windows-gotchas.md)).
- **A failed fsync can't be retried.** After a failure the kernel may have discarded the unwritten data and marked it clean, so a retry "succeeds" without writing it ([PostgreSQL: Fsync Errors](https://wiki.postgresql.org/wiki/Fsync_Errors); [Rebello et al., USENIX ATC 2020](https://www.usenix.org/conference/atc20/presentation/rebello)). The only safe response is to stop and recover from what is really on disk.

### 1.3 Torn writes, and why every record has a checksum

If a crash interrupts a write, the last record may be only partly on disk: a **torn write**. A disk can also, rarely, return different bytes from the ones written. To notice either, every record carries a **CRC32C** checksum of its contents ([RFC 3720 §12.1](https://www.rfc-editor.org/rfc/rfc3720), [`hash/crc32`](https://pkg.go.dev/hash/crc32)). Reading a record back, we recompute the checksum and compare.

A bad checksum on the *last* record of the log, with nothing after it, is the signature of a torn write: the process died mid-append, and nothing was acknowledged for that record, so it is safe to cut off. A bad checksum *in the middle*, with valid-looking data after it, is something else (corruption), and silently cutting there could throw away acknowledged commands. [Decision 011](../decisions/011-wal-format.md#the-torn-tail-rule) has the exact rule. The short version is "damage in the last segment, followed only by zeros or the end of the file, is torn; anything else stops the server".

### 1.4 Group commit

If every command needs its own 1.5 ms fsync, the engine handles at most about 660 commands per second. **Group commit** amortises the cost: many commands written close together share one fsync. It comes in two flavours here:

- **`always`, with batching:** the sequencer takes every command already waiting, writes them all, fsyncs once, and only then lets the engine see them. Nothing is applied before it is durable. Under load, many commands share each fsync.
- **`group:1ms`:** commands go to the engine *immediately* after being written; a separate goroutine fsyncs every 1 ms. The engine never waits for the disk. But now the engine has applied commands that aren't durable yet, so their consequences must be **held back** until the fsync catches up.

### 1.5 The durability gate

The rule ([decision 012](../decisions/012-durability-and-fsync-policies.md)):

> No byte that depends on a command may leave the process before that command's WAL record has been fsynced.

"Depends on" includes other people. When a buy order matches a resting sell, the *seller's* fill depends on the *buyer's* order, so it also waits for the buyer's fsync.

The mechanism is one number: **the durable sequence number**, the highest seq known to be on disk. Every event carries the seq of the command that caused it. The publisher keeps events in a queue and releases them only once `event.Seq ≤ durable`. Under `always` the number has already advanced by the time the engine sees the command, so events pass straight through. Under `group` they wait up to about 1 ms plus an fsync. The same gate serves every policy, so there is exactly one place where "don't leak before durable" is enforced.

### 1.6 What each policy loses, and what a crash test can and can't show

| | process crash (`kill -9`) | power loss or OS crash |
|---|---|---|
| `always` | nothing | nothing acknowledged |
| `group:1ms` | nothing | nothing acknowledged; up to about 1 ms of applied-but-unacknowledged commands |
| `never` | nothing | **acknowledged commands** still in the page cache |

A process crash loses nothing under *any* policy, because the OS keeps the page cache. So a test that kills the process (milestone 5's crash harness) **can't tell the policies apart**: it would pass with `never`. Fsync behaviour is proven instead by the gating tests below, which wrap the file so that fsync can be blocked or made to fail, and by the throughput difference in the benchmark (milestone 9). PRD 5.4 requires the README to say this plainly.

---

## 2. Code walkthrough

### `internal/wal/wal.go`: the log

- **Constants** ([wal.go:39](../../internal/wal/wal.go#L39)): the segment magic `MTCHWAL`, the format version, the 8-byte segment header, the 8-byte record header (length + CRC), and `MaxPayload` = 4 KB. **`castagnoli`** is the CRC table. **`ErrCorrupt`** marks anything recovery must not paper over.
- **`File`** ([wal.go:61](../../internal/wal/wal.go#L61)): `Write`, `Sync`, `Close`. `*os.File` satisfies it; tests substitute wrappers that count, block or fail `Sync`.
- **`Log`** ([wal.go:111](../../internal/wal/wal.go#L111)):
  - `mu` guards the current file handle and size;
  - `syncMu` serialises `Sync` with segment rolls;
  - the atomics `appended` (written to the OS) and `synced` (fsynced);
  - `pending`, the batch being built, touched only by the appender.
- **`Open`** ([wal.go:148](../../internal/wal/wal.go#L148)):
  1. lists the segments in seq order;
  2. scans each one, calling `replay` for every record;
  3. truncates a torn tail in the last segment, or removes a segment that crashed while its header was being written;
  4. reopens the last segment for appending;
  5. sets both `appended` and `synced` to the last surviving seq. Whatever survived on disk counts as durable.
- **`scanSegment` / `scanBytes`** ([wal.go:276, 286](../../internal/wal/wal.go#L276)): check the header (magic, version), then loop over records. They check that each record's seq is exactly the previous one + 1, and that the segment's first record matches its file name. `scanBytes` works on bytes in memory, so the fuzzer can call it millions of times without touching the disk.
- **`parseRecord`** ([wal.go:365](../../internal/wal/wal.go#L365)) decodes one record or describes what is wrong with it. It also reports where the damage ends, which **`tailIsTorn`** uses: everything after it must be zero for the damage to count as a torn write. The long comment there is the torn-tail rule.
- **`Append`** ([wal.go:406](../../internal/wal/wal.go#L406)) adds a record to `pending`: a placeholder CRC, then seq, time and payload, then the CRC is computed over the body and written in. It panics on a seq gap.
- **`Flush`** ([wal.go:437](../../internal/wal/wal.go#L437)) rolls to a new segment if needed, writes all of `pending` with one `Write` under `mu`, and then publishes `appended`.
- **`roll`** ([wal.go:462](../../internal/wal/wal.go#L462)), under `syncMu`:
  1. fsyncs and closes the old segment, so a finished segment is always fully durable;
  2. creates the new one and writes its header;
  3. fsyncs the directory.
- **`Sync`** ([wal.go:496](../../internal/wal/wal.go#L496)), under `syncMu`:
  1. takes `mu` only to read the file handle and `appended`;
  2. releases `mu` and fsyncs, so appends carry on meanwhile;
  3. on success, publishes `synced` as the value read before the fsync. Records written during the fsync aren't counted.
- **`syncdir_windows.go` / `syncdir_other.go`:** directory fsync on Linux; a documented no-op on Windows.

### `internal/engine/codec.go`: commands on disk

- **`AppendCommand`** ([codec.go:58](../../internal/engine/codec.go#L58)) writes a kind byte, then a fixed layout per kind: `NewOrder` is 36 bytes, `Cancel` 17, and `AddInstrument` 22 + the symbol.
- **`DecodeCommand`** ([codec.go:91](../../internal/engine/codec.go#L91)) checks the exact length for each kind and leaves `Seq` and `Time` alone; those come from the WAL header.

The layout is persistent: changing it means a new WAL format version.

### `internal/durable/durable.go`: the durable seq

**`Seq`** ([durable.go:15](../../internal/durable/durable.go#L15)) holds:
- an atomic counter;
- `notify`, a capacity-1 ping channel;
- `failed`, closed once by `Fail`.

**`Advance`** ([durable.go:44](../../internal/durable/durable.go#L44)) raises the counter with a compare-and-swap loop (never lowering it) and pings without blocking; it does nothing after `Fail`. **`Fail`** records the error and closes `failed`, exactly once.

### `internal/sequencer/sequencer.go`: policies

- **`Policy`** ([sequencer.go:47](../../internal/sequencer/sequencer.go#L47)) holds the mode, the window and the batch size. **`ParsePolicy`** reads `always`, `never`, `group` or `group:<duration>`.
- **`New`** ([sequencer.go:113](../../internal/sequencer/sequencer.go#L113)) continues from the log's last seq.
- **`LogNow`** ([sequencer.go:138](../../internal/sequencer/sequencer.go#L138)) stamps a command, writes it, fsyncs and advances durability, whatever the policy. Startup uses it for instruments.
- **`Run`** ([sequencer.go:157](../../internal/sequencer/sequencer.go#L157)) loops:
  1. take one command (or notice durability has already failed);
  2. drain up to `MaxBatch` more without blocking;
  3. stamp and encode each, `Flush` once;
  4. apply the policy:
     - **Always:** `Sync`, advance `durable` (on error, fail *without* forwarding);
     - **Never:** advance `durable` to what was written;
     - **Group:** `markDirty`, and `kick` if `MaxBatch` commands are unsynced;
  5. forward the batch to the engine.

  When its input closes, **`finish`** stops the syncer and does a final fsync.
- **`groupSyncer`** ([sequencer.go:260](../../internal/sequencer/sequencer.go#L260)) runs a loop ([sequencer.go:311](../../internal/sequencer/sequencer.go#L311)):
  1. wait for "dirty" or a kick;
  2. wait out the window, using one reused timer, unless kicked or stopped;
  3. `Sync`, then `Advance` (or `Fail`).

### `internal/publisher/publisher.go`: the gate

- **`MaxPending`** = 65,536 held events.
- **`Run`** ([publisher.go:235](../../internal/publisher/publisher.go#L235)) selects on three channels:
  - the event channel. It is set to `nil` while 65,536 events are held, so the engine waits; that is the backpressure.
  - `durable.Notify`;
  - `durable.Failed`.

  It returns only when its input has closed **and** nothing is still held, or when durability fails.
- **`release`** ([publisher.go:271](../../internal/publisher/publisher.go#L271)) routes the prefix of held events whose seq is ≤ durable. Seqs never decrease, so the releasable events are always a prefix. It then reuses or compacts the slice so it doesn't grow forever.
- **`withhold`** ([publisher.go:306](../../internal/publisher/publisher.go#L306)) drops everything held, drains the rest of the engine's output so the engine is never left blocked, counts it in `Withheld`, and logs an error.

### `internal/server/server.go`: recovery and failure

**`Start`** ([server.go:126](../../internal/server/server.go#L126)):
1. opens `DataDir/wal` with a `replay` callback that decodes each command, restores its seq and time, and calls `engine.Step`, discarding the events. It refuses a log that doesn't start at seq 1, because there are no snapshots yet.
2. logs the recovery summary;
3. checks configured instruments against the log, or adds them via `LogNow`;
4. starts the pipeline, plus a watcher that closes the gateway if durability fails.

*Superseded in milestone 5:* step 1 now loads the newest snapshot first and replays only the WAL after it (`snapshot.Recover`), so the WAL no longer has to start at seq 1. See [05-recovery.md](05-recovery.md#2-code-walkthrough).

**`Close`** ([server.go:344](../../internal/server/server.go#L344)) runs once: gateway, then `cmds`, then wait for the pipeline (which ends with the sequencer's final fsync), then close the WAL. It returns the durability error, if any. **`Failed`** and **`Err`** expose the failure to `cmd/engine`, which shuts down on it.

### Tests

- **`internal/wal`:**
  - round trip across 10+ segments, with reopen-and-continue;
  - **every byte offset** at which the final record can be cut;
  - four torn-tail shapes, and a torn segment header;
  - **ten** corruption cases, each checked to fail for the intended reason;
  - fsync failure doesn't advance `synced`;
  - concurrent `Sync` and append under `-race`;
  - a seq-gap panic; **`FuzzScan`** (3.77M inputs per minute);
  - the two benchmarks above.
- **`internal/engine`:** command round trip (10,000 random commands), golden bytes for `NewOrder` and `Cancel`, malformed input, `FuzzDecodeCommand`, zero allocations for encoding and decoding a `NewOrder`.
- **`internal/durable`:** monotonic advance, coalesced pings, stop after `Fail`, concurrent advance.
- **`internal/sequencer`:**
  - policy parsing;
  - always forwards only after fsync; 1,000 waiting commands = 2 fsyncs;
  - group forwards first, then catches up; kicks work even with a 1-hour window;
  - failure behaviour for both policies;
  - never doesn't fsync.
- **`internal/publisher`:** held-then-released in order, including partial release; failure withholds everything; the pending limit back-pressures.
- **`internal/server/durability_test.go`:** the end-to-end proof, through real TCP clients and a gate around the WAL file:
  - acks gated on fsync under both policies;
  - **a failed fsync produces no ack and no fill for the batch**, for either party;
  - fsync counts per policy;
  - restart gives identical state, and sequence numbers and order ids carry on;
  - an instrument config mismatch is rejected;
  - restart after a torn tail.

### Planted bugs, all caught

| planted bug | caught by |
|---|---|
| `always` advances durability without fsyncing | `TestAlwaysForwardsOnlyAfterFsync` ("command 1 forwarded before its fsync completed") |
| the publisher ignores the durable seq | `TestHeldUntilDurableThenReleasedInOrder` |
| the WAL marks data synced before the fsync succeeds | `TestSyncFailureDoesNotAdvance` and `TestFsyncFailureEmitsNothing` |
| the group syncer is never kicked early | `TestGroupBurstAndKick` |
| every damaged record treated as a torn tail | `TestCorruptionIsFatal` |

### Measured here (illustrative; not the benchmark protocol)

| observation | value |
|---|---|
| one write + fsync | 1.51 ms median (1.37–1.62) |
| one write, no fsync | 3.7 µs median (3.4–4.8) |
| `always`: 1,000 commands queued behind a blocked fsync | 2 fsyncs |
| `always`: 20 sequential round trips | 20 fsyncs |
| `group:2ms`: 500 pipelined orders | 4 fsyncs |
| replay after a hard kill of the real binary (`taskkill /F`) | 3 records replayed; the next order got seq 4, order id 3, match id 2, and filled the resting order from before the kill |

---

## 3. Design decisions

- [011: WAL format](../decisions/011-wal-format.md). Segments named by first seq, with a magic and version header; records `length | crc32c | seq | time | payload`; `MaxPayload` 4 KB; strict seq continuity; the persistent command encoding; the torn-tail rule and its blind spot; a concurrency design that never blocks appends during fsync; directory fsync (and its absence on Windows).
- [012: Durability and fsync policies](../decisions/012-durability-and-fsync-policies.md). The exact rule for what may leak before fsync; one gate in the publisher for all policies; `always` with batching (chosen with you); `group` with a 1 ms window and an early fsync at 4,096 (the Section 10 question); `never`; what each loses; fail-stop instead of retrying fsync; "unacknowledged isn't the same as didn't happen"; full-log replay at startup.
- [010: Windows gotchas](../decisions/010-windows-gotchas.md). New: the cost of `FlushFileBuffers`; directories can't be fsynced; hard-killing a process from Git Bash.

---

## 4. What breaks if

1. **An ack is sent before the fsync.** Suppose the publisher released events as soon as the engine produced them, under `group`. A client is told "order 812 filled at 100" and hedges elsewhere. The machine loses power 0.5 ms later, before the fsync. On restart, replay rebuilds the book from the log, which doesn't contain order 812. The exchange has no record of a fill it confirmed. The counterparty's fill is gone too, so one side is hedged against a trade that doesn't exist. *Planted (the publisher ignoring the durable seq) and caught.*

2. **Replay uses a fresh clock instead of the logged time.** Suppose recovery stamped each replayed command with `time.Now()`. Every rebuilt event, and from milestone 6 every market-data message regenerated from the log, would carry restart-day timestamps. A trade that happened at 09:30:00.000123 would appear in the replayed fill log at 16:02:11. The replay check (milestone 5) compares fill logs, would report every record as different, and could no longer prove recovery correct. That's why the sequencer is the only place that reads the clock, and why the time is stored in the record. (No test catches this yet, because no engine *state* depends on time. Milestone 5's replay check, which compares the fill log, will. Recorded here so it isn't forgotten.)

3. **A failed fsync is retried.** Under `group`, the fsync for a batch returns an I/O error. The code logs it and tries again, and the second attempt succeeds, because the kernel had already dropped the dirty pages and marked them clean, so there was nothing left to write. The batch's acks are then released. The data was never written, so after the next restart those acknowledged orders don't exist. Our code never retries: `Fail` closes the gate permanently, and the server stops.

4. **The torn-tail rule is too generous.** Suppose recovery truncated at the *first* bad record anywhere in the last segment. A single bit flip in a record from yesterday morning would then silently delete every command after it, including thousands of acknowledged ones, and the server would start up "fine". Our rule refuses: damage followed by any non-zero byte is `ErrCorrupt`, and the server won't start until someone decides. *Planted (every damaged record treated as torn) and caught.*

5. **The group syncer marks durable what was appended *after* its fsync started.** If `Sync` read `appended` *after* the fsync returned, records written during the fsync (which may not be covered by it) would be counted as durable, and their acks released early. That's the same failure as #1, in a narrower window and much harder to see in testing. `Sync` reads `appended` under `mu` *before* the fsync; the comment on that line explains why.

---

## 5. Go idioms introduced

**`os.File`, `Sync`, `os.Truncate`, `os.ReadDir`.** `os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)` opens for appending, creating the file if needed (`0o644` is an octal permission literal). `f.Sync()` is fsync. `os.Truncate(path, n)` cuts a file to `n` bytes. `os.ReadDir` lists a directory sorted by name ([package os](https://pkg.go.dev/os)).

**`hash/crc32` with a table.** `crc32.MakeTable(crc32.Castagnoli)` builds the lookup table once, at package initialisation. `crc32.Checksum(data, table)` then computes the CRC. Go uses the CPU's CRC instruction when it can.

**`binary.LittleEndian.PutUint32` to fill in a field afterwards.** `Append` writes a placeholder 0 for the CRC, appends the body, then computes the CRC over the body and writes it into the placeholder with `PutUint32`. That pattern (reserve, fill, patch) avoids a second buffer.

**Compare-and-swap loops.** `durable.Advance` must only ever *raise* the counter, even with several goroutines advancing at once. A plain `Store` could lower it. The loop is: read the current value; if ours isn't higher, stop; otherwise `CompareAndSwap(old, new)`, and if another goroutine changed it in between, try again ([package atomic](https://pkg.go.dev/sync/atomic)).

**Setting a channel variable to `nil` inside a `select`.** Receiving from a `nil` channel blocks forever, so a `select` case on a `nil` channel is never chosen. The publisher sets its input to `nil` while 65,536 events are held, which "turns off" that case without restructuring the loop. It also sets the event channel to `nil` once it's closed, so it stops receiving the zero values a closed channel returns.

**`time.Timer` reuse.** `time.NewTimer` allocates. The group syncer creates one timer and calls `Reset(window)` for each window, and `Stop()` when it doesn't need it. Since Go 1.23, a stopped or reset timer can't deliver a stale value afterwards, which makes reuse safe.

**`errors.Join`.** `errors.Join(e1, e2, e3)` combines several errors into one, ignoring nils, and `errors.Is` sees through it. `Server.Close` uses it to report the sequencer's, the gateway's and the WAL's errors together.

**Labelled `break`.** In the sequencer, `break drain` leaves the labelled `for` loop from inside a `select`. A plain `break` inside a `select` only leaves the `select`.

**Function values as hooks.** `wal.Options.OpenFile func(path string) (File, error)` lets tests swap in a wrapper without an interface hierarchy or mocking library: production passes nothing (the default), and tests pass a closure.

---

## 6. Self-check questions

1. Under `group:1ms`, a buy order crosses a resting sell. List, in order, what happens from the buyer's frame arriving to both parties receiving their fills, and say at which point the order is durable.
2. Why can't a test that kills the process with `kill -9` (or `TerminateProcess`) distinguish `fsync=always` from `fsync=never`? What *does* show the difference in this repo?
3. The last segment ends with a record whose checksum is wrong, followed by 512 zero bytes. Is it truncated or is the server refused? What if it's followed by `0x55 0x55`? Why the difference?
4. An fsync fails under `always`. What happens to the batch that was being synced, to the clients, and to the process? After a restart, might those orders exist?
5. Why does `Log.Sync` read `appended` *before* calling fsync rather than after?

<details>
<summary>Answers</summary>

1. The buyer's reader decodes the frame and puts the command on `cmds`. The sequencer takes it (plus anything else waiting), stamps its seq and time, appends it, and `Flush`es: one `write`, so the data is now in the page cache. It calls `markDirty` and forwards the command at once. The engine matches it and emits Ack, taker Fill, maker Fill and BookDelta, all carrying the order's seq. The publisher **holds** them, because `durable` is still below that seq. Within about 1 ms the group syncer wakes, fsyncs, and **only now is the order durable**; it advances `durable`. That pings the publisher, which releases the four events: the Ack and taker fill to the buyer's session, the maker fill to the seller's. The writers then send them.

2. Killing a process doesn't lose the OS page cache; the kernel still writes it to disk. So under any policy, every record that was written before the kill is there on restart. Only a power or OS failure discards unsynced pages, and a test can't easily cause one. The difference is shown by (a) the fsync-gating tests, which wrap the file and *block* or *fail* fsync, then check that nothing is acknowledged before it, or after a failure; and (b) the throughput and latency difference between policies in the benchmark.

3. Followed by zeros: **truncated** as a torn tail. Zeros are what a file system leaves in an extended-but-unwritten region, and nothing was appended after the damaged record. Followed by `0x55 0x55`: **refused** (`ErrCorrupt`). Non-zero bytes after the damaged record mean something was written after it, so the damage isn't an interrupted final write. Truncating there could throw away real records.

4. The sequencer's `Sync` returns the error. The sequencer calls `durable.Fail` and returns **without forwarding** the batch, so under `always` the engine never applies it. The server's watcher closes the gateway, which disconnects every client; none of them gets an ack for that batch. `cmd/engine` logs the error and exits. After a restart: **possibly yes**. The batch was *written* to the OS before the fsync failed, so if those pages did reach the disk, replay includes them, and the orders exist although nobody was acknowledged. "Not acknowledged" means "unknown", not "rejected".

5. Records appended while the fsync is running may or may not be covered by it. Reading `appended` first gives a value that is definitely already in the file when fsync starts, so it is definitely covered when fsync returns. Reading it afterwards could include records written *during* the fsync, and marking those durable could release their acks before they are really on disk.

</details>
