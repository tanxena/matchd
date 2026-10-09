# 010 — Windows-specific gotchas

- **Status:** living record. Started in milestone 3; milestones 4 (fsync), 5 (atomic rename, process kill) and 7 (Linux pass) add to it.
- **Why:** PRD 6.1: developed on Windows 11, must also build and pass on Linux; "record Windows-specific gotchas in a decision record".

Each entry gives what happens, how we found out, and what the code does about it.

## 1. The race detector needs a C compiler (milestone 1)

`go test -race` needs cgo, and on Windows cgo needs a C compiler. With none installed, it fails with `cgo: C compiler "gcc" not found`.

**Fix:** MSYS2 UCRT64 gcc, then `CGO_ENABLED=1 go test -race ./...`. Details in [decision 004](004-toolchain.md).

## 2. Line endings (milestone 1)

Git's `core.autocrlf=true` checked files out with CRLF, and `gofmt -l` then flagged them.

**Fix:** `.gitattributes` with `* text=auto eol=lf` ([decision 004](004-toolchain.md)).

## 3. Connection resets use Winsock error codes (milestone 3)

**What happens:** when the peer closes a socket that still has unread data, or a process exits mid-connection, a read on the other end fails with:
- Linux: `ECONNRESET`;
- Windows: Winsock's `WSAECONNRESET` (10054, "An existing connection was forcibly closed by the remote host") or `WSAECONNABORTED` (10053, "An established connection was aborted by the software in your host machine").

On Windows, Go's `syscall.ECONNRESET` is **not** equal to these, so a portable `errors.Is(err, syscall.ECONNRESET)` never matches.

**How we found out:** running the real `engine` binary with `engctl connect`, an abrupt client exit was logged as a generic `read error: ... wsarecv: An existing connection was forcibly closed by the remote host`.

**Fix:** `internal/gateway/reset_windows.go` checks for `syscall.Errno(10054)` and `(10053)`; `reset_other.go` (build tag `!windows`) checks `ECONNRESET` and `ECONNABORTED`. `readErrorReason` reports both as `client reset the connection`.

## 4. "Aborted" also appears on the reading side (milestone 3)

**What happens:** in the slow-consumer test, the server closes a client that has stopped reading. When the test then drains that socket, the error is sometimes a reset and sometimes `WSAECONNABORTED`.

**How we found out:** the end-to-end test failed 2 times in 30 runs. Its error check matched "reset" and "forcibly closed" but not "aborted".

**Fix:** the test no longer matches error strings at all. It asserts the one thing it cares about, that the read did **not** time out (a timeout would mean the server never closed the socket), and accepts EOF, reset or aborted as "closed". It then passed 60 runs out of 60. **Lesson:** don't match OS error text in tests; assert the property you need.

## 5. Stopping a server process from a script (milestone 3)

There is no portable way to send Ctrl-C (SIGINT) to another console process from Git Bash on Windows. `kill -INT` doesn't reach a native Windows process the way it does on Linux.

The manual runs in this milestone stopped the engine with `taskkill //F //IM engine.exe`, which is a hard kill with no graceful shutdown. `cmd/engine` does handle `os.Interrupt` (Ctrl-C in its own console) and `SIGTERM`.

The milestone 5 crash harness needs a hard kill anyway (`TerminateProcess` on Windows, `SIGKILL` on Linux). Graceful shutdown (milestone 7) will be tested in-process through `server.Close` rather than by signalling a child process.

## 6. Loopback socket buffers (milestone 3)

The slow-consumer test shrinks the server's send buffer (`SO_SNDBUF`, via `gateway.Config.SocketWriteBuffer`) and the client's receive buffer to 4 KB, so a non-reading client fills its session queue quickly. On Windows loopback, the test disconnected the slow client within about 20 ms. **To check in the Linux pass:** Linux treats `SO_SNDBUF` as a hint and doubles the value, so the flood may need more orders before the queue fills. The test loops until disconnection, with a 20 s deadline, rather than assuming a count.

## Pending (later milestones)

- **fsync** is `FlushFileBuffers` on Windows (milestone 4). Measure its cost against Linux `fdatasync`.
- **Atomic replace by rename:** `os.Rename` over an existing file on Windows (milestone 5).
- **Process kill** in the crash harness (milestone 5).
