# 004 — Toolchain, dependencies, and repository conventions

- **Status:** accepted (milestone 1); race-detector item resolved before milestone 2
- **Files:** `go.mod`, `.gitattributes`, `.gitignore`

## Go version

**Go 1.27.0** (windows/amd64) is installed on the development machine. `go.mod` declares `go 1.27.0`, as `go mod init` wrote it.

PRD 6 asks for "Go 1.22+, use what's installed". The code currently needs nothing newer than 1.22:

- `math/rand/v2`;
- `for i := range n` over an integer;
- the `slices` package and the `min`/`max` builtins (both 1.21).

*(Milestone 7: the Linux pass cross-compiles every binary and test binary with this toolchain in `make ci`; running the suite on Linux is left to the CI workflow, decision 010.)* If the Linux pass has to run on an older toolchain, `go.mod` can be lowered to `go 1.22` with no code changes. We keep the installed version until something forces that.

## Dependencies

**None.** Production code and tests use only the standard library:

- `math/rand/v2`'s PCG generator gives seeded, reproducible test streams;
- `testing` and `flag` cover everything the differential harness needs.

Every future dependency needs its own decision record ([build process](../process/build-process.md#working-rules)).

## Lint

golangci-lint 2.13.2 with its default configuration (no config file yet). If we enable more linters, we'll add a `.golangci.yml` and record why.

## Line endings: `.gitattributes`

`* text=auto eol=lf`.

Git on this machine has `core.autocrlf=true`, so `git checkout` was writing CRLF files. gofmt treats CRLF as unformatted (`gofmt -l` flagged a freshly checked-out `book.go`). Forcing LF in the repository and in every working copy keeps Windows and Linux byte-identical, and stops `gofmt -l` from producing false positives.

## Race detector

PRD 6 requires `go test -race ./...` to be clean. On Windows the race detector needs cgo, which needs a C compiler (see the [race detector docs](https://go.dev/doc/articles/race_detector)).

**History.** At milestone 1 there was no C compiler on the machine, and `go test -race` failed with `cgo: C compiler "gcc" not found`. That didn't matter for milestone 1, because the book runs on one goroutine. It became a hard requirement from milestone 2 (engine goroutine) and milestone 3 (connection goroutines).

**Resolution (before milestone 2).** MSYS2's UCRT64 gcc 14.2.0 is installed at `C:\msys64\ucrt64\bin` and on `PATH`. The race detector runs on Windows with `CGO_ENABLED=1`:

```
CGO_ENABLED=1 go test -race ./...
```

The choice was between this and running `-race` only in the Linux pass. We chose to run it locally because race bugs should be caught where the code is written, not one milestone later.

**Costs.**
- `CGO_ENABLED=1` is set per command, not globally with `go env -w`, so other projects on the machine are unaffected. The Makefile (milestone 7) exports it for every target.
- Race-enabled runs are slower: the book package's suite takes about 13–16 s under `-race`, against about 2.5 s without it.
- The race detector needs cgo, but the shipped binaries don't. `go build` with the default `CGO_ENABLED=0` still produces a pure-Go binary.
