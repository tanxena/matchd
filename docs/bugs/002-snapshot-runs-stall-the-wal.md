# 002: with snapshots on, WAL fsyncs stall and the whole intake path stops

- **Status:** open observation. The mechanism below is measured; its cause on the disk side isn't established, and nothing is fixed.
- **Seen:** in every `snapshots-on` run of the committed benchmark (`bench/results/full/*-snapshot-snapshots-on-*.json`), and reproduced on 2026-10-13 with a 20 s run.
- **Doesn't affect:** matching-core latency, or correctness: the durability gate holds, and nothing is released early.

## What the benchmark shows

In the snapshot row, core latency is the same with snapshots on and off, so copy-on-write keeps the engine goroutine out of it. End-to-end latency isn't the same: from the result files' end-to-end histograms,

| runs | commands answered in more than 50 ms |
|---|---|
| 5 × `snapshots-on` (6 snapshots per run) | 10,787–11,927 of about 1.2 million per run (about 0.9%) |
| 5 × `snapshots-off` | 0 |
| 5 × `mix-resting` (same configuration, snapshots off) | 0 |

That's about 1,800 commands per snapshot, about 90 ms of traffic at 20,000/s. The WAL's *mean* fsync time is the same with and without snapshots (1.4–1.7 ms), so the cost is a few long stalls, not a general slowdown.

## What a reproduction shows

The reproduction ran an engine with `-snapshot-interval 5s` and loadgen at 20,000/s for 20 s, sampling `/metrics` every 3–4 ms.

1. **Long WAL fsyncs happen in snapshot runs.** `matchd_wal_fsync_stalls_total` (fsyncs over 25 ms, decision 022) counted 2 in one run and 4 in another, of 31–78 ms each. They land 1–3 s *after* a snapshot is written, not during the write (each snapshot took 10–17 ms).
2. **During such an fsync, nothing new reaches the engine.**
   - Through one 78 ms fsync, `matchd_events_held` stayed flat at 186 and no command was released.
   - The engine wasn't receiving commands, so the sequencer, which appends each batch to the WAL before forwarding it, was blocked.
   - When the fsync returned, 8,000–12,000 events were held at once, waiting for the next fsync.

   That fits Windows serialising a `WriteFile` to a file behind a `FlushFileBuffers` in progress on the same file. **Unverified:** it isn't confirmed by a direct test.
3. So one slow fsync delays **intake** as well as release. Every command arriving during it waits for it, then for the next fsync.

## Not known

- **Why snapshot activity slows a later WAL fsync.** A snapshot creates, fsyncs and renames a file, rewrites the manifest, and deletes old snapshot files. NTFS metadata (journal) flushing triggered by that activity is a guess, not a finding. The Defender exclusion was in place for these runs.
- **Whether Linux behaves the same.** Not measured (README: Windows-only numbers).

## Possible directions (not chosen)

- Write snapshots to a different volume from the WAL.
- Stop the WAL append from waiting behind an fsync, e.g. by appending into a fresh segment while the old one syncs.
- Spread snapshot I/O, or snapshot less often.

Each changes the durability design (decisions 011–013) and is the user's call.

## Reproduce

```powershell
bin\engine.exe -data $env:TEMP\matchd-bench\snapprobe -fsync group:1ms -listen 127.0.0.1:19000 -http 127.0.0.1:19100 -instrument BENCH:1:1 -snapshot-every 0 -snapshot-interval 5s
bin\loadgen.exe -addr 127.0.0.1:19000 -http 127.0.0.1:19100 -mix resting -rate 20000 -warmup 3s -duration 20s -out probe.json
```

Then look for `durability stall: slow fsync` in the engine's log, and at `e2e_hist` in `probe.json`.
