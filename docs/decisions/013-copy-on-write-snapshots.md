# 013 — Snapshots: copy-on-write over a chunked slab

- **Status:** accepted (milestone 5). Copy-on-write rather than the stop-the-world fallback, and the snapshot triggers (every 1,000,000 commands or every 10 s, whichever comes first), were chosen with the user.
- **Code:** `internal/book/cow.go`, `internal/book/book.go`, `internal/engine/snapshot.go`, `internal/engine/engine.go` (`Run`), `internal/snapshot/writer.go`, `internal/snapshot/format.go`, `internal/snapshot/manifest.go`, `internal/snapshot/recover.go`

## Context

PRD 5.4:

> The engine goroutine must not pause to serialise the book, because that pause lands in the latency histogram the benchmark measures. Required design: on a snapshot trigger the engine records the current seq and hands a copy-on-write view of the book to a snapshot goroutine (immutable price-level slabs are shared; levels mutated after the trigger are copied first), which serialises off-thread and writes atomically.

In milestone 1 every book kept its orders in one flat `[]orderNode` slice. Sharing that with another goroutine while the engine mutates it in place is a data race. Copying all of it is the stop-the-world fallback.

## Options considered

1. **Stop-the-world copy** (the PRD's fallback). The engine copies the whole slab (48 bytes per order slot, so about 1 ms per 100,000 orders), then serialises off-thread. It is simple, and it leaves the hot path untouched. But every snapshot causes a pause proportional to book size, which would have to be reported as its own benchmark row.
2. **A shadow replica engine** fed the same command stream, snapshotting itself. There is no pause at all, but it doubles the matching CPU and memory, and it isn't what the PRD describes.
3. **Copy-on-write over a chunked slab** (chosen).

## Decision

### Storage: fixed-size chunks

The order slab is now `chunks []*chunk`. Each chunk holds **4,096 order slots** (4,096 × 48 bytes = 192 KiB), plus an `epoch`. A slot index `i` lives at `chunks[i >> 12].nodes[i & 4095]`.

Chunks are separate heap objects whose addresses never change. That is what lets a snapshot share them, and it also removes milestone 1's "pointer into a slice that `append` may move" hazard: growing the slab adds a chunk and never moves existing ones.

### The access discipline

- `node(i)` returns a slot **for reading**.
- `nodeW(i)` returns a slot **for writing**. If its chunk's epoch is older than the book's, a snapshot may still be reading that chunk, so `nodeW` first replaces it with a private copy (`copyChunk`, a 192 KiB array copy), then returns the slot in the copy.

**Every** write to an order goes through `nodeW`: the maker's quantity in `Submit`, linking in `rest`, unlinking neighbours in `unlink`, and clearing in `release`. A write through `node` would mutate memory a snapshot is reading. A planted bug doing exactly that, in the fill loop, was caught at once (below).

### Freeze, share, thaw

- **`Book.Freeze()`**, on the engine goroutine, between two commands:
  - copies each price level's `(price, head)` into the frozen view: O(levels);
  - copies the chunk *pointer* list: O(chunks);
  - makes sure the spare pool holds at least as many chunks as are shared, so any copies needed while frozen never allocate (PRD 6);
  - increments the book's `epoch`, which marks every existing chunk as shared.

  **No orders are copied.**
- **`Engine.Freeze()`** freezes every book and records `Seq`, `LastOrderID` and `LastMatchID`. That is the snapshot's seq: the last command applied.
- **The snapshot goroutine** reads the frozen view (`Frozen.Each` walks each level's FIFO chain through the shared chunks) while the engine keeps trading. The engine only ever writes to chunks at the current epoch: either new ones, or copies.
- **`Book.Thaw(f)`**, on the engine goroutine, once the snapshot has been written:
  - chunks nobody copied are still live, so their epoch is updated and they are unshared;
  - chunks that *were* copied leave the original orphaned, and it goes to the spare pool for next time.

  At most one snapshot is in flight per engine.

### Triggers (chosen with the user)

*(Milestone 7)* A clean shutdown also writes a snapshot, after the final fsync, unless nothing changed since the last one or durability failed ([decision 019](019-graceful-shutdown.md)). The next start then replays no WAL.

A snapshot is taken after **1,000,000 commands** (`-snapshot-every`) or **every 10 s** (`-snapshot-interval`), whichever comes first, and only if at least one command arrived since the last one. `Engine.Run` checks the count after each command and selects on a ticker. If a trigger fires while a snapshot is still in flight, another is taken as soon as it comes back.

### Writing: durability first, then atomically

The writer goroutine (`snapshot.Writer`):

1. **Waits until `durable.Seq ≥ snapshot.Seq`.** Under group commit the engine runs ahead of the disk. A snapshot published first could, after a power failure, contain commands the WAL lost. The next recovery would then find the WAL ending before the snapshot, and new commands numbered from the snapshot would leave a gap. `TestSnapshotWaitsForDurability` holds fsync and checks that no snapshot is published.
2. Materialises the state, encodes it (the file format below), and writes it with **`fsutil.WriteFileAtomic`**: write `<seq>.snap.tmp`, fsync it, rename it over `<seq>.snap`, fsync the directory. A crash leaves the old file or the new one, never a mix.
3. Adds the snapshot to **`manifest.json`**, also written atomically, keeping the newest **2**. Only listed snapshots are used for recovery. A snapshot file that exists but isn't listed (a crash between steps 2 and 3) is ignored and later removed.
4. **Prunes**, but only once 2 snapshots are listed: snapshot files that aren't listed, WAL segments whose records are all older than the *older* kept snapshot (PRD: "deletable once a snapshot's seq exceeds its last seq"), and fill-log segments before that snapshot's last match.
5. Returns the snapshot to the engine to thaw, whether or not writing succeeded.

A failed snapshot is logged and counted, but isn't fatal: durability comes from the WAL, and a snapshot only shortens recovery.

### The two-snapshot rule (a design flaw found by a test)

The first version pruned the WAL below the oldest listed snapshot even when only **one** was listed. `TestRecoveryFallsBackToOlderSnapshot` corrupted that single snapshot and recovery failed: the WAL from seq 1 had already been deleted, so the data was unrecoverable.

Now pruning waits until 2 snapshots are listed, and keeps the WAL from the older one onward. That means:
- **one damaged snapshot is always survivable**: recovery falls back to the older one, or, with only one snapshot, replays the unpruned WAL from seq 1 (`TestRecoveryFromWALWhenOnlySnapshotDamaged`);
- **two damaged snapshots are not**, and recovery then refuses to start.

### Snapshot file format

```
"MTCHSNP" version(1)
seq u64, last_order_id u64, last_match_id u64, n_instruments u32
per instrument: symbol_len u8, symbol, tick i64, lot i64,
                n_bids u32, bids (priority order), n_asks u32, asks (priority order)
                each order: id u64, client u32, clordid u64, price i64, qty i64   (36 bytes)
crc32c u32 of everything before it
```

The file stores only what determines future behaviour: the orders in priority order, and the three counters. Slab layout, free lists, chunk epochs and hash-table seeds are not stored. `book.Load` rebuilds a book by resting the orders in priority order, which recreates every FIFO queue exactly.

`TestLoadBehavesIdentically` checks that the loaded book then produces **identical results and fills** for 5,000 further commands. `Decode` rejects every single-byte flip, every truncation, and trailing bytes.

### Recovery (`snapshot.Recover`)

1. Read the manifest.
2. Load the newest snapshot that decodes and matches its manifest entry, falling back to the older one if not.
3. `engine.Restore`.
4. Replay the WAL records after the snapshot's seq, requiring each to be exactly the next seq.
5. If no snapshot is usable, replay from seq 1, which fails clearly if the WAL has been pruned.

The server also refuses to start if the fill log reaches a match the recovered engine never produced (decision 014).

## Measured costs

- **Freeze** is O(instruments + price levels + chunks). It never touches orders.
- **The first write to a shared chunk** copies 192 KiB. In `TestFrozenViewSurvivesHeavyTradingAcrossChunks`, 20,000 trading operations during one snapshot copied 5 of 5 chunks in the first cycle and 3 of 5 in later ones. Each chunk is copied at most once per snapshot. The effect on p99.9 latency is to be measured in milestone 9 with snapshots every 10 s, as PRD 5.4 requires.
- **Every order access** goes through a two-level index (`chunks[i>>12].nodes[i&4095]`), and every write adds one epoch comparison. The full differential suite and the engine's zero-allocation tests pass unchanged. The cost in nanoseconds per operation is for the milestone 9 benchmark.
- **Memory:** up to one spare chunk per live chunk (192 KiB each), so book memory can double during a snapshot. Spares are reused, so steady state allocates nothing, including while frozen (`TestZeroAllocWhileFrozen`).
- **Minimum footprint:** each book starts with one 192 KiB chunk, even if empty.
