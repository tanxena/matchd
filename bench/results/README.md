# Benchmark results

One JSON file per run, written by `bench/benchrun`. The format is `bench/internal/result`, schema 1.

- `full/`: `make bench-full`, the High performance plan suite. 45 runs: 5 rounds of the clock probe, the fsync table, the mix table and the snapshot pair.
- `balanced/`: `make bench-balanced`, the same resting row and clock probe under the Balanced plan. 10 runs.

Files are named `<order>-<table>-<config>-r<round>.json`. `order` is the run's position in its suite, so the interleaving can be read back from the names. The README's benchmark section is generated from these files by `make bench-report`, and they are never edited by hand.
