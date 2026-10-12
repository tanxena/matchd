# 010 — Windows-specific gotchas

- **Status:** living record. Started in milestone 3; extended in milestones 4 (fsync) and 5 (rename, process kill); milestone 7 (Linux pass) adds to it.
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

## 7. fsync is FlushFileBuffers, and it is slow on this laptop (milestone 4)

Go's `os.File.Sync` calls `FlushFileBuffers` on Windows ([docs](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers)) and `fsync` on Linux. Measured with `BenchmarkAppendSync` (one 36-byte record written and synced per operation, 3,000 operations × 5 runs): median **1.51 ms** (1.37–1.62 ms), against **3.7 µs** (3.4–4.8 µs) to write the same record without syncing.

That ratio of about 400 is why `fsync=always` batches the commands already waiting ([decision 012](012-durability-and-fsync-policies.md)). **To check in the Linux pass:** fsync cost on Linux depends heavily on the file system and disk, so the numbers are not comparable across machines; the benchmark records the hardware.

## 8. Directories can't be fsynced (milestone 4)

On Linux a new file's directory entry is only durable after fsyncing the directory. Windows has no equivalent reachable from Go: `os.File.Sync` on a directory handle fails, because `FlushFileBuffers` needs a handle opened for writing. `internal/wal/syncdir_windows.go` is therefore a documented no-op, relying on NTFS's metadata journal. **This guarantee is weaker than Linux's and can't be tested locally** ([decision 011](011-wal-format.md)).

## 9. A hard kill from a script (milestone 4)

Manual crash checks in this milestone stopped `engine.exe` with `taskkill //F` (Git Bash needs the doubled slash, or it rewrites `/F` as a path). That is a `TerminateProcess` kill: no deferred code runs, and nothing is flushed. Restart then replayed the WAL correctly. The milestone 5 harness will do this programmatically (`os.Process.Kill`, which is `TerminateProcess` on Windows and `SIGKILL` on Linux).

## 10. `os.Rename` is not atomic on Windows (milestone 5)

Snapshots and the manifest are written "atomically": write a temp file, fsync it, rename it over the target, fsync the directory (`fsutil.WriteFileAtomic`). On Linux, `rename` over an existing file is atomic ([POSIX rename](https://pubs.opengroup.org/onlinepubs/9799919799/functions/rename.html)): any observer sees the old file or the new one. Go's own documentation for [`os.Rename`](https://pkg.go.dev/os#Rename) says: *"Even within the same directory, on non-Unix platforms Rename is not an atomic operation."* Go uses [`MoveFileEx`](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-movefileexw) with `MOVEFILE_REPLACE_EXISTING` there.

What that means for us:
- **Snapshot files** are renamed to new names that never existed (`<seq>.snap`), so nothing is replaced. The worst case is that the new file doesn't appear, and the previous snapshot is still listed.
- **The manifest is replaced** on every snapshot. A crash in the middle of that rename could, on Windows, leave `manifest.json` missing.

**How we found out:** reading the `os.Rename` documentation while writing decision 013. No test could have shown it, because a crash *inside* a rename can't be arranged.

**Fix:** if `manifest.json` is missing, `snapshot.ReadManifest` falls back to `manifest.json.tmp` (complete and fsynced before the rename), and then to scanning the snapshot files, which are only renamed into place after being fsynced. `TestRecoveryWithoutManifest` covers both fallbacks.

The fallback itself **introduced a bug**: the writer also used `ReadManifest`, so on the first snapshot it "found" the file it had just renamed into place, and listed it twice. Every later manifest write then failed its order check. The writer now reads only what it published (`readPublished`), never the scan. The new test caught this because it asserts that the writer had no failures.

## 11. Killing the engine from the crash harness (milestone 5)

`exec.Cmd.Process.Kill()` is `TerminateProcess` on Windows and SIGKILL on Linux, so one call works on both. Two details:
- The built binary needs an `.exe` suffix on Windows. `TestMain` adds it when `os.PathSeparator == '\\'`.
- The engine is started with `-listen 127.0.0.1:0`, and the harness reads the chosen port from the `gateway listening addr=...` log line, so concurrent test runs never fight over a port.

After a `TerminateProcess` kill, Windows closes the dead process's file handles. Restarting on the same directory immediately was never blocked by a lingering handle in 150 kill-and-restart runs. That is consistent with handles being released at process exit, though not a guarantee about antivirus or indexer software holding files open.

## Pending (later milestones)

- **Linux pass (milestone 7):** run the crash test and the fsync benchmark on Linux, and record the differences.
