# 009 — Gateway, sessions, and the publisher's slow-consumer policy

- **Status:** accepted (milestone 3)
- **Code:** `internal/gateway/` (`Gateway`, `session`), `internal/publisher/publisher.go`, `internal/sequencer/sequencer.go`, `internal/server/server.go`

## Context

Milestone 3 connects the network to the engine. The PRD's requirements:

- per-connection reader and writer goroutines;
- a writer with a bounded buffer;
- "a client that cannot keep up is disconnected with a logged reason, never allowed to stall the engine" (PRD 5.5);
- acceptance 5: a stalled subscriber is disconnected and the engine's latency doesn't move.

## Decisions

### The pipeline

```
reader goroutines (one per connection)
        │ engine.Command, no Seq yet
        ▼
   cmds channel (bounded, 4096)           <- the single total order of all clients' commands
        ▼
   sequencer goroutine                    stamps Seq and Time
        ▼
   engine goroutine                       decision 005
        ▼ engine.Event
   publisher goroutine                    routes by client id; never blocks
        ▼ non-blocking offer
   session.out (bounded, 4096 per client)
        ▼
writer goroutines (one per connection)
```

Each stage closes its output channel when its input closes, so `server.Close` shuts the whole pipeline down in order. Everything already accepted is processed, and nothing is lost in a channel.

### Two goroutines per connection

- **Reader:** reads frames, handles `Login`, `Heartbeat` and `Subscribe` locally, and turns `NewOrder` and `Cancel` into commands on the shared channel.
- **Writer:** the **only** goroutine that writes to the socket. It takes engine events from the session's queue and gateway replies from a small control channel. Two goroutines writing to one socket could interleave the bytes of two frames.

The writer **flushes only when both of its queues are empty**. Under load, many frames leave in one system call; when idle, each frame is sent immediately. `TCP_NODELAY` is set so the kernel doesn't add Nagle's delay on top.

A session is closed exactly once (`sync.Once`), from any goroutine: the reader on EOF or a protocol error, the writer on a write error, the publisher on a slow consumer, or `Gateway.Close`. Closing the `done` channel releases both goroutines from any select. Closing the socket releases a reader blocked in `Read` and a writer blocked in `Write`.

### Sessions and login

- **The first useful frame must be `Login`.** An order or cancel before login gets `Reject(NotLoggedIn)` and the connection is closed. The reject is written and flushed before closing (the `closeAfter` control), so the client learns why.
- **There is no authentication**; PRD section 3 excludes it. A client id is a claim, not a credential.
- **One connection per client id at a time.** A second login with a live id gets `Reject(ClientInUse)`. Otherwise two connections would each receive half of the client's events, depending on which one the publisher's registry held. The id becomes free again when the first session closes, a little after the socket closes.
- **The client id on every command comes from the session**, never from the frame. `NewOrder` and `Cancel` have no client field. This is what makes owner-scoped cancel ([decision 003](003-integer-domain-and-reject-semantics.md)) mean anything.

### Validation is split

The gateway validates the **protocol**: frame length, version, known type, exact payload size, logged in. Any failure closes the connection, because the stream can't be trusted.

The gateway **does not validate business fields**. A side of 7 or a quantity of 0 is passed to the engine unchanged, and the engine rejects it. Validating in the gateway would duplicate the book's rules. Worse, those rejects would never be sequenced or logged, so a WAL replay (milestone 4) couldn't reproduce them, and the recovered event stream would differ from the original.

### The slow-consumer policy

The publisher **never blocks**. `Deliver` is a non-blocking send into the session's bounded queue (`OutQueue`, default 4,096 events). If the queue is full, the publisher:
1. unregisters the client;
2. counts it;
3. logs `slow consumer disconnected`;
4. calls `SlowConsumer()`, and the session closes with reason `slow consumer: outbound queue full (N events)`.

Why disconnect instead of dropping events or waiting:
- **Waiting** would stall the engine for everyone. This was planted as a bug and tested: with a blocking `Deliver`, a healthy client got **zero** responses for 5 s, and even server shutdown hung.
- **Dropping some events** would leave the client with a silently wrong view of its own orders. Disconnecting is honest: the client knows its stream broke, and must reconnect.

A `WriteTimeout` (default 10 s) separately catches a socket that accepts nothing at all while the queue is not yet full.

### Inbound backpressure

If the shared `cmds` channel is full, a reader blocks sending to it, and therefore stops reading its socket. TCP flow control then slows that client down. The engine is protected without a reject or a drop.

### The publisher's registry: copy-on-write

The registry (client id to session) is read for every event by the publisher goroutine, and changed only on login and logout. It is an immutable map behind an `atomic.Pointer`:
- the hot-path lookup takes no lock;
- `Register` and `Unregister` copy the map under a mutex and swap the pointer.

`Unregister` removes the entry only if it still points to *this* session, so a slow-to-close old connection can't remove its replacement.

### Events for disconnected clients are dropped

If a client isn't connected when its event is routed, for example a maker fill after a one-shot `engctl order` has exited, the event is counted (`Dropped`) and lost to that client. Its orders stay on the book. **v1 has no order-status query**, so a reconnecting client can't learn what it missed. This is a known limitation for the README. A real exchange would let the client replay its own events from a sequence number; milestone 6's resend mechanism could be extended to do this later.

### Gateway replies are not ordered with engine events

`Heartbeat` replies, `LoginAck`, and gateway rejects are produced by the reader and written by the writer directly. Engine events travel through the sequencer, engine and publisher first. Their relative order on the wire is not guaranteed.

Observed in a real run: `cancel 2` then `hb` produced `Heartbeat` *before* the `CancelAck`. So a client must not treat a heartbeat reply as "everything I sent earlier has been processed"; it should wait for the specific Ack, Reject or CancelAck.

### The minimal sequencer

*Superseded in milestone 4 by [decision 012](012-durability-and-fsync-policies.md): the sequencer now writes the WAL and applies the fsync policy. Kept here as the milestone 3 record.*

Milestone 3 needs sequence numbers before milestone 4's WAL exists. `sequencer.Sequencer` is the final component's contract without durability: it stamps `Seq` and `Time` from an injectable `Clock`, on one goroutine. Milestone 4 adds the log, fsync policies and ack gating behind the same `Run(in, out)` shape.

## Costs

- **Two goroutines and a 4,096-event queue (about 390 KB) per connection.** Fine for hundreds of clients; at tens of thousands, the queue size would need revisiting.
- **A disconnected client loses events** (above).
- **Slow-consumer detection is by queue depth, not by time.** A client that reads slowly but steadily is fine until its queue fills during a burst, and then it is disconnected at once.
- **Closing on any protocol error is strict**: one malformed frame ends the session. A buggy client is told immediately, rather than limping on with a desynchronised stream.
