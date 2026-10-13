# Glossary

Terms used in this repo's code and docs. Each entry names the milestone where the term first matters. Terms marked *(later)* are defined now so earlier docs can refer to them.

## Market structure

- **Instrument / symbol** (M1): the thing being traded, such as a stock. Each instrument has its own independent book.
- **Limit order book (LOB)** (M1): the list of all resting buy and sell orders for one instrument, grouped by price. Its two halves are the *bid* side (buyers) and the *ask* or *offer* side (sellers).
- **Bid / ask** (M1): a bid is a resting buy order; an ask is a resting sell order. The **best bid** is the highest bid price; the **best ask** is the lowest ask price.
- **Top of book** (M1): the best bid and best ask, with the quantity at each.
- **Spread** (M1): best ask − best bid. It is always positive in a healthy book.
- **Mid** (M1): the price halfway between best bid and best ask. The differential generator uses a drifting "mid" to choose where to place orders.
- **Crossed book / locked book** (M1): crossed means best bid > best ask; locked means best bid = best ask. A matching engine can never be in either state after processing a command, because any overlap would already have traded. `CheckInvariants` asserts best bid < best ask.
- **Price level** (M1): all resting orders on one side at one exact price. In the code this is `level`, with a FIFO queue of `orderNode`s.
- **Depth** (M1): how many levels or orders are resting. "Max depth" in the harness's coverage line is the most resting orders seen at once.
- **Tick / tick size** (M1): the smallest allowed price increment. Prices in this engine are *counts of ticks* (`book.Price`).
- **Lot / lot size** (M1): the smallest tradable quantity unit. Quantities are *counts of lots* (`book.Qty`).

## Orders

- **Limit order** (M1): "buy (or sell) up to Q at price P or better". Whatever doesn't trade immediately **rests** on the book at P.
- **Market order** (M1): "buy (or sell) Q at whatever price is available". It never rests; any unfilled remainder is cancelled (`Result.Expired`).
- **IOC (immediate or cancel)** *(later, M8)*: a limit order that never rests; its unfilled remainder is cancelled.
- **FOK (fill or kill)** *(later, M8)*: an order that fills completely right away or does nothing at all.
- **Time in force (TIF)** (M1): how long an order may rest. **GTC** (good till cancelled) rests until filled or cancelled. **Day** would expire at the end of the session; in v1 it is treated as GTC (decision 003).
- **Order id** (M1): the engine's unique number for an accepted order (`book.OrderID`). The caller assigns it, never the book.
- **Client order id (ClOrdID)** (M1): the client's own label for an order. It must be unique among that client's *live* orders (PRD 5.1).
- **Cancel** (M1): removing a resting order. Here it is scoped to the owning client (decision 003).
- **Reject** (M1): refusing a command, with a reason (`book.RejectReason`). A rejected command doesn't change the book.

## Matching

- **Price-time priority** (M1): the matching rule. Better price first; at equal prices, earlier arrival first (FIFO).
- **FIFO queue** (M1): first in, first out. The order of orders within a level.
- **Taker / aggressor** (M1): the incoming order that trades against the book.
- **Maker / resting order** (M1): the order already on the book that the taker trades against.
- **Fill / execution / trade** (M1): one match between a taker and one maker, for some quantity at the **maker's** price (`book.Fill`).
- **Partial fill** (M1): a fill for less than an order's remaining quantity. A partially filled maker keeps its place in the queue.
- **Sweep / walking the book** (M1): a taker consuming several levels in a row, from the best price outward.
- **Self-trade** (M1): a client's order matching that client's own resting order. Not prevented in v1 (PRD 5.2).

## Testing

- **Reference book / oracle** (M1): `internal/book/ref`, a deliberately simple implementation whose output is taken as correct.
- **Differential testing** (M1): running the same inputs through two implementations and failing on any difference (W. M. McKeeman, "Differential Testing for Software", *Digital Technical Journal* 10(1), 1998).
- **Seed** (M1): the starting value of a pseudo-random generator. The same seed always produces the same command stream, so any failure can be replayed.
- **Invariant** (M1): a property that must hold after every command, for example "level total equals the sum of its orders".
- **Mutation check** (M1): deliberately introducing a small bug to confirm that a test catches it. A test that can't fail proves nothing.
- **Coverage floor** (M1): a minimum count for each kind of event in the harness. It fails the test if the generator stops exercising a path.

