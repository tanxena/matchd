// Package doccheck has no code; its test keeps the learning docs and
// decision records honest: a stale learning doc counted as an unfinished
// milestone (docs/process/build-process.md). Code links like [book.go:284](../../internal/book/book.go#L284)
// silently rot as files change; the test fails the build when one does.
package doccheck
