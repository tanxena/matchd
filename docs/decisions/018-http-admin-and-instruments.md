# 018 — The HTTP side: health, metrics, and instruments without touching the order path

- **Status:** accepted (milestone 7). `GET /instruments` content, and serving it from the publisher's view, were chosen with the user.
- **Code:** `internal/server/http.go`, `internal/server/admin.go`, `internal/publisher/marketdata.go` (`Instruments`, `ExpectInstrument`), `internal/gateway/gateway.go` (`ReservedClientID`), `cmd/engctl` (`instruments`, `add-instrument`, symbol lookup)

## Context

PRD 5.5: "A small side HTTP listener for `/metrics`, `/healthz`, and an admin `POST /instruments` (JSON). Nothing on the HTTP side touches the order path." PRD 5.7 writes `engctl book <symbol>`, but until now nothing could turn a symbol into the id the wire protocol uses (decision 016's deviation).

## Decisions

### Endpoints

Served by `net/http` with Go 1.22 method patterns; a wrong method gets 405.

| endpoint | answer |
|---|---|
| `GET /healthz` | 200 `{"status":"ok"}`; 503 `{"status":"shutting down"}` once `Close` begins; 503 `{"status":"failed","error":…}` for good after a durability failure |
| `GET /metrics` | Prometheus text (decision 017) |
| `GET /instruments` | `[{"id","symbol","tick_size","lot_size","md_seq","orders"}]`, by id |
| `POST /instruments` | body `{"symbol","tick_size","lot_size"}` → 201 with the new instrument; 400 invalid; 409 duplicate symbol; 504 no answer in time; 503 shutting down or failed |

- **Address:** default `127.0.0.1:9100`, set with `-http`, `""` to disable.
- **No authentication:** it's for operators on the machine. Binding to a public address would let anyone add instruments, which is noted in `cmd/engine`'s documentation.
- **Timeouts:** reads and headers are bounded, so a slow client can't hold a connection open forever.

### `POST /instruments` is a command like any other

Adding an instrument goes through the **sequencer and the WAL** (decision 005), so it is replayed on restart and gets the same id. Calling the engine's registry directly would break exactly that. The answer comes back the way a client's does:
- The admin submits under **`gateway.ReservedClientID`** (2³² − 1). The publisher has a subscriber registered for that id, and a TCP `Login` with it is refused (`InvalidClientID`), so no connection can ever receive an admin answer.
- Each request uses a fresh `ClOrdID`, which `InstrumentAdded` and `Reject` now echo, to match the answer to the request.
- The answer is released through the **durability gate**, so a 201 means the instrument is on disk.

Requests are **validated before sequencing**, so garbage never reaches the log:
- **Symbol rule:** the instrument's `Validate` now limits symbols to 1–32 bytes of letters, digits, `.`, `_` and `-`. The WAL's command encoding panics above 255 bytes, so before this rule, a long symbol, from the HTTP side or from `-instrument`, would have crashed the sequencer. That was a latent bug since milestone 4.
- **Duplicates** can't be checked before sequencing, since only the engine knows the registry, so a duplicate is a sequenced, logged reject. `TestInstrumentEndpoints` checks that among five bad requests, only the duplicate reached the log.

### `GET /instruments` is answered from the publisher's view

It never touches the engine goroutine, and everything it reports has been released, so nothing leaks that isn't durable:
- `md_seq` and the level view already lived in the publisher (decision 016).
- **The resting-order count** rides on every `BookDelta` as `Event.Orders`, the instrument's count after the command. It's read in O(1) from the book's index, sits in what was padding (`Event` stays 112 bytes, as measured), and isn't sent on the wire. Every change to the count produces a delta (rest, cancel, a maker filled away), so the latest released delta is always current. An engine-side atomic counter was rejected: it would show the effect of commands that aren't yet durable.
- **The spec** (symbol, tick and lot sizes) can't ride on `InstrumentAdded`, because `Event` must stay pointer-free and a symbol is a string. So:
  - startup seeds each instrument's spec from the recovered state;
  - at runtime, the admin path tells the publisher the spec before submitting (`ExpectInstrument`, keyed by client and `ClOrdID`);
  - the publisher attaches it when the durable `InstrumentAdded` is released, and forgets it on a reject.

  Because that happens on the publisher goroutine before the admin is answered, a `GET` after a 201 always shows the new instrument.

It's served like Subscribe and Resend: a request answered between events on the publisher goroutine.

### `engctl` resolves symbols (closes decision 016's deviation)

Wherever an instrument is expected, `--instrument` takes an id or a symbol, and `book` and `subscribe` take it positionally, as PRD 5.7 writes them (`engctl book XYZ`). Symbols are looked up with `GET /instruments` on `--http`. New commands:
- `engctl instruments` lists the instruments;
- `engctl add-instrument --symbol S --tick T --lot L` adds one.

## Costs

- A 201 waits for an fsync and a trip through the publisher. That's milliseconds, which is fine for an admin action.
- If the request times out (504), the outcome is unknown: the command may still execute. Repeating it gets a 409 if it did.
- A second listener and port to secure. It's local by default.

## Tests

- `TestInstrumentEndpoints`: the listing; add; 409/400/405; nothing invalid logged; `md_seq` and order count after trading; the reserved id refused at login; the instrument still there, with the same id, after a restart.
- `TestAdminRejectForgetsSpec`: a rejected add leaves nothing behind; 8 concurrent adds get distinct ids and all show up.
- `TestHealthzAfterDurabilityFailure`: 503 "failed", and POST refused.
- `TestSymbolsThroughHTTP` (engctl): add, duplicate, resolve, order by symbol, `book ABC`.

**Planted bugs, caught:**
- the reserved id accepted at login;
- validation skipped, which made the sequencer panic on a 300-byte symbol;
- the order count not taken from deltas.