## Data structures

- **Slab** (M1): one big slice holding all records of a kind, addressed by index (`Book.nodes`).
- **Free list** (M1): a linked list of unused slab slots or structs, reused before allocating new ones.
- **Intrusive linked list** (M1): a list whose links (`prev`, `next`) are fields of the elements themselves, rather than separate list nodes.

## Engine (milestone 2)

- **Command** (M2): an input to the engine: `NewOrder`, `Cancel`, `AddInstrument` (`engine.Command`).
- **Event** (M2): an output of the engine: `Ack`, `Reject`, `Fill`, `Cancelled`, `BookDelta`, `InstrumentAdded` (`engine.Event`).
- **Ack** (M2): the event confirming a new order was accepted, carrying its engine-assigned order id. It is always the first event for that order.
- **Sequence number (Seq)** (M2; assigned by the sequencer since M3, logged since M4): a global, gap-free counter on every command, including rejected ones. The engine panics on a gap.
- **Gap-free** (M2): every number in a series is used exactly once, in order. A missing number then proves something was lost.
- **Match id** (M2): the number shared by the taker's and maker's fill events of one match.
- **Leaves** (M2): an order's open quantity remaining after a fill.
- **Book delta** (M2): a market-data event giving one price level's new total size; 0 means the level is gone.
- **Market by price (MBP) / market by order (MBO)** (M2): market data described per price level (our deltas) versus per individual order (NASDAQ ITCH).
- **Determinism** (M2): same inputs in the same order give the same outputs, with no hidden inputs such as the clock, randomness or map iteration order. The foundation of recovery by replay.
- **State machine approach** (M2): treating a service as state plus a deterministic transition function, so replaying the same ordered inputs reproduces the same state (Schneider, 1990).
- **Backpressure** (M2): a slow consumer slowing its producer, because sends block when buffers are full. Wanted between engine and publisher; never allowed to reach the engine from a single slow client.
- **Fail-stop** (M2): stopping the process on an impossible state rather than continuing with possibly corrupted state.
- **Steady state** (M2): running at a constant book size. The zero-allocation requirement applies here; growing to a new peak may allocate.
- **Allocation / garbage collector (GC)** (M2): heap memory requested at run time, and the runtime component that reclaims it. Both cost time, especially in tail latency.

## Hash tables (milestone 2)

- **Open addressing / linear probing** (M2): storing keys directly in an array, and on a collision trying the next slot, then the next.
- **Home slot** (M2): the slot a key's hash points to; the start of its probe path.
- **Load factor** (M2): the fraction of slots in use. The book's index keeps it at or below 1/2.
- **Tombstone** (M2): a marker left in a deleted slot so lookups keep probing past it. It fills the table and eventually forces a rebuild.
- **Backward-shift deletion** (M2): deleting without tombstones by moving later entries of the probe run back into the gap, when doing so can't place an entry before its home.
- **Hash flooding** (M2): an attack where chosen keys all collide, turning lookups into linear scans. Defended by a secret per-table hash seed.

## Network and protocol (milestone 3)

