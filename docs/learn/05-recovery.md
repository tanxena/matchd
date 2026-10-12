# 05 — Snapshots, recovery, and proving it with a crash harness

Milestone 4 made the engine durable: every command goes into the write-ahead log (WAL) before anyone hears about it, and startup replays the whole log. That works, but the log grows forever, and so does startup time. Milestone 5 adds **snapshots**: a saved copy of the engine's state at some sequence number. Recovery becomes "load the newest snapshot, replay only the log after it", and old log segments can be deleted.

The hard part is the PRD's rule that **taking a snapshot must not pause the engine**. Matching keeps going while the snapshot is written, and the snapshot still has to be an exact picture of one instant. The answer is **copy-on-write**, and it reshapes how the book stores orders.

This milestone also builds:
- a **fill log** (what the engine told clients about trades);
- **`replay-check`**, an independent checker that rebuilds the state from the log and compares;
- a **crash harness** that kills the real engine binary at random moments, more than 50 times in a row, and verifies recovery each time.

---

## 1. Concepts

### 1.1 Why snapshots, and what a consistent snapshot means

Replaying a million commands takes time. Replaying ten billion takes far longer. A snapshot at seq *S* records the full state after command *S*, so recovery needs only the commands after *S*. Once a snapshot covers a WAL segment, that segment can be deleted.

A snapshot is only useful if it is **consistent**: exactly the state after *S*, not part of the state at *S* and part at *S* + 40. The engine is a single goroutine processing commands one at a time, so the obvious moment to take one is **between two commands**. The difficulty is that writing a snapshot to disk takes milliseconds, and the engine mustn't stop for that long (PRD 5.4): the pause would show up directly in the p99.9 latency the benchmark measures.

### 1.2 Copy-on-write

**Copy-on-write (COW)** shares data between a reader and a writer until the writer actually changes something, and only then copies the part being changed. Operating systems use it for `fork()`. Databases use it for snapshots. Go's own strings are immutable for a related reason.

Here:

1. Between two commands, the engine **freezes** each book. It records which chunks of order storage exist and where each price level's queue starts, and increments an **epoch** counter. That takes microseconds. No orders are copied.
2. It hands this frozen view to a **snapshot goroutine**, which reads it at leisure.
3. The engine keeps trading. The first time it needs to **write** to a chunk the snapshot is still sharing (an older epoch), it copies that chunk first and writes to the copy. The snapshot keeps reading the untouched original.
4. When the snapshot is written, the engine **thaws**: chunks nobody copied simply belong to the current epoch again, and originals that were replaced go to a spare pool for next time.

The cost moves from "copy everything, once, while everyone waits" to "copy only what changes, a chunk at a time, spread across normal work".

*If you know TypeScript or React:* it is the immutable-update idea (`{...state, field: newValue}`), applied only to the 192 KiB block that's changing, and only while someone else holds the old version.

### 1.3 Why the book had to change shape

In milestone 1 all of a book's orders lived in one Go slice. A slice can't be partially shared: either another goroutine reads the same array while the engine writes it (a data race), or someone copies the whole thing. So the slab became **fixed-size chunks**: 4,096 orders each, allocated separately on the heap, never moved. An order is found with two lookups: `chunks[i >> 12].nodes[i & 4095]`.

Chunks can be shared one at a time, and copied one at a time. As a side effect, the milestone 1 hazard where `append` could move the slab under a pointer is gone: growing adds a chunk, and existing chunks never move.

The rule that makes it safe: **every write to an order goes through one function, `nodeW`**, which copies the chunk if it is shared. A single write that bypasses it corrupts the snapshot. That is the planted bug in section 4.

### 1.4 Writing a file so a crash can't leave half of it

`write` + `fsync` makes data durable, but a crash *during* the write leaves a half-written file. The standard trick is the **atomic replace**:

1. write the whole file under a temporary name, and fsync it;
2. **rename** it over the real name;
3. fsync the directory, so the rename itself is durable.

