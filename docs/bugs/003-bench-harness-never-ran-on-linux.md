# 003: bench harness never ran on Linux until CI

- **Status:** closed. The Windows-only harness tests now skip on other platforms, with a reason; the harness is not ported.
- **Seen:** in the first CI run on `ubuntu-latest`, [run 38553623640](https://github.com/tanxena/matchd/actions/runs/38553623640) (2026-10-14, commit `b2bad8a`), the first time any test ran on Linux.
- **Doesn't affect:** the engine. Every engine package (`cmd/...`, `internal/...`) passed the race suite in that run, `internal/crashtest`'s 5 kill-and-recover runs included. gofmt, vet and lint passed too. The workflow's separate crash-test step (`-v`, with the signal-shutdown test) was skipped because the race suite failed before it.

## What failed

The benchmark harness is Windows-only: it pins processes with Windows' affinity API, reads CPU times and power settings through `bench/internal/sysinfo` (whose non-Windows build returns `sysinfo.ErrUnsupported` from every measurement and affinity call), and writes cmd.exe command lines. Until CI, its tests had only ever run on Windows. Three failed on Linux:

| package | test | failure |
|---|---|---|
| `bench/benchrun` | `TestRunIDAndStartLine` | `startLine` builds a cmd.exe `start /affinity` line, and names the program with `filepath.Base`, which on Linux doesn't split `bin\engine.exe` |
| `bench/internal/load` | `TestRunAgainstEngine` | `errors: [sysinfo: not supported on this platform sysinfo: not supported on this platform]`: `Run` reads CPU times through `sysinfo` |
| `bench/report` | `TestBenchrunLineIsRelative` | `benchrunLine("D:\\x\\MATCHD\\bin\\benchrun.exe") = "bin\\D:\\x\\MATCHD\\bin\\benchrun.exe -suite full"`: `filepath.Base` again |

## What changed

- `TestRunAgainstEngine` skips off Windows, and `TestRunIDAndStartLine` skips its start-line half (the run-id half still runs), each with a reason naming the Windows-only harness.
- `bench/report`'s `benchrunLine` now splits the program path on either separator, so the README's command lines come out the same whichever OS runs `report`, and `TestBenchrunLineIsRelative` runs on both.
- `TestStartReportsUnsupportedAffinityElsewhere` (off Windows) checks that `benchrun` refuses to run a process it can't pin, returning `sysinfo.ErrUnsupported`, rather than running it unpinned. `sysinfo` itself was unchanged: its non-Windows stubs already return that error.

The other bench tests (the plan's interleaving, the order mixes, the report's statistics and rendering, the disturbance checks) are portable and still run on both platforms.
