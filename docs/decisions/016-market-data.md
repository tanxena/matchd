# 016 — Market data: engine-assigned md_seq, snapshot then deltas, resend from a per-instrument ring

- **Status:** accepted (milestone 6). Four choices were made with the user: the ring is per instrument, 65,536 events (the Section 10 question); `md_seq` is assigned by the engine; a resend that is too old gets a reject and the client re-subscribes; `engctl` names instruments by id until milestone 7.
- **Code:** `internal/engine/engine.go` (`nextMD`, `EvTrade`), `internal/engine/types.go`, `internal/publisher/marketdata.go`, `internal/gateway/session.go`, `internal/proto/messages.go`, `internal/client/feed.go`, `internal/server/server.go`, `cmd/engctl` (`book`, `subscribe`)

## Context

PRD 5.6:
- every book-changing event is published with a per-instrument `md_seq`;
- subscribers receive deltas (price level add, update or remove, plus trades);
- a new subscriber gets a `Snapshot` (the full book and the current `md_seq`), then deltas from there;
- consumers detect gaps from `md_seq` and recover with `Resend(from_seq)`, served from a bounded ring buffer; beyond it, the client must re-snapshot.

Acceptance 4: a subscriber joining mid-stream gets a snapshot, detects an induced gap, resends, and converges to the same book as a subscriber that was there from the start.

## Decisions

### What is published: market by price, plus trades

Market data is **market by price**: each `BookDelta` gives one price level's **new total**, with 0 meaning the level is gone (the engine already emitted these, decision 006). Milestone 6 adds a **`Trade`** event per match: anonymous, with price, quantity, the aggressor's side and the match id. It comes right after the match's two private fill events. Within a command, a level's trades come before that level's delta, so a consumer sees the prints, then the level's new size.

### md_seq: assigned by the engine (chosen with the user)

`Engine.nextMD(instrument)` numbers each instrument's deltas and trades gap-free from 1. Because the engine assigns it, `md_seq` is part of the **deterministic state**:
- replay reproduces it;
- snapshots store it, which is why the snapshot file format moved to **version 2** (version 1 files are refused, and none existed outside tests);
- `replay-check` verifies it along with everything else;
- after a restart, numbering continues exactly where it stopped (`TestMarketDataAfterRestart`).

The alternative, numbering in the publisher, would restart from 0 at every server start and couldn't be checked by replay.

### Where snapshots come from: the publisher's own view

The publisher keeps, for each instrument, a **price-level view** (price to total quantity, per side), built only from **released** deltas. That gives three properties:
- a snapshot is exactly the book as of the last released `md_seq`, and never shows anything that isn't durable yet;
- answering a snapshot never touches the engine or its goroutine;
- after a restart, the server seeds each view from the recovered engine's books and `md_seq` (`InitMarketData`).

### The subscription protocol

```
client → Subscribe(instrument)
server → Snapshot(instrument, md_seq = S, levels = n)
server → SnapshotLevel × n          bids best-first, then asks best-first; streamed by the writer
server → BookDelta / Trade with md_seq S+1, S+2, …
```

Subscribe and Resend arrive on gateway goroutines but are **handled on the publisher goroutine**, between events, through a request channel. When a snapshot at S is sent, no event after S has yet been fanned out to that subscriber, and from then on it gets every one. That leaves no gap and no overlap, with no locks.

**Market data needs no login:** it is public. A connection can be market-data only. Subscribing again sends a fresh snapshot.

Snapshots are sent as a header plus **one fixed-size frame per level**, rather than one variable-length frame, which keeps the protocol's "each message type has exactly one size" rule ([decision 008](008-wire-protocol.md)).

### Gaps and resend

A consumer that sees `md_seq` jump from N to N + k (k > 1) sends **`Resend(instrument, from = N+1)`**. The publisher answers from that instrument's ring with every event from N + 1 up to the latest, in order, before any later live event. The consumer ignores anything at or below what it has already applied.

**The ring (chosen with the user):** per instrument, the last **65,536** market-data events, about 7.3 MB per instrument at 112 bytes per event. Per instrument rather than shared, so a busy instrument can't push a quiet one's history out. It is allocated on an instrument's first market-data event.

