# 022 — Slow consumers are judged by time, and a durability stall is visible

- **Status:** accepted (milestone 9). Chosen with the author, after a benchmark smoke run lost a client to a stalled disk:
  - the durability gate stays;
  - the slow-consumer policy changes: a client is slow only if it fails to drain within `WriteTimeout`, never because a burst released after a durability stall made its queue long;
  - option (b) of the two offered;
  - durability stalls get a metric.
- **Amends:** [decision 009](009-gateway-sessions.md)'s slow-consumer policy ("a full queue disconnects at once") and [decision 016](016-market-data.md)'s fan-out note.
- **Code:** `internal/gateway/outqueue.go` (new), `internal/gateway/session.go` (`Deliver`, `SlowConsumer`, `writeLoop`, `writeFailed`; `deadlineWriter` since review F-001), `internal/wal/wal.go` (`fsync`, `SyncInProgress`), `internal/server/metrics.go`, `cmd/engine` (`-fsync-stall`)

> **Superseded in part (2026-10-13, `15cf825`):** rule 3's write deadline is no longer set only at the writer's explicit flushes: every socket write, including the write buffer's own flushes when it fills, sets a fresh `WriteTimeout` deadline (`deadlineWriter`; review F-001, under rule 3 below).

## What happened

In the benchmark smoke run (`10-balanced-resting-r1`), the WAL completed **one** fsync in a 5-second window where `group:1ms` normally does hundreds. The disk had stalled. The durability gate did its job: no answer was released before its command was on disk. But the load generator kept sending, so answers piled up in the publisher (up to `MaxPending`, 65,536 events). When the disk came back, they were released at once into each client's outbound queue. A queue that could hold 4,096 events overflowed, and the publisher disconnected that client as a **slow consumer**. The client wasn't slow: it was reading as fast as answers came, and the burst was the engine's.

Under decision 009's rule ("full queue, disconnect"), any durability stall longer than about (queue size ÷ a client's event rate) disconnects every busy client. A stalled disk then becomes a mass disconnect, and the clients reconnect to an engine that is fine.

## The options

- **(a) Release a post-stall backlog in bounded chunks,** pausing whenever a destination queue is near full until its writer drains it.
- **(b) Let the queue exceed its soft limit during a burst,** and judge slowness by time: a client is slow if it stays over the limit for longer than `WriteTimeout`, or if a socket write blocks for longer than `WriteTimeout`.

**Chosen: (b).**

**Why not (a):** the publisher releases strictly in sequence order, across all clients, because the durability gate is a prefix of one queue. Pausing the release until one client's queue drains would make every other client wait for that one, which is exactly what decision 009 exists to prevent ("one slow client must never stall the engine, and with it everyone else"). A genuinely slow client would still need a time-based rule to be cut off. (a) would add per-client flow control and still need (b)'s rule.

## The policy

Each session's outbound queue (`outQueue`) has:

| limit | value | meaning |
|---|---|---|
| **soft** | `-out-queue` (`OutQueue`, default 4,096 events) | a client may be above it only briefly |
| **patience** | `-write-timeout` (`WriteTimeout`, default 10 s) | how long a client may stay above soft before it is a slow consumer |
| **hard** | soft + `publisher.MaxPending` (4,096 + 65,536) | the memory bound: refused at once |

