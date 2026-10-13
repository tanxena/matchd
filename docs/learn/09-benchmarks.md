# 09 — Benchmarks: measuring an engine without fooling yourself

A benchmark is an experiment, and most benchmark numbers are wrong in one of a few well-known ways: the load generator hides its own delays, the machine was doing something else, the best of five runs is reported as "the" number, the clock costs as much as the thing it times. Milestone 9 builds a harness whose job is to make those mistakes **impossible or visible**:
- a separate, pinned load generator;
- a fresh engine per run;
- interleaved repetitions;
- a check on every run for anything that could have disturbed it;
- README tables generated from the raw files by a program, never typed in.

This doc is also the **walkthrough of the harness**, line by line where it matters (§2), written before any benchmark numbers exist. [`bench/CHECKLIST.md`](../../bench/CHECKLIST.md) is the pre-flight checklist for running it.

---

## 1. Concepts

### 1.1 Latency and throughput are different questions

- **Latency** is how long one thing takes.
- **Throughput** is how many things finish per second.

They're linked by **Little's law**: on average, *(things in flight) = (throughput) × (latency)*. Push a system to its maximum throughput and work queues up, so latency at saturation is mostly *waiting*. With 256 commands in flight and 64,000 answered per second, each spends about 4 ms in the system, however fast the matching is.

So the harness asks the two questions separately (decision 021, Decision 2):
- **Throughput:** keep a fixed number of commands in flight (**closed loop**) and count answers per second.
- **Latency:** send at a fixed rate well below saturation (**open loop**) and time each command.

### 1.2 Coordinated omission

The classic way a load generator lies: it sends a request, waits for the answer, sends the next. If the system stalls for 100 ms, the generator stalls with it. It records *one* slow request, and silently doesn't send the hundreds it should have sent during the stall. Those would all have been slow too. The tail it reports is far too optimistic.

