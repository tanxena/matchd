# bench: the benchmark harness

PRD section 7's protocol, as programs. Why each piece is the way it is: [decision 021](../docs/decisions/021-benchmark-harness.md). How each works, line by line: [the guide](../docs/guide/09-benchmarks.md) §2. How to run it: [CHECKLIST.md](CHECKLIST.md).

| directory | what |
|---|---|
| `loadgen/` | the load generator: a separate process that drives the engine over TCP (closed loop for throughput, open loop at a fixed rate for latency) and writes what it measured as JSON |
| `clockprobe/` | the clock-overhead row: cost and resolution of `internal/clock`, the engine's hot-path clock |
| `benchrun/` | the interleaving script: runs a suite's configurations round by round, a fresh engine each time, engine and load generator pinned to separate cores, one result file per run, every run (load and clock probe) checked for disturbance |
| `report/` | generates the README's benchmark tables from the result files: median (min–max) across runs, disturbed runs never counted (`-smoke`, a harness check that can't write the README, is the exception) |
| `internal/load/` | the load generator's logic: order mixes, the depth control, connections, the measurement window |
| `internal/result/` | the result-file format, and `Assess`: when a run doesn't count |
| `internal/sysinfo/` | Windows: CPU times (per process, every process, interrupts), affinity, CTRL_BREAK, the power plan, the CPU topology |
| `results/` | the committed result files the README is generated from |

```powershell
make bench-smoke                 # every configuration once, briefly: does it work? (about 1.5–2 min)
make bench-full                  # High performance plan, 45 runs (about 75 min)
make bench-balanced              # Balanced plan, 10 runs (about 10 min)
make bench-report                # regenerate the README section
bin\benchrun -suite full -dry-run   # print the plan and every command, run nothing
```
