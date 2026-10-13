// Command loadgen is the benchmark's load generator (PRD 7.3, decision
// 021): a separate process that drives a running engine and writes what it
// measured as JSON. benchrun starts it, pinned to its own cores; it can
// also be run by hand against an engine started with -instrument BENCH:1:1.
//
//	loadgen -addr 127.0.0.1:9000 -http 127.0.0.1:9100 -engine-pid 1234 \
//	    -mix resting -rate 0 -window 64 -conns 4 -warmup 30s -duration 60s -out run.json
//
// -rate 0 is closed loop (throughput: -window commands in flight per
// connection); -rate N is open loop (latency: N commands per second in
// total, latency measured from each command's scheduled send time).
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/tanxena/matchd/bench/internal/load"
	"github.com/tanxena/matchd/bench/internal/result"
)

func main() {
	var cfg load.Config
	flag.StringVar(&cfg.Addr, "addr", "127.0.0.1:9000", "engine TCP address")
	flag.StringVar(&cfg.HTTPAddr, "http", "127.0.0.1:9100", "engine HTTP address (/metrics, /debug/latency, /instruments)")
	flag.IntVar(&cfg.EnginePID, "engine-pid", 0, "engine process id, for its CPU time (0 = don't measure)")
	flag.StringVar(&cfg.Mix, "mix", "resting", "order mix: "+load.MixNames())
	flag.IntVar(&cfg.Conns, "conns", 4, "connections (clients 1..N)")
	flag.IntVar(&cfg.Rate, "rate", 0, "open loop: commands per second over all connections (0 = closed loop)")
	flag.IntVar(&cfg.Window, "window", 64, "closed loop: commands in flight per connection")
	flag.IntVar(&cfg.Depth, "depth", 10_000, "resting orders the book is held near")
	flag.DurationVar(&cfg.Warmup, "warmup", 30*time.Second, "warmup, discarded")
	flag.DurationVar(&cfg.Duration, "duration", 60*time.Second, "measured window")
	flag.Uint64Var(&cfg.Seed, "seed", 1, "generator seed")
	out := flag.String("out", "", "write the result JSON here (default stdout)")
	flag.Parse()

	res, err := load.Run(cfg)
	if res != nil {
		if werr := write(*out, res); werr != nil {
			fmt.Fprintln(os.Stderr, "loadgen:", werr)
			os.Exit(1)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func write(path string, res *result.Load) error {
	if path == "" {
		return result.Encode(os.Stdout, res)
	}
	return result.Write(path, res)
}
