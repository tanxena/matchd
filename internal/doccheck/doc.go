// Package doccheck has no code; its test keeps the learning docs and
// decision records honest. PRD 0.1: "Stale learning docs = failing
// milestone". Code links like [book.go:284](../../internal/book/book.go#L284)
// silently rot as files change; the test fails the build when one does.
package doccheck
