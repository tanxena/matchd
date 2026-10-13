package main

import (
	"strings"
	"testing"
	"time"
)

// PRD 7.5: each table's configurations cycle in order, interleaved: the
// fsync table runs always, group, never, always, ...; the mix table
// resting, crossing, cancel-heavy, ...; the snapshot row on, off, on, ...
// Every configuration runs at least 5 times.
func TestFullPlanInterleaves(t *testing.T) {
	plan := suites["full"].plan(20000)
	byTable := map[string][]string{}
	count := map[string]int{}
	for _, sp := range plan {
		byTable[sp.Table] = append(byTable[sp.Table], sp.Config)
		count[sp.Table+"/"+sp.Config]++
	}
	cycle := func(table string, configs ...string) {
		t.Helper()
		got := byTable[table]
		if len(got) != 5*len(configs) {
			t.Fatalf("%s: %d runs, want %d", table, len(got), 5*len(configs))
		}
		for i, c := range got {
			if want := configs[i%len(configs)]; c != want {
				t.Fatalf("%s run %d is %s, want %s (order %v)", table, i+1, c, want, got)
			}
		}
	}
	cycle("fsync", "always", "group:1ms", "never")
	cycle("mix", "resting", "crossing", "cancel-heavy")
	cycle("snapshot", "snapshots-on", "snapshots-off")
	cycle("clock", "clock")
	for k, n := range count {
		if n < 5 {
			t.Errorf("%s runs %d times", k, n)
		}
	}
	// Rounds: all of round r before any of round r+1, so tables interleave
	// with each other too.
	for i := 1; i < len(plan); i++ {
		if plan[i].Round < plan[i-1].Round {
			t.Fatalf("run %d is round %d after round %d", i+1, plan[i].Round, plan[i-1].Round)
		}
	}
	// What each table holds fixed.
	for _, sp := range plan {
		switch sp.Table {
		case "fsync":
			if sp.Rate != 0 || sp.Mix != "resting" || sp.Snapshots {
				t.Errorf("fsync run %+v: want closed loop, resting, no snapshots", sp)
			}
		case "mix":
			if sp.Rate != 20000 || sp.Fsync != "group:1ms" || sp.Snapshots {
				t.Errorf("mix run %+v", sp)
			}
		case "snapshot":
			if sp.Rate != 20000 || sp.Mix != "resting" || sp.Snapshots != (sp.Config == "snapshots-on") {
				t.Errorf("snapshot run %+v", sp)
			}
		}
	}
}

func TestSmokeCoversEveryConfiguration(t *testing.T) {
	want := map[string]bool{}
	for _, name := range []string{"full", "balanced"} {
		for _, sp := range suites[name].plan(1) {
			want[sp.Table+"/"+sp.Config] = true
		}
	}
	smoke := suites["smoke"].plan(1)
	got := map[string]bool{}
	for _, sp := range smoke {
		got[sp.Table+"/"+sp.Config] = true
	}
	for k := range want {
		if !got[k] {
			t.Errorf("smoke doesn't run %s", k)
		}
	}
	if d := estimate(smoke, suites["smoke"].Timing); d > 5*time.Minute {
		t.Errorf("smoke takes about %v; the point is under 5 minutes", d)
	}
	if d := estimate(suites["full"].plan(1), benchTiming) + estimate(suites["balanced"].plan(1), benchTiming); d > 95*time.Minute {
		t.Errorf("full + balanced take about %v", d)
	}
}

func TestRunIDAndStartLine(t *testing.T) {
	if id := runID(7, spec{Table: "fsync", Config: "group:1ms", Round: 3}); id != "07-fsync-group_1ms-r3" {
		t.Errorf("runID = %q", id)
	}
	got := startLine(0x0F, []string{"GOMAXPROCS=4"}, []string{`bin\engine.exe`, "-data", `C:\a dir\x`})
	want := `set "GOMAXPROCS=4" && start "engine" /affinity 0F bin\engine.exe -data "C:\a dir\x"`
	if got != want {
		t.Errorf("startLine:\n %s\nwant\n %s", got, want)
	}
	if !strings.Contains(startLine(0xF0, nil, []string{"loadgen"}), "/affinity F0 ") {
		t.Error("mask not two hex digits")
	}
}
