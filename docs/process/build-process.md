# Build process

How this repository is built and checked. This covers the conventions themselves; the targets are listed in the README's Development section.

## The doc-link check is enforced at docs commits, not every code commit

`internal/doccheck` checks that every link from a doc to a line of code (link text `engine.go:112`, target `engine.go#L112`) lands on a declaration of the symbol its text names. It runs in `make test` and `make ci`, and **must pass at every docs commit and at the head of the branch.**

It is **not** expected to pass at every code commit. Each milestone is committed as code first, then its docs. Line-anchored links go stale as soon as a code commit moves the lines they point at, and stay stale until the milestone's docs commit updates them. From milestone 6 on, most code commits fail this check, and the next docs commit makes it pass again.

So **bisecting on `internal/doccheck` is not meaningful.** A failure there between two docs commits only says code moved before its docs were updated. To bisect a real regression, run the package that shows it, for example `go test ./internal/engine/`, not `./...`.

Rewriting history so that every commit passes was considered and declined (2026-10-13): the commits are kept as they were made.
