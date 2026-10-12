# 03 — The wire protocol and the gateway: bytes in, bytes out

Milestone 3 puts the engine on the network:
- a binary **wire protocol** (`internal/proto`);
- a TCP **gateway** with two goroutines per connection (`internal/gateway`);
- a **publisher** that routes events back to their clients without ever blocking (`internal/publisher`);
- a minimal **sequencer** (`internal/sequencer`);
- the wiring (`internal/server`);
- a client library (`internal/client`) and two binaries (`cmd/engine`, `cmd/engctl`).

The idea worth taking away is **who is allowed to wait for whom**. A network server is a set of queues between goroutines and sockets. Every queue is a place where one side can make the other wait, and the design has to say where waiting is acceptable and where it would freeze the exchange.

---

## 1. Concepts

### 1.1 TCP is a byte stream, so messages need framing

TCP ([RFC 9293](https://www.rfc-editor.org/rfc/rfc9293)) guarantees that bytes arrive in order and intact, but it knows nothing about messages. If a client writes a 37-byte frame and then a 53-byte frame, the server's reads might return 90 bytes at once, or 10, then 60, then 20, or any other split. Code that assumes "one read = one message" works on a quiet laptop and breaks under load.

**Framing** puts message boundaries back. Ours is a **length prefix**: every frame starts with a 4-byte count of the bytes that follow. The reader:

1. reads exactly 6 header bytes (length, version, type);
2. checks the length is sane (≤ 1,024 payload bytes), so a garbage length can't make it allocate gigabytes;
3. reads exactly `length − 2` more bytes.

"Exactly N bytes" is `io.ReadFull`: it keeps calling `Read` until it has N bytes or the stream ends. That makes framing correct however TCP splits the data. `TestRoundTripAllTypes` proves it by feeding 2,000 frames through a reader that returns **one byte per read**.

### 1.2 Fixed binary layouts and endianness

Each message has one fixed layout. `NewOrder` is always 31 payload bytes: instrument at offset 0 (4 bytes), client order id at 4 (8 bytes), side at 12, and so on. The full tables are in [decision 008](../decisions/008-wire-protocol.md) and on each type in `messages.go`.

**Endianness** is the order in which a multi-byte number's bytes are stored. The number 0x04030201 in **little-endian** is the bytes `01 02 03 04`, least significant first. x86 and ARM CPUs store numbers this way in memory, so encoding is cheap. Many network protocols, including NASDAQ's [OUCH](https://www.nasdaqtrader.com/content/technicalsupport/specifications/TradingProducts/OUCH4.2.pdf) and [ITCH](https://www.nasdaqtrader.com/content/technicalsupport/specifications/dataproducts/NQTVITCHspecification.pdf), use **big-endian** ("network byte order"). OUCH and ITCH are also fixed-layout binary formats, which is where this design comes from. Since we don't need to interoperate with them, we use the CPU's native order.

*If you know TypeScript:* this is a `DataView` with `getUint32(offset, true)`, where `true` means little-endian. Compared with JSON, a fixed binary layout has no parsing, no allocation, no field names on the wire, and a fixed size you can check before reading anything.

### 1.3 Goroutine per connection

In Node.js, one thread handles every socket with callbacks or `async/await`, because a blocking read would stall everything. Go turns this around. Each connection gets its own goroutines, which make simple **blocking** calls (`conn.Read`, `conn.Write`). Under the hood, the Go runtime's network poller parks a goroutine waiting on a socket and runs another, so a blocked goroutine costs a few KB of memory, not an OS thread.

Each connection here has exactly two goroutines:
- the **reader** loops: read frame → decode → act;
- the **writer** loops: take the next event or reply → encode → write.

Only the writer writes to the socket. If two goroutines both called `Write`, the bytes of two frames could interleave on the wire and the client could never parse them again.

### 1.4 Backpressure, in both directions

**Backpressure** means a slow consumer makes its producer slow down. Each queue in the system makes a choice about it:

| queue | if it fills up | why |
|---|---|---|
| client's socket → gateway reader | TCP flow control pauses the client | standard TCP: the receiver advertises how much buffer it has left ([RFC 9293 §3.8](https://www.rfc-editor.org/rfc/rfc9293)) |
| readers → `cmds` channel → sequencer | the reader **waits**, so it stops reading its socket, so TCP pauses that client | slowing clients down is the right response to an overloaded engine |
| engine → publisher | the engine **waits** | the publisher is ours and never blocks, so this only happens if the whole server is overloaded |
| publisher → a session's queue | **never waits**: a full queue disconnects that client | one slow client must never stall the engine, and with it everyone else |
| writer → client's socket | the writer waits, for up to `WriteTimeout` | it's that client's own goroutine; only that client is affected |

The fourth row is the **slow-consumer policy** (PRD 5.5). The alternatives are worse:
- if the publisher waited for a slow client, every client would wait, because the publisher is a single goroutine feeding everyone;
- dropping some of the client's events silently would leave it with a wrong picture of its own orders.

Disconnecting is visible and honest: the client knows its stream broke.

### 1.5 Sessions and identity

A connection becomes a **session** when it logs in. The client id is taken from the `Login` frame **once**, and every later command gets its client id **from the session**. `NewOrder` and `Cancel` frames don't carry one. This is what makes owner-scoped cancel secure: a client can't cancel your order by writing your client id into a frame, because the frame has nowhere to put it. (There is no authentication in v1, PRD section 3, so the `Login` itself is still just a claim.)

### 1.6 Latency details: Nagle's algorithm and flushing

**Nagle's algorithm** ([RFC 896](https://www.rfc-editor.org/rfc/rfc896)) makes the kernel hold back small writes while earlier data is unacknowledged, in order to merge them into bigger packets. That is good for bulk transfer and bad for a 59-byte order acknowledgement, which could wait tens of milliseconds. Both sides set `TCP_NODELAY` to turn it off.

The writer still batches, but on its own terms: it writes frames into a buffer and **flushes only when it has nothing else queued**. Under load, many frames leave in one system call; when idle, each frame leaves immediately.

### 1.7 Fuzzing

A **fuzzer** generates millions of inputs, mutating towards ones that reach new code, and reports any that crash your code or break a property you assert. Go has fuzzing built in ([Go Fuzzing](https://go.dev/doc/security/fuzz)). `FuzzReader` feeds arbitrary bytes to the frame reader and decoders, and asserts two things: nothing panics, and anything that decodes re-encodes to the identical bytes. It ran about 5.9 million inputs in 60 s with no failure. Decoders face the open internet, so they must survive any input.

---

## 2. Code walkthrough

### `internal/proto`: the wire format

- **`frame.go`**
  - **`Version`, `HeaderSize`, `MaxPayload`** are the framing constants.
  - **`ErrVersion`, `ErrFrameLength`, `ErrPayloadSize`** are sentinel errors. Each means the stream can't be trusted.
  - **`MsgType`** ([frame.go:46](../../internal/proto/frame.go#L46)): inbound types are below 0x80, outbound types 0x80 and up, and Heartbeat (0x05) is used in both directions.
  - **`appendHeader`** writes the length, version and type.
  - **`Reader`** ([frame.go:102](../../internal/proto/frame.go#L102)) holds a `bufio.Reader`, a 6-byte header array and a 1,024-byte payload buffer, all reused. **`Next`** ([frame.go:119](../../internal/proto/frame.go#L119)):
    1. `io.ReadFull` the header;
    2. reject a bad length or version;
    3. `io.ReadFull` the payload;
    4. return the type and a slice of the internal buffer, valid until the next call.

    A stream that ends cleanly between frames gives `io.EOF`; one that ends inside a frame gives `io.ErrUnexpectedEOF`.
- **`messages.go`**
  - The **field enums** (`SideBuy`, `TypeLimit`, `RoleTaker`, `CancelExpired`, ...) and **`Reason`** ([messages.go:31](../../internal/proto/messages.go#L31)): 1–99 are the engine's reasons, 100 and up the gateway's.
  - **`Size*` constants** give each message's one valid payload size.
  - **`dec`** ([messages.go:106](../../internal/proto/messages.go#L106)) is a tiny cursor (`u8`, `u32`, `u64`, `i64`) used on the stack, so decoding doesn't allocate.
  - **`checkSize`** enforces the exact payload size.
  - Then one block per message, each with a layout table in its doc comment:
    - a struct;
    - **`AppendX(dst, *X)`**, which appends header plus payload using `binary.LittleEndian.AppendUint32/64`;
    - **`(*X).Decode(payload)`**, which checks the size and then reads fields in order.

### `internal/sequencer/sequencer.go`: the minimal sequencer

- **`Clock`** and **`WallClock`**: the system reads the time in exactly one place.
- **`Sequencer.Stamp`** ([sequencer.go:122](../../internal/sequencer/sequencer.go#L122)) assigns `lastSeq+1` and the current time.
- **`Run`** stamps everything from `in` in arrival order and closes `out` when `in` closes.

Arrival order on the one shared `cmds` channel *is* the global order of all clients' commands.

*Superseded in milestone 4:* the sequencer now also batches commands, writes them to the WAL and applies the fsync policy, behind the same `Run(in, out)` shape. See [04-wal.md](04-wal.md#2-code-walkthrough).

### `internal/publisher/publisher.go`: routing and the slow-consumer policy

- **`Subscriber`** ([publisher.go:45](../../internal/publisher/publisher.go#L45)) is the publisher's view of a session. `Deliver` must not block; `SlowConsumer` tells the session to disconnect.
- **`Publisher`** ([publisher.go:58](../../internal/publisher/publisher.go#L58)):
  - `clients` is an `atomic.Pointer` to an immutable map, so reads take no lock;
  - `mu` serialises the rare writers;
  - three atomic counters: delivered, dropped, slow consumers.
- **`Register`** ([publisher.go:104](../../internal/publisher/publisher.go#L104)) copies the map, adds the session, and swaps the pointer. It refuses if the id is taken.
- **`Unregister`** removes the entry only if it is *this* session.
- **`route`** ([publisher.go:223](../../internal/publisher/publisher.go#L223)):
  - skip client 0 (book deltas, startup commands);
  - look up the session; if it's missing, count the event as dropped;
  - otherwise `Deliver`; if that fails, unregister, count, log, and call `SlowConsumer()`.

### `internal/gateway`: connections and sessions

- **`Config`** ([gateway.go:30](../../internal/gateway/gateway.go#L30)): listen address, `OutQueue` (the slow-consumer threshold), `IdleTimeout`, `WriteTimeout`, and `SocketWriteBuffer` (used by tests). **`DefaultConfig`** gives 4,096 events, 60 s and 10 s.
- **`Gateway`** ([gateway.go:61](../../internal/gateway/gateway.go#L61)):
  - the shared `cmds` channel;
  - the publisher;
  - the listener;
  - `sessions` (guarded by `mu`), so `Close` can disconnect everyone;
  - `wg`, which counts every connection goroutine.
- **`Serve`** accepts in a loop. **`start`** ([gateway.go:124](../../internal/gateway/gateway.go#L124)):
  1. sets `TCP_NODELAY` (and `SO_SNDBUF` if configured);
  2. creates the session;
  3. under `mu`, checks the gateway isn't closing and does `wg.Add(2)`, so `Close` can never miss a goroutine;
  4. starts `readLoop` and `writeLoop`.
- **`Close`** ([gateway.go:162](../../internal/gateway/gateway.go#L162)) marks the gateway closed, closes the listener, closes every session, and waits for `wg`. After it returns, nothing will send on `cmds`, so the server can close that channel safely.
- **`session`** ([session.go:31](../../internal/gateway/session.go#L31)) holds:
  - `out`, the bounded event queue fed by the publisher;
  - `ctrl`, the reader's replies to the writer;
  - `done`, closed once to release both goroutines;
  - `client`, which is 0 until login.
- **`Deliver`** ([session.go:67](../../internal/gateway/session.go#L67)) is a `select` with a `default` case: a send that gives up instead of waiting.
- **`close`** ([session.go:85](../../internal/gateway/session.go#L85)) runs once, through `sync.Once`. It records `closed`, closes `done` and the socket, unregisters the client, and logs the reason.
- **`readLoop`** ([session.go:121](../../internal/gateway/session.go#L121)) sets the idle deadline, calls `Next`, then switches on the type:
  - `Login` → `handleLogin`;
  - `NewOrder` and `Cancel` → refuse if not logged in, otherwise build an `engine.Command` with **`Client: s.client`** and `submit` it;
  - `Subscribe` → `Reject(Unsupported)` until milestone 6;
  - `Heartbeat` → echo it;
  - anything else → close with a protocol error.
- **`handleLogin`** ([session.go:216](../../internal/gateway/session.go#L216)) refuses a second login, client 0, and a client id already in use. It sets `client` and registers with the publisher under `closeMu`, so a concurrent `close` either sees the id and unregisters it, or the login notices the session is closed.
- **`submit`** ([session.go:247](../../internal/gateway/session.go#L247)) sends on `cmds`, or gives up if the session closes. Blocking here is the inbound backpressure.
- **`readErrorReason`** ([session.go:256](../../internal/gateway/session.go#L256)) turns read errors into log reasons: EOF, idle timeout, protocol error, reset. **`isConnReset`** has a Windows version and an other-platforms version ([decision 010](../decisions/010-windows-gotchas.md)).
- **`writeLoop`** ([session.go:277](../../internal/gateway/session.go#L277)) selects on `out`, `ctrl` and `done`, encodes into a reused buffer, writes to a 32 KB `bufio.Writer`, and **flushes when both queues are empty**. A `control` with `closeAfter` (a fatal reject) is written and flushed, and then the session is closed.
- **`appendEvent`** ([session.go:321](../../internal/gateway/session.go#L321)) maps engine events to wire messages, converting enums by casting, which `wire_test.go` makes safe. **`appendControl`** encodes the gateway's own replies.

### `internal/server/server.go`: the wiring

**`Start`** ([server.go:75](../../internal/server/server.go#L75)):
1. applies the configured instruments **synchronously**, through `Stamp` and `Step` before any goroutine starts, so a bad config fails startup;
2. creates the three channels;
3. starts the sequencer, engine and publisher goroutines and the accept loop.

**`Close`** ([server.go:234](../../internal/server/server.go#L234)) closes the gateway, then closes `cmds`. The sequencer, engine and publisher then each drain their input and close their output in turn, and `Close` waits for the publisher to finish.

### `internal/client` and the binaries

- **`client.Dial`** ([client.go:41](../../internal/client/client.go#L41)) connects, sends `Login`, and waits for `LoginAck`; a `Reject` becomes `ErrLoginRejected`. **`Next`** decodes into a `Msg` union. Sends hold a mutex, so several goroutines may send.
- **`cmd/engine`** has flags for the gateway config and repeatable `-instrument SYMBOL:TICK:LOT`. It runs `server.Start` and shuts down on Ctrl-C or SIGTERM.
- **`cmd/engctl`**:
  - `order` and `cancel` connect, send, and print messages until none arrives for `--wait` (300 ms). A one-shot command can't know how many fills are coming.
  - `connect` is interactive (`buy 5 100`, `sell 2 mkt`, `cancel 12`, `hb`). On `quit` or end of input it waits for the stream to go quiet, so answers to the last commands are printed.

### Tests

- **`proto`:**
  - round trip, one byte at a time;
  - golden bytes for `NewOrder`;
  - wrong sizes, reader errors, the exact `MaxPayload` boundary;
  - `FuzzReader`;
  - `TestZeroAllocCodec`: encode and decode for `NewOrder`, `Ack` and `Fill`, plus reading a frame, all zero allocations by both checks.
- **`publisher`:**
  - routing; one session per client;
  - a stale `Unregister` can't remove a live session;
  - a slow consumer is disconnected once, without blocking, while a healthy subscriber keeps receiving;
  - concurrent registration during routing, under `-race`.
- **`gateway`:** `TestEnumsMatchWire` (engine and wire numbering agree, names included); `TestAppendEvent` (every event kind maps field by field).
- **`server`:** end to end over loopback.
  - Fills reach both parties with matching `MatchID`s.
  - Owner-only cancel; engine rejects carry a `seq`; market orders expire.
  - Login rules: before login, client 0, a second login, client in use, and the id freed after disconnect.
  - Five kinds of malformed frame each close only their own connection.
  - Subscribe gets `Unsupported`; heartbeats are answered; idle timeout.
  - **Slow consumer:** the slow client is disconnected, and the healthy client keeps trading meanwhile.
  - **8 concurrent clients × 400 orders:** every order answered exactly once, order ids gap-free, every match reported to exactly two parties.
- **`client`, `cmd/engine`, `cmd/engctl`:**
  - client: login rejected or garbled, and a dial timeout;
  - `cmd/engine`: `-instrument` parsing;
  - `cmd/engctl`: argument errors, plus `order` and `cancel` against a real in-process server.

### Planted bugs, all caught

| planted bug | caught by |
|---|---|
| `Deliver` blocks instead of disconnecting | slow-consumer test: the healthy client got **0 responses in 5 s**, goroutines stuck in `chan send`, and even shutdown hung until the 60 s test timeout |
| command built without the session's client id | `TestOrderAckFillOverTCP` |
| login check removed for orders | `TestLoginRules` |
| writer never flushes when idle | `TestOrderAckFillOverTCP` (nothing ever arrives) |
| `price` and `qty` swapped in **both** encoder and decoder | **only** `TestNewOrderGoldenBytes`; the round-trip test passed |
| `MaxPayload` check off by one | `TestReaderMaxPayloadBoundary` |

The fifth row is why golden-bytes tests exist. A round trip only proves the encoder and decoder agree with *each other*, not with the documented layout that other implementations will follow.

### Measurements

**Codec** (`go test ./internal/proto -run '^$' -bench BenchmarkCodec -benchtime 20000000x -count 5`). These are microbenchmarks for intuition, not the PRD section 7 protocol:

| operation | median ns/op | min–max over 5 runs | allocs/op |
|---|---:|---:|---:|
| encode NewOrder | 7.9 | 7.3–8.7 | 0 |
| encode Ack | 12.0 | 9.7–12.7 | 0 |
| encode Fill | 15.5 | 13.6–16.2 | 0 |
| decode NewOrder | 16.2 | 13.7–17.5 | 0 |
| decode Ack | 15.5 | 15.5–18.0 | 0 |
| decode Fill | 17.6 | 16.5–20.0 | 0 |

*Go 1.27.0, windows/amd64, Intel Core i7-8650U, laptop on the default power plan.*

**Observed, not benchmarked:** during the slow-consumer test, the healthy client's worst order round trip over loopback was 0.5–1.3 ms across runs, while another client flooded the server.

---

## 3. Design decisions

- [008: Wire protocol](../decisions/008-wire-protocol.md). A u32 length prefix, a version byte in every frame, `MaxPayload`; fixed little-endian layouts with exact sizes; the message table (including the added `LoginAck`); enum values owned by the wire; reject reasons 1–99 from the engine and 100+ from the gateway; a hand-written codec; no frame checksum and no resynchronisation.
- [009: Gateway, sessions, and the slow-consumer policy](../decisions/009-gateway-sessions.md). The pipeline; two goroutines per connection with a single writer; flush when idle; login first, one session per client, client id from the session; protocol validation in the gateway and business validation in the engine; a non-blocking publisher with disconnect on a full queue; inbound backpressure through TCP; a copy-on-write registry; events dropped for disconnected clients; gateway replies not ordered with engine events; the minimal sequencer.
- [010: Windows gotchas](../decisions/010-windows-gotchas.md). Winsock reset and abort codes; why tests shouldn't match OS error text; stopping processes from scripts; loopback socket buffers. Started this milestone, and extended later.

---

## 4. What breaks if

1. **The publisher waits for a slow client.** *Planted and observed.* The publisher is one goroutine serving every client. If it waits on one client's full queue, it stops taking events from the engine. The engine's next send blocks, so matching stops, so every reader blocks on `cmds`, so TCP pauses every client. One laptop on a bad Wi-Fi link freezes the exchange. In the planted run, a healthy client got nothing for 5 s, and shutdown hung too.

2. **Two goroutines write to the same socket.** Suppose the reader wrote heartbeat replies directly, while the writer wrote fills. Each `Write` of a whole frame is usually atomic on loopback, but nothing guarantees it. A partial write, or a `bufio.Writer` shared without a lock, interleaves bytes from two frames. The client then reads a length field from the middle of a fill and is permanently out of step with the stream. That's why every reply goes through `ctrl` to the single writer.

3. **The client id is taken from the frame instead of the session.** Any client writes another client's id into its `Cancel` and cancels their orders. Or it writes it into a `NewOrder`, and then the victim gets the fills. Owner-scoped cancel ([decision 003](../decisions/003-integer-domain-and-reject-semantics.md)) becomes decoration. The wire format makes this impossible to write by accident: `NewOrder` and `Cancel` have no client id field. (The planted bug "command without the client id" was caught by the first end-to-end test.)

4. **There is no bound on the length field.** One frame starting `ff ff ff 7f` claims a 2 GB payload. A reader that allocates `make([]byte, length)` tries to allocate 2 GB, for every such connection, so a few malicious packets exhaust the server's memory. `MaxPayload` rejects the frame after reading 6 bytes. `TestReaderMaxPayloadBoundary` pins the exact boundary, and a planted off-by-one was caught.

5. **The gateway rejects bad fields itself.** It looks tidy: why send `qty = 0` all the way to the engine? But a gateway reject is never sequenced or written to the WAL (milestone 4). After a crash, replay rebuilds the engine's events from the log, and those rejects aren't in it. The client's recovered event history then differs from what it actually received, and the replay check (milestone 5) can't tell an honest gap from a lost record. Only protocol problems (bad framing, not logged in) are the gateway's business.

6. **A client uses a heartbeat reply as a barrier.** It sends `cancel 2` and then `hb`, and assumes that when the heartbeat comes back, the cancel has been processed. It hasn't necessarily: the heartbeat is answered by the gateway directly, while the cancel goes through the sequencer, engine and publisher first. This was seen in a real run, with `Heartbeat` printed before the `CancelAck`. A client must wait for the specific response to each command.

---

## 5. Go idioms introduced

**`net.Listen`, `Accept`, `net.Conn`.** `net.Listen("tcp", addr)` returns a listener; `Accept` blocks until a client connects and returns a `net.Conn`, which is an `io.Reader` and `io.Writer` with deadlines. Its methods are safe to call from different goroutines (one reading, one writing, a third calling `Close`), which the session design relies on ([package net](https://pkg.go.dev/net)).

**`io.ReadFull` and `io.EOF`.** `io.ReadFull(r, buf)` reads until `buf` is full. Go's errors distinguish "the stream ended cleanly before anything was read" (`io.EOF`) from "it ended partway" (`io.ErrUnexpectedEOF`) ([package io](https://pkg.go.dev/io#ReadFull)). The frame reader passes through the first and converts a short payload into the second.

**`bufio.Reader` and `bufio.Writer`.** These wrap a reader or writer with an in-memory buffer, to cut the number of system calls. On the writing side, **data isn't sent until `Flush`**. Forgetting to flush is a classic bug: the planted "never flush when idle" bug made every end-to-end test time out.

**`encoding/binary` append functions.** `binary.LittleEndian.AppendUint64(dst, v)` appends 8 bytes and returns the slice: the same append-to-a-buffer idiom as `Submit` in milestone 1. `binary.LittleEndian.Uint64(b[off:])` reads 8 bytes at any offset ([package binary](https://pkg.go.dev/encoding/binary)).

**Deadlines and `net.Error`.** `conn.SetReadDeadline(t)` makes a blocked `Read` fail at time `t`. You then check the error with `errors.As(err, &ne)` and `ne.Timeout()`. A deadline is an absolute time, so it is set again before every read for the idle timeout, and before every flush for the write timeout.

**Sentinel errors, `%w`, `errors.Is` and `errors.As`.** `var ErrVersion = errors.New(...)` declares a value callers can compare with. `fmt.Errorf("%w: %d", ErrVersion, v)` wraps it with detail while keeping it detectable: `errors.Is(err, ErrVersion)` looks through the wrapping. `errors.As` looks for an error of a *type*, for example `net.Error`.

**`select` with `default`: a non-blocking send.** `select { case ch <- v: ...; default: ... }` takes the `default` branch at once if the send can't proceed. That one construct is the slow-consumer policy (`session.Deliver`). Without `default`, the `select` would wait.

**`sync.Once`, `sync.WaitGroup`, `sync.Mutex`.**
- `once.Do(f)` runs `f` exactly once even if many goroutines call it; `session.close` relies on this.
- `wg.Add(n)` / `wg.Done()` / `wg.Wait()` counts goroutines and waits for them; `Gateway.Close` relies on this.
- Each mutex here has a comment saying what it guards.

**`sync/atomic` types.** `atomic.Uint64`, `atomic.Int64`, `atomic.Bool` and `atomic.Pointer[T]` are values that many goroutines can read and write safely without a lock ([package atomic](https://pkg.go.dev/sync/atomic)). `atomic.Pointer[registry]` holds the publisher's copy-on-write map: readers `Load` the current map, writers build a new one and `Store` it.

**Implicit interface satisfaction.** `*session` satisfies `publisher.Subscriber` just by having `Deliver` and `SlowConsumer` methods; it never declares that it implements anything. The publisher package defines the interface it needs and never imports the gateway, which avoids an import cycle.

**Build constraints and file-name suffixes.** A file named `reset_windows.go` is compiled only on Windows. `//go:build !windows` at the top of `reset_other.go` compiles it everywhere else. Together they give one function, `isConnReset`, with a different body per platform ([Build constraints](https://pkg.go.dev/cmd/go#hdr-Build_constraints)).

**`flag.FlagSet` and `flag.Value`.**
- `flag.NewFlagSet("order", ...)` gives each `engctl` subcommand its own flags; Go's `flag` package accepts both `-qty` and `--qty`.
- Any type with `String()` and `Set(string) error` methods is a `flag.Value`. `instrumentFlags` uses that to collect a repeated `-instrument`.

**`os/signal`.** `signal.Notify(ch, os.Interrupt, syscall.SIGTERM)` delivers those signals as values on a channel, so `main` can block on `<-ch` and then shut down cleanly ([package os/signal](https://pkg.go.dev/os/signal)). The channel must be buffered, because the runtime won't wait if nobody is ready to receive.

**Native fuzzing.** `func FuzzReader(f *testing.F)` adds seed inputs with `f.Add`, then `f.Fuzz(func(t *testing.T, data []byte) {...})`. Under plain `go test`, only the seeds run; `go test -fuzz FuzzReader` mutates them for as long as you allow.

**`testing/iotest`.** `iotest.OneByteReader(r)` returns at most one byte per `Read`, the worst possible fragmentation. It's the cheapest way to prove framing code doesn't assume one read per message.

**`t.Cleanup`.** It registers a function to run when the test, including its subtests, finishes, in last-in, first-out order. Helpers like `start` and `dial` use it to shut servers down and close connections without the caller writing `defer`.

---

## 6. Self-check questions

1. A client writes one 37-byte `NewOrder` frame (6-byte header plus 31-byte payload), and the server's first `Read` returns only 20 bytes. Walk through what `proto.Reader.Next` does, and say what would go wrong in a reader that called `conn.Read` once per frame.
2. Why is the publisher's send into a session queue non-blocking, while a gateway reader's send into the shared command channel *is* allowed to block? What happens upstream in each case when the queue is full?
3. Why doesn't the gateway reject a `NewOrder` with `qty = 0` itself, when it could do so before the order even reaches the sequencer?
4. A planted bug swapped the `price` and `qty` fields in both the encoder and the decoder, and the round-trip test still passed. Why, and which test caught it?
5. A client sends `cancel 2` and then `hb`, and receives the Heartbeat first. Is that a bug? Explain using the pipeline.

<details>
<summary>Answers</summary>

1. `Next` calls `io.ReadFull` for the 6-byte header. The `bufio.Reader` underneath already holds the 20 bytes, so the header comes from there. It checks the length (33 means a 31-byte payload, under `MaxPayload`) and the version. Then `io.ReadFull` for 31 payload bytes takes the remaining 14 buffered bytes and keeps calling `Read` on the socket until the other 17 arrive. Only then does it return the frame. A one-`Read`-per-frame reader would try to decode a 20-byte fragment, failing the size check or misreading fields, and then treat the next 17 bytes as the start of a new frame. Its framing would be wrong forever after, because there is no way to resynchronise.

2. The publisher is one goroutine serving **every** client. If it blocked on one client's full queue, the engine's sends to the publisher would block, matching would stop for everyone, and readers would back up behind it, freezing the whole exchange. So a full session queue disconnects that one client instead. A reader blocking on `cmds` only stops reading **its own** socket. TCP flow control then slows that client, and its own goroutine is the only thing waiting. That is ordinary, fair backpressure when the engine is overloaded.

3. Because a reject the gateway produces isn't sequenced or written to the log. The engine's output must be a pure function of the logged commands, so that replay after a crash reproduces exactly the same events. If the gateway rejected some commands itself, those rejects would exist in the client's history but not in the log, and replay couldn't reproduce them. The gateway only handles protocol problems, where the stream itself can't be trusted.

4. A round trip only checks that the decoder inverts the encoder. If both put `qty` at offset 15 and `price` at offset 23, they still agree with each other, and every field comes back intact. But the bytes no longer match the documented layout, so any other client implementation would read price and quantity backwards. `TestNewOrderGoldenBytes` compares against hand-written bytes at the documented offsets, and it caught the swap.

5. Not a bug. The heartbeat is answered by the gateway itself: the reader hands it to the writer immediately. The cancel goes reader → `cmds` → sequencer → engine → publisher → session queue → writer. There is no ordering between those two paths. A client that needs to know its cancel was processed must wait for the `CancelAck` or `Reject` for that order ([decision 009](../decisions/009-gateway-sessions.md)).

</details>