The fix (the [wrk2](https://github.com/giltene/wrk2) and [HdrHistogram](http://hdrhistogram.org/) projects describe it) is to **decide in advance when each request should be sent**, and measure latency from that scheduled time. If the generator falls behind, every late request carries its lateness. `loadgen`'s open loop does exactly this, and also records how late each send was (**send lag**), so you can tell "the engine was slow" from "the generator was slow".

### 1.3 Quantiles, and why histograms have buckets

**p99** is the latency 99% of commands beat; **p99.9**, 999 in 1,000. The tail matters because a trader sending 1,000 orders a second sees the p99.9 every second. Keeping every sample and sorting is exact but costs memory, so latency is usually recorded in a **histogram** of counts per range.

- **Fixed buckets** (like `/metrics`: 0.25, 0.5, 1, 2, 5 µs …) can only say "the p50 is between 0.5 and 1 µs".
- **A log-linear (HDR-style) histogram** splits every power of two into equal sub-buckets. With 32 sub-buckets, values in [1024, 2048) ns fall into buckets 32 ns wide, values in [1 ms, 2 ms) into buckets ~31 µs wide. Every bucket is narrower than 1/32 of its values, so any quantile is known to **within 3.125%**, at any scale, in about 9 KB. Reporting a bucket's *upper* bound means the reported value is never below the truth.

### 1.4 A laptop is a noisy instrument

- **Frequency scaling.** The CPU changes its clock with load, temperature and the **power plan**. The same code can run twice as fast under "High performance" as under "Balanced" with a power-saving mode. That's why the plan is part of the hardware statement, and why one row is repeated under Balanced: to show the effect.
- **Hyperthreads.** A physical core can run two **logical CPUs** (hyperthreads) that share its execution units and L1/L2 caches. Two programs on sibling hyperthreads slow each other down. Windows numbers logical CPUs; which pairs are siblings isn't guaranteed, so the harness asks the OS ([`GetLogicalProcessorInformation`](https://learn.microsoft.com/en-us/windows/win32/api/sysinfoapi/nf-sysinfoapi-getlogicalprocessorinformation)). On this i7-8650U the pairs are {0,1}, {2,3}, {4,5}, {6,7}. The engine on 0–3 and the load generator on 4–7 therefore share **no physical core**, but they do share the **L3 cache** (8 MB, all cores) and memory bandwidth.
- **Affinity.** A process's **affinity mask** says which logical CPUs its threads may run on ([`SetProcessAffinityMask`](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-setprocessaffinitymask); `start /affinity 0F` in cmd.exe sets the same mask at launch). Pinning keeps the engine and load generator from competing for one CPU, and keeps the engine's caches warm.
- **Other programs.** A browser tab, a virus scan or Windows Update can take a CPU for seconds. "Nothing else running" is only a claim unless it's measured.

### 1.5 Repetition, interleaving, and never the best run

One run tells you little: the next can differ by 10% for reasons outside the code. So every configuration runs **5 times**. Machines also **drift**: they warm up, background jobs start. Five runs of A followed by five of B would confuse drift with the difference between A and B, so runs are **interleaved** (A, B, C, A, B, C, …). The README reports the **median** (robust to one odd run) and the **min–max** (the spread), never the best run, which would be cherry-picking.

### 1.6 Interrupts and DPCs

When a network packet or disk completion arrives, the CPU runs an **interrupt** handler and then a **deferred procedure call (DPC)** to finish the work. That time is charged to no process: Task Manager shows it as part of "System interrupts". Loopback TCP generates a lot of it. Machine-wide busy time minus the engine's and load generator's process time therefore isn't "other programs": it includes this. The harness learned that the hard way (§4, scenario 5).

---

## 2. Code walkthrough

### The engine side

- **`SetFineLatency`** ([engine.go:67](../../internal/engine/engine.go#L67)) and **`Run`** ([engine.go:127](../../internal/engine/engine.go#L127)): `Run` now computes `took := clock.Now() - start` once, observes it in the `/metrics` histogram, then in the fine one: `e.fine.Observe(took)`. The fine observation comes **after** the closing clock read, so it isn't part of the span it measures.
- **`LogHistogram`** ([loghist.go:22](../../internal/metrics/loghist.go#L22)) has 1,152 atomic counters and a sum.
  - **`logIndex`** ([loghist.go:41](../../internal/metrics/loghist.go#L41)): values under 64 get their own bucket. Otherwise:

    ```go
    shift := bits.Len64(uint64(v)) - logSubBits - 1 // v has bits.Len64 bits; keep the top 6
    i := shift*logSub + int(uint64(v)>>shift)       // v>>shift is in [32, 64)
    ```

    For v = 1,000: `bits.Len64` is 10, so shift = 4, and 1000>>4 = 62, giving index 4×32 + 62 = 190, a bucket holding 992–1007.
  - **`logBounds`** ([loghist.go:57](../../internal/metrics/loghist.go#L57)) inverts it.
  - **`QuantileNS`** ([loghist.go:148](../../internal/metrics/loghist.go#L148)) walks the counts to rank ⌈q·n⌉ and returns that bucket's upper bound.
  - **`Sub`** ([loghist.go:99](../../internal/metrics/loghist.go#L99)) gives the window: counts at the end minus counts at the start.
- **`handleLatency`** ([http.go:57](../../internal/server/http.go#L57)) serves `{"matching": LogSnapshot}` at `GET /debug/latency`.

### The load generator: `bench/internal/load`

**`mix.go`: what to send.**
- **`Mixes`** ([mix.go:86](../../bench/internal/load/mix.go#L86)): the three mixes as fractions plus quantity ranges, with the volume arithmetic in the comment above them.
- **`Next`** ([mix.go:157](../../bench/internal/load/mix.go#L157)) draws one uniform number `r`:
  - `r < Cancel`: cancel a random own resting order (`TakeRandom` removes it from the live set at once, so it's never cancelled twice). If there's none, send a passive order and say so (`noTarget`).
  - `r < Cancel+Crossing`: an IOC at `Mid±Band`. Its quantity is drawn, then **scaled** by `depth.Scale()`.
  - otherwise passive: 1–10 ticks from mid on its own side, unless this connection already has `liveCap` resting orders, in which case a cap cancel is sent instead.
- **`Depth.Scale`** ([mix.go:125](../../bench/internal/load/mix.go#L125)) is the feedback: resting orders ÷ target, clamped to [¼, 4]. Below the target, crossing orders shrink and the book fills; above, they grow and it drains.
- **`Live`** ([mix.go:189](../../bench/internal/load/mix.go#L189)) is a connection's resting order ids. A slice plus a map from id to index gives O(1) random choice and O(1) removal (swap with the last element, shrink). Each `Add` or `Remove` also moves the shared `Depth` counter.

**`conn.go`: one connection.**
- **`pending`** ([conn.go:15](../../bench/internal/load/conn.go#L15)): a command sent and not answered. It holds its kind, its client order id (orders) or order id (cancels), `intended` (open loop's scheduled time), `sent`, and `measured` (sent inside the window).
- **`conn`** ([conn.go:32](../../bench/internal/load/conn.go#L32)): the generator, the live set and the `pend` FIFO, all under one mutex shared by sender and reader. **Why a FIFO works:** the engine answers one connection's commands in the order they were sent. There is one sequencer, one engine goroutine, and one writer per session, so the first answer to arrive is always for the oldest unanswered command, and the reader just checks the head.
- **`appendBatch`** ([conn.go:85](../../bench/internal/load/conn.go#L85)) generates `n` commands into one buffer (`proto.AppendNewOrder`/`AppendCancel`), queueing a `pending` for each. **`stamp`** ([conn.go:130](../../bench/internal/load/conn.go#L130)) sets their send time just before the write. Both happen under the lock, so the reader can never see an answer for a command it doesn't know about yet.
- **`writeClosed`** ([conn.go:141](../../bench/internal/load/conn.go#L141)), the closed loop: `tokens` is a channel holding one value per free slot (64). The writer blocks for one token (time spent waiting counts as "window full"), grabs any others without blocking, and writes that many commands in **one** `WriteFrames` call.
- **`sendDue`** ([conn.go:177](../../bench/internal/load/conn.go#L177)), the open loop's per-connection send: the commands' scheduled times come in. After the write it records `now - t` for each as **send lag**.
- **`read`** ([conn.go:202](../../bench/internal/load/conn.go#L202)) and **`handle`** ([conn.go:220](../../bench/internal/load/conn.go#L220)): the timestamp is taken right after decoding, then:
  - `Ack`, `Reject` and requested `CancelAck` are **first answers**: `answer` pops the head (an acked passive order joins the live set), and `done` records latency.
  - A `CancelAck` with `IOCUnfilled` is an IOC's remainder, not a first answer, so it's only counted.
  - A maker `Fill` with nothing left removes the order from the live set.
  - Anything unexpected ends the run.
- **`answer`** ([conn.go:282](../../bench/internal/load/conn.go#L282)): the head must be the NewOrder with that client order id, or (client order id 0) the Cancel of that order id; anything else is an error. It doesn't take a closure for the match. The first version did, and `go build -gcflags=-m` showed that closure moving a `pending` to the heap on every message (§5).
- **`done`** ([conn.go:301](../../bench/internal/load/conn.go#L301)):

  ```go
  from := p.sent
  if k.r.cfg.Rate > 0 {
      from = p.intended // open loop: a late send counts against latency
  }
  k.e2e.Observe(now - from)
  ```

  It also returns the token in closed loop.

**`run.go`: one run.**
- **`Run`** ([run.go:117](../../bench/internal/load/run.go#L117)):
  1. Check that instrument 1 has tick 1 and lot 1 (`checkInstrument`).
  2. Dial the connections, and start a reader for each and the senders.
  3. Sleep through the warmup.
  4. **Window start:** read the engine (`/metrics`, `/debug/latency`, `/instruments`), then CPU times, then flip `phase` to measuring and take `start`.
  5. Sleep through the window.
  6. **Window end, in reverse:** flip the phase (stop counting), take `end`, CPU times, read the engine. The engine's window therefore *encloses* the load generator's, by the time an HTTP read takes.
  7. `halt` stops the senders. **Drain:** wait until every command has its answer, so no connection closes with answers still queued at the engine. Then close.
  8. Assemble: sum the connections, merge their histograms (`LogSnapshot.Add`), subtract the engine's before from its after.
- **`pace`** ([run.go:284](../../bench/internal/load/run.go#L284)), the open-loop sender. One goroutine, staggered schedules per connection (`interval × i / n`), and a loop:

  ```go
  now := clock.Now()
  for next[i] <= float64(now) && len(due) < MaxBatch { due = append(due, int64(next[i])); next[i] += interval }
  ```

  Every due command is sent in one write. It **spins** rather than sleeps (a sleep wakes late by the timer's granularity), and calls `runtime.Gosched` every 1,024 spins so the readers aren't starved of a P.
- **`cpuUse`** ([run.go:370](../../bench/internal/load/run.go#L370)) turns two CPU samples into cores used: engine, load generator, machine busy, interrupts + DPCs, and **every other process**, from the OS's process list, with the top five named.

**`scrape.go`.** **`parseMetrics`** ([scrape.go:111](../../bench/internal/load/scrape.go#L111)) reads `series value` lines into a map. **`counters`** ([scrape.go:172](../../bench/internal/load/scrape.go#L172)) subtracts before from after for commands, trades, fsyncs (count and sum, giving the mean), WAL bytes, snapshots and slow consumers, and reads the clock's source and cost.

### The result format and the verdict: `bench/internal/result`

- **`Run`** ([result.go:24](../../bench/internal/result/result.go#L24)) is one file. It holds the exact argv of every process and its `start /affinity` equivalent, masks requested and read back, the machine, power before and after, the git commit, and the measurement (`Load` or `Clock`), including full histograms. Field names carry their units (`_ns`, `_per_sec`).
- **`Assess`** ([assess.go:36](../../bench/internal/result/assess.go#L36)) returns the reasons a run can't be trusted, each a plain sentence naming the measured value and the limit. The limits are constants at the top of the file, each with its reason.

### The OS: `bench/internal/sysinfo`

Thin wrappers over Windows APIs, each loaded with `syscall.NewLazyDLL(...).NewProc(...)`:
- **`SetAffinity`** ([sysinfo_windows.go:93](../../bench/internal/sysinfo/sysinfo_windows.go#L93)) and **`Affinity`** ([sysinfo_windows.go:106](../../bench/internal/sysinfo/sysinfo_windows.go#L106)): open the process with just the access needed, set or read its mask.
- **`Interrupt`** ([sysinfo_windows.go:129](../../bench/internal/sysinfo/sysinfo_windows.go#L129)): CTRL_BREAK to a process group, which Go delivers as `os.Interrupt`, so the engine shuts down as on Ctrl-C (decision 019).
- **`ReadPower`** ([sysinfo_windows.go:217](../../bench/internal/sysinfo/sysinfo_windows.go#L217)): AC or battery, then the active plan's GUID, name and **personality**, read from the plan's `GUID_POWERSCHEME_PERSONALITY` setting. The personality says what kind of plan it is, however it's named. Then the power-mode overlay.
- **`ReadMachine`** ([sysinfo_windows.go:310](../../bench/internal/sysinfo/sysinfo_windows.go#L310)): the CPU name from the registry, the cores, caches and packages from `GetLogicalProcessorInformation` (32-byte records: a mask, a relationship, a union), RAM, and the OS. Windows 11 still says "Windows 10" in `ProductName`, so build ≥ 22000 decides.
- **`ReadSnapshot`** ([sysinfo_windows.go:418](../../bench/internal/sysinfo/sysinfo_windows.go#L418)): `NtQuerySystemInformation` returns every process in one buffer of variable-length records, the call Task Manager makes. The struct offsets (user time at 40, kernel at 48, image name at 56, pid at 80) are partly marked "reserved" in the public header, so `TestSnapshotMatchesProcessTimes` checks them: this process must appear under its own name with the CPU time `GetProcessTimes` gives. The second query sums interrupt and DPC time over all CPUs.

### The clock row: `bench/clockprobe`

**`probe`** ([main.go:47](../../bench/clockprobe/main.go#L47)):
- **Pass 1**, cost: 20 million `clock.Now()` calls timed as a whole with `time.Now`. Hundreds of milliseconds, so `time.Now`'s 0.5 ms tick is under 0.5% of it.
- **Pass 2**, steps: pairs `a := clock.Now(); b := clock.Now()`, with `b - a` recorded in a `LogHistogram`. That is what timing *nothing* measures. Its smallest non-zero value is the resolution the clock shows in practice. For the TSC that's about one read's cost, far above the counter's 0.47 ns tick.

### The interleaving script: `bench/benchrun`

**`plan.go`.**
- **`suites`** ([plan.go:85](../../bench/benchrun/plan.go#L85)) defines `full`, `balanced`, `mix-throughput` and `smoke`, each as a number of rounds plus a function building one round.
- **`plan`** ([plan.go:125](../../bench/benchrun/plan.go#L125)) concatenates the rounds, so for `full` the order is round 1 (clock, always, group, never, resting, crossing, cancel-heavy, snapshots on, off), then round 2, and so on.
- **`estimate`** ([plan.go:150](../../bench/benchrun/plan.go#L150)) is what the ETA and the dry run print.

**`main.go`.**
- **`preflight`** ([main.go:192](../../bench/benchrun/main.go#L192)) checks, before anything runs:
  - the binaries exist;
  - the masks share no physical core (from the topology) and don't overlap;
  - the power source and plan personality fit the suite (else refuse, unless `-force`);
  - ports 19000/19100 are free;
  - at least 5 GB is free for data directories;
  - the results directory is empty.
- **`run`** ([main.go:253](../../bench/benchrun/main.go#L253)), per run:
  1. Build a `result.Run` with the machine, commit and power.
  2. Run it (`clockRun` or `loadRun`) and read power again.
  3. Judge it: failed (an error) or `Assess`.
  4. Write `<order>-<table>-<config>-r<round>.json`, print one line with the ETA, and cool down.

  Ctrl-C closes `interrupted`. The run in progress is abandoned (processes killed, no file), and finished files stay.
- **`loadRun`** ([main.go:389](../../bench/benchrun/main.go#L389)):
  1. A fresh data directory under `%TEMP%\matchd-bench\<id>`.
  2. Start the engine (`engineArgs`: the run's `-fsync`, `-snapshot-interval 0` or `10s`, one instrument `BENCH:1:1`, ports 19000/19100) and pin it.
  3. Wait for `/healthz`.
  4. Start loadgen with `-engine-pid` and pin it.
  5. Wait for loadgen (warmup + window + 2 min at most).
  6. Stop the engine with CTRL_BREAK and wait for a clean exit. A kill would lose nothing (the directory is deleted next), but a clean exit also shows the engine survived the run.
  7. Read loadgen's JSON into the result.
  8. Delete the directory.
- **`start`** ([main.go:458](../../bench/benchrun/main.go#L458)): `exec.Command` with the child in its own process group (`Interruptible`) and stdout and stderr captured. After `Start`, `SetAffinity`, then `Affinity` to read the mask back.
- **`env`** ([main.go:114](../../bench/benchrun/main.go#L114)): `GOMAXPROCS` = the mask's CPU count, because the Go runtime sizes it at startup, before the mask exists.
- **`startLine`** ([main.go:155](../../bench/benchrun/main.go#L155)) renders the cmd.exe equivalent, `set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe …`. That's for the README and for repeating one run by hand. The quotes in `set "X=Y"` matter: without them cmd keeps the space before `&&` as part of the value.
- **`gitCommit`** ([main.go:550](../../bench/benchrun/main.go#L550)): `HEAD`, plus `-dirty` if tracked files outside `bench/results` changed. The results being written don't count.

### The report: `bench/report`

- **`groups`** ([report.go:50](../../bench/report/report.go#L50)) sorts runs into rows by (suite, table, config). A failed or disturbed run goes to `Excluded`, never into the numbers. The exception is `-smoke`, where disturbed runs count and the report says "not benchmark results" at the top.
- **`check`** ([report.go:76](../../bench/report/report.go#L76)) refuses rows with fewer than 5 counted runs, and refuses any exclusion unless `-allow-disturbed`.
- **`stats`** ([report.go:137](../../bench/report/report.go#L137)): the median (the middle value, or the mean of the middle two) and min–max.
- **`hardware`** ([report.go:310](../../bench/report/report.go#L310)) writes one hardware-and-environment line from the files:
  - CPU (cores / threads, L3), RAM, OS and Go;
  - each suite's power plan and source;
  - the pinning claim, which says "no shared physical core" only if the recorded topology says so;
  - any `-asserted` statement, for what the harness can't measure, such as the Defender exclusion, labelled "stated by the operator".

  It then gives "nothing else running" as the measured median and maximum other-process CPU, naming the worst run's biggest process. `-min-runs-suite balanced=3` lets one suite's rows report fewer runs; such a row shows **n=3** of 5†, with the left-out runs listed.
- The table functions each state their conditions in a sentence and give every cell as `median (min–max)`. Every core-latency table carries **`coreFootnote`** ([report.go:459](../../bench/report/report.go#L459)): the clock's source and per-read cost, and the 3.1% bound.
- **`commands`** ([report.go:683](../../bench/report/report.go#L683)) lists each row's exact `start /affinity` lines.
- **`splice`** ([main.go:94](../../bench/report/main.go#L94)) replaces only what lies between the README's `bench:begin` and `bench:end` markers.

### Tests and what the smoke runs showed

- The tests are listed in decision 021, and the 11 planted bugs were all caught.
- `make bench-smoke`, run while building, took **81 s** for every configuration. It also taught four things that are now in the code:
  - "other CPU" needed the process list;
  - a closure was allocating per message;
  - Ctrl-C only ended the current run;
  - the report took its pinning and protocol lines from the suite's first run, which is a clock probe with neither, so they were silently missing.
- **Book depth.** The first mixes kept the book bounded by draining it, to 80–870 orders: too thin to be representative. The depth control now holds 6,000–10,000 (decision 021, Decision 4).
- The smoke runs' **numbers aren't results.** They were short, ran under Balanced with "best power efficiency", and Chrome was playing media. Still, they gave an order of magnitude:
  - 50,000–68,000 commands/s at saturation;
  - core p50 around 1 µs;
  - end-to-end p50 3–4 ms, dominated by a 1.4–2.4 ms fsync on this SSD.

  That set the open-loop rate at 20,000/s.

### What the committed results showed

The README's tables come from `bench/results/`. The snapshot row is the one surprise. Core latency is the same with snapshots on and off: copy-on-write works. But about 0.9% of commands wait over 50 ms end to end with snapshots on, and none do with them off. A reproduction sampling `/metrics` every few milliseconds showed why: a WAL fsync of 30–80 ms a few seconds after a snapshot, during which `matchd_events_held` stays flat and nothing reaches the engine, because the sequencer's next WAL append waits behind the fsync. That's [open observation 002](../bugs/002-snapshot-runs-stall-the-wal.md). The stall metric added in decision 022 is what made it visible rather than inferred.

---

## 3. Design decisions

- [021](../decisions/021-benchmark-harness.md): the harness:
  - closed loop for throughput, open loop for latency;
  - mixes and the depth control;
  - interleaving and a fresh engine per run;
  - pinning and its verification;
  - the disturbance checks;
  - the file format and the generated README;
  - Windows only.
- [017](../decisions/017-metrics-and-clock.md), amended: the benchmark reads core latency from a log-linear histogram at `GET /debug/latency`, not the `/metrics` buckets.
- [018](../decisions/018-http-admin-and-instruments.md): the new endpoint.
- [014](../decisions/014-fill-log-and-replay-check.md): the fill log stays matches-only (settled at this milestone's start), with cancel-on-arrival records left for a v2.

---

## 4. What breaks if

1. **Open-loop latency is measured from the actual send.** A 50 ms stall in the load generator delays the sends due during it, and each is then timed from its late send. The tail looks fine. In fact every one of those commands waited, from the client's point of view. *(Planted and caught: `TestLatencyIsMeasuredFromTheSchedule`.)*

2. **`GOMAXPROCS` isn't set.** Each process starts with GOMAXPROCS 8, sized before benchrun pins it, then runs on 4 CPUs. The engine's goroutines (gateway readers and writers, sequencer, engine, publisher) are scheduled as if there were twice the CPUs: more OS threads competing, more preemption, noisier tails. Nothing fails; the numbers are just worse and more variable, for a reason that has nothing to do with the engine.

3. **The masks split hyperthread siblings.** Suppose the OS numbered siblings {0,4}, {1,5}, … (some machines do). Then masks 0x0F and 0xF0 would put the engine and the load generator on the *same four physical cores*, competing for execution units and L1/L2. Throughput drops and latency rises, and the README's "share no physical core" would be false. `preflight` reads the topology and refuses such masks. `TestSharedCores` covers that numbering.

4. **The report takes the best run, or includes a disturbed one.** Either way the table stops describing the engine. The best run is the luckiest of five, and a disturbed run describes Chrome. Both are planted and caught in `report_test.go`. The rule is mechanical: `report` won't generate while any run is disturbed unless told, and then it shows "4 of 5†" and lists why.

5. **"Other CPU" is computed as machine busy time minus engine and loadgen.** This is what the harness did first. Closed-loop runs reported about 2 CPUs of "other processes" on a quiet machine, and every run would have been rejected. The missing piece was interrupt and DPC time: the loopback network stack carrying the benchmark's own traffic is charged to no process. Counting other processes from the process list, with interrupts kept separate, fixed it, and also named the real culprits when there were some (`chrome.exe 1.63`).

6. **The book is left to find its own size.** Tuned to stay bounded by draining, it held 80–870 orders. Every level is short, cancels find their order in a tiny index, and the numbers flatter the engine compared with a book of thousands. The depth control fixes the size at about 10,000, whatever the mix (planted: disabling it fails `TestMixesHoldTheBookNearTarget`).

7. **The disk stalls for a few seconds under load.** It happened in a smoke run: one fsync in a 5-second window. The durability gate holds every answer, and the load generator keeps sending, so up to 65,536 answers pile up in the publisher. When the disk comes back they're released at once. Under the original slow-consumer rule (a full 4,096-event queue disconnects at once), a client that was reading perfectly well was disconnected, and so would every busy client have been. The fix ([decision 022](../decisions/022-slow-consumers-and-durability-stalls.md)) judges slowness by time: over the soft limit for longer than `WriteTimeout`. `matchd_wal_fsync_stalls_total` now makes the stall itself visible. `TestDurabilityStallBurstDoesNotDisconnect` injects a 50 ms stall under load and requires zero disconnects.

---

## 5. Go idioms introduced

**`os/exec` with `SysProcAttr`.** `cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}` passes a Windows-specific flag to `CreateProcess`, which `exec.Cmd` otherwise hides. Here it puts each child in its own console process group, so benchrun can send CTRL_BREAK to just that child. `syscall.SysProcAttr` has different fields on every OS, which is why `interruptible` lives in `sysinfo_windows.go`.

**Calling Windows APIs without cgo.** `syscall.NewLazyDLL("kernel32.dll").NewProc("SetProcessAffinityMask")` finds the function the first time it's called. `proc.Call(args ...uintptr)` calls it with machine-word arguments and returns `(r1, r2, err)`. For most APIs, `r1 == 0` means failure and `err` is `GetLastError`. Structs passed by pointer (`systemPowerStatus`, `memoryStatusEx`) must match the C layout field for field, including padding (`slpi`'s hidden 4 bytes before the union). `proc.Find()` checks a function exists before calling it (`PowerGetEffectiveOverlayScheme` is Windows 10 1709+).

**Reading a variable-length C buffer.** `NtQuerySystemInformation` returns records chained by `NextEntryOffset`, with pointers into the same buffer. Rather than casting addresses back to Go pointers (which `go vet` flags as possible misuse of `unsafe.Pointer`), `ReadSnapshot` reads fields with `binary.LittleEndian` at offsets, and turns an embedded pointer into an index: `p - uintptr(unsafe.Pointer(&buf[0]))`, checked to lie within the buffer.

**A channel as a semaphore.** `tokens := make(chan struct{}, 64)`, pre-filled with 64 values: the writer receives one per command, and the reader sends one back per answer. The channel's capacity *is* the window, the sender blocks when it's full, and the send never blocks, because there are never more than 64 outstanding. `struct{}` values take no memory.

**Closing a channel to broadcast.** A Ctrl-C notification sent on a channel is received once: one `select` gets it and the others never do. That was the first version's bug, where the run in progress swallowed it and the suite carried on. Closing a channel instead makes every receive on it succeed immediately, forever, so `go func() { <-sig; close(interrupted) }()` turns a one-shot signal into a state every loop can see.

**Escape analysis, `-gcflags=-m`.** `go build -gcflags=-m ./pkg` prints the compiler's decisions: "moved to heap: p", "x escapes to heap". A value whose address is captured by a closure, or that might outlive its function, goes to the heap. In `answer`, `match(&p)` with `match` a closure parameter sent `p` to the heap on every message. Comparing fields inline keeps it on the stack.

**Platform files.** `sysinfo_windows.go` is compiled only on Windows, by its filename alone. `sysinfo_other.go` starts with `//go:build !windows` and provides stubs returning `ErrUnsupported`, so `make ci`'s Linux cross-compile still builds every bench program. The test files follow the same rule: `probe_windows_test.go` uses `mustGUID`, which exists only on Windows.

---

## 6. Self-check questions

1. Why does the fsync table use a closed loop and the mix table an open loop? What would each table show if you swapped them?
2. In an open-loop run at 20,000/s the load generator's thread is descheduled for 10 ms. What happens to the commands due in those 10 ms, and where does the delay show up in the result file?
3. The engine's matching-latency p99.9 is reported as 31.7 µs. What do you know about the true value, and why?
4. Why does `benchrun` set `GOMAXPROCS=4` in the environment, when it pins the process anyway?
5. A run's other-process CPU is 0.6, mostly `chrome.exe`. What happens to that run in the README? What do you do about it?
6. Why are the core-latency tables not given per fsync policy, while throughput is?
7. Why can't the Balanced row be interleaved with the High performance runs, and what does that mean for comparing them?

<details>
<summary>Answers</summary>

1. The fsync table asks "how many commands per second can be made durable?". Only a load that never lets the engine idle (a fixed number in flight, refilled at once) finds the maximum, so closed loop. The mix table asks "how long does a command take at a known load?". That needs a load that doesn't depend on the answer times, so open loop at a fixed rate with latency from the schedule. Swapped: an open-loop fsync table at 20,000/s would show throughput 20,000 for every policy that keeps up, measuring nothing. A closed-loop mix table would show latency ≈ (window ÷ throughput), mostly queueing set by the window you chose (Little's law).

2. When the thread runs again, the pacer finds 200 commands overdue (20 per ms × 10) and sends them, in batches of up to 256, immediately. Each keeps its *scheduled* time. Its end-to-end latency is measured from that time, so all 200 carry up to 10 ms of lateness in `e2e`. Each one's lateness is also recorded in `send_lag`. If send lag p99 exceeds 100 µs, `Assess` marks the run disturbed ("load generator fell behind its schedule"), and the report leaves it out.

3. It's the upper bound of the log-linear bucket holding the p99.9 sample. Buckets are under 1/32 of their values wide, so the true p99.9 is between about 30.7 and 31.7 µs: never above the reported value, at most 3.125% below it. It's a quantile of the engine's own timing: from dequeue to the last event sent, with `internal/clock`, which adds about one clock read inside the span (the footnote gives its cost).

4. The Go runtime decides `GOMAXPROCS` once, at startup, from the CPUs the process may use *then*. `benchrun` can only pin a process after it has started, by which point the runtime has already chosen 8. Pinning then restricts its threads to 4 CPUs, but the scheduler still runs 8 Ps, so twice the OS threads compete for them. Setting the environment variable makes the runtime choose 4 from the start.

5. 0.6 exceeds the 0.35 limit, so `Assess` marks the run disturbed and names `chrome.exe`. `report` then refuses to generate the README. Either re-run the suite with Chrome closed (cleanest; interleaving needs the whole suite), or report with `-allow-disturbed`: the row shows "4 of 5†", the run is listed with its reason, and with only 4 runs the row is below PRD 7.5's minimum unless you accept that with `-min-runs 4`.

6. Matching-core latency is timed from the engine dequeuing a command to it emitting the events, and the WAL write and fsync happen before the engine sees the command (sequencer) and after it (publisher). The fsync policy isn't inside the span, so per-policy core tables would be the same table three times (PRD 7.1 says so). Throughput and end-to-end latency include waiting for the fsync, so they depend on the policy directly.

7. The power plan is a machine-wide setting, and changing it mid-suite would be a disturbance in itself (`Assess` checks the plan is unchanged during every run). So the Balanced runs come afterwards, as their own suite. The comparison is then between two suites run at different times. It's a fair indication of the plan's effect, since both are 5 runs with the same configuration and the same checks, but slow drift between the two sessions isn't controlled for the way it is within a suite. The table says so.

</details>
