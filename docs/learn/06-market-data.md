# 06 — Market data: everyone sees the same book, even after losing messages

Until now, clients only heard about their *own* orders. Milestone 6 publishes the **book itself**: every change to every price level, and every trade, to anyone who subscribes. A subscriber can join at any moment and lose messages along the way, and must still end up with exactly the book everyone else sees.

This milestone builds:
- per-instrument **sequence numbers for market data** (`md_seq`), assigned deterministically by the engine;
- the publisher's own **price-level view** of each book, the source of **snapshots**;
- a **ring buffer** of recent messages, to serve **resends**;
- the client's **`Feed`**, which detects gaps and recovers;
- **`engctl book`** and **`engctl subscribe`**.

---

## 1. Concepts

### 1.1 What market data is

A market-data feed lets anyone keep a copy of the order book without seeing who owns which order. Ours is **market by price** (MBP; see [02-engine.md §1.3](02-engine.md#13-market-data-market-by-price-deltas)): instead of individual orders, it describes **price levels**, "the bid at 100 now totals 12 lots", plus **trade prints**, "5 lots traded at 101; the buyer was the aggressor". NASDAQ's [TotalView-ITCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHspecification.pdf) is the market-*by-order* equivalent, and the place to look up the vocabulary.

### 1.2 Snapshot, then deltas

A newcomer can't rebuild the book from deltas alone; it missed everything before it arrived. So it first receives a **snapshot**: the whole book, stamped with the `md_seq` it reflects, say 4,000. Then it applies deltas 4,001, 4,002, … in order. The two must join exactly:
- if the snapshot reflects 4,000 but the first delta it then gets is 4,002, it has a hole;
- if it then gets 3,999, it would apply a change twice.

Our publisher guarantees the join by handling the subscription **on its own goroutine, between two events**. At that instant it knows exactly which `md_seq` its book view reflects, and everything it fans out afterwards is newer.

### 1.3 Gaps and why sequence numbers matter

TCP doesn't lose bytes within a connection, so where do gaps come from? From everything around it:
- a consumer that dropped messages internally;
- a UDP multicast feed, which is how real exchanges publish market data (ITCH runs over [MoldUDP64](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/moldudp64.pdf));
- a reconnect;
- a bug.

Without numbering, a missed delta is **invisible**: the consumer's book is quietly wrong forever. With a **gap-free sequence number** per instrument, the consumer knows the instant something is missing: it saw 4,005 and then 4,007.

The recovery is to ask for the missing range again, **`Resend(from = 4,006)`**. The server keeps the last 65,536 messages per instrument in a **ring buffer**. If 4,006 is still there, it resends 4,006 onward, in order. If it's too old, it says so (`ResendUnavailable`), and the consumer starts over with a fresh snapshot. Real exchanges work the same way: MoldUDP64 has a "re-request server" for recent packets, plus a snapshot channel for late joiners.

### 1.4 Why the engine assigns md_seq

The numbering could be done anywhere downstream. Doing it **in the engine** makes it part of the deterministic state (milestone 2): replay after a crash produces exactly the same numbers, snapshots store them, and replay-check verifies them. So `md_seq` doesn't reset when the server restarts, and a subscriber reconnecting after a restart continues with the same numbering.

### 1.5 Market data and durability

Market data is subject to the same rule as everything else ([decision 012](../decisions/012-durability-and-fsync-policies.md)): **nothing leaves before the command that caused it is on disk**. If a delta escaped before its fsync and the machine then lost power, every subscriber would hold a book the recovered exchange never had. So deltas and trades wait in the publisher's durability gate like acks and fills, and the publisher's book view, the source of snapshots, is built only from **released** deltas.

---

## 2. Code walkthrough

### Engine: md_seq and trades

- **`nextMD`** ([engine.go:191](../../internal/engine/engine.go#L191)) bumps and returns the instrument's counter. `e.mdSeq` grows alongside `e.books`.
- **`delta`** stamps every `BookDelta` with `nextMD`.
- In **`newOrder`**, each match now appends three events: the taker's fill, the maker's fill, and an **`EvTrade`** print (client 0, aggressor side, match id, `md_seq`).
- **`Snapshot`/`State`/`Restore`** carry each instrument's `MDSeq`.
- **`snapshot.Encode`/`Decode`** write it after the lot size, which is why the snapshot file format is now **version 2**.
- **New event kinds:** `EvTrade` (7), plus two the **publisher** creates: `EvSnapshot` (8), a marker for where a snapshot goes in a session's stream, and `EvResendUnavailable` (9).
- `Event` gained an `MDSeq` field, bringing it to **112 bytes**.

### Protocol: five messages

| message | struct | payload |
|---|---|---|
| inbound `Resend` | [messages.go:498](../../internal/proto/messages.go#L498) | instrument, `from_md_seq` |
| `BookDelta` | [messages.go:528](../../internal/proto/messages.go#L528) | seq, time, instrument, `md_seq`, side, price, new level total |
| `Trade` | [messages.go:574](../../internal/proto/messages.go#L574) | seq, time, instrument, `md_seq`, match id, aggressor side, price, qty |
| `Snapshot` | [messages.go:620](../../internal/proto/messages.go#L620) | instrument, `md_seq`, number of levels |
| `SnapshotLevel` | [messages.go:650](../../internal/proto/messages.go#L650) | instrument, side, price, qty |

There is also a new reject reason, `ResendUnavailable` (105). Each message is still one fixed size.

### Publisher: `internal/publisher/marketdata.go`

- **`mdBook`** ([marketdata.go:29](../../internal/publisher/marketdata.go#L29)) holds, per instrument:
  - `mdSeq` (last applied);
  - `bids`/`asks` maps from price to total;
  - `ring`, indexed by `md_seq % size`;
  - `ringFirst` (the first `md_seq` stored since this process started);
  - `subs`.

  **`oldestAvailable`** ([marketdata.go:47](../../internal/publisher/marketdata.go#L47)) is the oldest `md_seq` still resendable: the larger of `ringFirst` and `mdSeq − size + 1`.
- **`InitMarketData`** ([marketdata.go:61](../../internal/publisher/marketdata.go#L61)) seeds an instrument's view at startup from the recovered engine.
- **`Subscribe`/`Resend`** ([marketdata.go:161, 167](../../internal/publisher/marketdata.go#L161)) are called by gateway goroutines. They put a `request` on `p.requests`, or give up if the publisher has stopped. **`Run`** gained a `case r := <-p.requests` that calls **`handle`** ([marketdata.go:178](../../internal/publisher/marketdata.go#L178)):
  - unknown instrument: an `EvReject` with `UnknownInstrument`;
  - subscribe: `sendSnapshot`, then add to `subs`;
  - resend: nothing if the request is beyond the latest; `EvResendUnavailable` if it's older than the ring; otherwise every ring event from `from` to the latest.
- **`Snapshot`** ([marketdata.go:219](../../internal/publisher/marketdata.go#L219)) is an immutable copy of one instrument's levels at an `md_seq`. **`sendSnapshot`** ([marketdata.go:234](../../internal/publisher/marketdata.go#L234)) builds it, sorted (bids high to low, asks low to high), and hands it over with `Subscriber.DeliverSnapshot`. The levels never go through the event queue, so a deep book doesn't need a deep queue.
- **`marketData`** ([marketdata.go:256](../../internal/publisher/marketdata.go#L256)) is called from `route` for each released delta or trade:
  1. check the `md_seq` follows the last (logging loudly if not);
  2. apply a delta to the level map;
  3. store the event in the ring;
  4. offer it to every subscriber, dropping closed ones and slow ones.
- **`offer`** ([marketdata.go:304](../../internal/publisher/marketdata.go#L304)) is a non-blocking deliver; on failure it counts a slow consumer and calls `SlowConsumer()`.
- `route` (in `publisher.go`) now sends deltas and trades to `marketData`, and creates a view on `InstrumentAdded`. `Subscriber` gained **`Closed()`**, and `SlowConsumer` must now be idempotent, because it can be called from both client routing and market-data fan-out.

### Gateway

The reader handles `Subscribe` by calling `pub.Subscribe(instrument, session)`, **without requiring login**, and `Resend` by calling `pub.Resend(...)`. `session` implements `Closed()`. **`appendEvent`** encodes `BookDelta`, `Trade` and `ResendUnavailable`, which goes out as a `Reject` with reason 105.

**Snapshots are streamed by the writer, outside the queue.**
- **`DeliverSnapshot`** ([session.go:91](../../internal/gateway/session.go#L91)) puts the snapshot on a small side channel `snaps` (at most 4 pending), then an `EvSnapshot` marker on `out`. It never blocks.
- When the writer reaches the marker, **`streamSnapshot`** ([session.go:397](../../internal/gateway/session.go#L397)) takes the snapshot and writes the header and every level frame from it.
- **`write`** ([session.go:426](../../internal/gateway/session.go#L426)) flushes under the write deadline whenever the buffer is full, so a client that stops reading is disconnected after `WriteTimeout`, while one that reads gets the whole book, however deep. Live events published meanwhile wait in `out`.

The first version queued one event per level, so a book deeper than the queue (4,096 by default) disconnected every subscriber, even fast ones. Streaming fixed that without making the queue grow.

### Server

At startup, for each recovered instrument, `Start` sums the resting orders per price (**`levelTotals`**, [server.go:472](../../internal/server/server.go#L472)) and calls `InitMarketData` with the instrument's `md_seq`. Config `MDRing` sets the ring size.

**A real bug was found and fixed here:** when shutdown caught a snapshot in flight, the engine goroutine exited before the writer handed it back, so the books stayed **frozen**, and any later `Engine().State()` panicked. `Close` now thaws a snapshot left on the return channel.

### Client: `internal/client/feed.go`

- **`Feed`** ([feed.go:26](../../internal/client/feed.go#L26)) holds the state (`awaitingSnapshot`, `loadingLevels`, `live`, `recovering`), the levels still to load, `mdSeq`, the `bids`/`asks` maps, the test hook `Drop`, and `Stats`.
- **`NewFeed`** ([feed.go:67](../../internal/client/feed.go#L67)) subscribes.
- **`Apply`** ([feed.go:85](../../internal/client/feed.go#L85)):
  - `Snapshot` resets the book (only while awaiting one);
  - `SnapshotLevel` fills it, and the Feed goes live after the last one;
  - `BookDelta`/`Trade` go to `sequenced`;
  - `Reject(ResendUnavailable)` re-subscribes.
- **`sequenced`** ([feed.go:126](../../internal/client/feed.go#L126)):
  - ignored before the snapshot is complete;
  - `md ≤ mdSeq`: a duplicate, ignored;
  - `md > mdSeq+1`: if live, a gap, so send `Resend(mdSeq+1)` and switch to recovering; if already recovering, ignore it (the resend will bring it);
  - otherwise apply it and go (back to) live.
- **`Book`** returns sorted levels, and **`String`** prints the top levels for `engctl`.
- `client.Dial` with client id 0 now connects **without logging in**, which is all market data needs.

### engctl

- **`engctl book --instrument N [--depth 10]`**, `cmdBook` ([main.go:456](../../cmd/engctl/main.go#L456)) subscribes, waits until the Feed is live, prints, and exits. It reports a reject, such as an unknown instrument.
- **`engctl subscribe --instrument N`**, `cmdSubscribe` ([main.go:481](../../cmd/engctl/main.go#L481)) prints every snapshot frame, delta and trade, the book once live, and any gap recovery it performs.

### Tests and results

- **`TestMarketDataConvergence`** ([marketdata_test.go:161](../../internal/server/marketdata_test.go#L161)) is **acceptance 4**:
  - two traders send 1,500 orders each, waiting for each answer;
  - subscriber A joins before trading starts;
  - halfway through, subscriber B joins, with a Feed that **drops every 37th** delta or trade, once;
  - after trading, dropping stops and one more order rests (its delta reveals a lost tail; see below);
  - then subscriber C joins fresh to learn the final `md_seq`.

  Result, in one run: final `md_seq` 4,424; B went through **59 gaps and 59 resends**; A, B and C have **identical books**, which also equal the engine's own book after shutdown. The final number varies a little from run to run with how the two traders interleave.

  **A test flaw found by running it 20 times:** one run's final `md_seq` was 4,440 = 37 × 120, so B dropped the *last* message, and nothing after it ever revealed the gap. That's why the closing order exists. Real feeds send heartbeats carrying the latest `md_seq` to catch exactly this.
- **`TestDeepSnapshotBeyondQueue`** ([deepsnapshot_test.go:41](../../internal/server/deepsnapshot_test.go#L41)): a 4,000-level book reaches a subscriber whose queue holds 64 events. It gets all levels, then the next trade and delta; nobody is disconnected.
- **`TestDeepSnapshotUnreadDisconnects`** ([deepsnapshot_test.go:76](../../internal/server/deepsnapshot_test.go#L76)): a subscriber to an 8,000-level book that never reads is disconnected by the 300 ms write deadline; a trader keeps getting acks.
- **`TestResendBeyondRingResnapshots`:** with `MDRing = 16`, B drops 40 consecutive messages. The resend is refused, B re-subscribes, and it still converges (1 re-snapshot).
- **`TestMarketDataAfterRestart`:** a subscriber after a restart sees the recovered book at the recovered `md_seq`, and the next event continues the numbering.
- **`TestMarketDataWaitsForFsync`:** with fsync held, no delta reaches a subscriber.
- **`TestSlowMarketDataSubscriber`:** a subscriber that never reads is disconnected; the trader keeps getting acks.
- **`internal/client/feed_test.go`:** the Feed's state machine over an in-memory pipe, with no server: snapshot then deltas, duplicates, gap then resend then recovery, `ResendUnavailable` then re-subscribe, and other instruments ignored.
- **`internal/engine/stream_test.go`:** per-instrument `md_seq` is gap-free, and every fill pair is followed by its trade.

**Running the real binaries** (`engine` plus `engctl subscribe` while four orders are placed):

```
Snapshot inst=1 md=0 levels=0
BookDelta md=1 inst=1 sell 101 -> qty 5
BookDelta md=2 inst=1 sell 102 -> qty 3
BookDelta md=3 inst=1 buy 99 -> qty 4
Trade md=4 inst=1 match=1 5 @ 101 (aggressor buy)
BookDelta md=5 inst=1 sell 101 -> qty 0
Trade md=6 inst=1 match=2 1 @ 102 (aggressor buy)
BookDelta md=7 inst=1 sell 102 -> qty 2
```

---

## 3. Design decisions

- [016: Market data](../decisions/016-market-data.md). MBP deltas plus trades; `md_seq` assigned by the engine (snapshot format v2); snapshots from the publisher's own view; the subscription protocol and why it has no gaps or overlaps; no login for market data; the per-instrument 65,536 ring; a reject then re-subscribe when a resend is too old; the Feed; reuse of the durability gate and the slow-consumer policy; ids in `engctl` for now; costs and tests.
- [006: The event model](../decisions/006-event-model.md) and [008: Wire protocol](../decisions/008-wire-protocol.md) have notes for the trade event, `md_seq`, and the new messages.

---

## 4. What breaks if

1. **There is no md_seq.** A subscriber's connection hiccups and it misses one delta, "level 101 is now 0". It keeps showing 101 × 5 forever. Its users see liquidity that isn't there, and try to buy it. Nothing in the stream reveals the problem, because every later delta is internally consistent. With `md_seq`, the very next message shows a jump, and recovery starts at once.

2. **The snapshot's md_seq is off by one.** The publisher sends a snapshot stamped S + 1 while its levels reflect S. The subscriber treats the real S + 1 delta as a duplicate and ignores it, and is permanently one change behind, with no gap ever detected. *Planted and caught*: the subscriber stuck at 4,424 waiting for 4,425, with exactly 1 "duplicate".

3. **Trades don't advance md_seq.** A trade carries the same `md_seq` as the delta before it. Every consumer sees a duplicate, drops the trade, and never knows a trade happened. *Planted and caught* by the engine's stream test, which checks numbering per instrument.

4. **The publisher builds snapshots from unreleased events.** Under group commit the engine is up to about 1 ms ahead of the disk. A snapshot built from the engine's live book would show orders whose commands aren't durable. If the power then failed, the recovered exchange would never have had those orders, but the subscriber would show them. Building snapshots only from **released** deltas makes that impossible. *Planted variant*, where market data skips the gate: caught by `TestMarketDataWaitsForFsync`.

5. **Shutdown leaves a snapshot frozen.** *Real, found by a test this milestone:* `Close` while a snapshot was in flight left every book frozen, because the engine had stopped before receiving the snapshot back. Nothing broke immediately. But any later use of the engine, here reading its state after `Close` (and, in milestone 7, snapshot-on-shutdown), panicked with "Freeze while a snapshot is still in progress". `Close` now thaws it.

6. **Snapshot levels go through the bounded event queue.** That was this milestone's first design. A 5,000-level book and a 4,096-event queue: every subscriber, however fast, is disconnected as a slow consumer before the snapshot finishes, and the book can't be watched by anyone. Growing the queue would let one client's snapshot take unbounded memory. *Planted variant*, a stream that stops at the queue's size: the deep-book test's subscriber never goes live.

---

## 5. Go idioms introduced

**`slices.SortFunc` with `cmp.Compare`.** `slices.SortFunc(levels, func(a, b lvl) int { return cmp.Compare(b.px, a.px) })` sorts in place using a three-way comparison: negative, zero or positive. Swapping `a` and `b` reverses the order, which is how bids sort high to low. `cmp.Compare` handles any ordered type ([package slices](https://pkg.go.dev/slices), [package cmp](https://pkg.go.dev/cmp)).

**`clear` on part of a slice.** After filtering subscribers in place (`kept := mb.subs[:0]`, then appending the survivors), `clear(mb.subs[len(kept):])` zeroes the slots past the new length, so the old backing array doesn't keep departed sessions alive for the garbage collector. `clear` (Go 1.21) zeroes a slice's elements, or empties a map.

**Filtering a slice in place.** `kept := s[:0]` creates a zero-length slice that shares `s`'s backing array, so appending survivors to it overwrites `s` from the front with no allocation. It's the standard Go idiom for "remove the elements that fail a test".

**`net.Pipe` in tests.** `net.Pipe()` returns two connected in-memory `net.Conn`s with no TCP and no ports. Writes on one end block until the other end reads. That's why the Feed test drains the pipe on a separate goroutine whenever the Feed is expected to send something ([net.Pipe](https://pkg.go.dev/net#Pipe)).

**A request channel to serialise access.** Instead of a mutex around the publisher's market-data state, other goroutines *send it a request*, and the one goroutine that owns the state handles it between events. That ownership is also what lets "snapshot at S, then everything after S" be exact without locks. It's the same "share memory by communicating" idea as the engine itself.

---

## 6. Self-check questions

1. A subscriber's book reflects `md_seq` 812, and the next message it receives is a `BookDelta` with `md_seq` 815. Describe exactly what the Feed does from here until it's live again, including what happens to a live message 816 that arrives before the resend.
2. Why does subscribing never produce a gap or a duplicate between the snapshot and the first delta, even though `Subscribe` arrives on a different goroutine from the one fanning out deltas?
3. Why does the publisher build snapshots from its own view of *released* deltas, rather than asking the engine for its book?
4. A client asks to resend from `md_seq` 100, and the instrument is at 90,000 with a 65,536-event ring. What does the server answer, and what does the Feed do next?
5. Why is `md_seq` assigned by the engine, not the publisher? Name two things that would break if the publisher numbered it.

<details>
<summary>Answers</summary>

1. 815 is past 813, so it's a gap, and the Feed was live. It counts the gap, sends `Resend(instrument, from = 813)`, and switches to **recovering**, ignoring 815. A live 816 that arrives before the resend answer is also past 813, so it's ignored too; no second resend is sent while recovering. The publisher handles the resend between events, sending 813, 814, 815, 816, … up to its latest, in order, *before* any newer live event. The Feed applies 813 (back to live), then 814, 815 and 816 in order. Anything at or below what it has already applied is counted as a duplicate and ignored.

2. Both the subscription and the fan-out happen on the **publisher's goroutine**. The gateway only *sends* a request. The publisher handles it between two events: it snapshots its view, which reflects exactly `mdSeq = S`, and adds the subscriber to the list. The next event it fans out is S + 1, and it goes to every subscriber, including the new one. Nothing else can run in between, because only that goroutine touches the view and the lists.

3. Two reasons. **Durability:** under group commit the engine's book is ahead of what's on disk, while the publisher's view contains only released, durable changes, so a snapshot never shows anything a crash could undo. **Isolation:** asking the engine would mean a request into the matching goroutine and a pause or a copy-on-write freeze for every subscriber who joins. The publisher's view costs the engine nothing.

4. The oldest resendable `md_seq` is 90,000 − 65,536 + 1 = 24,465, and 100 is older, so the server answers **`Reject(ResendUnavailable)`** for that instrument. The Feed counts a re-snapshot, sends **`Subscribe`** again, waits for the new snapshot (ignoring deltas until then), loads it, and continues live from the snapshot's `md_seq`.

5. Because the engine is deterministic and its state is what's persisted. Numbered by the engine, `md_seq` is in snapshots, recreated by replay, and checked by replay-check. If the publisher numbered it: (a) a server restart would reset `md_seq` to 0, so every subscriber would see numbers go *backwards*, and would have to re-snapshot even though the book hadn't changed; (b) replay-check and recovery couldn't verify the numbering, because it wouldn't be part of the recoverable state. Also, (c) two publishers (say, a future replica) could number the same events differently.

</details>
