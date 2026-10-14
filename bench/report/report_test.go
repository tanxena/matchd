package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/bench/internal/sysinfo"
)

func TestStats(t *testing.T) {
	for _, tc := range []struct {
		in          []float64
		med, lo, hi float64
	}{
		{[]float64{5, 1, 3, 2, 4}, 3, 1, 5}, // odd: the middle value, whatever the order
		{[]float64{4, 1, 3, 2}, 2.5, 1, 4},  // even: the mean of the middle two
		{[]float64{7}, 7, 7, 7},
	} {
		s := stats(tc.in)
		if s.med != tc.med || s.lo != tc.lo || s.hi != tc.hi {
			t.Errorf("stats(%v) = %+v, want med %v lo %v hi %v", tc.in, s, tc.med, tc.lo, tc.hi)
		}
	}
	if got := (stat{med: 910, lo: 880, hi: 1500}).us(); got != "0.910 (0.880–1.50)" {
		t.Errorf("us() = %q", got)
	}
	if got := (stat{med: 52000, lo: 51000, hi: 53500}).big(); got != "52,000 (51,000–53,500)" {
		t.Errorf("big() = %q", got)
	}
	if got := (stat{med: 3, lo: 3, hi: 3}).num("%.1f"); got != "3.0" {
		t.Errorf("num() with no spread = %q", got)
	}
}

// fixture builds n clean runs of a mix-table row with the given core p50s.
func fixture(suite, table, config string, p50s ...int64) []*result.Run {
	var out []*result.Run
	for i, p := range p50s {
		out = append(out, &result.Run{
			Schema: result.Schema, ID: config + string(rune('a'+i)), Suite: suite, Table: table, Config: config, Round: i + 1, Order: 10*i + len(config),
			Machine:     sysinfo.Machine{CPU: "Test CPU", Logical: 8, Cores: []sysinfo.Core{{Logical: []int{0, 1}}, {Logical: []int{2, 3}}, {Logical: []int{4, 5}}, {Logical: []int{6, 7}}}},
			PowerBefore: sysinfo.Power{AC: true, SchemeName: "High performance", Personality: "high performance", Overlay: "none"},
			PowerAfter:  sysinfo.Power{AC: true, SchemeName: "High performance", Personality: "high performance", Overlay: "none"},
			Pinning:     result.Pinning{EngineMask: 0x0F, EngineReadBack: 0x0F, LoadgenMask: 0xF0, LoadgenReadBack: 0xF0},
			Commands:    result.Commands{Benchrun: []string{"benchrun", "-suite", suite}, EngineStart: "engine-line", LoadgenStart: "loadgen-line"},
			GitCommit:   strings.Repeat("a", 40),
			Load: &result.Load{Mode: "open", Mix: config, RatePerSec: 20000, Depth: 10000, Conns: 4, RequestedNS: 60e9, MeasuredNS: 60e9,
				RequestedWarmupNS: 30e9, Core: result.Latency{Count: 1_200_000, P50NS: p, P99NS: 4 * p, P999NS: 20 * p, MaxNS: 1000 * p, MeanNS: float64(2 * p)},
				E2E: result.Latency{Count: 1_200_000, P50NS: 3_000_000}, SendLag: &result.Latency{P99NS: 20_000},
				Engine: result.EngineCounters{ClockSource: "tsc", ClockReadCostNS: 15}},
		})
	}
	return out
}

// A disturbed run is never in the numbers: the report refuses, or (with
// -allow-disturbed) leaves it out and lists it.
func TestDisturbedRunsAreExcluded(t *testing.T) {
	runs := fixture("full", "mix", "resting", 900, 910, 920, 930, 940)
	runs[2].Disturbed = []string{"other processes used 1.50 CPUs"}
	runs[2].Load.Core.P50NS = 99_000 // would move the median if it counted

	if err := check(groups(runs, false), 5, nil, false); err == nil || !strings.Contains(err.Error(), "4 counted runs, need 5") {
		t.Fatalf("check passed or said the wrong thing: %v", err)
	}
	if err := check(groups(runs, false), 4, nil, false); err == nil || !strings.Contains(err.Error(), "disturbed") {
		t.Fatalf("check let a disturbed run through: %v", err)
	}
	if err := check(groups(runs, false), 4, nil, true); err != nil {
		t.Fatal(err)
	}
	// A per-suite minimum applies to that suite only.
	if err := check(groups(runs, false), 5, map[string]int{"full": 4}, true); err != nil {
		t.Fatalf("per-suite minimum 4 not applied: %v", err)
	}
	if err := check(groups(runs, false), 4, map[string]int{"other": 3, "full": 5}, true); err == nil {
		t.Fatal("per-suite minimum 5 not applied")
	}
	// A suite/table minimum applies to that table only, ahead of its suite's.
	if err := check(groups(runs, false), 5, map[string]int{"full/mix": 4}, true); err != nil {
		t.Fatalf("per-table minimum 4 not applied: %v", err)
	}
	if err := check(groups(runs, false), 5, map[string]int{"full/clock": 4}, true); err == nil {
		t.Fatal("another table's minimum applied to this one")
	}
	if err := check(groups(runs, false), 4, map[string]int{"full": 4, "full/mix": 5}, true); err == nil {
		t.Fatal("the suite's minimum overrode the table's")
	}
	out := render(runs, false)
	// Median of 900, 910, 930, 940 (the disturbed 920 left out) = 920 ns.
	if !strings.Contains(out, "| resting¹ | 1,200,000 | 0.920 (0.900–0.940) |") {
		t.Errorf("mix row wrong:\n%s", out)
	}
	if !strings.Contains(out, "**n=4** of 5†") || !strings.Contains(out, "other processes used 1.50 CPUs") {
		t.Errorf("exclusion not shown:\n%s", out)
	}
	if strings.Contains(out, "99.0") {
		t.Error("the disturbed run's number leaked into the table")
	}
	// Failed runs never count, even for a smoke report.
	runs[2].Disturbed, runs[2].Failed = nil, "loadgen: connection reset"
	if g := groups(runs, true)[[3]string{"full", "mix", "resting"}]; len(g.Runs) != 4 {
		t.Errorf("smoke grouping counted a failed run: %d runs", len(g.Runs))
	}
}

