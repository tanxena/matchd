# Build process

How this repository was built and checked. The product requirements are in [`PRD.md`](../../PRD.md). This file describes the process around them: the working rules the builder followed, the documentation each milestone produced, the questions settled along the way, and the review that followed. The make targets are listed in the README's Development section.

The code was written with Claude Code, an AI coding agent, working from `PRD.md` with the author. A separate Claude Code session reviewed the finished code. The instructions given to those sessions are kept here as written: [`REVIEWER.md`](REVIEWER.md) for the review, and [`PRESENTATION_PASS.md`](PRESENTATION_PASS.md) for the final documentation pass before publishing.

## Working rules

The project was built as a pair-programming exercise, one roadmap milestone at a time (PRD section 9):

- **Small increments.** Each milestone was finished and explained to the author before the next began. Nothing was built ahead of its milestone.
- **Decision records.** Every non-trivial design decision (book data structure, WAL format, sequencing, locking, protocol framing) was written up in [`docs/decisions/`](../decisions/): the options considered, the choice, why, and what it costs.
- **Standard library first.** Any dependency needed its own decision record. There are none: the module has no requirements beyond Go itself ([decision 004](../decisions/004-toolchain.md)). There is no gRPC or protobuf; the wire protocol is the project's own.
- **No unexplained concurrency.** Every goroutine, mutex, channel and atomic carries a comment saying what it protects or coordinates.
- **Tests per package.** Anything touching shared state runs under `go test -race`. Anything touching disk has a crash or recovery test. The matching core is tested differentially against the reference book (PRD 5.3).
- **Planted bugs.** Each milestone deliberately broke its own code to show that its tests fail when they should; the planted bugs are listed in the decision records.
- **Local git.** Commits were made after each milestone with conventional messages, and nothing was pushed until the review was finished.
- **Benchmarks.** The harness under `bench/` was walked through with the author before any run. Every README number comes from a committed file in `bench/results/`, under the protocol in PRD section 7.
- **Review questions.** At the end of each milestone, the builder gave the author three to five questions about the code it had just written.

## The guide

Each milestone also produced a document in [`docs/guide/`](../guide/README.md). These cover the concepts the milestone depends on, from first principles (price-time priority, what `fsync` guarantees, TCP framing, market-data sequencing), with links to primary sources. Each document has the same sections, in this order:

1. **Concepts**: the ideas the milestone depends on.
2. **Code walkthrough**: file by file and function by function, with real paths and names.
3. **Design decisions**: a summary, linking to `docs/decisions/`.
4. **What breaks if**: three to five concrete failure scenarios and exactly what goes wrong.
5. **Go idioms**: one paragraph each, the first time an idiom is used.
6. **Review questions**: with answers in collapsed `<details>` blocks.

The guide also has a [glossary](../guide/00-glossary.md) and a [reading order](../guide/README.md), including how an order flows from socket to fill to market-data event. A milestone whose guide document was stale counted as unfinished. The doc-link check below enforces part of that.

## Questions settled during the build

The PRD listed five open questions, each to be decided at the milestone that needed it. Each was decided with the author and recorded:

| question | answer | record |
|---|---|---|
| Price-level storage: sorted slice, skip list, or red-black tree? | A sorted slice of levels, best price at the end, so that the hot end is cheap to reach and change | [001](../decisions/001-price-level-storage.md) |
| Order lookup for cancel: a map to a linked-list node, or an indexed slab? | An indexed slab with `uint32` prev/next links and a free list, found through the book's own open-addressing hash table | [002](../decisions/002-order-storage-and-cancel.md) |
| Group-commit window default, and how acks are released | 1 ms, or sooner once 4,096 commands are waiting. Answers are released in seq order once their command is fsynced | [012](../decisions/012-durability-and-fsync-policies.md) |
| Matching-latency histogram: HDR-style with fixed buckets, or sampled? | Fixed buckets for `/metrics`, every command timed. A separate log-linear histogram (3.1% precision) was added for the benchmark | [017](../decisions/017-metrics-and-clock.md), [021](../decisions/021-benchmark-harness.md) |
| Market-data ring size, and whether it's per instrument | Per instrument, the last 65,536 events | [016](../decisions/016-market-data.md) |

## Review

When milestone 9 was complete and every test passed, the commit was tagged `v0.1-unreviewed`. A separate reviewer session, following [`REVIEWER.md`](REVIEWER.md), then audited the code against the PRD's acceptance criteria and wrote numbered findings (`F-001` to `F-020`) to [`docs/reviews/full-review.md`](../reviews/full-review.md). The author could strike findings out; none were struck.

The builder responded under each finding in the same file: "Accepted, fixed in `<sha>`", or a rejection with a technical reason citing code, a test or the PRD ("works as intended" was not accepted as a reason). Fixes went in severity order (Critical, then Should-fix, then Nit), one commit per finding or tight group, as `fix(review): F-00N <title>`. A fix that changed behaviour described in a guide document or decision record updated that document in the same commit. Where a finding showed a documented claim was wrong, the claim was corrected even if the code stood. Each round ended with a build, `go vet`, `go test -race` and lint, reported to the author.

The reviewer then verified each response in one follow-up round, re-running the relevant tests rather than taking the builder's word. The author ruled on what remained open, the rulings were applied the same way, and the result was tagged `v0.1-reviewed`.

Interview-preparation notes about the code are private (`docs/interviews/`, gitignored). When one shows a gap in a guide document, only that section is expanded, with at least one new concrete example or "what breaks if" scenario and two new review questions, committed as `docs(guide): expand <file> on <topic>`.

## The doc-link check is enforced at docs commits, not every code commit

`internal/doccheck` checks that every link from a doc to a line of code (link text `engine.go:112`, target `engine.go#L112`) lands on a declaration of the symbol its text names: the leading name in the backtick spans just before the link (`Order.Validate`, `rest(o, qty)`), with its receiver when one is given. Until 2026-10-14 any backtick identifier earlier on the doc's line counted, which let three wrong links through. It runs in `make test` and `make ci`, and **must pass at every docs commit and at the head of the branch.**

It is **not** expected to pass at every code commit. Each milestone is committed as code first, then its docs. Line-anchored links go stale as soon as a code commit moves the lines they point at, and stay stale until the milestone's docs commit updates them. From milestone 6 on, most code commits fail this check, and the next docs commit makes it pass again.

So **bisecting on `internal/doccheck` is not meaningful.** A failure there between two docs commits only says code moved before its docs were updated. To bisect a real regression, run the package that shows it, for example `go test ./internal/engine/`, not `./...`.

Rewriting history so that every commit passes was considered and declined (2026-10-13): the commits are kept as they were made.