On Linux, `rename` replacing an existing file is atomic ([POSIX rename](https://pubs.opengroup.org/onlinepubs/9799919799/functions/rename.html)): anyone looking sees the old file or the new one, never a mix.

**On Windows it isn't guaranteed.** Go's own [`os.Rename` documentation](https://pkg.go.dev/os#Rename) says so. The defence ([decision 010](../decisions/010-windows-gotchas.md) #10) is to make recovery survive a missing manifest: fall back to the fully written temp file, then to scanning the snapshot files themselves.

### 1.5 The manifest and retention

The **manifest** (`manifest.json`) lists the snapshots that are complete *and* whose commands are durable, oldest first. Recovery trusts only listed snapshots, or, if the manifest itself is lost, the snapshot files themselves. Two snapshots are kept:
- if the newest is damaged, recovery falls back to the older one;
- `replay-check` can rebuild the newest from the older plus the WAL between them.

The WAL is pruned only once two snapshots exist, and only below the **older** one. So one damaged snapshot is always survivable. A test found that pruning earlier made data unrecoverable (section 4).

### 1.6 Durability before publishing

Under group commit (decision 012) the engine is up to about 1 ms ahead of the disk. A snapshot of seq *S* taken at that moment contains commands the WAL hasn't made durable yet. If the snapshot were published and then the power failed, the WAL would end before *S* while the snapshot claimed *S*, and new commands numbered *S* + 1 onward would leave a hole in the log. So the writer **waits until the durable seq reaches *S*** before writing anything.

### 1.7 Testing recovery without being able to pull the power

A **crash harness** starts the real program, drives load, kills it, and checks what restarts. Killing a process is not the same as losing power, though. The OS page cache survives, so anything the program had `write`-ten reaches the disk anyway. A kill test proves **recovery logic** (replay, snapshots plus log, sequence continuity) but **not fsync**. It would pass with `fsync=never`, and ours runs every third round with `never` to show it. Fsync was proven separately, in milestone 4, by blocking and failing fsync directly ([decision 015](../decisions/015-crash-test.md)).

---

## 2. Code walkthrough

### `internal/book/cow.go`: chunked storage and copy-on-write

- **`chunk`** ([cow.go:31](../../internal/book/cow.go#L31)) holds 4,096 `orderNode`s and an `epoch`. **`chunkShift = 12`** turns a slot index into a chunk number and an offset.
- **`node(i)`** ([cow.go:37](../../internal/book/cow.go#L37)) gives read access. **`nodeW(i)`** ([cow.go:43](../../internal/book/cow.go#L43)) gives write access: if the chunk's epoch isn't the book's, it calls **`copyChunk`**, which takes a spare chunk, copies the whole array into it, and installs it.
- **`takeChunk`** pops from the spare pool, or allocates.
- **`alloc`** ([cow.go:78](../../internal/book/cow.go#L78)) reuses a freed slot, or hands out the next one, adding a chunk when the last one is full.
- **`Freeze`** ([cow.go:112](../../internal/book/cow.go#L112)):
  1. copies the chunk-pointer slice;
  2. copies each level's `(price, head)`, best first, into a `Frozen`;
  3. tops up the spare pool to one chunk per shared chunk, so copying never allocates while frozen;
  4. increments `epoch` and sets `frozen`.
- **`Thaw`** ([cow.go:136](../../internal/book/cow.go#L136)): for each chunk the view shared, if the book still holds the same pointer, set its epoch (unshare); otherwise the book replaced it with a copy, so put the orphaned original in the spare pool.
- **`Frozen.Each`** ([cow.go:156](../../internal/book/cow.go#L156)) walks each frozen level's FIFO chain through the frozen chunks.
- **`Load`** ([cow.go:177](../../internal/book/cow.go#L177)) checks the orders' sides, priority order and uniqueness, rests them in order, and runs `CheckInvariants`.

`book.go` changed so that every write uses `nodeW` and every read uses `node`. `CheckInvariants` gained a check: outside a snapshot, every chunk is at the current epoch.

### `internal/engine/snapshot.go` and `Run`

- **`Engine.Freeze`/`Thaw`** ([snapshot.go:24, 35](../../internal/engine/snapshot.go#L24)) freeze or thaw every book, plus `Seq`, `LastOrderID` and `LastMatchID`.
- **`State`** ([snapshot.go:52](../../internal/engine/snapshot.go#L52)) is the engine as plain data: instruments, and their orders in priority order. **`Snapshot.State()`** materialises a frozen view, safe from any goroutine. **`Restore`** ([snapshot.go:82](../../internal/engine/snapshot.go#L82)) rebuilds an engine from a `State`.
- **`SnapshotHooks`** ([snapshot.go:110](../../internal/engine/snapshot.go#L110)) has `Every`, `Tick`, `Out` and `Done`. **`Run`** ([engine.go:76](../../internal/engine/engine.go#L76)) now selects on three channels: commands, ticks, and returned snapshots. After each command it checks the count. `trigger` freezes and sends the snapshot, unless one is in flight (it then remembers to take another as soon as the current one returns) or nothing changed since the last.

### `internal/snapshot`: files, manifest, writer, fill log, recovery, check

- **`format.go`:**
  - **`Encode`** ([format.go:48](../../internal/snapshot/format.go#L48)) writes the layout in the comment: header, counters, per-instrument orders, CRC32C.
  - **`Decode`** ([format.go:82](../../internal/snapshot/format.go#L82)) verifies header and checksum *before* parsing, then reads with a bounds-checked `reader`. It rejects truncation and trailing bytes, and leaves empty sides `nil`, exactly as `engine.State` produces them (otherwise `reflect.DeepEqual` would report a difference).
- **`manifest.go`:**
  - **`ReadManifest`** ([manifest.go:45](../../internal/snapshot/manifest.go#L45)) reads `manifest.json`, then the `.tmp`, then scans the snapshot files.
  - **`readPublished`** ([manifest.go:58](../../internal/snapshot/manifest.go#L58)) is the no-scan version the writer uses.
  - **`WriteManifest`** writes atomically.
  - **`ReadSnapshot`** also checks that the file agrees with its manifest entry.
- **`writer.go`:**
  - **`Writer.Run`** ([writer.go:43](../../internal/snapshot/writer.go#L43)) writes each snapshot and always returns it on `done`.
  - **`write`** ([writer.go:63](../../internal/snapshot/writer.go#L63)) polls until `durable ≥ seq`, encodes, writes atomically, appends to the manifest (keeping 2), writes the manifest, then calls `prune`.
  - **`prune`** ([writer.go:107](../../internal/snapshot/writer.go#L107)) removes unlisted snapshot files, and, only when 2 are listed, WAL segments below the older snapshot and fill-log segments before its last match.
- **`filllog.go`:**
  - **`Match`** ([filllog.go:19](../../internal/snapshot/filllog.go#L19)) holds both sides of one trade.
  - **`FillLog.Released`** ([filllog.go:121](../../internal/snapshot/filllog.go#L121)) keeps the taker half, and on the maker half appends one record with the match id as its seq.
  - The log is a `wal.Log` with `AllowGaps`, and isn't fsynced.
- **`recover.go`:**
  - **`LoadLatest`** ([recover.go:28](../../internal/snapshot/recover.go#L28)) picks the newest listed snapshot that decodes, falling back to an older one, and finally to none.
  - **`Recover`** ([recover.go:50](../../internal/snapshot/recover.go#L50)) restores it and opens the WAL, replaying records after the snapshot's seq, each required to be exactly the next. It refuses a WAL that ends before the snapshot.
- **`check.go`:** **`ReplayCheck`** ([check.go:51](../../internal/snapshot/check.go#L51)):
  1. opens everything read-only;
  2. picks the base (an empty engine if the WAL still starts at 1, otherwise the older snapshot);
  3. replays, comparing the full state with the newest snapshot when it reaches that seq;
  4. collects every match;
  5. checks every fill-log record after the base against the replay.

  It returns the end state and the matches for the crash harness.

### `internal/fsutil` and `internal/wal` changes

- **`fsutil.WriteFileAtomic`** ([fsutil.go:21](../../internal/fsutil/fsutil.go#L21)): temp file, fsync, rename, directory fsync. **`SyncDir`** moved here from `wal`, with its per-platform files.
- **`wal`** gained three things:
  - **`AllowGaps`**: strictly increasing seqs, for the fill log;
  - **`ReadOnly`**: report a torn tail without truncating it, and create nothing;
  - **`RemoveSegmentsBelow`**: never touches the last segment.

### Server and commands

- **`server.Start`:**
  - recovers through `snapshot.Recover`;
  - opens the fill log, and refuses to start if it is ahead of the recovered engine;
  - creates the snapshot writer goroutine and a ticker;
  - runs the engine with hooks.

  **`Close`** waits for an in-flight snapshot.
- **`cmd/engine`** gained `-snapshot-every` (1,000,000), `-snapshot-interval` (10 s) and `-wal-segment-bytes`.
- **`engctl replay-check --data DIR`** prints the report, and exits 1 on a mismatch.

### `internal/crashtest`: the harness

- **`TestMain`** builds `cmd/engine` once.
- **`startEngine`** ([crash_test.go:170](../../internal/crashtest/crash_test.go#L170)) runs it on port 0 and reads the port from its log.
- **`runClient`** ([crash_test.go:82](../../internal/crashtest/crash_test.go#L82)) has a writer (random orders and cancels) and a reader that records every sequenced answer and every fill under a mutex.
- **`TestCrashRecovery`** ([crash_test.go:216](../../internal/crashtest/crash_test.go#L216)), for each run:
  1. start the engine, with the policy and segment size rotating;
  2. start three clients, wait 80–400 ms, then `Process.Kill()`;
  3. check: `ReplayCheck`; every answer's seq is ≤ the WAL's end, and, where still present, the WAL record is that command; no `(match, role)` is received twice across runs; every received fill equals the replayed match; `Recover`'s state equals the replay's; no answered seq reuses a number from before.

  [Decision 015](../decisions/015-crash-test.md) maps each check to the PRD's wording.

### Tests added this milestone (besides the harness)

- **`internal/book`:**
  - the differential harness now **freezes and thaws mid-stream** (210 snapshots per 100,000 commands), checking each frozen view against what the book held at freeze time;
  - `TestFrozenViewSurvivesHeavyTradingAcrossChunks` (20,000 orders, 5 chunks, 3 cycles);
  - `TestNoCopiesWithoutSnapshot`;
  - `TestLoadBehavesIdentically` (5,000 follow-up commands give identical results);
  - `TestLoadRejectsBadInput`;
  - `TestZeroAllocWhileFrozen`.
- **`internal/engine`:**
  - `TestStateRestoreRoundTrip` (then identical events);
  - `TestRunSnapshotsMatchReplay` (28 snapshots taken while the engine runs under `-race`, each equal to a replay to its seq, with an unchanged event stream);
  - `TestTickTrigger`.
- **`internal/snapshot`:** format round trip; **every** single-byte flip rejected; the manifest; the fill log.
- **`internal/server`:**
  - snapshots written, pruned, recovered and continued;
  - `replay-check` detects a tampered snapshot, a fabricated fill, and a missing WAL tail;
  - fallback to the older snapshot;
  - recovery from the WAL alone when the only snapshot is damaged;
  - snapshots wait for durability;
  - recovery without a manifest.
- **`internal/fsutil`, `internal/wal`:** atomic replace, gaps, read-only, segment removal.

### Results

| what | result |
|---|---|
| crash harness, 50 consecutive runs, three separate executions | all passed: 1,074,705 acknowledged commands in total; no ack beyond the WAL; no fill delivered twice; every snapshot rebuilt from the WAL matched |
| differential suite with mid-stream snapshots | 100,000 commands, 210 snapshots, 209 chunk copies, 0 divergences; the fill count is identical to milestone 4 |
| snapshot write time (server test, about 900 commands) | about 13–20 ms, mostly fsync |

---

## 3. Design decisions

- [013: Copy-on-write snapshots](../decisions/013-copy-on-write-snapshots.md). The chunked slab, `node`/`nodeW`, Freeze/Thaw, the spare pool, triggers (1M commands or 10 s, chosen with you), durability before publishing, atomic writes, the manifest, keeping 2 snapshots and pruning only then, the file format, recovery, and the costs.
- [014: The fill log and replay-check](../decisions/014-fill-log-and-replay-check.md). What's recorded and when; the WAL format with gaps allowed; no fsync; what replay-check verifies and what it can't; the startup cross-check.
- [015: The crash test](../decisions/015-crash-test.md). The configuration, the table mapping PRD wording to assertions, **what it cannot prove**, and the results.
- [010: Windows gotchas](../decisions/010-windows-gotchas.md). New: `os.Rename` isn't atomic, and the bug the fix introduced; killing processes from tests.

---

## 4. What breaks if

1. **One write bypasses copy-on-write.** In `Submit`, the maker's quantity is reduced through `node(mi)` instead of `nodeW(mi)`. The engine still trades correctly, and every test that only looks at the live book passes. But while a snapshot is in flight, that write lands in a chunk the snapshot is reading. The file on disk then shows quantities from *after* its seq, so recovery from it plus the WAL applies those fills twice. *Planted and caught*: in the first cycle of `TestFrozenViewSurvivesHeavyTradingAcrossChunks`, the frozen view no longer matched what the book held at freeze time.

2. **A snapshot is published before its commands are durable.** Under group commit, the snapshot at seq 1,000 is written while the WAL is durable only to seq 990. The power fails. On restart the snapshot says 1,000, while the WAL ends at 990 with a torn tail after it. Recovery restores to 1,000, then continues numbering at 1,001. The WAL now has no records 991–1,000, and the *next* restart that has to replay from an older snapshot hits a gap and refuses to start. *Planted and caught* by `TestSnapshotWaitsForDurability`. The first version of that test **hung** on failure instead of failing: its cleanup waited on the held fsync. A test that can hang is a bug too. Gated tests now release the gate in `t.Cleanup`.

3. **The WAL is pruned while only one snapshot exists.** *Real, found by a test.* The writer kept 2 snapshots but pruned the WAL below the oldest *listed*, even when only one was listed. Damage that single snapshot file, and the data is gone: the snapshot can't be read, and the WAL from seq 1 has been deleted. Now pruning waits for two snapshots, and keeps the WAL from the older one. *Planted afterwards* (pruning with one snapshot) and caught by `TestRecoveryFromWALWhenOnlySnapshotDamaged`, but only after that test was fixed to use small segments. With one big segment, nothing could be pruned, so the bug went **uncaught**: a test that can't exercise the path proves nothing.

4. **The manifest disappears in a crash.** On Windows a crash during the manifest's replacing rename can leave no `manifest.json`. Without a fallback, recovery sees "no snapshots" and replays the WAL from seq 1, but the WAL was pruned, so the server refuses to start even though a perfectly good snapshot sits on disk. The fallback (the `.tmp`, then a scan) fixes that. The fallback's first version made the writer **list a snapshot twice** (section 2), which `TestRecoveryWithoutManifest` caught because it checks that the writer had no failures.

5. **The snapshot forgets the last match id.** The engine restores with `LastMatchID = 0`, and its next trade is numbered match 1 again. Clients receive two different fills both called "match 1", the fill log has a repeated id, and anyone reconciling by match id merges two trades. *Planted*: the restarted engine **refused to start**, because the fill log was already ahead of the recovered engine. That check exists for exactly this.

---

## 5. Go idioms introduced

**Arrays as values.** `nodes [4096]orderNode` is an *array*, not a slice: a fixed-size value, so `nc.nodes = old.nodes` copies all 192 KiB in one assignment. A slice assignment would copy only the three-word header and share the data. That difference is the whole copy-on-write mechanism.

**Bit shifts for indexing.** `i >> 12` divides by 4,096, and `i & 4095` takes the remainder, because 4,096 is a power of two. It's a common trick for fixed-size blocks, and the compiler would make the same choice for constant divisors.

**Type aliases.** `type State = engine.State` (note the `=`) makes `snapshot.State` *the same type*, not a new one, so values pass between the packages without conversion. Without the `=`, it would be a distinct type, like `type Price int64` in milestone 1.

**`encoding/json` with struct tags.** `` Seq uint64 `json:"seq"` `` names the JSON key. `json.MarshalIndent` writes readable output, and `json.Unmarshal` fills a struct, ignoring unknown keys ([package encoding/json](https://pkg.go.dev/encoding/json)). The manifest is JSON because people read it when something goes wrong; the data files are binary because the engine reads them.

**`reflect.DeepEqual`.** It compares two values recursively: slices element by element, structs field by field ([reflect.DeepEqual](https://pkg.go.dev/reflect#DeepEqual)). The tests and replay-check use it on `engine.State`. Its catch: a `nil` slice and an empty slice are *not* equal. That's why `Decode` leaves empty sides `nil`.

**`os/exec`.** `exec.Command(path, args...)` builds a child process, `cmd.StderrPipe()` gives a stream of its log, `cmd.Start()` runs it, `cmd.Process.Kill()` kills it (SIGKILL / `TerminateProcess`), and `cmd.Wait()` reaps it ([package os/exec](https://pkg.go.dev/os/exec)). The harness uses all of them on the real binary.

**`TestMain`.** A `func TestMain(m *testing.M)` in a package's tests runs *instead of* the tests directly. It does setup (here, building the engine binary once), calls `m.Run()`, does teardown, and calls `os.Exit` with the result ([testing: Main](https://pkg.go.dev/testing#hdr-Main)).

**`t.Cleanup` order matters.** Cleanups run last-in, first-out. Registering `t.Cleanup(g.open)` *after* `start` (whose cleanup closes the server) means the gate is opened *before* `Close` runs, so a failing test can't hang on its own held fsync.

**A regular expression to read a log.** The harness compiles the pattern `msg="gateway listening" addr=(\S+)` once, at package init, with `regexp.MustCompile`, and `FindStringSubmatch` returns the captured address. Structured `slog` output makes logs easy to parse this way.

---

## 6. Self-check questions

1. A snapshot freezes a book with 10 chunks. During the snapshot the engine writes to orders in chunks 2, 2, 7 and 2. How many chunks are copied, which ones, and what does `Thaw` do with each of the 10 afterwards?
2. Why must the snapshot writer wait for `durable ≥ seq` before writing, under group commit? Describe the bad state on disk if it didn't, and why it only bites at a *later* restart.
3. Why does the writer refuse to prune the WAL until two snapshots are listed?
4. The crash test kills the process and checks recovery. Why can't it validate fsync, and what in this repo does?
5. `replay-check` finds a fill-log record whose match id the WAL replay never produced. What does that mean happened?

<details>
<summary>Answers</summary>

1. **Two chunks are copied**: chunk 2, on its first write (later writes to chunk 2 go to the copy, which is already at the current epoch), and chunk 7. At `Thaw`, chunks 0, 1, 3, 4, 5, 6, 8 and 9 are still the same pointers the snapshot shared, so their epoch is updated and they're unshared. The originals of chunks 2 and 7 aren't in the book any more (the copies replaced them), so they go to the spare pool for the next snapshot's copies.

2. Under group commit the engine applies commands up to about 1 ms before they are durable, so a freeze at seq *S* can include commands the WAL hasn't fsynced. If the snapshot were published and the power then failed, the disk would hold a snapshot at *S* and a WAL ending before *S*. The restart that follows works: it restores *S* and numbers new commands *S* + 1 onward. But the WAL now lacks the records between its old end and *S*. A **later** restart that has to start from an older snapshot (because the newest is damaged, or for replay-check) replays the WAL across that hole, finds a seq gap, and refuses to start. Waiting for durability means the snapshot's commands are always in the WAL too.

3. Pruning below the oldest kept snapshot is only safe if a second snapshot exists to fall back on. With one snapshot, pruning would make that single file the only record of everything before it. If it's damaged, the data is gone. With two, the older one plus the WAL after it is always intact, so any one damaged snapshot is survivable.

4. Killing a process doesn't lose the OS page cache. Every byte the engine had written, synced or not, still reaches the disk, so the test sees the same files under `always`, `group` and `never`. It passes even with `never`, and runs some rounds with `never` to show it. Fsync is validated by `internal/server/durability_test.go`, which wraps the WAL file so fsync can be **held** (no ack arrives until it's released) or made to **fail** (no ack or fill is ever sent for that batch), and by the measured throughput difference in the benchmark.

5. The engine **released a fill for a command that isn't in the WAL**. Either an event escaped before its command was durable (a durability-gate bug), or the WAL lost records that were acknowledged (a storage problem). Both mean a client was told about a trade the system's own history doesn't contain. That's why replay-check fails on it, while it tolerates *missing* fill-log records (released after the last fill-log write, or never released).

</details>