- **Byte stream** (M3): what TCP delivers: ordered bytes with no message boundaries. One write can arrive as several reads, or merged with the next write.
- **Frame / framing** (M3): one message on the wire, and the rule for finding where it ends. Ours is a 4-byte length prefix.
- **Length prefix** (M3): a count at the start of a frame saying how many bytes follow.
- **Endianness** (M3): the byte order of multi-byte numbers. Little-endian stores the least significant byte first, as our wire format and x86/ARM CPUs do; big-endian ("network byte order") stores the most significant first, as NASDAQ ITCH and OUCH do.
- **Golden bytes** (M3): a test that compares an encoding against hand-written expected bytes, catching layout changes that a round trip can't see.
- **Fuzzing** (M3): automatically generating huge numbers of inputs to find crashes or broken properties.
- **Session** (M3): a connection after a successful `Login`. It owns the client id used for every command it sends.
- **Heartbeat** (M3): an empty message that proves a connection is alive. The server echoes it; silence for `IdleTimeout` disconnects.
- **Slow consumer** (M3): a client that can't read its events as fast as they're produced. It is disconnected when its bounded queue fills, and never allowed to stall the engine.
- **Flow control (TCP)** (M3): the receiver tells the sender how much buffer it has left, so a sender can't overrun a slow reader. This is how a blocked reader slows its own client down.
- **Nagle's algorithm / `TCP_NODELAY`** (M3): kernel batching of small writes, which adds latency to small messages; we turn it off on both ends.
- **Copy-on-write** (M3): updating shared data by building a new copy and swapping a pointer to it, so readers never take a lock. Used for the publisher's client registry.

## Durability (milestone 4)

- **WAL (write-ahead log)** (M4): an append-only file of every command, written before the command's effects may become visible. Replaying it rebuilds the engine's state.
- **Page cache** (M4): the operating system's in-memory copy of file data. `write` usually stops here; a process crash doesn't lose it, a power failure does.
- **fsync** (M4): the call that blocks until a file's data is on stable storage (`FlushFileBuffers` on Windows). It is slow: about 1.5 ms here, against about 3.7 µs for a plain write.
- **Durable** (M4): on stable storage, so it would survive a power failure.
- **Durable seq** (M4): the highest command seq known to be durable. The publisher releases an event only when the event's seq is at or below it.
- **Segment** (M4): one file of the WAL, named after its first record's seq.
- **Torn write / torn tail** (M4): a record only partly written when a crash interrupted it, at the end of the log. Truncated on recovery; damage anywhere else is corruption.
- **CRC32C** (M4): a 32-bit checksum (Castagnoli polynomial) stored with every record, to detect torn or corrupted bytes.
- **Group commit** (M4): letting many commands share one fsync. `always` batches commands that are already waiting; `group:1ms` fsyncs on a timer while the engine carries on.
- **Replay / recovery** (M4): rebuilding the engine's state at startup by feeding every logged command back through it.
- **Fail-stop on fsync error** (M4): after a failed fsync nothing more is acknowledged and the process stops, because a retry can falsely succeed.

## Snapshots and recovery (milestone 5)

- **Snapshot** (M5): a saved copy of the engine's full state (every resting order in priority order, plus the last seq, order id and match id) after some command seq. Recovery loads one and replays only the WAL after it.
- **Consistent snapshot** (M5): one that is exactly the state after a single seq, not a mix of moments. Ours are taken between two commands on the engine goroutine.
- **Copy-on-write (COW)** (M5): sharing data with a reader until the writer changes it, and only then copying the part being changed. Lets the engine keep trading while a snapshot is written.
- **Chunk** (M5): a fixed block of 4,096 order slots, the unit of sharing and copying.
- **Epoch** (M5): a counter bumped at each freeze. A chunk with an older epoch is shared with a snapshot and is copied before its first write.
- **Freeze / thaw** (M5): starting a snapshot (share chunks, copy level heads) and ending it (unshare, recycle replaced chunks).
- **Atomic replace** (M5): write a temp file, fsync it, rename it over the target, fsync the directory, so a crash leaves the old file or the new one, never a mix. Not guaranteed atomic on Windows.
- **Manifest** (M5): `manifest.json`, the list of snapshots that are complete and durable. Recovery trusts only these (or, if the manifest is lost, the snapshot files themselves).
- **Retention** (M5): keeping the newest two snapshots, and the WAL from the older one onward, so one damaged snapshot is always survivable.
- **Fill log** (M5): an append-only record of every match the publisher released to clients, checked by replay-check.
- **replay-check** (M5): an offline tool that rebuilds the newest snapshot from an older base plus the WAL, and checks the fill log against the replay.
- **Crash harness** (M5): a test that runs the real binary, kills it at random moments under load, and verifies recovery each time. It proves recovery logic, not fsync.

