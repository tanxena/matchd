// Package crashtest holds the crash harness (PRD 5.4, acceptance 2). It has
// no production code: everything is in crash_test.go.
//
//	go test ./internal/crashtest            5 kill-and-recover runs
//	go test ./internal/crashtest -crash     50 runs (acceptance 2)
//	go test ./internal/crashtest -crashruns 200
//
// What it does NOT prove: killing the process leaves the operating system's
// page cache intact, so unsynced writes still reach the disk. The test
// validates recovery logic (replay, determinism, torn tails, sequence
// continuity), not power-loss durability; it would pass with fsync=never,
// and it runs some rounds with never to show exactly that. Fsync is
// validated separately (internal/server/durability_test.go and the
// benchmark). Decision 015.
package crashtest
