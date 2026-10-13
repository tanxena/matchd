# 001: one unreproduced `internal/server` failure under `-race`

- **Status:** open observation. It isn't reproduced, so there's nothing to fix yet.
- **Seen:** once, milestone 8, during a full `go test -race -count=1 ./...` run.
- **Reproduce with:** `make test-race-loop N=50` (or `PKG=./...` to repeat the whole suite as in the original run).

## What was seen

A single `FAIL github.com/tanxena/matchd/internal/server` line in a full race-detector run of the whole suite. **The output wasn't kept**, so it isn't known:
- which test failed;
- whether it was a data race report, a test assertion, or a timeout.

Every other package passed in that run.

## What has been tried

| what | runs | failures |
|---|---|---|
| the full suite, `go test -race ./...`, right afterwards | 2 | 0 |
| the full suite in the background with output captured | 4 | 0 (3 failed only `internal/doccheck`, because the docs hadn't been updated yet) |
| `internal/server` alone under `-race` | 8 | 0 |
| `make ci` (includes the full race suite) | 1 | 0 |
| **`make test-race-loop N=50`** (`internal/server` alone, `-race -count=1 -v`, every run's output kept in `bin/race-loop.log`), 2026-10-13, 42 min | **50** | **0** |

In the 50-run loop each package run took 46–50 s. The slowest tests were all timing-based:

| test | mean | max |
|---|---|---|
| `TestSlowConsumerDoesNotMoveEngineLatency` | 10.5 s | 12.2 s |
| `TestMarketDataConvergence` | 6.7 s | 9.1 s |
| `TestManyConcurrentClients` | 5.2 s | 5.4 s |
| `TestSlowMarketDataSubscriber` | 3.1 s | 3.9 s |

## What the loop doesn't recreate

The original failure happened while **every package's tests ran in parallel**, so the machine was much busier than when one package runs alone. A test that depends on timing (a deadline, a quiet period, a latency comparison) is the likeliest kind to fail only under that load. `make test-race-loop PKG=./...` reproduces those conditions, at several minutes per run.

## If it happens again

The loop's log has the full `-v` output of every run, and a failed run is marked `=== run N FAILED`. Keep the log, and record here the test name, the failure text, and whether it was a `DATA RACE` report.
