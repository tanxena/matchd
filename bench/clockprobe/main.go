// Command clockprobe measures the cost and resolution of internal/clock,
// the clock the engine times itself with: PRD 7.1's clock-overhead row
// (decision 021). benchrun runs it pinned to the engine's cores, under
// each power plan.
//
//	clockprobe -reads 20000000 -out clock.json
//
// Two passes over the same clock:
//
//  1. cost: a tight loop of back-to-back reads, timed as a whole; the mean
//     per read is what each clock read adds to a timed span.
//  2. steps: pairs of back-to-back reads, the difference of each pair
//     recorded: what timing zero work measures. Its minimum non-zero value
//     is the resolution the clock shows in practice.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/bench/internal/sysinfo"
	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/metrics"
)

func main() {
	reads := flag.Int("reads", 20_000_000, "clock reads per pass")
	out := flag.String("out", "", "write the result JSON here (default stdout)")
	flag.Parse()

	res := probe(*reads)
	var err error
	if *out == "" {
		err = result.Encode(os.Stdout, res)
	} else {
		err = result.Write(*out, res)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "clockprobe:", err)
		os.Exit(1)
	}
}

func probe(n int) *result.Clock {
	info := clock.Init()
	res := &result.Clock{Source: info.Source, TSCHz: info.TSCHz, Reads: n, InitCostNS: int64(info.ReadCost)}
	// Other processes' CPU while the probe runs: the same disturbance
	// check as a load run's (review F-014). Unavailable off Windows.
	before, snapErr := sysinfo.ReadSnapshot()
	began := time.Now()
	defer func() {
		after, err := sysinfo.ReadSnapshot()
		if snapErr != nil || err != nil {
			return
		}
		window := float64(time.Since(began))
		for i, p := range sysinfo.Others(before, after, os.Getpid()) {
			use := float64(p.CPU) / window
			res.Other += use
			if i < 5 {
				res.TopOther = append(res.TopOther, result.ProcUse{PID: p.PID, Name: p.Name, CPUs: use})
			}
		}
	}()

	// Pass 1: cost. Timed with time.Now over the whole loop (hundreds of
	// milliseconds), so its coarse Windows tick is under 0.5% of it.
	var sink int64
	start := time.Now()
	for range n {
		sink += clock.Now()
	}
	res.CostNS = float64(time.Since(start).Nanoseconds()) / float64(n)

	// Pass 2: steps between back-to-back reads.
	h := metrics.NewLogHistogram()
	res.Resolution = -1
	for range n {
		a := clock.Now()
		b := clock.Now()
		d := b - a
		h.Observe(d)
		switch {
		case d == 0:
			res.Zero++
		case res.Resolution < 0 || d < res.Resolution:
			res.Resolution = d
		}
	}
	res.StepsHist = h.Snapshot()
	res.Steps = result.Summarise(res.StepsHist)
	if sink == 42 { // keep the first loop from being optimised away
		fmt.Fprint(os.Stderr, "")
	}
	return res
}
