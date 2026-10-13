# 07 — Operations: metrics, timing a microsecond, the admin endpoint, and shutting down without losing an answer

Milestones 1–6 built an engine that is correct, durable and observable to its clients. Milestone 7 makes it **operable**. An operator can:
- **watch it:** Prometheus metrics, a health check, a log line per command;
- **change it while it runs:** add instruments over HTTP;
- **stop it** without a client losing a single answer.

Along the way it turned out that **Windows can't time anything this short with `time.Now`**. Most of this document's most interesting material came from that discovery and its fix.

---

## 1. Concepts

### 1.1 Metrics, and why histograms

A **metric** is a number a program keeps about itself, read periodically by a monitoring system. **Prometheus** *pulls*: every few seconds it fetches `GET /metrics`, a plain-text page of `name{labels} value` lines ([exposition format](https://prometheus.io/docs/instrumenting/exposition_formats/)). Three kinds matter here:
- **Counter:** only goes up (commands executed, bytes written). Prometheus computes rates from successive readings.
- **Gauge:** goes up and down (open connections, seconds since the last snapshot).
- **Histogram:** counts observations into **buckets** by upper bound. `le="1e-06"` means "at most 1 µs", and the counts are cumulative, plus a `_sum` and a `_count`.

**Why histograms, not averages:** an average hides the tail. A matching engine's 99.9th percentile is what a trading client feels on a bad day. From bucket counts you can say which bucket the p99 is in, and you can aggregate histograms across time and machines, which you can't do with precomputed percentiles ([histograms and summaries](https://prometheus.io/docs/practices/histograms/)).

**Naming:** Prometheus convention is base units in the name: `_seconds`, `_bytes`, `_total` for counters ([naming](https://prometheus.io/docs/practices/naming/)). Our buckets are *defined* in microseconds and *shown* in seconds, so 0.25 µs is `le="2.5e-07"`.

### 1.2 What "matching latency" means, exactly

PRD 7.1: from the moment the engine goroutine **dequeues** a command to the moment it has **emitted** all its events to the publisher channel. That excludes the WAL, which is before the engine, and the network, which is after the publisher. It includes the handoff of events to the publisher, so if the engine were ever blocked by a slow downstream, this is where it would show. That's what acceptance 5 checks. We also record `Step` alone (`matchd_matching_step_seconds`), the matching work itself.

### 1.3 Timing sub-microsecond work on Windows

This milestone's real lesson. The numbers are from this machine (Windows 11, a 2.1 GHz x86-64 laptop).

**Why `time.Now` can't do it on Windows.** On Windows, Go's clock reads a time value that the kernel updates on each **timer interrupt**: a periodic tick, whose interval is the system's timer resolution (15.6 ms by default, down to 0.5 ms when some program asks for finer timing). Between ticks, every reading returns the same value. Measured here:
- 999,980 of a million back-to-back pairs of `time.Now` readings were identical;
- the rest differed by about **515 µs**, one tick.

This isn't a Go bug: it's the clock the operating system keeps for ordinary timekeeping, and reading it is cheap (about 9 ns). But a matching step takes about **1 µs**, so a histogram fed by `time.Now` would record about 0 for nearly every command and 515 µs for an unlucky few. The p50 would be "0" and the p99.99 "half a millisecond", and both would be fiction.

On **Linux** the same `time.Now` reads `clock_gettime(CLOCK_MONOTONIC)` through the vDSO, a kernel page mapped into every process, so no system call is made. It has nanosecond resolution and typically costs about 20 ns. That's the usual figure, *not measured here*, since Linux hasn't been run locally. So the problem is Windows-specific.

**QueryPerformanceCounter (QPC)** is Windows' answer for high-resolution timing ([Acquiring high-resolution time stamps](https://learn.microsoft.com/en-us/windows/win32/sysinfo/acquiring-high-resolution-time-stamps)): a counter, here at 10 MHz, so **100 ns resolution**. The catch is the cost *from Go*: **78–179 ns per call** measured (178 ns in the first measurement, 78–94 ns in later best-of-three runs). The function itself is cheap. The cost is Go's **syscall boundary**: calling a DLL function means switching from the goroutine's small stack to the system stack and telling the scheduler this goroutine is in a system call (so it can run others meanwhile), then undoing all that. Two reads per command would add 160–360 ns to a 1 µs operation: the measurement would cost about as much as part of what it measures.

**RDTSC.** Every x86 CPU has a **time-stamp counter (TSC)**, a 64-bit register counting at a fixed rate, readable with one instruction, `RDTSC`. Measured here: about **12 ns** per read through a small assembly function, ticking at 2.112 GHz (sub-nanosecond resolution). Two caveats made it unusable in the past:
- On old CPUs it counted *cycles*, so its rate changed with frequency scaling (turbo, power saving), and it could stop in deep sleep states.
- Different cores' counters weren't guaranteed to agree, so a goroutine moved to another thread mid-measurement could see time go backwards.

An **invariant TSC** fixes both. It ticks at a constant rate, regardless of frequency changes and sleep states, and on such CPUs the counters are synchronised across cores. The CPU advertises it with CPUID leaf `0x80000007`, EDX bit 8 (Intel SDM, Vol. 3, "Time-Stamp Counter"). Windows itself builds QPC on the TSC when the TSC is reliable. We read it directly to skip the syscall.

**Calibration.** The TSC counts ticks, not nanoseconds, and its rate isn't exposed directly. So at startup `clock.Init` measures it against QPC:
1. Read QPC, then the TSC, then QPC again, 16 times. Keep the try whose two QPC reads are closest together, and use their midpoint as "the QPC time of that TSC read".
2. Sleep 100 ms.
3. Do step 1 again.

The rate is ΔTSC ÷ Δtime(QPC). Each end is uncertain by about one QPC tick (100 ns) plus the bracket's width, so over 100 ms the rate is off by roughly a few parts per million (ppm). Nanoseconds are then `(tsc − base) × mult >> 32`, with `mult` a 32.32 fixed-point "ns per tick" and a 128-bit multiply (`math/bits.Mul64`), so it never overflows. The TSC timeline is anchored to QPC's, so switching between the two can't make time jump.

**How much drift?** `TestCalibrationDriftAgainstQPC` compares 3 s of TSC-derived time with QPC:
- measured −0.02, 1.02 and 3.62 ppm, and −9.85 ppm under the race detector;
- **the stated bound is 50 ppm**, which is 0.05 ns on a 1 µs command and 150 µs over 3 s.

For durations of microseconds the calibration error is irrelevant. It would only matter for long intervals, which this clock isn't used for.

**Why about 12 ns is acceptable and 2 × 178 ns isn't.** The engine reads the clock three times per command (start, after `Step`, after the sends): about 45 ns with the TSC. Under sustained load `Step` averages **821 ns** and the whole command **1.5 µs**, so timing costs about 3–5%, small enough that PRD 7.1's fallback (batch timing) isn't needed. With QPC the same three reads would cost 240–540 ns: 30–60% of the measured work, dominating the low percentiles and inflating every number. The fallback to QPC still exists for CPUs without an invariant TSC, and `TestForcedQPCFallback` runs it.

**What else the clock fixes.** The sequencer stamps every command with a wall-clock time. With `time.Now` on Windows, a burst of commands would all share one timestamp per 515 µs tick. `clock.WallNanos` (the wall time once at startup, plus elapsed TSC time) gives each command a distinct, increasing timestamp.

**One more lesson: how long a command takes depends on how busy the engine is.**
- With clients sending one order at a time, the engine idles between commands, and each command runs on a cold core (cold caches, clocked-down CPU): `Step` lands in the 2–10 µs buckets.
- Under a sustained burst it's under 1 µs at the median.

Milestone 2's 0.3 µs was a hot loop of `Step` with nothing else running. A latency number is meaningless without saying which regime it came from, and the benchmark (milestone 9) will.

### 1.4 Shutting down a server without losing answers

A clean shutdown has an order, and each step protects something:

1. **Stop taking new work.** Otherwise there's no end.
2. **Drain and fsync.** Everything accepted gets executed and made durable. Durability first, because everything after is optional.
3. **Deliver.** Clients get the answers to what they sent. Before this milestone, `Close` disconnected clients *first*, so orders were executed but their acks were lost, and the clients had to treat those orders' fate as unknown.
4. **Snapshot.** An optimisation for the next start, so it never comes before durability.
5. **Close.**

**A TCP subtlety: closing with unread data sends a reset.** RFC 1122 §4.2.2.13 ([RFC 1122](https://www.rfc-editor.org/rfc/rfc1122)) says a host that closes a connection while received data is still unread SHOULD send a **RST** (reset) instead of a FIN, to signal that data was lost. Windows and Linux both do. A RST also makes the *other* side discard data it has received but not yet read. During shutdown that's exactly the situation: our readers have stopped, the client is still sending, and its answers are in flight or unread. The fix, the **lingering close** web servers use:
1. **half-close** (`shutdown(SHUT_WR)`, Go's `CloseWrite`), which sends a FIN: "I'm done sending";
2. read and discard until the client closes, or a short timeout;
3. then close, with nothing unread, so no reset.

TCP's half-close is in [RFC 9293](https://www.rfc-editor.org/rfc/rfc9293) §3.6.

### 1.5 Admin commands go through the log too

Adding an instrument could be a direct call into the engine's registry. But then it wouldn't be in the WAL: after a restart the instrument would be gone, or worse, get a different id, and every order logged against id 3 would replay against the wrong book. So `POST /instruments` builds an `AddInstrument` **command**, sequenced and logged like an order (decision 005). Its answer comes back through the durability gate, routed to a client id reserved for the admin.

---

## 2. Code walkthrough

### `internal/clock`

- **`Now`** ([clock.go:64](../../internal/clock/clock.go#L64)): if a calibration is stored, read the TSC and convert (`calibration.ns`); otherwise the fallback source. One atomic load, no allocation.
- **`WallNanos`** ([clock.go:76](../../internal/clock/clock.go#L76)): wall time at `Init`, plus elapsed `Now`.
- **`Init`** ([clock.go:85](../../internal/clock/clock.go#L85)) and **`setup`** ([clock.go:92](../../internal/clock/clock.go#L92)): choose and calibrate once (`sync.Once`), then measure the chosen source (`describe`: resolution, and read cost as the best of three passes). Takes about 0.1 s on Windows.
- **`calibrate`** ([clock_windows.go:51](../../internal/clock/clock_windows.go#L51)) and **`pair`** ([clock_windows.go:76](../../internal/clock/clock_windows.go#L76)): the 100 ms comparison against QPC described in §1.3, rejecting implausible rates (under 100 MHz or over 20 GHz).
- **`qpc`** ([clock_windows.go:29](../../internal/clock/clock_windows.go#L29)): `syscall.SyscallN` into `kernel32.dll`. **`qpcNS`** ([clock_windows.go:37](../../internal/clock/clock_windows.go#L37)) converts ticks to nanoseconds without overflowing: `ticks × 1e9` would overflow after about ten days of uptime.
- **`invariantTSC`** ([tsc_amd64.go:13](../../internal/clock/tsc_amd64.go#L13)), with `rdtsc` and `cpuid` in `tsc_amd64.s`. On other architectures, `tsc_noasm.go` reports no TSC; on other OSes, `clock_other.go` uses Go's monotonic clock.

### `internal/metrics`

- **`Histogram`** ([metrics.go:82](../../internal/metrics/metrics.go#L82)): bounds in nanoseconds, one atomic counter per bucket plus an overflow bucket, an atomic sum. **`Observe`** ([metrics.go:100](../../internal/metrics/metrics.go#L100)) scans the bounds linearly (about a dozen; most observations land early), then makes two atomic adds: 14 ns.
- **`HistogramSnapshot`** ([metrics.go:116](../../internal/metrics/metrics.go#L116)): `Sub` (the observations between two readings) and `QuantileBucket` (which bucket holds the p99); the tests use both.
- **`Micros`** ([metrics.go:179](../../internal/metrics/metrics.go#L179)) and the bound lists `MatchingBuckets` and `FsyncBuckets`: the decided microsecond values, as exact nanoseconds.
- **`Registry`** ([metrics.go:201](../../internal/metrics/metrics.go#L201)): `Counter`, `Gauge`, `Histogram`, plus `CounterFunc` and `GaugeFunc` for values a component already keeps, read at scrape time. **`WriteText`** ([metrics.go:276](../../internal/metrics/metrics.go#L276)) renders families sorted by name into a buffer and writes it once, which leaves a single write error to report; **`formatFloat`** ([metrics.go:339](../../internal/metrics/metrics.go#L339)) uses the shortest round-tripping form.

### Where it's recorded

- **`Engine.Run`** ([engine.go:117](../../internal/engine/engine.go#L117)): `start := clock.Now()`, `Step`, `stepped := clock.Now()`, the sends, then `step.Observe(stepped-start)` and `latency.Observe(clock.Now()-start)`. **`SetLatency`** ([engine.go:50](../../internal/engine/engine.go#L50)) wires the two histograms. `Step` now also stamps each event with its command's kind (`Event.Cmd`), and every delta carries the instrument's resting order count (`Event.Orders`, from `Book.Len`, O(1)), both in what was padding.
- **`Log.Sync`** ([wal.go:513](../../internal/wal/wal.go#L513)): times every fsync; `Flush` counts bytes written.
- **`Publisher.count`** ([publisher.go:143](../../internal/publisher/publisher.go#L143)): called for each **released** event. It counts the command decided by `Ack`, `Reject`, `Cancelled(requested)` or `InstrumentAdded`, plus fills and trades, and logs the debug line per command. The **`Metrics`** struct ([publisher.go:128](../../internal/publisher/publisher.go#L128)) holds the counters.
- **`registerMetrics`** ([metrics.go:16](../../internal/server/metrics.go#L16), server package) creates every series and wires the hot-path ones. **`snapshotAge`** ([metrics.go:85](../../internal/server/metrics.go#L85)) reports +Inf when no snapshot exists.

### The HTTP side (`internal/server`)

- **`listenHTTP`** ([http.go:17](../../internal/server/http.go#L17)): Go 1.22 method patterns (`"GET /instruments"`), with read and header timeouts.
- **`handleHealth`** ([http.go:63](../../internal/server/http.go#L63)), **`handleListInstruments`** ([http.go:74](../../internal/server/http.go#L74)) and **`handleAddInstrument`** ([http.go:90](../../internal/server/http.go#L90)): a 4 KB body limit, unknown fields rejected, errors mapped to 400/409/503/504.
- **`AddInstrument`** ([admin.go:70](../../internal/server/admin.go#L70)):
  1. validate;
  2. allocate a request id;
  3. tell the publisher the spec (`ExpectInstrument`);
  4. `submit` the command under `ReservedClientID`;
  5. wait for the answer, which the **`admin`** subscriber ([admin.go:27](../../internal/server/admin.go#L27)) receives from the publisher like any client.
- **`Publisher.Instruments`** ([marketdata.go:104](../../internal/publisher/marketdata.go#L104)): a request answered between events on the publisher goroutine, returning `InstrumentInfo` ([marketdata.go:91](../../internal/publisher/marketdata.go#L91)) entries. **`ExpectInstrument`** ([marketdata.go:143](../../internal/publisher/marketdata.go#L143)) holds a pending spec until its `InstrumentAdded` is released.

### Graceful shutdown

- **`Server.Close`** ([server.go:346](../../internal/server/server.go#L346)): the order in §1.4.
- **`Gateway.StopReading`** ([gateway.go:215](../../internal/gateway/gateway.go#L215)): sets `draining`, closes the listener, wakes every reader with `SetReadDeadline(now)`, and waits on the `readers` WaitGroup. The reader loop checks `draining` before and after re-arming its idle deadline, and on a read error, then returns without closing the session.
- **`Gateway.FinishWriters`** ([gateway.go:240](../../internal/gateway/gateway.go#L240)): closes `finishing`; each writer, once its queues are empty, flushes, lingers and closes. After the timeout, everything left is closed by force.
- **`session.lingerClose`** ([session.go:548](../../internal/gateway/session.go#L548)): `CloseWrite`, then discard input until EOF or `lingerTimeout` (1 s).
- **`shutdownSnapshot`** ([server.go:400](../../internal/server/server.go#L400)): skipped if snapshots are disabled, durability failed, or nothing changed. Otherwise freeze the stopped engine, `Writer.WriteNow`, thaw.
- `cmd/engine`: `-http`, `-shutdown-timeout`, and a second signal exits at once.

### `engctl`

`resolveInstrument` turns a symbol into an id via `GET /instruments`. `splitTarget` lets `book XYZ` and `book --instrument XYZ` both work. `instruments` and `add-instrument` are new.

### Tests and results

| test | result |
|---|---|
| `TestTextFormat`, `TestBoundsRoundTrip`, `TestRecordingDoesNotAllocate`, `TestConcurrentRecordAndScrape` | exact output; every bound round-trips; 0 allocations; consistent under `-race` |
| `TestCalibrationDriftAgainstQPC` | −0.02 / 1.02 / 3.62 ppm (bound 50) |
| `TestForcedQPCFallback` | QPC path works, 78–94 ns per read, 0 allocations |
| `TestRunRecordsLatencyWithoutAllocating` | 2 heap allocations over 20,000 commands (runtime background noise; a planted per-command allocation shows 40,006) |
| `TestMetricsEndpoint`, `TestMetricsCountOnlyDurableCommands` | every series correct; nothing counted before its fsync |
| `TestSlowConsumerDoesNotMoveEngineLatency` (**acceptance 5**) | stalled subscriber disconnected; p50 and p99 in the same buckets as the baseline (e.g. 3,000 commands each, p50 ≤ 50 µs under `-race`) |
| `TestInstrumentEndpoints`, `TestAdminRejectForgetsSpec`, `TestHealthzAfterDurabilityFailure` | as in decision 018 |
| `TestGracefulShutdownDrainsAndSnapshots` | e.g. 1,002 of 2,000 pipelined orders accepted before shutdown, all 1,002 answered; restart replayed 0 WAL records |
| `TestShutdownDoesNotResetAClientStillSending` | every ack readable after shutdown; **0** without the lingering close |
| `TestShutdownTimeoutCutsAStuckClient`, `TestCrashDuringShutdownSnapshotLosesNothing`, `TestSignalShutsDownGracefully` | as in decision 019 |
| crash test, 50 runs | passed, 258,665 acknowledged commands. Each engine start now spends ~0.1 s calibrating the clock, which is most of why it took 33 s against milestone 6's 19 s |

**Latency, measured in-process** (from `/metrics`, this laptop, not a benchmark):

| regime | `Step` | whole command (dequeue → events sent) |
|---|---|---|
| one order at a time (round trips) | p50 bucket 2–10 µs | one bucket higher |
| sustained burst, 50,000 pipelined orders | p50 ≤ 1 µs, mean 821 ns | mean 1.5 µs |

---

## 3. Design decisions

- [017: Metrics and the clock](../decisions/017-metrics-and-clock.md). Why the exporter is written by hand; the decided buckets (µs, shown in seconds); what is measured where; counting only released events; the Windows clock problem, the options measured, the TSC solution and its conditions; and how load changes latency.
- [018: The HTTP side](../decisions/018-http-admin-and-instruments.md). The endpoints; `POST /instruments` as a logged command with a reserved client id; validation before sequencing (and the latent WAL panic it closed); `GET /instruments` from the publisher's view, with the order count on deltas; `engctl` symbols.
- [019: Graceful shutdown](../decisions/019-graceful-shutdown.md). The order; why a crash during the snapshot loses nothing; the lingering close.

---

## 4. What breaks if

1. **The histogram uses `time.Now` on Windows.** Nearly every command records 0 ns, and about one in 2,000 crosses a timer tick and records about 515 µs. The dashboard shows "p50 = 0, p99.9 = 0.5 ms", and someone spends a week hunting a non-existent 0.5 ms stall. Worse, acceptance 5 becomes meaningless: both runs show the same garbage, so a real stall could hide in it.

2. **Metrics count commands as the engine executes them, not on release.** Under group commit, a power failure loses the last window of executed-but-unsynced commands. But `matchd_commands_total` had already counted them, and a scrape may have recorded them: monitoring says 1,000 orders were accepted, while the recovered exchange has 990 and no client ever got those 10 acks. *Planted and caught:* `before fsync: accepted 1`.

3. **Shutdown closes the connections before draining (the old behaviour).** Every pipelined client loses the answers to whatever was in flight. Those orders were executed and logged; they're on the book after restart. But the client never heard, so its view of its own orders is wrong. *Planted and caught:* 851 acks received for 1,004 orders executed.

4. **Shutdown closes each session with a plain close.** *Real, found by the test:* the client is still sending, so the server's socket has unread input, the close becomes a RST, and the RST destroys acks sitting unread in the client's receive buffer. It's intermittent in normal traffic (3 runs in 8) and total when the client hasn't read yet (0 of 300). Fixed by the lingering close.

5. **`POST /instruments` called the engine's registry directly.** Instruments added at runtime wouldn't be in the WAL. After a restart they vanish, and the next one added takes the same id: orders logged against the old id 3 replay against a different instrument's book. Through the log, the instrument is replayed first and keeps its id (`TestInstrumentEndpoints` restarts and checks).

*And one latent bug closed:* a symbol longer than 255 bytes made the WAL's command encoder panic inside the sequencer. Nothing checked symbol length before this milestone. *Planted variant:* validation skipped, and the test server crashed with `symbol of 300 bytes exceeds 255`.

---

## 5. Go idioms introduced

**Assembly functions.** `tsc_amd64.s` defines `TEXT ·rdtsc(SB),NOSPLIT,$0-8`:
- `·` joins package and symbol, and `SB` is the static base;
- `NOSPLIT` skips the stack-growth check, which a leaf function this small doesn't need;
- `$0-8` means 0 bytes of locals and 8 bytes of arguments and results.

The result goes to `ret+0(FP)`. A `.go` file declares `func rdtsc() uint64` with no body. The `_amd64` filename suffix limits both to that architecture ([A Quick Guide to Go's Assembler](https://go.dev/doc/asm)).

**Build constraints by filename and by `//go:build`.** `clock_windows.go` builds only on Windows; `clock_other.go` starts with `//go:build !windows`; `tsc_noasm.go` with `//go:build !amd64`. The compiler picks exactly one implementation of `fallbackNow` and `calibrate` per target. `make ci` cross-compiles for Linux to prove the other combination builds.

**Methods on a nil receiver.** `func (h *Histogram) Observe(ns int64) { if h == nil { return } … }`. In Go a method can be called on a nil pointer: the receiver is just a nil argument. So the engine always calls `e.latency.Observe(...)`, and unit tests that never wire metrics pay one nil check instead of needing `if e.latency != nil` everywhere.

**`atomic.Pointer[T]` and `sync.Once`.** The calibration is stored in an `atomic.Pointer[calibration]`: `Now` does one atomic load, and nil means "use the fallback". `sync.Once` makes `Init` run its body exactly once, however many servers start in a test binary, and every caller of `Init` sees its effects.

**`math/bits.Mul64`** returns the full 128-bit product of two `uint64`s as `(hi, lo)`. `(hi<<32 | lo>>32)` is then the product shifted right by 32, the fixed-point multiply, with no overflow however long the process runs ([bits.Mul64](https://pkg.go.dev/math/bits#Mul64)).

**`net/http` method patterns (Go 1.22).** `mux.HandleFunc("POST /instruments", h)` matches the method and path; a request with another method gets 405 automatically ([ServeMux](https://pkg.go.dev/net/http#ServeMux)). `http.MaxBytesReader` caps the body, and `json.Decoder.DisallowUnknownFields` turns a typo in a field name into a 400 instead of a silently ignored field.

**`(*net.TCPConn).CloseWrite`** half-closes: it sends FIN, but the socket can still be read. That's the basis of the lingering close ([TCPConn.CloseWrite](https://pkg.go.dev/net#TCPConn.CloseWrite)).

---

## 6. Self-check questions

1. Why does `time.Now` advance in ~515 µs steps on this Windows machine but in nanoseconds on Linux? Is it a Go limitation?
2. The engine reads the clock three times per command. Compare the cost with QPC and with the TSC against a command's 0.8–1.5 µs, and explain why one is acceptable and the other isn't. What makes a QPC read from Go so slow?
3. What does "invariant TSC" mean, and what would go wrong measuring with a TSC that isn't invariant? How is the TSC's rate found, and how large is the error?
4. `matchd_commands_total` is counted in the publisher on release, not in the engine. What could a scrape show, under group commit and a power failure, if it were counted in the engine?
5. During shutdown, why can closing a client's socket destroy answers the server already sent successfully? What does the server do instead?
6. Why is the shutdown snapshot written *after* the final fsync, and why is it skipped after a durability failure?

<details>
<summary>Answers</summary>

1. On Windows, Go reads a time value that the kernel updates once per **timer interrupt**. Between ticks every reading is identical. The tick here is 0.5 ms, the system's timer resolution (15.6 ms by default; some program had asked for finer). So it's the operating system's ordinary clock, not a Go limitation; reading it is cheap (~9 ns) but coarse. On Linux, `time.Now` calls `clock_gettime` through the vDSO, a nanosecond-resolution clock read in user space for ~20 ns (the typical figure; not measured here).

2. Three QPC reads at 78–179 ns cost 240–540 ns, which is 30–60% of the work being measured. The measurement would dominate the low percentiles and inflate every figure. Three TSC reads at ~15 ns cost ~45 ns, 3–5%, and the decision records that cost so the benchmark can footnote it. A QPC call is slow from Go because of the **syscall boundary**: calling a DLL means switching to the system stack and doing the scheduler's enter/exit-syscall bookkeeping, so other goroutines can run if the call blocks. That costs far more than QPC's own work.

3. An invariant TSC ticks at a **constant rate** regardless of CPU frequency changes and sleep states, and on such CPUs it is synchronised across cores. A non-invariant one counts cycles: with turbo or power saving, its rate changes, so the same tick count means different durations; in deep sleep it may stop; and readings on two cores may disagree, so a goroutine moved between threads mid-measurement could see negative time. The rate is found at startup by comparing with QPC: read QPC, TSC, QPC (keeping the tightest bracket of 16 tries), wait 100 ms, repeat, and divide. The error is about one QPC tick (100 ns) plus the bracket at each end over 100 ms: a few ppm. Measured −0.02 to 3.6 ppm over 3 s against a 50 ppm bound, so about 0.05 ns at worst on a 1 µs command.

4. Under group commit the engine executes commands up to one fsync window before they're durable. If the engine counted them and a scrape ran before a power failure, monitoring would record commands the recovered exchange doesn't have, and that no client was ever told about. Counting on release, after the durability gate, means a metric never shows anything a crash could undo: the same rule as for acks and market data (decision 012).

5. Because the server stopped reading, the client's further orders sit **unread** in the server's socket. Closing a socket with unread input sends a **RST** (RFC 1122 §4.2.2.13), and on receiving a RST the client's stack **discards data it has received but not yet read**, including the acks the server just sent. Instead, the server half-closes (`CloseWrite`, a FIN), reads and discards the client's input until it closes or 1 s passes, then closes with nothing unread: a normal close, and the client reads every answer and then EOF.

6. The snapshot is only an optimisation; durability comes from the WAL. Doing the final fsync first means a crash at any moment during the snapshot write leaves a complete, durable WAL (and the previous snapshot), so recovery replays and loses nothing; the snapshot file itself only counts once it's complete and listed in the manifest. It's skipped after a durability failure because the engine may then hold commands whose WAL records never reached the disk. A snapshot would persist them, resurrecting commands no client was told about and that the log can't account for.

</details>
