# 011 — WAL format: segmented records with CRC32C, and the torn-tail rule

- **Status:** accepted (milestone 4)
- **Code:** `internal/wal/wal.go`, `internal/wal/syncdir_*.go`, `internal/engine/codec.go`

## Context

PRD 5.4:
- every accepted command is written to a write-ahead log (WAL) before it is acknowledged;
- a record is "length, CRC32C, seq, timestamp, command bytes";
- the log is segmented, and a segment can be deleted once a snapshot covers it (milestone 5);
- "Torn tail (bad CRC at end of last segment) is truncated and logged; a bad CRC elsewhere is fatal."

## Decisions

### Files

```
<data>/wal/00000000000000000001.log    first record's seq, 20 digits
<data>/wal/00000000000000004097.log
```

- **Named by first seq.** Sorting names numerically gives log order, and finding the segment that holds seq *n* is a search of the names. A segment's last seq is its successor's first seq − 1, so deciding which segments a snapshot makes deletable (PRD 5.4: once the snapshot's seq exceeds the segment's last seq) needs only the file names. Milestone 5 implements the deletion.
- **Each segment starts with an 8-byte header:** `"MTCHWAL"` and a format version (1). A file that isn't one of ours, or is from a future format, is rejected before any record is read.
- **Rolling.** A new segment starts once the current one reaches 64 MiB (configurable). The old segment is fsynced and closed first, so a finished segment is always fully durable.
- **Strict directory contents.** Any other `.log` file in the directory is reported as corruption rather than ignored. A stray file is a sign that something is wrong.

### Records

```
offset  size  field
0       4     length   u32: bytes after the crc field (16 + payload)
4       4     crc      CRC32C (Castagnoli) of those bytes
8       8     seq      u64
16      8     time     i64, ns since the Unix epoch (from the sequencer)
24      n     payload  the encoded command
```

- **CRC32C, the Castagnoli polynomial,** rather than the IEEE CRC-32 used by zip. It detects errors at least as well, it is the checksum iSCSI standardised ([RFC 3720 §12.1](https://www.rfc-editor.org/rfc/rfc3720)), and modern CPUs have an instruction for it, which Go's [`hash/crc32`](https://pkg.go.dev/hash/crc32) uses.
- **The checksum covers seq, time and payload,** so a flipped bit in any of them is caught. The length is *not* covered: a corrupted length is caught because the checksum then fails over the wrong bytes, or because the length is out of range.
- **`MaxPayload` = 4 KB.** The largest command is under 300 bytes. The bound keeps a corrupt length from sending the scanner megabytes ahead, and it limits the torn-tail blind spot described below.
- **Seq must be exactly the previous seq + 1,** across records and across segments, and each segment's first record must match its file name. A gap or repeat is corruption, even when every checksum is good.

### Command encoding (the payload)

`engine.AppendCommand` and `engine.DecodeCommand` use a fixed binary layout per command kind: a kind byte, then fixed fields; `AddInstrument` adds a length-prefixed symbol of at most 255 bytes. The layout table is in `codec.go`.

This encoding is **persistent**: a log written today must replay tomorrow. Changing it requires a new WAL format version, never an in-place edit. Golden-bytes tests pin both `NewOrder` and `Cancel`. `Seq` and `Time` live in the record header, not in the payload.

### The torn-tail rule

A crash can interrupt a write partway, leaving a **torn** final record. A disk or bug can also damage bytes anywhere. The two must be told apart: truncating real corruption could silently drop acknowledged commands, while refusing to start over a torn tail would make every crash need manual repair.

**The rule.** Damage counts as a torn tail only if it is in the **last** segment **and** every byte after the damaged record is zero (or there are none). Anything else is `ErrCorrupt`, and the server refuses to start.

| what the scanner finds | verdict |
|---|---|
| fewer bytes than a record header, at end of file | torn: truncate |
| a record whose length runs past end of file | torn: truncate |
| bad checksum, then end of file or only zero bytes | torn: truncate |
| length out of range, and everything from there on is zero (a zero-filled tail reads as length 0) | torn: truncate |
| length out of range, followed by non-zero data | **corrupt** |
| bad checksum followed by any non-zero byte | **corrupt** |
| any damage in an earlier segment | **corrupt** |
| a seq gap or repeat, a bad header, a name that doesn't match the first record | **corrupt** |
| last segment holds only part of its 8-byte header | torn: remove the file |

Why zeros count as "nothing": some file systems extend a file with zero-filled blocks whose data never got written before a crash. A later, separate append would have written non-zero bytes (a record header, at the very least).

Why a garbage length followed by data is corruption, not a tear: the length is the *first* thing a record write puts down, so a torn write always has a valid length. A length that is nonsense, with data after it, means bytes changed after they were written.

**Known blind spot:** if a record's length is corrupted to a value that is in range but points past the end of the file, and the record is within the last `MaxPayload` bytes of the last segment, it looks exactly like a torn final record and is truncated. Any intact records after it in that window would be lost. This requires real on-disk corruption of a length field in the last 4 KB of the log; the 4 KB bound keeps the window small.

Truncation is logged at warning level, with the segment, the offset, the number of bytes, and the last good seq.

### Write path and concurrency

- `Append` encodes into an in-memory buffer; `Flush` writes the whole batch with **one** `Write` call. The sequencer calls both from its single goroutine.
- `Sync` fsyncs the current segment and records how far it got. It can run on **another goroutine** (the group-commit syncer) at the same time as `Flush`:
  - `mu` guards the current file and its size. `Sync` holds `mu` only long enough to read the file handle and the "appended so far" seq, **never during the fsync itself**, so a slow fsync never blocks an append.
  - `syncMu` serialises `Sync` with segment rolls, so a file is never closed while an fsync on it is in progress.
  - `Sync` marks durable only the seq that was appended *before it started*. Records written during the fsync may or may not be covered, so they are not counted.

### Directory entries

On Linux, fsyncing a new file makes its *contents* durable but not necessarily its *name* in the directory. So after creating a segment, the directory itself is fsynced ([fsync(2)](https://man7.org/linux/man-pages/man2/fsync.2.html)).

On Windows this isn't possible through Go: `os.File.Sync` on a directory handle fails, because `FlushFileBuffers` needs a handle opened for writing. We rely on NTFS's metadata journal and **can't test the guarantee locally**. This is recorded in [decision 010](010-windows-gotchas.md).

## Costs

- **Strictness has a price.** A single flipped bit in the middle of the log stops the server until a human decides what to do. That is deliberate: the alternative is silently losing or inventing history.
- **Recovery reads every segment in full.** It is O(log size). Snapshots (milestone 5) bound this by letting old segments be deleted.
- **No preallocation.** Segments grow by appending, which costs file-system metadata work on some writes. Preallocating would make zero-filled tails the normal case; the rule above already handles them, so this is a possible later optimisation.
- **The blind spot** described above.
