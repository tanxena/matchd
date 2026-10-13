# 017 — Metrics: a hand-written exporter, fixed buckets, and a clock that can time a microsecond

- **Status:** accepted (milestone 7). Chosen with the user:
  - a hand-written exporter, not `client_golang`;
  - fixed bucket boundaries in microseconds, exposed in seconds under Prometheus naming;
  - the TSC-based clock on Windows, with the conditions recorded below. This was the Section 10 question "HDR-style with fixed buckets, or sampled?".
- **Code:** `internal/metrics`, `internal/clock`, `internal/server/metrics.go`, `internal/engine/engine.go` (`Run`, `SetLatency`), `internal/wal/wal.go` (`Sync`), `internal/publisher/publisher.go` (`count`)

## Context

PRD 5.8 asks for `/metrics` in Prometheus text format with:
- commands by type and outcome, and fills;
- a matching-latency histogram ("engine in → events emitted") and a WAL fsync-latency histogram;
- WAL bytes, snapshot age, connections, slow-consumer disconnects and market-data resends.

PRD 7.1 defines matching-core latency exactly: from when the engine goroutine dequeues a command to when it has emitted all resulting events, measured with a monotonic clock. It also requires reporting the clock's own cost and resolution, because "two clock reads per command are not negligible, and Windows timer resolution is coarse at the low end".

## Decision 1: write the exporter by hand

`internal/metrics` is about 350 lines:
- **Counter and Gauge:** one atomic each.
- **Histogram:** fixed bounds in integer nanoseconds, one atomic counter per bucket plus a sum.
- **Registry:** renders the text exposition format: `# HELP`, `# TYPE`, labelled samples, and for each histogram cumulative `_bucket{le=…}`, `_sum` and `_count`.

Every method works on a nil receiver, so a component can hold metrics that were never wired up.

**Rejected: `github.com/prometheus/client_golang`.**
- **Weight.** It's the project's first third-party dependency, and it brings protobuf, `client_model`, `common`, `procfs` and more, for a format that takes a few dozen lines to write.
- **We don't need its registry.** Every metric is created once at startup, in one place (`Server.registerMetrics`). Collectors, dynamic registration, exemplars and the protobuf exposition solve problems this program doesn't have.
- **Control over the hot path.** The engine records on every command. Ours is two atomic adds and a scan of about a dozen bounds: 14 ns, no allocation (`BenchmarkObserve`, `TestRecordingDoesNotAllocate`).

**Cost:**
- we own the format's escaping rules (tested against exact expected output);
- no OpenMetrics, no exemplars;
- a scrape reads the atomics while writers keep going, so a histogram's buckets, sum and count can disagree by the observations made during the scrape. `_count` is computed from the buckets, so it always equals the `+Inf` bucket.

## Decision 2: fixed buckets, defined in microseconds, exposed in seconds