**A resend older than the ring** gets **`Reject(ResendUnavailable)`**, and the client subscribes again for a fresh snapshot (chosen with the user, matching the PRD's "the client must re-snapshot"). The alternative, the server pushing a snapshot unasked, would mean a client could receive a snapshot at any moment.

The ring only holds events since this process started. After a restart, a resend for anything before the restart is refused, and the client re-snapshots.

### The client side: `client.Feed`

`Feed` is the reference consumer. It is a state machine:
1. awaiting snapshot;
2. loading its levels;
3. live;
4. recovering (a gap was seen and a resend sent).

It ignores duplicates, and on `ResendUnavailable` it subscribes again. `engctl book`, `engctl subscribe` and the tests all use it. A test hook, `Drop`, discards chosen deltas or trades to **induce gaps**. It never drops a snapshot: TCP doesn't lose a single frame, and a dropped snapshot once wedged a test's Feed forever.

### Delivery reuses everything that exists

- **Durability:** market-data events go through the same gate as everything else (decision 012). `TestMarketDataWaitsForFsync` holds fsync and sees no delta reach a subscriber.
- **Slow consumers:** fan-out is a non-blocking offer into each subscriber's session queue, exactly like client routing, and a full queue disconnects (decision 009; since milestone 9, a queue that stays over its soft limit for longer than `WriteTimeout`, [decision 022](022-slow-consumers-and-durability-stalls.md)). `TestSlowMarketDataSubscriber` shows it while a trader keeps trading.
- **Order within a session:** snapshot and resend answers keep their place among the session's events. The publisher creates two event kinds the engine never does: `EvResendUnavailable`, and `EvSnapshot`, a **marker** for where a snapshot goes. The snapshot's levels don't travel through the queue at all (next section).

### Snapshots are streamed outside the event queue (revised after review)

The first version queued **one event per level**. A book with more levels than the session queue (`OutQueue`, default 4,096) could then never be snapshotted to anyone: every subscriber to it would be disconnected as a slow consumer, however fast it read. Making the queue grow with the book was rejected: memory per session would be unbounded, and the queue size would stop meaning "how far behind a client may fall".

The design now:
1. On Subscribe, the publisher copies its level view into a sorted, **immutable** `publisher.Snapshot` (it already built this slice to sort the levels). It hands it over with `Subscriber.DeliverSnapshot`, which doesn't block.
2. The session puts the snapshot on a small side channel (at most 4 pending), and an `EvSnapshot` marker on its normal queue. The marker fixes the snapshot's place in the stream: after every event before it, before every event after it.
3. When the writer goroutine reaches the marker, it **streams** the header and every level frame straight from the snapshot, with blocking socket writes. Each flush is bounded by `WriteTimeout`. Then it goes back to the queue.

The writer can't read the publisher's own level maps in place: the publisher goroutine keeps changing them, so that would be a data race. The copy is what makes "directly from the book view" safe. It costs O(levels) on the publisher goroutine, the same as the sort.

**What it guarantees:**
- **A client that can't consume the snapshot is still disconnected.** If it stops reading, a socket write blocks past `WriteTimeout` and the session closes. If the market moves faster than it reads while the snapshot streams, the queue (which holds the events after the snapshot) fills, and it is a slow consumer as before.
- **A client that can consume it isn't, however deep the book.** A 4,000-level book goes through a 64-event queue (`TestDeepSnapshotBeyondQueue`).

### engctl names instruments by id (a deviation, chosen with the user; closed in milestone 7)

PRD 5.7 writes `engctl book <symbol>`, but the wire protocol identifies instruments by numeric id and has no symbol lookup. Milestone 6 uses `--instrument N`; milestone 7's instrument endpoint will let `engctl` resolve symbols. *(Done: `engctl book XYZ` resolves the symbol through `GET /instruments`, [decision 018](018-http-admin-and-instruments.md).)*

## Costs

- **Memory:** up to about 7.3 MB of ring per active instrument, plus the publisher's level view.
- **Snapshots cost a copy:** each subscription copies and sorts the instrument's levels on the publisher goroutine, and that copy lives until the writer has streamed it. A burst of subscriptions to a deep book costs publisher time while the copies are made, and memory until they're streamed.
- **A slow reader slows only itself, up to `WriteTimeout` per write:** while a snapshot streams, that session's writer sends nothing else. Its queue absorbs live events, and fills if the stream is too slow.
- **Resend sends everything from the gap to now,** even if the client only missed one message. That's simple and in order, at the cost of some duplicates, which the client ignores.
- **The event struct grew to 112 bytes** (an `MDSeq` field plus alignment), which every channel and queue pays for.
- **No unsubscribe message.** A subscription ends when the connection does. It wasn't required, and it's easy to add later.

## How it's tested

| test | shows |
|---|---|
| `TestMarketDataConvergence` (**acceptance 4**) | a from-start subscriber; a mid-stream joiner whose Feed drops every 37th md message once while trading runs (in one run, **59 gaps, 59 resends**); and a third subscriber's fresh snapshot all reach the same `md_seq` (about 4,400; it depends on how the two traders interleave) with identical books, which also equal the engine's own book after shutdown |
| `TestDeepSnapshotBeyondQueue` | a 4,000-level book reaches a subscriber whose queue holds 64 events: all levels, then live data, with no disconnects |
| `TestDeepSnapshotUnreadDisconnects` | a subscriber to an 8,000-level book that never reads is disconnected by the write deadline; trading carries on |
| `TestResendBeyondRingResnapshots` | with a 16-event ring, a 40-message hole gets `ResendUnavailable`, the Feed re-subscribes, and still converges |
| `TestMarketDataAfterRestart` | after a restart, a subscriber's snapshot has the recovered `md_seq` and book, and the next event continues the numbering |
| `TestMarketDataWaitsForFsync` | no market data before fsync |
| `TestSlowMarketDataSubscriber` | a non-reading subscriber is disconnected; trading carries on |
| `TestSubscribeUnknownInstrument`, `TestSubscribeAndHeartbeat` | a reject for an unknown instrument; an empty book's snapshot |
| `internal/client/feed_test.go` | the Feed's state machine with no server: snapshot, then deltas; duplicates; gap, then resend, then recovery; `ResendUnavailable`, then re-subscribe |
| `internal/engine/stream_test.go` | md events are gap-free per instrument, and every fill pair is followed by its trade |

**Planted bugs, all caught:**
- a trade that doesn't advance `md_seq`;
- a snapshot that claims `md_seq` + 1;
- a ring that never stores events;
- a Feed that applies past a gap;
- market data bypassing the durability gate;
- `md_seq` not restored at startup;
- `Close` leaving a snapshot frozen;
- a snapshot stream that stops at the queue size (the old limit);
- snapshot writes without a deadline.

**A test flaw found by repetition:** the convergence test failed 1 run in 20, when the final `md_seq` (4,440 = 37 × 120) was one the joiner dropped. A lost **last** message is only noticed when another one arrives, and none did. The test now stops dropping when trading ends, and sends one more order, whose delta reveals any lost tail. A production feed would send heartbeats carrying the latest `md_seq` for the same reason; v1 has none, so over TCP, where frames aren't lost, it doesn't need them.