## Market data (milestone 6)

- **md_seq** (M6): the per-instrument, gap-free sequence number on every book delta and trade, assigned by the engine. Subscribers use it to detect gaps.
- **Trade print** (M6): the public, anonymous report of one match: price, quantity, the aggressor's side, the match id.
- **Aggressor** (M6): the incoming order that caused a trade; the taker.
- **Market-data snapshot** (M6): a full price-level book for one instrument at a stated `md_seq`, sent to a new subscriber before any deltas. (Not the same as a recovery snapshot, milestone 5.)
- **Snapshot then deltas** (M6): the join rule: after a snapshot at S, the subscriber applies deltas from S + 1, with nothing missing or doubled.
- **Gap** (M6): a jump in `md_seq` (4,005 then 4,007): something was lost.
- **Resend** (M6): a request to send again the market-data messages from a given `md_seq`, served from the ring buffer.
- **Ring buffer** (M6): fixed-size circular storage of the last N messages per instrument (65,536 here); older ones are overwritten.
- **Re-snapshot** (M6): starting over with a fresh snapshot when a gap is older than the ring holds.
- **Feed** (M6): the client-side book builder (`client.Feed`) that applies snapshots and deltas and recovers from gaps.

## Operations (milestone 7)

- **Metric** (M7): a number the program keeps about itself, read periodically by monitoring. **Counter**: only goes up. **Gauge**: up and down. **Histogram**: counts observations into buckets by upper bound.
- **Prometheus / scrape** (M7): the monitoring system that pulls `GET /metrics` every few seconds (a *scrape*) and stores the series.
- **Bucket / `le`** (M7): one histogram bin, labelled by its inclusive upper bound ("less than or equal"); counts are cumulative. Ours are defined in microseconds and shown in seconds.
- **Percentile (p50, p99, p99.9)** (M7): the value below which that fraction of observations fall; read from a histogram as the bucket holding it.
- **Matching-core latency** (M7): from the engine dequeuing a command to having sent all its events to the publisher (PRD 7.1); `matchd_matching_latency_seconds`. `Step` alone is `matchd_matching_step_seconds`.
- **Timer tick / timer resolution** (M7): the interval at which Windows updates its ordinary clock (0.5–15.6 ms); `time.Now` can't resolve anything shorter.
- **QPC (QueryPerformanceCounter)** (M7): Windows' high-resolution counter (10 MHz here); about 80–180 ns per read from Go because of the syscall boundary.
- **TSC / RDTSC** (M7): the CPU's time-stamp counter and the instruction that reads it (~12 ns). **Invariant TSC**: ticks at a constant rate regardless of frequency scaling and sleep, synchronised across cores.
- **Calibration** (M7): measuring the TSC's rate against QPC at startup, so ticks convert to nanoseconds; **ppm** (parts per million) measures its error.
- **Syscall boundary** (M7): the cost of Go calling into the operating system: a stack switch and scheduler bookkeeping, around a hundred nanoseconds.
- **Health check** (M7): `GET /healthz`: 200 while serving, 503 while shutting down or after a durability failure.
- **Reserved client id** (M7): 2³²−1, under which the admin endpoint submits commands; a TCP login with it is refused.
- **Graceful shutdown** (M7): stop accepting, drain and fsync, deliver queued answers, snapshot, close (decision 019).
- **Half-close / FIN / RST** (M7): FIN ends one direction of a TCP connection normally; RST aborts it, and makes the receiver discard data it hasn't read. A *half-close* (`CloseWrite`) sends FIN but keeps reading.
- **Lingering close** (M7): half-close, discard the peer's remaining input, then close, so unread input doesn't turn the close into a RST.

## Later milestones
- **ITCH / OUCH** (reference): NASDAQ's market-data (ITCH) and order-entry (OUCH) protocols. They are cited for terminology:
  - [TotalView-ITCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHspecification.pdf)
  - [OUCH 4.2](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH4.2.pdf)
  - [OUCH 5.0](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH5.0.pdf)
