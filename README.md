# matchd

A single-threaded order matching engine in Go:
- price-time priority limit and market orders;
- a write-ahead log with deterministic replay;
- copy-on-write snapshots;
- a binary TCP protocol;
- market data with gap recovery;
- Prometheus metrics.

Order types: limit (GTC, Day, IOC, FOK) and market. Known limitations so far: no self-trade prevention, Day orders behave as GTC, and no market FOK (IOC and FOK are limit-only).

The full write-up (architecture, benchmark tables, known limitations) arrives with milestone 9. Until then, the [learning docs](docs/learn/README.md) and [decision records](docs/decisions/) are the documentation.

## Quick start

```sh
go build -o bin/ ./cmd/...
bin/engine -data ./data -instrument XYZ:1:1          # TCP 127.0.0.1:9000, HTTP 127.0.0.1:9100

bin/engctl order --client 7 --instrument XYZ --side sell --px 101 --qty 5
bin/engctl book XYZ                                   # top levels from a fresh snapshot
bin/engctl subscribe XYZ                              # stream deltas and trades
bin/engctl add-instrument --symbol ABC --tick 5 --lot 100
curl 127.0.0.1:9100/metrics
```

Ctrl-C shuts down gracefully: clients receive every answer, a snapshot is written, and the next start replays nothing.

## Observability

`/metrics` serves Prometheus text format: commands by type and outcome, fills, matching latency, WAL fsync latency, WAL bytes, snapshot age, connections, slow-consumer disconnects and market-data resends ([decision 017](docs/decisions/017-metrics-and-clock.md)). There's also `/healthz`.

- **Units:** latency histograms are named `_seconds` and labelled in seconds, as Prometheus expects. Their bucket bounds are defined in microseconds (0.25, 0.5, 1, 2, 5 … µs), and the benchmark tables report microseconds: `le="2.5e-07"` is 0.25 µs.
- **Clock:** latencies are timed with `internal/clock`, not `time.Now`, which on Windows only advances every ~515 µs.
  - On Windows with an invariant TSC it reads the CPU's timestamp counter, calibrated against QueryPerformanceCounter: ~15 ns per read on the development machine.
  - Without one, it uses QPC: 80–180 ns per read from Go.
  - On Linux, Go's monotonic clock.

  The source and its measured cost are logged at startup and exported as `matchd_clock_info` and `matchd_clock_read_cost_seconds`. Core-latency tables (milestone 9) state both in a footnote.

## Development

Go 1.27 ([decision 004](docs/decisions/004-toolchain.md)). The race detector needs cgo, so on Windows it needs gcc on `PATH` (e.g. MSYS2's `C:\msys64\ucrt64\bin`).

| target | does |
|---|---|
| `make build` | `bin/engine`, `bin/engctl` |
| `make test` | the suite with `-race`, including 5 kill-and-recover crash runs |
| `make test-long` | the 1M-command differential test (acceptance 1) and 50 crash runs (acceptance 2) |
| `make test-race-loop N=50` | one package (`PKG`, default `./internal/server/`) under `-race` N times, every run's output kept in `bin/race-loop.log` ([open observation 001](docs/bugs/001-unreproduced-server-race-failure.md)) |
| `make lint` | `golangci-lint` |
| `make run` | an engine on `./data` with one instrument |
| `make bench` | every benchmark, with allocation counts |
| `make ci` | gofmt, vet (Windows and Linux), lint, Linux cross-compile of every binary and test binary, and the race suite |

On Windows, run `make` (MSYS2's `C:\msys64\usr\bin\make.exe`) from PowerShell, cmd or an MSYS2 shell, **not Git Bash**, where it starts with an empty environment ([decision 010](docs/decisions/010-windows-gotchas.md) §13).

**Linux:** development is on Windows. `make ci` cross-compiles everything for Linux, but **the suite has not been run on Linux locally.** [`.github/workflows/ci.yml`](.github/workflows/ci.yml) runs gofmt, vet, lint, the race suite and the short crash test on `ubuntu-latest`. **It will not run until the repository is pushed to GitHub.**
