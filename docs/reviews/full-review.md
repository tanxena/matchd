# Full review — 2026-10-13 — commit 2d75636

## Build and test results

All of this ran on the development machine (Windows 11, go1.27.0, MSYS2 gcc for `-race`) at HEAD `2d75636`, with a clean tree.

| command | result |
|---|---|
| `go build ./...` | ok |
| `go vet ./...` | ok |
| `gofmt -l .` | clean |
| `golangci-lint run ./...` | `0 issues.` |
| `go test -count=1 ./...` | all packages ok (`internal/server` 50 s, `internal/crashtest` 14 s) |
| `go test -race -count=1 ./...` | all packages ok (`internal/server` 56 s) |
| `make test-long` (MSYS2 make from PowerShell) | ok: differential 6.1 s; crash test 32.2 s, 0.67 min in total |
| `GOOS=linux go vet ./...`, `GOOS=linux go build ./...` | ok. Linux tests were **not** run: there is no Linux machine here, and CI has never run (see F-010). |
| `bench/report`, with `make bench-report`'s arguments, against a copy of README.md | **byte-identical** to the committed README (`diff` and `cmp` print nothing) |

**What the long runs actually exercised**, from `-v` re-runs:
- **Differential:** `20 seeds x 60000 commands … max-depth=177 snapshots=2643 cow-copies=2637`.
- **Crash, 50 runs:** `405668 acknowledged commands (284455 checked against the WAL, the rest pruned behind snapshots), 364791 fills received (104726 compared with the replay) … torn WAL tails recovered in 0 runs`.

**Flaky test:** `go test ./bench/internal/sysinfo -run TestAffinityAndTimes` failed 2 of 6 consecutive runs (`probe_test.go:106: self times {0s 0s}`) and failed independently in the benchmark audit (F-016). It passed in both full-suite runs above.

**Reproductions (F-001, F-002, F-003):** these were run in a `git archive HEAD` copy under a scratch directory, with added `zz_review_test.go` files. Nothing in the repository was changed.

**Not run, per REVIEWER.md:** `make bench-full` and `make bench-balanced`. `make bench-smoke` was also not run, because the regeneration check above already covers what it would show for the committed results.

## Acceptance criteria audit

| # | Criterion (PRD §8) | Status | Evidence / gap |
|---|---|---|---|
| 1 | Differential test: 1M+ commands across ≥ 20 seeds, zero divergence; normal suite 10k per seed, ≥ 10 seeds | Met (coverage gap) | `internal/book/diff_test.go:28-33,36-81`. The 1.2M / 20-seed run passes. The generator never builds a book deeper than 177 orders, so the book's slab never leaves one chunk and its indexes never grow under the oracle. Separately, extreme quantities make the books diverge (F-002, F-004). |
| 2 | Crash test: ≥ 50 consecutive runs, zero lost acked commands, zero duplicate fills, recovered book identical, sequence continuous | Met (weaker than the README states) | `internal/crashtest/crash_test.go:222-380`. 50 runs pass. In the run measured here, 30% of acked commands and 71% of received fills were never compared with the WAL or the replay, and 0 torn tails occurred (F-007). |
| 3 | Zero allocations on the engine's steady-state path for limit add, cancel, match (proven by test) | Met | `internal/engine/alloc_test.go:117-139` (AllocsPerRun plus the strict `OwnAllocations` check), `run_alloc_test.go:16`; `TestZeroAllocWhileFrozen` in `internal/book/cow_test.go:203`. All pass. |
| 4 | A new md subscriber joins mid-stream, gets a snapshot, detects an induced gap, resends, and converges to the from-start subscriber's book (test) | Met | `internal/server/marketdata_test.go:161-229`. It also compares against the engine's book and md_seq after Close. |
| 5 | A subscriber that stops reading is disconnected and the engine's latency histogram does not move | Partially met | `internal/server/ops_test.go:437-497`. The disconnect is asserted. "Does not move" is asserted only at p50 and p99, with one bucket of slack on buckets 2–2.5× wide, so up to about a 4× shift passes (F-008). |
| 6 | Benchmark tables per section 7, with the generated-from-results script committed | Partially met | `bench/report` regenerates the README byte-for-byte. The fsync, snapshot and Balanced tables omit the count (and some quantiles) that PRD 7.1 requires on every reported latency (F-011). The Balanced row reports n=3 against PRD 7.5's ≥ 5 (F-014). |
| 7 | `go test -race ./...` and lint clean; Linux and Windows both pass | Not met | Race and lint are clean on Windows. The suite has never run on Linux (`README.md:372`), and `.github/workflows/ci.yml` has never executed. `bench/internal/sysinfo` is flaky on Windows (F-010, F-016). |
| 8 | README: architecture, single-threaded rationale, wire protocol, WAL/recovery diagram, quick start, benchmark tables, known limitations | Met | `README.md:14-41, 43-70, 72-106, 115-129, 143-324, 340-350`. The quick-start commands were run against the built binaries and work as written. Several README statements are inaccurate (F-009, F-010, F-012, F-013). |

## Findings

### F-001 — Stale socket write deadline disconnects healthy clients as "slow consumers", losing their fills
- **Severity:** Critical
- **Location:** `internal/gateway/session.go:L337-L418`, `internal/gateway/session.go:L505-L510`
- **Category:** Concurrency
- **Problem:**
  - The writer sets the socket's write deadline only in `flush()` (L505-L510), to `now + WriteTimeout`.
  - Event frames are written with `bw.Write(buf)` (L406) into a 32 KiB `bufio.Writer` (L339). When that buffer fills, `bufio` flushes to the socket by itself, with whatever deadline was set at the last explicit `flush()`.
  - Suppose a session has been quiet for longer than `WriteTimeout`, and then gets a burst of 32 KiB or more (about 430 Fill frames) before the writer's queue first empties. The implicit flush then hits an already-expired deadline and fails at once with a timeout.
  - `writeFailed` (L423ff) counts that as a slow consumer and closes the session.