The boundaries are decided once. *(Amended in milestone 9, below: the benchmark doesn't report from these buckets.)*

| histogram | bounds (µs) |
|---|---|
| `matchd_matching_latency_seconds`, `matchd_matching_step_seconds` | 0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 5000 |
| `matchd_wal_fsync_seconds` | 50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 1000000 |

**Units (the user's call):** Prometheus convention is base units, so the names end in `_seconds` and the `le` labels are seconds: 0.25 µs is `le="2.5e-07"`. A standard dashboard expects exactly that. The benchmark tables stay in microseconds, and the README notes the conversion.

**No precision problem:**
- The bounds are stored as integer nanoseconds (`Micros(0.25)` = 250), and observations are integer nanoseconds, so bucket membership is an exact integer comparison.
- Seconds appear only when rendering, as the shortest decimal that parses back to the same `float64`. `TestBoundsRoundTrip` checks every bound renders, parses and converts back to the same nanoseconds.

**Rejected:** HDR-style histograms (log-linear, thousands of buckets, more memory and more to render), and sampling (only some commands timed), which would undercount exactly the tail the histogram exists to show.

**Amendment (milestone 9, chosen with the user; [decision 021](021-benchmark-harness.md)).** For the benchmark these buckets are too coarse: a p50 could only be reported as "between 0.5 and 1 µs", and no maximum at all. So the engine also records each command's matching latency (the same span) into a log-linear histogram (`metrics.LogHistogram`: 32 sub-buckets per power of two, quantiles at most 3.125% high, 12 ns per observation). It is served as JSON at `GET /debug/latency`, **not** in `/metrics`. The rejection above still holds for `/metrics`: the scrape stays small and the dashboard buckets are unchanged. The benchmark's core-latency tables come from `/debug/latency`.

## Decision 3: what is measured where

| metric | recorded by | when |
|---|---|---|
| `matchd_commands_total{type, outcome}`, `matchd_fills_total`, `matchd_trades_total` | publisher | on **release**, so only durable commands are counted, the same rule as everything that leaves the process (decision 012) |
| `matchd_matching_latency_seconds` | engine goroutine | per command, dequeue → last event sent: PRD 7.1's matching-core latency. It includes handing the events to the publisher, so an engine blocked by a downstream would show up here (acceptance 5) |
| `matchd_matching_step_seconds` | engine goroutine | `Step` alone: the matching work. Added beside the PRD's metric because the two differ by the cost of the handoff |
| `matchd_wal_fsync_seconds` | WAL | every fsync, whoever calls it (sequencer, group syncer, startup) |
| `matchd_engine_output_blocked_total` | engine goroutine | *(review F-008)* event sends that found the output channel full: the engine had to wait for the publisher, which is how a stalled consumer would reach matching latency |
| `matchd_wal_fsync_stalls_total`, `matchd_wal_fsync_in_progress_seconds` | WAL | *(milestone 9, [decision 022](022-slow-consumers-and-durability-stalls.md))* fsyncs slower than `-fsync-stall` (25 ms), and the age of the fsync running now, so a stalled disk is visible rather than inferred |
| `matchd_wal_written_bytes_total`, `matchd_durable_seq` | WAL, durable seq | read at scrape time |
| `matchd_snapshot_age_seconds`, `matchd_snapshots_total{result}` | snapshot writer | age at scrape time; **+Inf when no snapshot exists**, so an age alert fires instead of staying silent |
| `matchd_connections`, `matchd_connections_accepted_total`, `matchd_slow_consumer_disconnects_total`, `matchd_events_held`, `matchd_md_*` | gateway, publisher | read at scrape time from the counters they already kept |
| `matchd_clock_info{source}`, `matchd_clock_read_cost_seconds` | clock | which clock timed everything above, and what one read costs |

Commands are counted once each, by the event that decides them: `Ack`, `Reject`, `Cancelled` with reason requested, or `InstrumentAdded`. A cancel and an order can be told apart because events now carry their command's kind (`Event.Cmd`, in what was padding). PRD 5.8's "one debug line per command" is logged at the same point, on release, guarded so that a disabled debug level costs one boolean check.

## Decision 4: a clock that can time the engine (chosen with the user)

**The problem, measured on this machine:** Go's `time.Now` on Windows advances once per system timer tick, about **515 µs** here. Of a million back-to-back pairs of readings, 999,980 were equal, and the rest differed by about 515 µs. A matching step takes about 1 µs, so a histogram fed by `time.Now` would put nearly every command in the lowest bucket and a scattering at half a millisecond. That says nothing about matching. On Linux, `time.Now` reads a nanosecond clock through the vDSO, typically around 20 ns. That's the usual figure, not measured here, since Linux hasn't been run locally.

**Options measured:**

| source | resolution | cost per read (from Go, this machine) |
|---|---|---|
| `time.Now` (Windows) | ~515 µs (timer tick) | ~9 ns |
| QueryPerformanceCounter | 100 ns (10 MHz) | **78–179 ns** depending on the run (178 ns in the first, single-pass measurement; 78–94 ns best-of-three) |
| RDTSC (TSC), calibrated | sub-ns (2.112 GHz) | ~12 ns raw; `clock.Now()` 14–35 ns including the conversion (benchmark 16.8 ns) |

QPC costs that much because every call crosses Go's syscall boundary: switching to the system stack and telling the scheduler the goroutine is in a syscall. Two reads per command would add 160–360 ns to work that takes about 1 µs, more than the matching itself in the cheapest cases. Two TSC reads add about 30 ns.

**The decision:** `internal/clock` is the **only** clock on the hot path. The engine never calls `time.Now`, and neither does the sequencer, which stamps commands with `clock.WallNanos`; otherwise a burst of Windows commands would all share one 515 µs timestamp.
- **Windows amd64 with an invariant TSC** (CPUID leaf 0x80000007, EDX bit 8): read the TSC, convert with a 32.32 fixed-point multiplier calibrated against QPC over 100 ms at startup.
- **Windows without one,** or with an implausible calibration result: QPC.
- **Elsewhere:** Go's monotonic clock.

**Conditions (from the user), and where they're met:**
- **Only clock on the hot path:** the engine's `Run`, the WAL's `Sync` and the sequencer's `WallClock` all read `internal/clock`.
- **Calibration drift test:** `TestCalibrationDriftAgainstQPC` compares TSC-derived elapsed time with QPC over 3 s. **Bound: ±50 ppm.** Measured −0.02, 1.02 and 3.62 ppm in three normal runs, and −9.85 ppm under the race detector with the suite running.
- **Forced fallback test:** `TestForcedQPCFallback` runs the "no invariant TSC" path. It checks the source is QPC, elapsed time is right, and it doesn't allocate (measured 78–94 ns per read).
- **The measured costs recorded:** here; at startup in the log (`msg=clock source=tsc read_cost=…`); in `/metrics` (`matchd_clock_read_cost_seconds`). Milestone 9's benchmark must carry them in its clock-overhead row and in the footnote of every core-latency table, naming the source and its cost.

**Is per-command timing too expensive (PRD 7.1's switch to batch timing)?** No. Three reads (start, after `Step`, after the sends) cost about 45 ns against a sustained-load mean of 821 ns for `Step` and 1.5 µs for the whole command: about 3–5%. Batch timing isn't needed.

**Costs and risks:**
- About 30 lines of amd64 assembly (`RDTSC` with `LFENCE`, `CPUID`).
- An invariant TSC is synchronised across cores on modern CPUs. This relies on that, because the engine goroutine can move between threads mid-measurement. Windows itself builds QPC on the same counter when it's reliable. *(Review F-020: if a core's counter nonetheless reads slightly below the calibration base, the conversion used to wrap, giving a time about 2^64 ticks in the future, which a command timestamp would then carry into the WAL. It is now signed: such a reading converts to a time just before the base. `TestConversionBelowTheBase` covers it.)*
- The calibration error (a few ppm) is irrelevant for durations of microseconds.
- `WallNanos` doesn't follow later steps of the system clock. Timestamps only need to be ordered and precise, and replay uses logged values.

## What was learned by measuring (milestone 7)

How long a command takes depends on how busy the engine is:
- **One command at a time** (clients wait for each answer): the engine idles between commands, its caches go cold, and the CPU clocks down. `Step` lands in the 2–10 µs buckets, and the whole command one bucket higher.
- **Sustained load** (a pipelined burst of 50,000 orders): `Step` has a median under 1 µs and a mean of 821 ns; the whole command averages 1.5 µs.

Milestone 2's ~0.3 µs was a hot loop of `Step` with nothing else in the system. The benchmark (milestone 9) must say which regime each figure comes from.

## Tests

`internal/metrics`:
- exact text output, including escaping and inclusive `le`;
- bound round trips;
- quantile buckets;
- no allocation;
- concurrent recording while scraping.

`internal/clock`:
- monotonic, with sub-microsecond steps;
- elapsed time agrees with Go's clock;
- no allocation;
- drift and the forced fallback (above).

`TestRunRecordsLatencyWithoutAllocating`: 2 heap allocations over 20,000 commands through `Run` with both histograms on. Those are the runtime's own background allocations; a planted per-command allocation shows 40,006.

`TestMetricsEndpoint`: every series, a well-formed format, and values after a known sequence of commands.

`TestMetricsCountOnlyDurableCommands`: with fsync held, the engine has executed the order but the counter is still 0 and no debug line was logged.

`TestSlowConsumerDoesNotMoveEngineLatency` (**acceptance 5**): the same load with and without a stalled subscriber. The subscriber is disconnected, and the p50 and p99 buckets stay within one of the baseline. A planted bug where a slow subscriber blocks the publisher fails this test. *(Review F-008: one bucket of slack on buckets 2–2.5× wide let up to about a 4× shift pass. The test now asserts exactly that the engine never waited on a full output channel, counted by the new `matchd_engine_output_blocked_total`. It compares p50, p99 and p99.9 from the 3.1% log-linear histogram within 1.4×, 1.8× and 2.5×.)*
