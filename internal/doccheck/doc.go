// Package doccheck has no code; its test keeps the guide and
// decision records honest: a stale guide document counted as an unfinished
// milestone (docs/process/build-process.md). Code links like [book.go:284](../../internal/book/book.go#L284)
// silently rot as files change; the test fails the build when one does.
package doccheck