- **Why it matters:** a client that is reading perfectly well is disconnected, and every event in the burst is gone. That includes its fills. v1 has no order-status query, so the client never learns its orders traded.
  - **Reproduction:** a maker rests 600 one-lot asks; both sessions then sit idle for 700 ms with `WriteTimeout` 300 ms; a taker sends a market buy for 600, under `group:150ms`. With `GOMAXPROCS=1`, 3 of 3 runs ended with both maker and taker cut off after **0 of 601** messages. The log says `slow consumer: socket write blocked for longer than WriteTimeout`, and `SlowWrites: 2`.
  - With the same test at `WriteTimeout=0`, all 601 messages arrive.
  - **Trigger:** with more cores it is a scheduling race: the writer usually wakes early, empties the queue and does an explicit flush, which refreshes the deadline. So it bites under CPU load, when the publisher pushes hundreds of events before the writer runs. That covers a large sweep, a large `Resend` answer, or the backlog released after a durability stall longer than `WriteTimeout` (decision 022's burst path).
- **Suggested fix:**
  - Give the `bufio.Writer` a wrapper `io.Writer` that sets `SetWriteDeadline(now+WriteTimeout)` before *every* `conn.Write`. Alternatively, refresh the deadline before each `bw.Write` whenever `bw.Available() < len(frame)`, as `write()` (L473ff) already does for snapshots.
  - Add a test that idles longer than `WriteTimeout`, then forces a burst larger than 32 KiB with `GOMAXPROCS=1`, or with the writer held back.
- **Builder response:** Accepted — fixed in 15cf825: every socket write now sets its own deadline (`deadlineWriter`, the writer's `bufio.Writer` writes through it), including bufio's implicit flushes. `TestIdleThenBurstIsNotASlowConsumer` (GOMAXPROCS 1, idle 700 ms past a 300 ms WriteTimeout, then a 599-fill burst) fails on the old code 3 of 3 times with 0 of 599 fills delivered, and passes now.
- **Reviewer follow-up:** Closed — every socket write in the gateway now goes through `deadlineWriter` (`internal/gateway/session.go:L529-L536`), which sets a fresh deadline first; it is the only `conn.Write` in the package. At 449ab7e with GOMAXPROCS=1, my original reproduction received 601 of 601 messages with `WriteTimeout` 300 ms, in 3 of 3 runs. `TestIdleThenBurstIsNotASlowConsumer` (`internal/server/deadline_test.go:L22`) failed 3 of 3 against 15cf825^'s `session.go` (0 of 599 fills, `SlowWrites:2`) and passes at HEAD.

### F-002 — Quantities are unbounded: price-level totals overflow int64 and the book diverges from the reference book
- **Severity:** Critical
- **Location:** `internal/book/types.go:L280-L291` (only `Qty <= 0` is rejected), `internal/book/book.go:L200`, `internal/book/book.go:L254-L268`, `internal/book/book.go:L306`
- **Category:** Matching
- **Problem:**
  - Validation accepts any positive `int64` quantity, and `level.total` is a plain `int64` sum (L306 `lv.total += qty`). Two resting orders at one price, `MaxInt64` and `1`, give `LevelQty = -9223372036854775808`.
  - `fillCheck` sums those totals (L263), so a FOK for 3 lots against that level is rejected with `FOKNoLiquidity`, while the reference book fills it. Reproduced:

    ```
    book: {Reject:FOKNoLiquidity Filled:0 ...}   ref: {Reject:none Filled:3 ...}
    ```

  - `CheckInvariants` doesn't notice, because it sums with the same overflow.
  - The engine then publishes `BookDelta` events with a negative level quantity (`engine.go:268,286`). The publisher's md view and every snapshot carry it.
- **Why it matters:**
  - **Matching:** any client can make matching diverge from the oracle just by sending large quantities. It needs no special access, and it persists in the WAL and replays identically.
  - **Market data:** the feed shows nonsense quantities.
  - **Tests:** the differential test can't find this, because quantities stay in 1..70 (`diff_test.go:211,215`).
- **Suggested fix:**
  - Define a maximum order quantity (and price) in the integer-domain decision (003). Reject above it with `InvalidQty` / `InvalidPrice` in both books.
  - Choose the bound so that a level's total, and a FOK's running sum, can't overflow: for example `MaxQty = 2^40` with a cap on resting orders per level, or 128-bit / saturating sums.
  - Extend the generator with edge quantities.
- **Builder response:** Accepted — fixed in dc42b11: both books reject quantity above `MaxQty` = 2^30 lots (`InvalidQty`) and a limit price above `MaxPrice` = 2^53 ticks (`InvalidPrice`). With fewer than 2^32 orders per book (uint32 indexes), no level total or FOK sum can exceed 2^62 (decision 003). `TestQuantityAndPriceBounds` fails on both books with the old validation (a level of `MaxInt64` + 1 read as negative) and passes now. Edge values were added to the generator under F-004.
- **Reviewer follow-up:** Closed — `Order.Validate` (`internal/book/types.go:L292ff`) and the reference book's `validate` both reject quantity above `MaxQty` (2^30) and limit price above `MaxPrice` (2^53). With uint32 indexes, a side's sum stays below 2^62. No other command carries a quantity, and nothing multiplies by tick or lot size. My original reproduction now gets `InvalidQty` from both books. I removed only the two bound checks in a scratch copy: `TestQuantityAndPriceBounds` then fails on both books. It passes at HEAD.

### F-003 — The duplicate client-order-id rule is enforced per instrument, not per client
- **Severity:** Should-fix
- **Location:** `internal/book/book.go:L125`, `internal/book/book.go:L175`, `internal/engine/engine.go:L18`, `internal/engine/engine.go:L225`
- **Category:** Matching
- **Problem:**
  - PRD 5.1: "`client_order_id` is unique **per client among live orders**". `docs/learn/01-book-core.md:67` restates it as "among that client's live orders".
  - The `byClOrd` index lives inside each per-instrument `Book`, and the engine keeps one book per instrument. So client 7 can rest ClOrdID 5 on instrument 1 and have ClOrdID 5 accepted on instrument 2 while the first still rests. Reproduced at engine level: both get `Ack`.
  - The reference book is per instrument too, so the differential test can't see it.
- **Why it matters:** this is a spec deviation in an externally visible rule, documented as if it were implemented. A client that keys its own state by ClOrdID alone gets two live orders under one id.
- **Suggested fix:** do one of the following:
  - Move the `(client, clOrdID)` index to the engine, across instruments, so it is rejected before `bk.Submit`. Remove it from the book, or keep the book's for the oracle.
  - Or explicitly re-scope the rule to "(client, instrument, client_order_id)" in decision 003 and the learning doc.

  Either way, add an engine-level test.
- **Builder response:** Accepted — fixed in fd338eb: the PRD rule (unique per client among live orders on every instrument) is implemented in both books. The engine shares one `book.ClOrdIDs` across all its books (`Book.Share`, rebuilt on restore), and the reference book implements it independently (`ref.Link`, looking through linked books' orders). `TestDifferentialAcrossInstruments` spreads the stream over two instruments in both implementations and catches either book reverted to per-instrument. `TestClientOrderIDUniqueAcrossInstruments` (engine, including after a restore) fails on the old engine. Decision 003 records that this changes reject behaviour and that the WAL format is unaffected, since no existing log is being replayed.
- **Reviewer follow-up:** Closed — `Book.duplicate` (`internal/book/book.go:L169-L178`) checks the shared `ClOrdIDs`. The set is updated on rest (L329) and on `release` (L385), the only removal path. The engine shares it when it adds a book (`internal/engine/engine.go:L347`) and on restore (`internal/engine/snapshot.go:L94`). My original engine-level reproduction now gets `Reject DuplicateClientOrderID`. I reverted each half separately in a scratch copy, and every revert is caught:
  - shared lookup disabled (L174): `TestDifferentialAcrossInstruments` diverges on every seed;
  - `ref.Link` made a no-op: the same test diverges on every seed;
  - `Share` not attaching the set: the size check and `TestClientOrderIDUniqueAcrossInstruments` both fail.

### F-004 — The differential test cannot reach the book's multi-chunk, index-growth or extreme-value paths
- **Severity:** Should-fix
- **Location:** `internal/book/diff_test.go:L193`, `internal/book/diff_test.go:L211-L216`, `internal/book/cow.go:L24-L25`, `internal/book/book.go:L139-L140`
- **Category:** Tests
- **Problem:**
  - Measured max depth across the 1.2M-command run is **177 orders**. The slab chunk holds 4,096 orders (`cow.go:L25`) and the indexes start sized for 1,024 (`book.go:L139-L140`).
  - So under the oracle, the following never run:
    - a second chunk, or slab growth while frozen;
    - `prev`/`next` links that cross chunks, or copy-on-write of more than one chunk;
    - `index.grow()`, or backward-shift deletion in a table that has grown.
  - `cow-copies=2637` against `snapshots=2643` confirms it: one chunk per freeze.
  - Prices stay in about 860..1140 and quantities in 1..70, so overflow and boundary values are never generated (F-002).
  - Multi-chunk copy-on-write *is* covered by `TestFrozenViewSurvivesHeavyTradingAcrossChunks` (`cow_test.go:75`), but against the book's own `Orders`, not the oracle.
- **Why it matters:** the benchmark runs against a book of about 10,000 orders (3 chunks), a regime the "zero divergence" result never covered. A bug in the chunk or index code at that depth would pass acceptance 1.
- **Suggested fix:**
  - Add a regime, or a `-long` seed group, that builds and holds a deep book (more than 10k resting orders, several chunks, index growth past 1,024) while freezing.
  - Add an edge-value regime (huge quantities, price 1, `MaxInt64` prices).
  - Assert the depth and chunk count in `requireCoverage`.
- **Builder response:** Accepted — fixed in 5772dbe: every seed now sends `MaxQty` orders, IOCs at price 1 and `MaxPrice`, and just-out-of-bound invalid orders, with coverage floors. `TestDifferentialDeepBook` builds more than 10,000 resting orders and requires at least 3 chunks, an order index grown past 1,024 slots, and multi-chunk copy-on-write. It measured 11,473–11,615 orders, 3 chunks and 32,768 slots, with zero divergence (3 seeds with `-long`, alongside the 1.2M-command run).
- **Reviewer follow-up:** Closed — `make test-long`'s differential step at 449ab7e, with `-v`:
  - `TestDifferentialDeepBook` seeds 301–303: max depth 11,473–11,615, 3 chunks, 32,768 index slots, 111–121 chunk copies over 49–53 snapshots, zero divergence.
  - The 1.2M-command run reports `max-qty-orders=1287 edge-price-orders=1476`, with floors in `requireCoverage`.

  In the deep test, resting state and `CheckInvariants` are compared every 50 commands, not after every command (`internal/book/diff_test.go:L470-L473`). Results and fills are still compared on every command, and the comment says so. That is acceptable.

### F-005 — Unauthenticated `Subscribe`/`Resend` frames make the publisher goroutine do unbounded work
- **Severity:** Should-fix
- **Location:** `internal/gateway/session.go:L238-L250`, `internal/publisher/marketdata.go:L178-L213`, `internal/publisher/marketdata.go:L234-L252`
- **Category:** Concurrency
- **Problem:**
  - Market-data requests need no login (`session.go:L238-L250`). They run on the publisher goroutine, which every client's answers and the engine's output channel depend on.
  - **Resend:** one 18-byte frame makes the publisher push up to `ringSize` (65,536) events into that session (`marketdata.go:L207-L211`). Each push takes a mutex and appends.
  - **Subscribe:** each one copies and sorts the whole level map (`marketdata.go:L234-L244`).
  - Nothing limits connections, request rate, or per-request work.
- **Why it matters:**
  - Each Resend costs milliseconds of publisher time. A handful of connections that read fast and repeat `Resend(oldest)` can keep the publisher busy.
  - Then `pending` fills, the 65,536-event channel fills, and the engine blocks on `out <- buf[i]` (`engine.go:150`). Every client's latency moves.
  - That contradicts "It is never allowed to stall the engine or other clients" (`README.md:70`). The trusted-network caveat (`README.md:347`) covers authentication, not this.
- **Suggested fix:**
  - Serve resends from the session's own goroutine, out of an immutable or locked copy of the ring range. Or bound each Resend (for example to the soft queue limit) and rate-limit requests per session.
  - Snapshot copying could be shared per md_seq across concurrent subscribers.
- **Builder response:** Accepted — fixed in bf4c722: a Resend more than `publisher.MaxResend` (4,096) events back is answered `ResendUnavailable` (re-snapshot). Each connection's Subscribe/Resend requests are rate-limited in the gateway (burst 32, then 16/s; beyond that `Reject(TooManyRequests)`, reason 106, additive), and `client.Feed` retries a refused request after `RetryDelay`. That bounds one connection's publisher work to about 65,000 events/s. The number of connections is still unlimited, which the README now lists as a known limitation. `TestResendIsBounded` and `TestMarketDataRequestsAreRateLimited` each fail with their bound removed.
- **Reviewer follow-up:** Closed — a Resend more than `MaxResend` (4,096) events back is refused (`internal/publisher/marketdata.go:L211`). Each connection's Subscribe and Resend pass a token bucket (`internal/gateway/session.go:L327-L345`) before they reach the publisher. Both new tests pass in the suite. The number of connections is still unbounded. That is now a stated known limitation, consistent with the trusted-network scope.

### F-006 — The fill log is "not part of the durability contract", yet it fsyncs on the publisher goroutine and can refuse startup
- **Severity:** Should-fix
- **Location:** `internal/snapshot/filllog.go:L96-L115`, `internal/publisher/publisher.go:L288-L294`, `internal/wal/wal.go:L447-L501`, `internal/server/server.go:L176-L189`
- **Category:** Durability
- **Problem:**
  - **Fsync on the publisher:** the fill log is a `wal.Log` with default 64 MiB segments, written by `p.fills.Flush()` on the publisher goroutine (`publisher.go:L291`). `Flush` rolls a full segment, and `roll` always fsyncs the old segment first (`wal.go:L476-L479`). So about every 615k matches, the publisher, which "must never block" (`publisher.go:L16-L20`), does a synchronous 64 MiB fsync. The benchmark measured WAL roll fsyncs of the same size at 6.7–20 ms. Release stalls for every client meanwhile.
  - **Startup coupling:** opening it at startup uses the WAL's strict scanner. Damage that isn't a clean torn tail (possible after power loss, because the file is never fsynced except at rolls) returns `ErrCorrupt`, and `server.Start` fails (`server.go:L176-L180`). An audit-only file thus gates recovery of the real state.
- **Why it matters:** these are periodic release stalls on a path documented as non-blocking, and a possible refusal to start for a file the docs say may lose its tail.
- **Suggested fix:**
  - Give the fill log `wal.Options` that skip the roll fsync (a `NoSync` option), or move fill-log writing to its own goroutine.
  - On open, treat fill-log damage as "truncate and warn", not fatal.
- **Builder response:** Accepted — fixed in b1ad711: the fill log opens with the new `wal.Options.NoSyncOnRoll`, so the publisher never fsyncs it (`TestFillLogNeverFsyncs`: 14 segments, 0 fsyncs; 13 with the option off). A fill log damaged beyond a torn tail is moved aside to `fills.damaged-<ns>` with a warning, and a fresh one is started instead of `server.Start` failing (`TestDamagedFillLogIsMovedAside`). Decision 014 is updated.
- **Reviewer follow-up:** Closed — the fill log opens with `NoSyncOnRoll` (`internal/snapshot/filllog.go:L140`), so `roll` skips the segment fsync (`internal/wal/wal.go:L487`). A fill log that fails to open with `ErrCorrupt` is moved aside and a fresh one started (`filllog.go:L142-L150`). One residue, not reopened: `roll` still calls `fsutil.SyncDir` (`wal.go:L506`) on the publisher goroutine. That is a no-op on Windows. On Linux it is one directory fsync per 64 MiB fill-log segment: far cheaper than the segment flush, but "the publisher never fsyncs" is not literally true there.

### F-007 — The crash test checks much less than the README says, and never exercises a torn tail
- **Severity:** Should-fix
- **Location:** `internal/crashtest/crash_test.go:L305-L310`, `internal/crashtest/crash_test.go:L330-L339`, `internal/crashtest/crash_test.go:L357-L358`, `README.md:L98-L103`
- **Category:** Tests
- **Problem:**
  - **Fills:** a received fill whose match isn't in the replay because it is at or before replay-check's base is skipped (L338). In the 50-run measurement here, 104,726 of 364,791 received fills (29%) were compared with anything. The rest were only checked for duplicates.
  - **Acks:** acks whose WAL record has been pruned are skipped (L310): 121,213 of 405,668.
  - **Torn tails:** they are counted but never required (L357-L358), and 0 of 50 runs produced one. Decision 015:58 admits this.
  - Yet the README says, unqualified, that every fill a client received "is the one replay produces" (`README.md:L101`), that every acked command is in the WAL at its seq (L99), and that the crash test validates "torn tails" (L103).
- **Why it matters:** the acceptance-2 claim reads stronger than the evidence. A bug that corrupts fill content before a snapshot boundary would pass in about 70% of cases.
- **Suggested fix:**
  - Have the harness keep pruned WAL and fill-log segments: a test flag that disables deletion, or copy segments aside before each restart. Then check every ack and fill against a replay from seq 1.
  - Correct `README.md:L99-L103` to say what is actually checked, and that torn tails come from the unit tests, not the kill test.
- **Builder response:** Accepted — fixed in 3852a5e: with the new `-archive DIR`, pruned WAL and fill-log segments are moved aside rather than deleted. So after the last run, every acknowledged command and every received fill from every run is checked against a replay from seq 1, and pruning still runs. Every fifth run, the harness appends a torn record that the real binary must report and cut off. Measured over 50 runs: 447,645 of 447,645 acks and 401,510 of 401,510 fills checked, and 9 of 9 injected torn tails recovered. The README and decision 015 now say exactly what is checked, and that kills alone don't tear records.
- **Reviewer follow-up:** Closed — the 50-run `-crash` test at 449ab7e reports "all 372246 acknowledged commands and all 332510 received fills checked against" a replay from seq 1, and "torn WAL tails recovered in 9 runs". `README.md:L98-L108` now describes the per-run checks, the whole-history check, and that torn tails are injected. One consequence, not a defect: with `-archive`, the crash test takes the rename branch of `RemoveSegmentsBelow` (`internal/wal/wal.go:L612`), not the `os.Remove` branch production uses (L619). Which segments get pruned is decided by the same code either way.

### F-008 — The acceptance-5 test can't detect a meaningful latency shift
- **Severity:** Should-fix
- **Location:** `internal/server/ops_test.go:L437-L497` (tolerance at L482-L486), `internal/metrics/metrics.go:L192`
- **Category:** Tests
- **Problem:**
  - The test compares only p50 and p99 bucket indices, and passes if the slow run's bucket is at most one above baseline.
  - `MatchingBuckets` are 0.25, 0.5, 1, 2, 5, 10, 20, 50 … µs. A value just above 5 µs moving to 20 µs (about 4×) passes.
  - Each run is 3,000 commands, so p99 is 30 samples, and p99.9 and max aren't checked at all.
- **Why it matters:** "the engine's latency histogram does not move" (PRD §8.5) is effectively asserted only to within a factor of 2–4. A regression where a slow consumer blocks the engine for a few microseconds per event would pass.
- **Suggested fix:**
  - Compare with the 3.1% `LogHistogram` (`s.fine`) on a larger sample, and assert the shift at p50, p99 and p99.9 within a stated ratio.
  - Also assert directly that the engine never blocked on `events` during the slow phase, for example with a counter of sends that found the channel full.
- **Builder response:** Accepted — fixed in 7d161f7: the engine counts event sends that found its output channel full (`Engine.OutputBlocked`, `matchd_engine_output_blocked_total`), and the test requires 0 in both phases. It also compares p50/p99/p99.9 from the 3.1% `LogHistogram` within 1.4×/1.8×/2.5× on 6,000 commands per phase; the measured ratio was 0.90–1.06× under `-race` over 5 runs.
- **Reviewer follow-up:** Closed — `Run` counts sends that found the output channel full (`internal/engine/engine.go:L165-L170`). The acceptance-5 test requires that count to be 0 in both phases. It also compares p50, p99 and p99.9 from the 3.1% histogram within 1.4×, 1.8× and 2.5× (`internal/server/ops_test.go:L476`), on 6,000 commands per phase. The added `select` doesn't allocate; the zero-allocation tests pass.

### F-009 — README claims answers wait for disk under every fsync policy; under `never` they don't
- **Severity:** Should-fix
- **Location:** `README.md:L93`, `internal/sequencer/sequencer.go:L211-L212`, `docs/decisions/012-durability-and-fsync-policies.md:L33`
- **Category:** Docs
- **Problem:** `README.md:93` says, in bold, "Under every policy, no answer leaves the process before its command is on disk." Under `never`, the sequencer advances the durable seq to `LastAppended` right after `write()` (`sequencer.go:L211-L212`): page cache, not disk. Decision 012:33 says so correctly, and README:91 says acknowledged work can be lost.
- **Why it matters:** this is the headline durability sentence, and it is false for one of the three documented policies.
- **Suggested fix:** "Under `always` and `group`, no answer leaves the process before its command is fsynced; under `never`, before it is written to the OS."
- **Builder response:** Accepted — fixed in a6e0e30: the README now reads "Under `always` and `group`, no answer leaves the process before its command is fsynced to disk. Under `never`, before it is written to the operating system".
- **Reviewer follow-up:** Closed — `README.md:L94` now reads as suggested.

### F-010 — Acceptance 7 (Linux) isn't met, and README line 145 says Linux is CI-tested
- **Severity:** Should-fix
- **Location:** `README.md:L145`, `README.md:L333-L334`, `README.md:L372`, `.github/workflows/ci.yml`
- **Category:** Docs
- **Problem:**
  - PRD §8.7 requires Linux and Windows both to pass. The suite has never run on Linux (`README.md:372`), and the workflow has never executed.
  - `README.md:145` nonetheless states "Linux is cross-compiled and CI-tested for correctness". The Linux code paths (`syncdir_other.go`, `clock_other.go`, SIGKILL in the crash harness, `reset_other.go`) are compiled but have never run.
- **Why it matters:** an acceptance criterion is unmet, and one README sentence implies otherwise.
- **Suggested fix:**
  - Run `go test -race ./...` and `make test-long` on Linux (WSL2 or a VM is enough), or push and let CI run, and record the result.
  - Until then, change L145 to "cross-compiled; not yet run".
- **Builder response:** Accepted — fixed in 44d7883: the README no longer says "CI-tested". It says the suite has never run on Linux, that acceptance 7 is not met, and which Linux-only code paths are therefore untested, and it lists this under known limitations. Meeting acceptance 7 still needs a Linux run (push for CI, or run in a Linux VM/WSL distribution or a Go container). That wasn't done: the only WSL distribution here is Docker Desktop's internal one, and starting Docker to pull a Go image is the user's call.
- **Reviewer follow-up:** Still open — the README wording is fixed (`README.md:L150`, `README.md:L344`, `README.md:L384`). Acceptance 7 itself is still not met, because no test has run on Linux. The parts that can run here pass at 449ab7e: `GOOS=linux` vet, build, and `go test -c` of every package. Closing it needs one real Linux run of `go test -race ./...` and `make test-long`, via a CI push, WSL2 or a container. That is the owner's decision.

### F-011 — Benchmark tables omit the count, and some quantiles, that PRD 7.1 requires
- **Severity:** Should-fix
- **Location:** `README.md:L210-L214`, `README.md:L222-L225`, `README.md:L233-L236`, `bench/report/report.go`
- **Category:** Benchmark
- **Problem:** PRD 7.1: "Every reported latency is labelled p50, p99, p99.9, and max, with the count."

  | table | missing |
  |---|---|
  | fsync | count per run |
  | snapshot | count; end-to-end p50 and p99 |
  | Balanced | count; end-to-end p99 and max |

  Only the mix tables comply.
- **Why it matters:** this is a stated hard requirement of the benchmark protocol (PRD §7, "requirements, not suggestions"), and it is part of acceptance 6.
- **Suggested fix:** add the count and the missing quantile columns to those three tables in `bench/report`. The data is already in the result files.
- **Builder response:** Accepted — fixed in f073625: the fsync, snapshot and Balanced tables now give the count, p50, p99, p99.9 and max for every latency they report (core and end-to-end), from the committed result files. The README was regenerated; no re-run needed.
- **Reviewer follow-up:** Closed — every latency in the fsync, snapshot and Balanced tables now has count, p50, p99, p99.9 and max (`README.md:L212-L242`). `bench/report`, run with `make bench-report`'s arguments in a scratch copy, regenerates the README byte-identically at 449ab7e.

### F-012 — The fsync table mixes segment-roll fsyncs into the "fsync mean" column, and the README cites that column wrongly
- **Severity:** Should-fix
- **Location:** `README.md:L158`, `README.md:L214`, `bench/report/report.go:L557-L578` (the label is hard-coded at about L566-L570), `internal/wal/wal.go:L472-L479`
- **Category:** Benchmark
- **Problem:**
  - Under `never` there is no commit-path fsync. The 0.05–0.07 fsyncs/s are segment rolls, each flushing about 64 MiB of dirty pages. The "fsync mean" cell shows 9,467 µs (6,654–20,199) next to the per-commit means for `always` and `group`.
  - The "segment rolls only" label sits in the *neighbouring* column.
  - `README.md:158` says "This laptop's SSD takes 1–4 ms per fsync (the fsync table's 'fsync mean' column)". The column shows 2.6–4.0 ms and 6.7–20 ms. The 1.3–1.7 ms values come from mix/snapshot runs whose fsync means no table shows.
- **Why it matters:** these are misleading benchmark numbers, against PRD 7.6's comparison of the policies.
- **Suggested fix:**
  - Render `–` for `never`'s fsync mean, or a separately labelled "roll fsync" column.
  - Fix L158 to cite figures a table actually shows.
- **Builder response:** Accepted — fixed in 1a4d126: the `never` row's fsyncs/s, commands-per-fsync and fsync-mean cells are blank, with a note that its only fsyncs are 64 MiB segment rolls. The end-to-end mix table gained an "fsync mean" column, and the README now points at the two columns that show per-fsync times instead of quoting a range neither showed.
- **Reviewer follow-up:** Closed — `never`'s fsync cells are `–`, with a note on segment rolls (`README.md:L220-L222`). The reading guide now points at the two columns that show per-fsync times (`README.md:L163`).

### F-013 — README benchmark prose states things the committed results don't support
- **Severity:** Should-fix
- **Location:** `README.md:L159`, `README.md:L184`, `README.md:L192`, `README.md:L227`, `README.md:L231`, `README.md:L245`, `bench/internal/load/mix.go:L79-L81`
- **Category:** Benchmark
- **Problem:**
  - **(a) Fsync stalls (L159):** "A few seconds after a snapshot is written, a WAL fsync can take 30–80 ms, and while it does, intake stops too, because the next WAL append waits for it."
    - The result files don't record the stall counter, so no committed result shows 30–80 ms stalls. `scrape.go:146-154` doesn't scrape `matchd_wal_fsync_stalls_total`. The only source is an uncommitted 20 s reproduction (`docs/bugs/002:21-23`).
    - The "append waits" mechanism is marked **Unverified** in observation 002:29, yet the README states it as fact. In the code, `Flush` doesn't take `syncMu` except on a roll.
  - **(b) Book size (L184):** "holds the book near 10,000 resting orders (8398 (6797–10115))". The figure pools three populations: resting about 10,100, crossing about 8,400, cancel-heavy about 6,800. Cancel-heavy is about 32% below the claim and contradicts `mix.go:79-81`.
  - **(c) Clock footnote (L192, L227):** "one read costs 13 (11–14) ns as the engine measured it at startup". That figure came from `clock.Init` timed with `time.Now` at 73e6c8d, the bug fixed in 2b3dc3a. The Balanced runs' startup figures span 13–35 ns, against clockprobe's 13.7–16.1 ns.
    - The footnote also says the span "includes one clock read in between". It also includes `e.step.Observe` (`engine.go:152`), a histogram update.
  - **(d) Power plan (L231):** "a laptop with default power settings". The recorded mode is Balanced plan plus "best power efficiency", which isn't Windows' default.
  - **(e) Commands block (L245):** says every row was started by `benchrun.exe -suite full`. That is wrong for the Balanced rows, and it publishes a local absolute path.
- **Why it matters:** README text is presented as measured fact, but some of it can't be traced to the committed results.
- **Suggested fix:**
  - Scrape and record the stall counter and the max fsync in the result JSON, or reword L159 as an unverified observation that links 002.
  - Report book size per mix, or fix the cancel-heavy book size in the load generator.
  - Re-run, or drop the startup read-cost from the footnote and cite the clockprobe row.
  - Correct the plan wording and the commands line.
- **Builder response:** Accepted — fixed in 13875f7: (a) the snapshot-stall mechanism is stated as an unverified observation from a separate reproduction (linking 002), and the load generator now records fsync stalls and the slowest fsync's bucket for future runs; (b) book size is reported per mix, and mix.go's comment no longer claims the same size; (c) the core-latency footnote cites the clock probe's read cost (the startup figure was timed with `time.Now` before 2b3dc3a) and says the span includes a histogram update; (d) the Balanced row names its power mode and says it isn't Windows' default; (e) each suite's own `benchrun` command, with no local absolute path. Committed results showing the stalls would need a re-run of the snapshot pair (not run).
- **Reviewer follow-up:** Closed — each part is addressed:
  - **(a)** `README.md:L165` states the stall mechanism as an unverified observation from a separate reproduction, and says the committed results don't record stalls. The load generator now scrapes `matchd_wal_fsync_stalls_total` (`bench/internal/load/scrape.go:L182`) for future runs.
  - **(b)** book size is a per-mix column.
  - **(c)** the footnote cites clockprobe's 13.5 ns and mentions the histogram update.
  - **(d)** the power mode is named.
  - **(e)** the commands block no longer carries the `D:\…\benchrun.exe` path.

  Outside this finding, and already present at 2d75636: the per-row commands still contain `C:\Users\…\matchd-bench\…` data paths (11 lines).

### F-014 — The Balanced row reports n=3, below PRD 7.5's minimum of 5
- **Severity:** Should-fix
- **Location:** `Makefile:L79-L82` (`-min-runs-suite balanced=3`), `README.md:L160`, `README.md:L236-L241`
- **Category:** Benchmark
- **Problem:** two of five Balanced runs were disturbed by Defender and excluded. Instead of re-running, the Makefile overrides the minimum. It is disclosed, but PRD 7.5 says every configuration is run ≥ 5 times and reports median and min–max across them. Three clean runs is below that. `bench/CHECKLIST.md:110-111` itself calls re-running the cleanest option.
  - The clock runs adjacent to the disturbed ones (balanced 07, 09) get no "other CPU" check at all (`bench/internal/result/assess.go:57-60`), so a disturbance there would pass unnoticed.
- **Why it matters:** this is a protocol deviation in a table on the resume-facing page.
- **Suggested fix:**
  - Re-run the Balanced suite with Defender quiesced and drop the override.
  - Apply the other-CPU check to clock runs too.
- **Builder response:** Accepted — fixed in 5b08015 (the clock-run check); re-run needed for the rest. Clock-probe runs now record other processes' CPU, and `Assess` applies the same limit. The n=3 Balanced row stays, disclosed, because meeting PRD 7.5 needs `make bench-balanced` re-run (about 10 min, with Defender quiet, into an emptied `bench/results/balanced`), followed by removing `-min-runs-suite balanced=3` from the Makefile. Per instructions, not re-run.
- **Reviewer follow-up:** Still open — the clock-run part is fixed: clock runs now go through the same other-process CPU limit in `bench/internal/result/assess.go`. The Balanced row is still n=3, and `Makefile:L81` still passes `-min-runs-suite balanced=3`, below PRD 7.5's minimum of 5. Closing it needs `make bench-balanced` re-run on a quiet machine, then the override removed. That is the owner's call; I didn't run it.

### F-015 — The headline tables use snapshots off; the shipped default has snapshots on, with a 7× worse end-to-end tail
- **Severity:** Should-fix
- **Location:** `cmd/engine/main.go:L82`, `README.md:L184`, `README.md:L196`, `README.md:L224-L225`, `docs/bugs/002-snapshot-runs-stall-the-wal.md`
- **Category:** Benchmark
- **Problem:**
  - The engine's default is `-snapshot-interval 10s` (`main.go:82`), and the README sells it as the normal mode (L94). Every mix and fsync row runs with `-snapshot-interval 0`.
  - With the default on, end-to-end p99.9 is 163,578 µs against 22,544 µs off, and about 0.9% of commands wait more than 50 ms (L224-L225; observation 002).
  - The configuration a user actually runs is therefore represented only by the snapshot table. The cause stays open.
- **Why it matters:** the headline end-to-end numbers describe a non-default configuration. Each table does state its configuration, but the README doesn't say the default is the slow one.
- **Suggested fix:** do one of the following:
  - State in the mix-table caption that the shipped default (snapshots every 10 s) has the end-to-end tail of the snapshot table.
  - Or investigate and fix observation 002 (for example a separate volume for snapshots, or appending into a fresh segment while the old one syncs) and re-run.
- **Builder response:** Accepted — fixed in 3fbec84 (the first suggested fix): the mix-table captions and the README's reading guide now say the headline tables run with snapshots off, that the shipped default snapshots every 10 s, and that the snapshot table's "on" row is the end-to-end tail for the default. The second option (investigate observation 002, change the WAL/snapshot I/O, re-run) is a design change left for the user.
- **Reviewer follow-up:** Closed — through the first suggested fix. The mix-table captions and `README.md:L164` say the headline tables run with snapshots off and point to the snapshot table's "on" row for the default. The cause, observation 002, is still open, as the builder says.

### F-016 — `bench/internal/sysinfo` `TestAffinityAndTimes` is flaky on Windows
- **Severity:** Should-fix
- **Location:** `bench/internal/sysinfo/probe_test.go:L104-L107`
- **Category:** Tests
- **Problem:** `SelfTimes()` reads 0 at Windows' 15.6 ms CPU-time accounting granularity when the test process has used less than one tick, so `self.Total() <= 0` fails. It failed 2 of 6 runs here and 2 of 4 in the benchmark audit.
- **Why it matters:**
  - `make test` and `make ci` fail at random, which undermines acceptance 7.
  - It is also a candidate source for future "unreproduced" failures like observation 001. 001 was in `internal/server`, so this doesn't explain it, but it shows the suite has a flaky test that `make test-race-loop`'s default package doesn't cover.
- **Suggested fix:** burn measurable CPU in the test process before calling `SelfTimes`, for example a 50 ms spin, or assert `>= 0` and only check that the call succeeds.
- **Builder response:** Accepted — fixed in 8675aad: the test spins 50 ms before reading its own CPU time, so it is never below Windows' 15.6 ms accounting tick. 20 consecutive runs pass.
- **Reviewer follow-up:** Closed — the test now burns CPU before reading its own CPU time. `go test -count=20 -run TestAffinityAndTimes ./bench/internal/sysinfo/` passed 20 of 20 here, and the package passed in both full-suite runs.

### F-017 — No way to detect a lost final market-data message
- **Severity:** Nit
- **Location:** `docs/decisions/016-market-data.md:L134`, `internal/client/feed.go` (`sequenced`), `internal/server/marketdata_test.go:L179-L186`
- **Category:** MarketData
- **Problem:** gap detection fires only when a later md_seq arrives. There's no heartbeat carrying the latest md_seq, so a dropped last message is never detected; the acceptance-4 test works around this by sending one more order.
- **Why it matters:** over TCP, single frames aren't lost; a stalled consumer is disconnected instead. So the gap/resend machinery is reachable only via the test's `Drop` hook, and the limitation is real but marginal. Decision 016 documents it.
- **Suggested fix:** periodically send a per-instrument md heartbeat (instrument, last md_seq) to subscribers, or list it under known limitations in the README.
- **Builder response:** Accepted — fixed in 5c9b3ba (the second suggested fix): the README's known limitations list the missing market-data heartbeat, including the related case F-005 introduced (a rate-limited Resend retried only on a later message). Decision 016 is noted. A heartbeat message is a protocol addition left for later.
- **Reviewer follow-up:** Closed — the missing heartbeat is listed under known limitations (`README.md:L356`), including the refused-Resend case that F-005 added.

### F-018 — The core-latency window is wider than documented
- **Severity:** Nit
- **Location:** `bench/internal/load/run.go:L168-L185`, `docs/decisions/021-benchmark-harness.md:L173`
- **Category:** Benchmark
- **Problem:**
  - The comment says the engine's scrape window encloses the load generator's "by the time a scrape takes (about a millisecond)".
  - In fact `readCPU` (a full process-list query) runs between the first scrape and the phase flip, and the pacer keeps sending until `halt()`, after the second scrape.
  - Core count minus end-to-end count is about 100–220 commands per open-loop run, which is 5–18 ms of traffic, not about 1 ms.
- **Why it matters:** the quantile effect is negligible (about 0.02% of samples), but the documentation is wrong, and the core max could come from an edge.
- **Suggested fix:** flip the phase immediately after the first scrape and halt before the second, or correct the comment and the decision.
- **Builder response:** Accepted — fixed in 667678f: CPU times are now read before the first scrape, and sending stops before the second. Decision 021 now states that the committed results' engine window was 100–220 commands (5–18 ms) wider, not about 1 ms.
- **Reviewer follow-up:** Closed — `bench/internal/load/run.go` now reads CPU times before the first scrape, and stops sending before the second (667678f). Decision 021 states that the committed runs' engine window was 5–18 ms wider, since those results predate the change.

### F-019 — Stale statements in `bench/CHECKLIST.md` and `bench/README.md`
- **Severity:** Nit
- **Location:** `bench/CHECKLIST.md:L15`, `bench/CHECKLIST.md:L62`, `bench/CHECKLIST.md:L111`, `bench/CHECKLIST.md:L136`, `bench/README.md:L9`
- **Category:** Docs
- **Problem:**
  - **L136:** "bench-report refuses if anything is disturbed", but the Makefile passes `-allow-disturbed`.
  - **L62:** says the environment line will report a missing Defender exclusion; nothing does that.
  - **L111:** mentions `-min-runs 4`, but the Makefile uses `-min-runs-suite`.
  - **bench/README.md L9:** "every run checked for disturbance", but clock runs aren't.
  - **Durations disagree:** smoke 1m20s / 1.5 min / 2 min; full 75 / 85 min.
- **Why it matters:** the operator follows a checklist that no longer describes the tool.
- **Suggested fix:** update the checklist and bench README to match the Makefile and `assess.go`.
- **Builder response:** Accepted — fixed in 1bceb0a: the checklist and bench README now match the Makefile and `assess.go`. `bench-report`'s `-allow-disturbed`/`-min-runs-suite` flags for the committed results are explained, `-min-runs-suite` replaces `-min-runs 4`, the claim that the README reports a missing Defender exclusion is replaced by how `-asserted` works, clock runs are now checked (F-014), and the durations agree everywhere.
- **Reviewer follow-up:** Closed — `bench/CHECKLIST.md:L62`, `bench/CHECKLIST.md:L111`, `bench/CHECKLIST.md:L136` and `bench/README.md:L9` now match the Makefile and `assess.go`.

### F-020 — The TSC conversion wraps if a core's TSC reads below the calibration base
- **Severity:** Nit
- **Location:** `internal/clock/clock.go:L48-L51`
- **Category:** Other
- **Problem:** `bits.Mul64(tsc-c.baseTSC, c.mult)` uses unsigned subtraction. If `rdtsc` on another core returns a value even slightly below `baseTSC` (cross-core skew), the result is a timestamp about 2^64 ticks in the future. Invariant TSCs are normally synchronised across cores on one socket, so this is unlikely on the benchmark machine.
- **Why it matters:** a negative or garbage span in a histogram, or a wildly wrong command timestamp, which is then logged to the WAL forever.
- **Suggested fix:** compute the delta as signed (`int64(tsc - base)`) and clamp, or take the base slightly in the past.
- **Builder response:** Accepted — fixed in 8e86d91: the tick difference is signed, so a reading a few ticks below the calibration base converts to a time just before it. `TestConversionBelowTheBase` fails on the old code (-9.2e18 ns) and passes now.
- **Reviewer follow-up:** Closed — `calibration.ns` (`internal/clock/clock.go:L51-L59`) handles a reading below the base with its own subtraction. `TestConversionBelowTheBase` passes.

## Overall assessment

The core is careful work. The publisher's seq-ordered release gate, the group syncer, the copy-on-write snapshot, and the snapshot-before-durable guard all hold up under reading, and I found no path where an event leaves before its command is durable under `always` or `group`. Replay is deterministic, md_seq included, and the regenerated benchmark tables are byte-identical.

It is not yet production-quality for its own scope:
- **Two real bugs reachable by ordinary clients.** The stale write deadline (F-001) can drop the fills of a client that is reading fine. Unbounded quantities (F-002) make matching diverge from the oracle.
- **The duplicate client-order-id rule** doesn't match the PRD (F-003).
- **The README is stronger than the evidence:**
  - the fsync=never durability sentence (F-009);
  - what the crash test actually checks (F-007);
  - "Linux CI-tested" (F-010);
  - several benchmark statements and PRD 7.1 omissions (F-011–F-015).
- **Acceptance 7 is not met**, and acceptances 5 and 6 are only partially met.

Before putting this on a resume, I'd want:
- F-001–F-003 fixed, with tests;
- the differential test extended to deep books and edge values (F-004);
- one real Linux run;
- the README benchmark and crash-test prose brought down to exactly what the committed data shows.

## Follow-up — 2026-10-14 — commit 449ab7e

All of this ran on the same machine as the review, at HEAD `449ab7e`, with a clean tree apart from the untracked `REVIEWER.md`. I read each fix commit, re-ran the relevant tests, and checked the F-001, F-002 and F-003 tests against reverted code in a scratch copy. The repository itself was not changed.

| command | result |
|---|---|
| `go build ./...`, `go vet ./...`, `gofmt -l .` | ok, clean |
| `golangci-lint run ./...` | `0 issues.` |
| `GOOS=linux` vet, build, and `go test -c` of every test package | ok (compile only; nothing ran on Linux) |
| `go test -count=1 ./...` | all packages ok |
| `go test -race -count=1 ./...` | all packages ok |
| `make test-long`'s two commands, with `-v` | ok: 1.2M differential commands plus 3 deep-book seeds; 50 crash runs with the whole-history check and 9 torn tails |
| `bench/report` with `make bench-report`'s arguments, against a copy of README.md | byte-identical |
| `go test -count=20 -run TestAffinityAndTimes ./bench/internal/sysinfo/` | 20 of 20 pass |

**Result:** 18 findings closed. Two remain open, and each needs the owner's action, not more code:
- **F-010:** acceptance 7 needs a Linux run.
- **F-014:** the Balanced row needs a re-run to reach n ≥ 5.

No fix introduced a new problem. Two residues are noted under their findings and not reopened: the directory fsync on Linux (F-006), and the user-directory paths in the per-row commands (F-013).

**Acceptance criteria now:**

| # | Status |
|---|---|
| 1 | Met. The coverage gap is closed by F-004. |
| 2 | Met. Every ack and fill is now checked, and torn tails are recovered (F-007). |
| 3 | Met |
| 4 | Met |
| 5 | Met (F-008) |
| 6 | Partially met. The tables are complete (F-011), but the Balanced row is still n=3 (F-014). |
| 7 | Pending CI (F-010): the workflow is set up but has not yet run, so nothing has run on Linux. |
| 8 | Met |

**Not reviewed:** the two commits outside the findings, ea3465f (crash-harness lock) and 449ab7e (open-loop throughput count). They are covered only by the passing suite and the byte-identical report.

## Status at v0.1-reviewed — 2026-10-14 — commit 661a659

Added by the builder after the author's rulings; the reviewer's lines above are unchanged. Since the follow-up, `make bench-balanced` was re-run (9abe845): all 5 Balanced resting runs are clean, and 1 of 5 Balanced clock-probe runs was disturbed by Defender and is left out, by the author's ruling. `make ci` (build, vet on Windows and Linux, lint, Linux cross-compile, the race suite) passed at 661a659.

| # | Status at v0.1-reviewed |
|---|---|
| 1 | Met |
| 2 | Met |
| 3 | Met |
| 4 | Met |
| 5 | Met |
| 6 | Met. Every latency row has 5 clean runs (F-014). The one exception, ruled by the author, is the Balanced clock-overhead row, which counts 4 of 5 runs, with the disturbed run listed under the tables. |
| 7 | Pending CI (F-010). The workflow is set up but has not yet run, so nothing has run on Linux. |
| 8 | Met |