A client is a **slow consumer**, and is disconnected, when any of these happens:
1. its queue has been above soft continuously for longer than the patience, checked when the next event arrives for it. Draining to soft or under ends the over-soft period;
2. its queue reaches the hard limit;
3. its writer's socket write blocks for longer than `WriteTimeout` (the writer was always bounded by this; it is now counted as a slow consumer too, rather than as a plain write error).

   *(Review F-001: until then the deadline was set only at the writer's explicit flushes. The 32 KiB write buffer also writes to the socket by itself when it fills, and those writes ran under a deadline set at the last flush. A session idle for longer than `WriteTimeout` that then got a burst over 32 KiB failed at once, and a client reading perfectly well was cut off with all its fills. Every socket write now sets its own deadline (`deadlineWriter`). `TestIdleThenBurstIsNotASlowConsumer` reproduces the old failure, with 0 of 599 fills delivered.)*

All three are counted in `matchd_slow_consumer_disconnects_total`, and the session's close reason names which one it was.

**Why the hard limit can't be reached by a burst alone:** the publisher holds at most `MaxPending` events in total. Beyond that it stops taking events from the engine, which back-pressures the engine (decision 012). So a post-stall release can add at most `MaxPending` events to one queue that was under soft. Reaching soft + `MaxPending` means the client also stopped draining.

**`WriteTimeout` 0** turns off rules 1 and 3, leaving only the hard limit.

### Costs

- **Memory.** A client that takes a burst can briefly hold up to about 70,000 events, about 7.8 MB at 112 bytes per event, against 460 KB before. The queue's memory is given back when it next empties (its capacity shrinks back to soft once it has grown past 4×), so this is per burst, not per session for ever.
- **A slow client is noticed later.** Under decision 009 it was cut off the instant its queue filled; now it may stay up to `WriteTimeout` (10 s by default) behind. During that time it costs memory, not other clients' latency, because delivery is still a non-blocking push.
- **A client that is behind gets its answers late.** That's the honest behaviour: it gets every answer, in order, or is disconnected and knows its stream broke. Nothing is ever dropped silently.

### What didn't change

- **The durability gate:** nothing is released before it is on disk.
- **The publisher never blocks:** `Deliver` is still a push under a short mutex, never a wait on a client.
- **Order:** events reach a session in the order they were released.

## The writer

The bounded channel the writer read from is replaced by `outQueue`: a slice under a mutex, plus a one-slot `ready` channel that the push signals. The writer takes up to 256 events at a time (`pop`) and writes them in order. Before each event it checks for a control reply without waiting, so a heartbeat answer or a fatal reject isn't queued behind a long burst. It flushes when everything is written, as before.

## Durability stalls are visible

| metric | meaning |
|---|---|
| `matchd_wal_fsync_stalls_total` | fsyncs slower than the stall threshold (`-fsync-stall`, default **25 ms**; this laptop's SSD averaged 1.4–2.4 ms in the smoke runs, and 1.33–1.66 ms in the committed order-mix runs, `bench/results/full`) |
| `matchd_wal_fsync_in_progress_seconds` | how long the fsync now running has taken so far, 0 when none is: a stuck disk shows here **before** the fsync returns |

Each stall is also logged at Warn (`durability stall: slow fsync took=… threshold=…`). All fsyncs, including the one before a segment roll, which wasn't timed before, now go through one function (`Log.fsync`) that times them.

## Tests

- **`TestDurabilityStallBurstDoesNotDisconnect`** (server):
  - setup: soft limit 32; three traders pipelining 6,000 resting orders each, reading every answer; a market-data subscriber reading everything;
  - a third of the way in, the next fsync is delayed by **50 ms**;
  - asserts: every answer arrives, **zero** slow-consumer disconnects (`/metrics` agrees), a queue really did go above the soft limit (peak 275 events against 32, so the test exercised the burst), and `matchd_wal_fsync_stalls_total` ≥ 1 with the warning logged.
  - Under decision 009's rule, planted back in, the clients are disconnected and only about 300–500 of 18,000 answers arrive.
- **`TestPersistentlySlowReaderIsDisconnected`** (server): a subscriber that reads one message every 20 ms, far slower than the market data, is disconnected as a slow consumer.
- **`TestOutQueuePolicy`** (gateway, fake clock):
  - a burst above soft that drains within the patience is accepted;
  - staying above soft longer than the patience is refused;
  - the hard limit refuses at once;
  - patience 0 applies only the hard limit.
- **`TestOutQueueOrderAndReuse`:** events come out in order across growth and compaction, and a burst's memory is given back.
- **The existing slow-consumer tests** (`TestSlowConsumerDisconnected`, `TestSlowConsumerDoesNotMoveEngineLatency`, i.e. acceptance 5, `TestSlowMarketDataSubscriber`, `TestDeepSnapshotUnreadDisconnects`) now set `WriteTimeout` to 300 ms, the time a client may stay behind, where they used to set it to 0 to rely on the queue filling.
- **Planted, all caught:**
  - decision 009's rule (refuse at the soft limit), caught by the stall test;
  - no patience (never time out), caught by `TestOutQueuePolicy`. The end-to-end slow-reader test doesn't catch it, because its reader is also caught by the socket write deadline;
  - a slow fsync not counted, caught by the stall test.
