# PRESENTATION_PASS.md — Final pass before publishing (matchd)

The review loop is finished and `v0.1-reviewed` is tagged. This repository is about to be pushed to GitHub (private first, so CI can run; public after the author's interview pass) and listed on a resume. Your job is to make it read as a well-run engineering project to a stranger who opens it cold: a recruiter for thirty seconds, then an engineer for ten minutes.

## Principles

- **Do not hide that AI tools were used.** This project was built with Claude Code from a written spec, with a separate review session that audited the finished code. That is a legitimate process and it stays visible. Do not rewrite git history, do not delete decision records, review logs, or open observations in `docs/bugs/`.
- **Do remove the scaffolding that only made sense mid-build.** Instructions addressed to an agent, notes addressed to the author as a learner, interview coaching, and private study material are not repository content.
- **Every claim in the repo must be true of the code as it exists now.** No stale numbers, no "arrives in milestone N", no features described that were cut, no benchmark statement the committed results don't support.
- **Do not change any Go source or test behaviour in this pass.** Docs, README, file moves, `.gitignore`, Makefile, and CI workflow only. If you find a code problem, list it in your final report; do not fix it.
- **No history rewriting.** No rebase, no amend of anything older than the commit you are currently making, no force of any kind.

## Tasks, in order

### 1. Separate the product spec from the process instructions

- Create `docs/process/`.
- Move `REVIEWER.md` (currently untracked at the root) into `docs/process/` and track it. Move this file there too.
- Split `PRD.md`: sections 0, 0.1, and 11 (rules addressed to the builder, the learning-doc requirement, the review procedure) move to `docs/process/build-process.md`, merged with whatever that file already contains. Rewrite them in the third person as a description of how the project was built ("Each milestone was followed by..."), not as instructions ("You must..."). What remains in `PRD.md` is a plain product requirements document: summary, goals, non-goals, architecture, functional and non-functional requirements, benchmark protocol, acceptance criteria, roadmap. Section 10 ("Open questions to raise with me") moves to build-process.md too, with each question's answer pointing at its decision record.
- Update every link that pointed at the moved content. The doc-link check must pass at the end of this task.

### 2. Reframe the learning docs as project documentation

- Rename `docs/learn/` to `docs/guide/`. Update every reference in the repo (`grep -rn "docs/learn"`), including Go comments and `docs/process/build-process.md`.
- In every file under `docs/guide/`, remove phrasing that names the author as a learner or addresses the author directly: "for someone new to Go", "so I can test myself", "the user", "you should be able to answer", "I" referring to the author, and similar. Rewrite as neutral documentation. Keep all technical content, examples, "what breaks if" sections, Go-idiom notes, and measured numbers (the Go-map rehash table, the clock-cost table, the Defender measurements); they are the value.
- Rename "Self-check questions" sections to "Review questions" and keep them, with answers still in `<details>` blocks.
- `docs/guide/README.md` keeps the reading order and the "how an order flows from socket to fill to market-data event" section.
- Any file under `docs/guide/` or elsewhere that is interview coaching or first-person study notes ("what to say when asked", narrative prep) is moved to `docs/interviews/` (gitignored) and its links removed. List each one in the report.

### 3. Private material

- Ensure `docs/interviews/` and `TODO.md` are in `.gitignore`. If any file under `docs/interviews/` was ever committed, tell me the commit and stop; do not rewrite history yourself.
- Check for scratch notes, local configs, or build outputs (`bin/`, `*.exe`, `bench/results/*/data`) that should be ignored and are not. Propose; do not delete without confirmation.

### 4. Review file consistency

- `docs/reviews/full-review.md` is a record and its original text stays. But its **closing status** section must be current: acceptance 6 still says "Balanced row is still n=3", which is no longer true. Append a dated note under the closing status (do not edit the reviewer's lines) stating the current status of every acceptance criterion at `v0.1-reviewed`, with acceptance 7 marked "pending CI".

### 5. README

Rewrite `README.md` to this structure, keeping any existing content that fits:

1. One-paragraph description: what it is, who it's for.
2. Features, as a short list, each one true of the current code.
3. Quick start: build, run the engine, submit an order and see the fill with `engctl`, subscribe to market data. Copy-pasteable, verified by actually running it from a fresh clone.
4. Architecture: the diagram from the PRD, the single-threaded-engine rationale in three sentences, and a link to `docs/guide/README.md` for the order-flow walkthrough.
5. Durability and recovery: what the WAL guarantees under each fsync policy, what the crash test proves and does not prove (recovery, not power-loss), the snapshot mechanism.
6. Benchmarks: the generated tables, with hardware, Go version, commit, power plan, pinning, Defender exclusion, and the exact command per row. The first line of this section states which snapshot setting the headline tables use and why. Every number must come from a committed file in `bench/results/`. The clock-overhead row and the 3.1% histogram rounding note stay.
7. Known limitations and open observations: no self-trade prevention, no replication, day orders as GTC, IOC/FOK limit-only, the snapshot-then-fsync stall (`docs/bugs/002`), the unreproduced race observation (`docs/bugs/001`), Linux untested locally.
8. Development process: three to five sentences. Built with Claude Code from `PRD.md`; each milestone produced decision records and a guide doc; a separate review session audited the completed code against the acceptance criteria and its findings and resolutions are in `docs/reviews/`; a GitHub Actions workflow runs the race suite and short crash test on ubuntu-latest (state plainly whether it has run yet). Link to `docs/process/build-process.md`.
9. Layout: brief directory listing.
10. License.

No marketing language. No emoji. No comparisons to other engines. The headline is this engine's measured numbers under the stated protocol.

### 6. Claim audit

Go through `README.md`, `PRD.md`, `docs/guide/`, `docs/decisions/`, `docs/bugs/`, `bench/README.md` and `bench/CHECKLIST.md` and check every factual claim against the code and the committed results:

- Every feature listed exists and has a test.
- Every number matches a committed results file or a test output quoted in a decision record.
- Every "not yet implemented" or "arrives in milestone N" is removed (if implemented) or moved to Known limitations (if not).
- Every decision record describes the code as it is now; where a later fix changed the decision (the slow-consumer rule, the fill-log scope, the wire values for IOC/FOK, the clock), add a dated "Superseded in part by" note pointing to the fixing commit.
- Go comments: grep for "milestone", "TODO", "the user", and stale function names; fix the comments only.

Fix wording where it's wrong. Where the code is wrong, list it in the report.

### 7. Repo hygiene

- Root should contain only: `README.md`, `PRD.md`, `LICENSE`, `Makefile`, `go.mod`, `go.sum`, `.gitignore`, `.gitattributes`, `.golangci.yml` if present, `.github/`, and the directories `cmd/`, `internal/`, `bench/`, `docs/`, `scripts/` if present. Anything else, propose a move or deletion.
- `make build`, `make test`, `make lint`, `make ci` all work from a clean checkout. Verify by cloning into a temp directory and running them there.
- Add a `LICENSE` (MIT) if there isn't one.
- The CI workflow in `.github/workflows/` runs on push and pull request to main: vet, lint, `go test -race ./...`, the short crash test. Confirm it does not try to run the benchmark suites.

### 8. Report and commit

Write a short report to me in chat, not in the repo:

- Files moved, renamed, deleted, newly ignored.
- Claims corrected, with before/after.
- Code problems found and not fixed.
- Anything you were unsure about and left alone.

Then commit in logical groups (`docs: split process from PRD`, `docs: reframe guide`, `docs: README rewrite`, `docs: claim audit`, `chore: repo hygiene`). Do not tag and do not push. Stop and wait.
