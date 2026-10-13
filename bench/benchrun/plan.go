package main

import (
	"fmt"
	"time"
)

// spec is one run of the plan.
type spec struct {
	Table  string // fsync, mix, snapshot, balanced, clock, mix-throughput
	Config string // the row it belongs to
	Round  int

	Clock bool // a clockprobe run; everything below is for load runs

	Fsync     string // engine -fsync
	Snapshots bool   // engine -snapshot-interval on (else 0)
	Mix       string
	Rate      int // 0: closed loop
}

// timing is a suite's durations.
type timing struct {
	Warmup, Duration, Cooldown time.Duration
	SnapshotInterval           time.Duration // for snapshots-on runs
	ClockReads                 int
}

// suite is a named plan.
type suite struct {
	Name        string
	Rounds      int
	Timing      timing
	Personality string // required power plan personality ("" = any, not checked)
	// Build lists one round's runs, in order.
	Build func(round int, rate int) []spec
}

// The fsync table (PRD 7.6): saturation throughput and end-to-end latency
// per fsync policy, resting mix, closed loop. Core latency doesn't depend
// on the fsync policy (PRD 7.1), so it isn't reported per policy.
func fsyncRuns(round int) []spec {
	var out []spec
	for _, f := range []string{"always", "group:1ms", "never"} {
		out = append(out, spec{Table: "fsync", Config: f, Round: round, Fsync: f, Mix: "resting"})
	}
	return out
}

// The order-mix table: core and end-to-end latency per mix, at a fixed
// rate (open loop) below saturation, group commit.
func mixRuns(round, rate int) []spec {
	var out []spec
	for _, m := range []string{"resting", "crossing", "cancel-heavy"} {
		out = append(out, spec{Table: "mix", Config: m, Round: round, Fsync: "group:1ms", Mix: m, Rate: rate})
	}
	return out
}

// The snapshot row (PRD 5.4): the same configuration with snapshots every
// 10 s and with none, alternating.
func snapshotRuns(round, rate int) []spec {
	return []spec{
		{Table: "snapshot", Config: "snapshots-on", Round: round, Fsync: "group:1ms", Mix: "resting", Rate: rate, Snapshots: true},
		{Table: "snapshot", Config: "snapshots-off", Round: round, Fsync: "group:1ms", Mix: "resting", Rate: rate},
	}
}

func clockSpec(round int) spec {
	return spec{Table: "clock", Config: "clock", Round: round, Clock: true}
}

// balancedSpec: the resting row of the mix table, under the Balanced plan.
func balancedSpec(round, rate int) spec {
	return spec{Table: "balanced", Config: "resting", Round: round, Fsync: "group:1ms", Mix: "resting", Rate: rate}
}

var (
	benchTiming = timing{Warmup: 30 * time.Second, Duration: 60 * time.Second, Cooldown: 15 * time.Second,
		SnapshotInterval: 10 * time.Second, ClockReads: 20_000_000}
	smokeTiming = timing{Warmup: 2 * time.Second, Duration: 5 * time.Second, Cooldown: time.Second,
		SnapshotInterval: 2 * time.Second, ClockReads: 2_000_000}
)

var suites = map[string]suite{
	// full: everything under the High performance plan. Each round runs
	// every configuration once, so each table's cycle (always, group,
	// never, always, ...; resting, crossing, cancel-heavy, ...; on, off,
	// on, ...) is interleaved with the others too: slow drift in the
	// machine's state lands on every configuration alike.
	"full": {Name: "full", Rounds: 5, Timing: benchTiming, Personality: "high performance",
		Build: func(round, rate int) []spec {
			out := []spec{clockSpec(round)}
			out = append(out, fsyncRuns(round)...)
			out = append(out, mixRuns(round, rate)...)
			return append(out, snapshotRuns(round, rate)...)
		}},
	// balanced: the resting row and the clock row again, under Balanced,
	// to show what a laptop's default plan does to the same numbers.
	"balanced": {Name: "balanced", Rounds: 5, Timing: benchTiming, Personality: "balanced",
		Build: func(round, rate int) []spec { return []spec{clockSpec(round), balancedSpec(round, rate)} }},
	// mix-throughput (optional, not in the README plan): saturation
	// throughput per mix.
	"mix-throughput": {Name: "mix-throughput", Rounds: 5, Timing: benchTiming, Personality: "high performance",
		Build: func(round, _ int) []spec {
			var out []spec
			for _, m := range []string{"resting", "crossing", "cancel-heavy"} {
				out = append(out, spec{Table: "mix-throughput", Config: m, Round: round, Fsync: "group:1ms", Mix: m})
			}
			return out
		}},
	// smoke: every configuration of full and balanced once, briefly, under
	// whatever plan is active: does the harness work, end to end?
	"smoke": {Name: "smoke", Rounds: 1, Timing: smokeTiming,
		Build: func(round, rate int) []spec {
			out := []spec{clockSpec(round)}
			out = append(out, fsyncRuns(round)...)
			out = append(out, mixRuns(round, rate)...)
			out = append(out, snapshotRuns(round, rate)...)
			return append(out, balancedSpec(round, rate))
		}},
}

// plan expands a suite into its runs, in order.
func (s suite) plan(rate int) []spec {
	var out []spec
	for r := 1; r <= s.Rounds; r++ {
		out = append(out, s.Build(r, rate)...)
	}
	return out
}

// runID names a run's result file: order in the suite, table, config, round.
func runID(order int, sp spec) string {
	return fmt.Sprintf("%02d-%s-%s-r%d", order, sp.Table, sanitize(sp.Config), sp.Round)
}

func sanitize(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c == ':' || c == '/' || c == '\\' || c == ' ' {
			b[i] = '_'
		}
	}
	return string(b)
}

// estimate is how long a plan takes: each load run is warmup + duration
// + startup and shutdown (about 5 s) + cooldown; a clock run a few seconds.
func estimate(plan []spec, t timing) time.Duration {
	var d time.Duration
	for _, sp := range plan {
		if sp.Clock {
			d += 3*time.Second + time.Duration(t.ClockReads/10_000_000)*time.Second
			continue
		}
		d += t.Warmup + t.Duration + 5*time.Second + t.Cooldown
	}
	return d
}