func TestRenderStatesHardwareAndFootnote(t *testing.T) {
	runs := append(fixture("full", "mix", "resting", 900, 910, 920, 930, 940), fixture("full", "mix", "crossing", 1000, 1010, 1020, 1030, 1040)...)
	// A suite starts with a clock run, which pins only itself and has no
	// load: the pinning and protocol must still come from a load run.
	clock := *runs[0]
	clock.ID, clock.Table, clock.Config, clock.Order, clock.Load = "clock", "clock", "clock", 0, nil
	clock.Pinning = result.Pinning{LoadgenMask: 0x0F, LoadgenReadBack: 0x0F}
	clock.Clock = &result.Clock{Source: "tsc", CostNS: 15.5, Reads: 1000, Resolution: 12}
	runs = append(runs, &clock)
	out := render(runs, false, "Defender exclusion on the bench directory")
	for _, want := range []string{
		"Test CPU, 4 cores / 8 threads",
		"High performance plan on AC",
		"engine pinned to CPUs 0–3 and load generator to 4–7 (no shared physical core",
		"Defender exclusion on the bench directory (stated by the operator; the harness can't check it)",
		"**median (min–max) across runs**, never the best run",
		"source `tsc`; one read costs 15.5 ns, measured by `clockprobe`",
		"one histogram update",
		"| crossing¹ |",
		"engine-line", "loadgen-line",
		"Commit `aaaaaaaaaaaa`",
		"30s warmup discarded, 1m0s measured",
		"| full (high performance) | tsc | 15.5 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Smoke test") {
		t.Error("a real report is labelled a smoke test")
	}
	if !strings.Contains(render(runs, true), "Smoke test of the harness, not benchmark results") {
		t.Error("smoke report not labelled")
	}
}

func TestSplice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	before := "# x\n\nintro\n\n" + beginMarker + "\nold tables\n" + endMarker + "\n\n## after\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := splice(path, "NEW"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	want := "# x\n\nintro\n\n" + beginMarker + "\n\nNEW\n" + endMarker + "\n\n## after\n"
	if string(got) != want {
		t.Fatalf("splice:\n%q\nwant\n%q", got, want)
	}
	if err := os.WriteFile(path, []byte("no markers"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := splice(path, "NEW"); err == nil {
		t.Fatal("spliced into a file without markers")
	}
}

// Review F-011: PRD 7.1 wants every reported latency labelled p50, p99,
// p99.9 and max, with the count, in every table.
func TestEveryLatencyHasItsCountAndQuantiles(t *testing.T) {
	var runs []*result.Run
	runs = append(runs, fixture("full", "mix", "resting", 900, 910, 920, 930, 940)...)
	runs = append(runs, fixture("full", "fsync", "always", 900, 910, 920, 930, 940)...)
	runs = append(runs, fixture("full", "snapshot", "snapshots-on", 900, 910, 920, 930, 940)...)
	runs = append(runs, fixture("balanced", "balanced", "resting", 900, 910, 920, 930, 940)...)
	out := render(runs, false)
	for _, header := range []string{
		"| fsync | commands/s | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max |",
		"| snapshots | core count per run¹ | core p50 | core p99 | core p99.9 | core max | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max |",
		"| plan | core count per run¹ | core p50 | core p99 | core p99.9 | core max | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max |",
	} {
		if !strings.Contains(out, header) {
			t.Errorf("missing table header %q in:\n%s", header, out)
		}
	}
	if !strings.Contains(out, "| `always` | 0 | 1,200,000 |") {
		t.Errorf("fsync row lacks its count:\n%s", out)
	}
}

// Review F-013: per-row commands carry no home-directory paths.
func TestRunPathsAreRelative(t *testing.T) {
	for in, want := range map[string]string{
		`bin\engine.exe -data C:\Users\u\AppData\Local\Temp\matchd-bench\02-x-r1\data -fsync always`: `bin\engine.exe -data matchd-bench\02-x-r1\data -fsync always`,
		`bin/clockprobe -out /tmp/matchd-bench/02-x-r1/clock.json`:                                   `bin/clockprobe -out matchd-bench/02-x-r1/clock.json`,
		`bin\loadgen.exe -engine 127.0.0.1:19000 -id 02-x-r1`:                                        `bin\loadgen.exe -engine 127.0.0.1:19000 -id 02-x-r1`,
	} {
		if got := relativeRunPaths(in, "02-x-r1"); got != want {
			t.Errorf("relativeRunPaths(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBenchrunLineIsRelative(t *testing.T) {
	for in, want := range map[string]string{
		`D:\x\MATCHD\bin\benchrun.exe`: `bin\benchrun.exe -suite full`,
		`/home/u/matchd/bin/benchrun`:  `bin/benchrun -suite full`,
	} {
		if got := benchrunLine([]string{in, "-suite", "full"}); got != want {
			t.Errorf("benchrunLine(%q) = %q, want %q", in, got, want)
		}
	}
}
