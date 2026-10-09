# 004 — Toolchain, dependencies, and repository conventions

- **Status:** accepted (milestone 1); the race-detector item is open
- **Files:** `go.mod`, `.gitattributes`, `.gitignore`

## Go version

**Go 1.27.0** (windows/amd64) is installed on the development machine. `go.mod` declares `go 1.27.0`, as `go mod init` wrote it.

PRD 6 asks for "Go 1.22+, use what's installed". The code currently needs nothing newer than 1.22:

- `math/rand/v2`;
- `for i := range n` over an integer;
- the `slices` package and the `min`/`max` builtins (both 1.21).

If the Linux pass (milestone 7) has to run on an older toolchain, `go.mod` can be lowered to `go 1.22` with no code changes. We keep the installed version until something forces that.

## Dependencies

**None.** Production code and tests use only the standard library:

- `math/rand/v2`'s PCG generator gives seeded, reproducible test streams;
- `testing` and `flag` cover everything the differential harness needs.

Every future dependency needs its own decision record (PRD 0).

## Lint

golangci-lint 2.13.2 with its default configuration (no config file yet). If we enable more linters, we'll add a `.golangci.yml` and record why.

## Line endings: `.gitattributes`

`* text=auto eol=lf`.

Git on this machine has `core.autocrlf=true`, so `git checkout` was writing CRLF files. gofmt treats CRLF as unformatted (`gofmt -l` flagged a freshly checked-out `book.go`). Forcing LF in the repository and in every working copy keeps Windows and Linux byte-identical, and stops `gofmt -l` from producing false positives.

## Race detector: OPEN

PRD 6 requires `go test -race ./...` to be clean. On Windows the race detector needs cgo, which needs a C compiler (see the [race detector docs](https://go.dev/doc/articles/race_detector)). **None is installed**, so `go test -race` fails with `cgo: C compiler "gcc" not found`.

Milestone 1 isn't affected in practice. The book is single-goroutine, and the only concurrency is `t.Parallel` subtests that share nothing except a mutex-guarded coverage total. But milestone 2 brings the engine goroutine and milestone 3 brings connection goroutines, and from then on `-race` is a hard requirement.

**Options:**
- install a MinGW-w64 gcc, for example via winget or the chocolatey package already present on this machine, and set `CGO_ENABLED=1`;
- or run `-race` only in the Linux pass.

The first is preferred, because race bugs should be caught where the code is written. **Decision deferred to the user** before milestone 2.
