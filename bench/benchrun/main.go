// Command benchrun runs a benchmark suite (PRD 7, decision 021): it starts
// a fresh engine for every run, pins it and the load generator to separate
// cores, runs the suite's configurations interleaved round by round, and
// writes one JSON result file per run.
//
//	benchrun -suite full     -results bench/results/full      # High performance plan, about 85 min
//	benchrun -suite balanced -results bench/results/balanced  # Balanced plan, about 10 min
//	benchrun -suite smoke    -results bin/bench-smoke         # every configuration once, about 1.5-2 min
//	benchrun -suite full -dry-run                             # print the plan and the commands
//
// It refuses to start a full or balanced suite on battery power or under
// the wrong power plan (-force overrides, and the result files record it),
// or into a results directory that already holds results.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/bits"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/bench/internal/sysinfo"
)

type options struct {
	suite      suite
	results    string
	bin        string
	dataRoot   string
	engineMask uint64
	loadMask   uint64
	rate       int
	window     int
	conns      int
	depth      int
	port       int
	force      bool
	dryRun     bool
	only       int // run only this many runs (0 = all); for trying the harness
}

func main() {
	var o options
	suiteName := flag.String("suite", "smoke", "suite: smoke, full, balanced, mix-throughput")
	flag.StringVar(&o.results, "results", "", "directory for the result files (default bench/results/SUITE; smoke: bin/bench-smoke)")
	flag.StringVar(&o.bin, "bin", "bin", "directory holding engine, loadgen and clockprobe")
	flag.StringVar(&o.dataRoot, "data-root", filepath.Join(os.TempDir(), "matchd-bench"), "where each run's engine data directory is created (and deleted)")
	engineMask := flag.String("engine-cpus", "0F", "engine affinity mask, hex as for start /affinity (0F = logical CPUs 0-3)")
	loadMask := flag.String("loadgen-cpus", "F0", "load generator affinity mask (F0 = logical CPUs 4-7)")
	flag.IntVar(&o.rate, "rate", 20_000, "open-loop rate for the mix, snapshot and balanced runs, commands/s")
	flag.IntVar(&o.window, "window", 64, "closed loop: commands in flight per connection")
	flag.IntVar(&o.conns, "conns", 4, "load generator connections")
	flag.IntVar(&o.depth, "depth", 10_000, "resting orders the book is held near")
	flag.IntVar(&o.port, "port", 19000, "engine TCP port (HTTP is port+100)")
	flag.BoolVar(&o.force, "force", false, "run even on battery or under the wrong power plan (recorded as disturbed)")
	flag.BoolVar(&o.dryRun, "dry-run", false, "print the plan, the commands and the expected duration; run nothing")
	flag.IntVar(&o.only, "only", 0, "run only the first N runs of the plan")
	flag.Parse()

	s, ok := suites[*suiteName]
	if !ok {
		fatalf("unknown suite %q", *suiteName)
	}
	o.suite = s
	if o.results == "" {
		o.results = filepath.Join("bench", "results", s.Name)
		if s.Name == "smoke" {
			o.results = filepath.Join("bin", "bench-smoke")
		}
	}
	var err error
	if o.engineMask, err = sysinfo.ParseMask(*engineMask); err != nil {
		fatalf("%v", err)
	}
	if o.loadMask, err = sysinfo.ParseMask(*loadMask); err != nil {
		fatalf("%v", err)
	}
	plan := s.plan(o.rate)
	if o.only > 0 && o.only < len(plan) {
		plan = plan[:o.only]
	}
	if o.dryRun {
		printPlan(&o, plan)
		return
	}
	if err := run(&o, plan); err != nil {
		fatalf("%v", err)
	}
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "benchrun: "+f+"\n", a...)
	os.Exit(1)
}

// env is the environment every child gets: GOMAXPROCS set to the number
// of CPUs it is pinned to. The Go runtime sizes GOMAXPROCS at startup,
// before benchrun can pin the process, so without this it would run 8 Ps
// on 4 CPUs.
func env(mask uint64) []string { return []string{"GOMAXPROCS=" + strconv.Itoa(bits.OnesCount64(mask))} }

func (o *options) exe(name string) string {
	p := filepath.Join(o.bin, name)
	if runtime.GOOS == "windows" {
		p += ".exe"
	}
	return p
}

func (o *options) addr() string     { return "127.0.0.1:" + strconv.Itoa(o.port) }
func (o *options) httpAddr() string { return "127.0.0.1:" + strconv.Itoa(o.port+100) }

// engineArgs is the engine command line for a run.
func (o *options) engineArgs(sp spec, dataDir string) []string {
	interval := "0"
	if sp.Snapshots {
		interval = o.suite.Timing.SnapshotInterval.String()
	}
	return []string{o.exe("engine"), "-data", dataDir, "-fsync", sp.Fsync,
		"-listen", o.addr(), "-http", o.httpAddr(), "-instrument", "BENCH:1:1",
		"-snapshot-every", "0", "-snapshot-interval", interval}
}

// loadArgs is the loadgen command line for a run (enginePID 0 in the
// dry-run listing).
func (o *options) loadArgs(sp spec, enginePID int, out string) []string {
	t := o.suite.Timing
	return []string{o.exe("loadgen"), "-addr", o.addr(), "-http", o.httpAddr(), "-engine-pid", strconv.Itoa(enginePID),
		"-mix", sp.Mix, "-rate", strconv.Itoa(sp.Rate), "-window", strconv.Itoa(o.window), "-conns", strconv.Itoa(o.conns),
		"-depth", strconv.Itoa(o.depth), "-warmup", t.Warmup.String(), "-duration", t.Duration.String(), "-seed", "1", "-out", out}
}

func (o *options) clockArgs(out string) []string {
	return []string{o.exe("clockprobe"), "-reads", strconv.Itoa(o.suite.Timing.ClockReads), "-out", out}
}

// startLine is the cmd.exe equivalent of a pinned start, for running one
// by hand: start /affinity takes the same hex mask, and opens a window
// titled after the program. (set "X=Y" with the quotes: without them cmd
// keeps the space before && in the value.)
func startLine(mask uint64, env []string, args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = a
		if strings.ContainsAny(a, " \t") {
			q[i] = `"` + a + `"`
		}
	}
	var sets strings.Builder
	for _, e := range env {
		sets.WriteString(`set "` + e + `" && `)
	}
	title := strings.TrimSuffix(filepath.Base(args[0]), ".exe")
	return fmt.Sprintf(`%sstart "%s" /affinity %02X %s`, sets.String(), title, mask, strings.Join(q, " "))
}

func printPlan(o *options, plan []spec) {
	t := o.suite.Timing
	fmt.Printf("suite %s: %d runs, about %v (warmup %v, measured %v, cooldown %v)\n", o.suite.Name, len(plan),
		estimate(plan, t).Round(time.Minute), t.Warmup, t.Duration, t.Cooldown)
	if o.suite.Personality != "" {
		fmt.Printf("requires: AC power, a %q power plan\n", o.suite.Personality)
	}
	fmt.Printf("results: %s\n\n", o.results)
	for i, sp := range plan {
		id := runID(i+1, sp)
		dir := filepath.Join(o.dataRoot, id)
		if sp.Clock {
			fmt.Printf("%s\n  %s\n", id, startLine(o.engineMask, env(o.engineMask), o.clockArgs(filepath.Join(dir, "clock.json"))))
			continue
		}
		fmt.Printf("%s\n  %s\n  %s\n", id, startLine(o.engineMask, env(o.engineMask), o.engineArgs(sp, filepath.Join(dir, "data"))),
			startLine(o.loadMask, env(o.loadMask), o.loadArgs(sp, 0, filepath.Join(dir, "load.json"))))
	}
}

// preflight checks what must hold before any run starts.
func preflight(o *options, plan []spec) (sysinfo.Machine, error) {
	for _, name := range []string{"engine", "loadgen", "clockprobe"} {
		if _, err := os.Stat(o.exe(name)); err != nil {
			return sysinfo.Machine{}, fmt.Errorf("%s missing (make bench-build): %w", o.exe(name), err)
		}
	}
	m, err := sysinfo.ReadMachine()
	if err != nil {
		return m, err
	}
	if shared := m.SharedCores(o.engineMask, o.loadMask); len(shared) > 0 {
		return m, fmt.Errorf("engine mask %#x and loadgen mask %#x share physical cores %v", o.engineMask, o.loadMask, shared)
	}
	if o.engineMask&o.loadMask != 0 {
		return m, fmt.Errorf("engine and loadgen masks overlap")
	}
	p, err := sysinfo.ReadPower()
	if err != nil {
		return m, err
	}
	fmt.Printf("power: %s\n", p)
	if want := o.suite.Personality; want != "" {
		var problems []string
		if !p.AC {
			problems = append(problems, "on battery")
		}
		if p.Personality != want {
			problems = append(problems, fmt.Sprintf("the active plan %q is %q, the suite needs %q", p.SchemeName, p.Personality, want))
		}
		if len(problems) > 0 && !o.force {
			return m, fmt.Errorf("%s (see bench/CHECKLIST.md; -force runs anyway, marking every run disturbed)", strings.Join(problems, "; "))
		}
	}
	// Ports free: no engine left over from an earlier run (or the operator's own).
	for _, a := range []string{o.addr(), o.httpAddr()} {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			return m, fmt.Errorf("%s is in use: is an engine still running? %w", a, err)
		}
		_ = ln.Close()
	}
	if err := os.MkdirAll(o.dataRoot, 0o755); err != nil {
		return m, err
	}
	if free, err := sysinfo.DiskFree(o.dataRoot); err == nil && free < 5<<30 {
		return m, fmt.Errorf("only %.1f GB free under %s; a run's WAL can reach a few GB", float64(free)/(1<<30), o.dataRoot)
	}
	if err := os.MkdirAll(o.results, 0o755); err != nil {
		return m, err
	}
	if old, _ := filepath.Glob(filepath.Join(o.results, "*.json")); len(old) > 0 {
		if o.suite.Name != "smoke" {
			return m, fmt.Errorf("%s already holds %d result files: move or delete them first (a suite is never mixed with another)", o.results, len(old))
		}
		for _, f := range old {
			_ = os.Remove(f)
		}
	}
	return m, nil
}

func run(o *options, plan []spec) error {
	machine, err := preflight(o, plan)
	if err != nil {
		return err
	}
	commit := gitCommit()
	t := o.suite.Timing
	fmt.Printf("suite %s: %d runs, about %v; results in %s\n", o.suite.Name, len(plan), estimate(plan, t).Round(time.Minute), o.results)

	// Ctrl-C ends the suite: the run in progress is abandoned (its
	// processes killed, no result file), finished runs keep theirs.
	// interrupted is closed rather than sent on, so every select sees it.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	interrupted := make(chan struct{})
	go func() { <-sig; close(interrupted) }()
	stopped := func(done int) error {
		return fmt.Errorf("interrupted after %d of %d runs; their results are in %s", done, len(plan), o.results)
	}
	began := time.Now()
	var disturbed, failed int
	for i, sp := range plan {
		select {
		case <-interrupted:
			return stopped(i)
		default:
		}
		id := runID(i+1, sp)
		left := estimate(plan[i:], t)
		fmt.Printf("[%2d/%d] %-34s ", i+1, len(plan), id)
		r := &result.Run{Schema: result.Schema, ID: id, Suite: o.suite.Name, Table: sp.Table, Config: sp.Config,
			Round: sp.Round, Order: i + 1, StartedAt: time.Now().UTC(), Machine: machine, GoVersion: runtime.Version(),
			GitCommit: commit, Disturbed: []string{}}
		r.Commands.Benchrun = os.Args
		r.PowerBefore, _ = sysinfo.ReadPower()
		if sp.Clock {
			err = clockRun(o, sp, id, r, interrupted)
		} else {
			err = loadRun(o, sp, id, r, interrupted)
		}
		r.PowerAfter, _ = sysinfo.ReadPower()
		select {
		case <-interrupted:
			fmt.Println("interrupted")
			return stopped(i)
		default:
		}
		if err != nil {
			r.Failed = err.Error()
			failed++
		} else {
			r.Disturbed = append(r.Disturbed, result.Assess(r)...)
		}
		if o.force && o.suite.Personality != "" && r.PowerBefore.Personality != o.suite.Personality {
			r.Disturbed = append(r.Disturbed, fmt.Sprintf("forced: plan %q, suite needs %q", r.PowerBefore.Personality, o.suite.Personality))
		}
		if len(r.Disturbed) > 0 {
			disturbed++
		}
		if werr := result.Write(filepath.Join(o.results, id+".json"), r); werr != nil {
			return werr
		}
		fmt.Println(summary(r), " (about", left.Round(time.Minute), "left)")
		if i < len(plan)-1 && !sp.Clock {
			time.Sleep(t.Cooldown)
		}
	}
	fmt.Printf("done in %v: %d runs, %d disturbed, %d failed; results in %s\n",
		time.Since(began).Round(time.Second), len(plan), disturbed, failed, o.results)
	if disturbed+failed > 0 {
		fmt.Println("disturbed or failed runs: see each file's \"disturbed\" and \"failed\" fields; bench/CHECKLIST.md says what to do")
	}
	return nil
}

// summary is a run's one-line console report.
func summary(r *result.Run) string {
	var b strings.Builder
	switch {
	case r.Failed != "":
		fmt.Fprintf(&b, "FAILED: %s", r.Failed)
		return b.String()
	case r.Clock != nil:
		c := r.Clock
		fmt.Fprintf(&b, "%s %.1f ns/read, steps p50 %d ns p99.9 %d ns", c.Source, c.CostNS, c.Steps.P50NS, c.Steps.P999NS)
	case r.Load != nil:
		l := r.Load
		fmt.Fprintf(&b, "%7.0f/s  core p50 %s p99.9 %s  e2e p50 %s p99.9 %s  cpu eng %.2f gen %.2f other %.2f",
			l.ThroughputPerSec, us(l.Core.P50NS), us(l.Core.P999NS), us(l.E2E.P50NS), us(l.E2E.P999NS),
			l.CPU.Engine, l.CPU.Loadgen, l.CPU.Other)
	}
	if len(r.Disturbed) > 0 {
		fmt.Fprintf(&b, "  DISTURBED: %s", strings.Join(r.Disturbed, "; "))
	}
	return b.String()
}

func us(ns int64) string {
	switch {
	case ns >= 10_000_000:
		return fmt.Sprintf("%.0fms", float64(ns)/1e6)
	case ns >= 10_000:
		return fmt.Sprintf("%.0fus", float64(ns)/1e3)
	}
	return fmt.Sprintf("%.2fus", float64(ns)/1e3)
}

// clockRun runs clockprobe pinned to the engine's CPUs.
func clockRun(o *options, sp spec, id string, r *result.Run, interrupted <-chan struct{}) error {
	dir := filepath.Join(o.dataRoot, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	out := filepath.Join(dir, "clock.json")
	args := o.clockArgs(out)
	r.Commands.Loadgen, r.Commands.Env = args, env(o.engineMask)
	r.Commands.LoadgenStart = startLine(o.engineMask, env(o.engineMask), args)
	r.Pinning.LoadgenMask = o.engineMask
	cmd, logs, err := start(args, env(o.engineMask), o.engineMask, &r.Pinning.LoadgenReadBack)
	if err != nil {
		return err
	}
	if err := wait(cmd, time.Minute, interrupted); err != nil {
		return fmt.Errorf("clockprobe: %w: %s", err, tail(logs))
	}
	var c result.Clock
	if err := readJSON(out, &c); err != nil {
		return err
	}
	r.Clock = &c
	return nil
}

// loadRun starts an engine on a fresh data directory, runs loadgen against
// it, and stops the engine.
func loadRun(o *options, sp spec, id string, r *result.Run, interrupted <-chan struct{}) error {
	dir := filepath.Join(o.dataRoot, id)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	data := filepath.Join(dir, "data")

	engArgs := o.engineArgs(sp, data)
	r.Commands.Engine, r.Commands.Env = engArgs, append(env(o.engineMask), env(o.loadMask)...)
	r.Commands.EngineStart = startLine(o.engineMask, env(o.engineMask), engArgs)
	r.Pinning.EngineMask, r.Pinning.LoadgenMask = o.engineMask, o.loadMask
	r.Pinning.SharedCores = r.Machine.SharedCores(o.engineMask, o.loadMask)

	eng, engLogs, err := start(engArgs, env(o.engineMask), o.engineMask, &r.Pinning.EngineReadBack)
	if err != nil {
		return err
	}
	stopped := false
	defer func() {
		if !stopped {
			_ = eng.Process.Kill()
			_ = eng.Wait()
		}
	}()
	if err := waitHealthy(o.httpAddr(), 30*time.Second); err != nil {
		return fmt.Errorf("engine: %w: %s", err, tail(engLogs))
	}

	out := filepath.Join(dir, "load.json")
	loadArgs := o.loadArgs(sp, eng.Process.Pid, out)
	r.Commands.Loadgen = loadArgs
	r.Commands.LoadgenStart = startLine(o.loadMask, env(o.loadMask), o.loadArgs(sp, 0, "run.json")) // the pid differs each time
	gen, genLogs, err := start(loadArgs, env(o.loadMask), o.loadMask, &r.Pinning.LoadgenReadBack)
	if err != nil {
		return err
	}
	t := o.suite.Timing
	genErr := wait(gen, t.Warmup+t.Duration+2*time.Minute, interrupted)

	// Graceful stop: CTRL_BREAK, as Ctrl-C would. A kill would do (the
	// data directory is deleted), but a clean exit also shows the engine
	// survived the run.
	stopErr := sysinfo.Interrupt(eng.Process.Pid)
	if stopErr == nil {
		stopErr = wait(eng, 30*time.Second, nil)
	}
	stopped = stopErr == nil

	var l result.Load
	if err := readJSON(out, &l); err != nil {
		return fmt.Errorf("loadgen wrote no result (%v): %s", genErr, tail(genLogs))
	}
	r.Load = &l
	if genErr != nil {
		return fmt.Errorf("loadgen: %w: %s", genErr, tail(genLogs))
	}
	if len(l.Errors) > 0 {
		return fmt.Errorf("loadgen: %s", strings.Join(l.Errors, "; "))
	}
	if stopErr != nil {
		return fmt.Errorf("engine didn't shut down cleanly: %w: %s", stopErr, tail(engLogs))
	}
	return nil
}

// start runs args with env added, in a process group of its own, and pins
// it to mask straight away, reading the mask back into readBack.
func start(args, extraEnv []string, mask uint64, readBack *uint64) (*exec.Cmd, *output, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), extraEnv...)
	logs := &output{}
	cmd.Stdout, cmd.Stderr = logs, logs
	sysinfo.Interruptible(cmd)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	if err := sysinfo.SetAffinity(cmd.Process.Pid, mask); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, err
	}
	*readBack, _ = sysinfo.Affinity(cmd.Process.Pid)
	return cmd, logs, nil
}

var errTimeout = errors.New("timed out")

// wait waits for cmd to exit, at most d, or until interrupted (benchrun's
// own Ctrl-C), killing it then.
func wait(cmd *exec.Cmd, d time.Duration, interrupted <-chan struct{}) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		_ = cmd.Process.Kill()
		<-done
		return errTimeout
	case <-interrupted:
		_ = cmd.Process.Kill()
		<-done
		return errors.New("interrupted")
	}
}

func waitHealthy(addr string, d time.Duration) error {
	hc := &http.Client{Timeout: time.Second}
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		resp, err := hc.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
	return errors.New("not healthy")
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// output collects a child's stdout and stderr; os/exec copies into it
// from its own goroutine while benchrun may read it.
type output struct {
	mu  sync.Mutex // guards buf
	buf bytes.Buffer
}

func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

// tail is the end of a process's output, for error messages.
func tail(o *output) string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	s := strings.TrimSpace(o.buf.String())
	o.mu.Unlock()
	if len(s) > 600 {
		s = "..." + s[len(s)-600:]
	}
	return s
}

// gitCommit is HEAD, plus "-dirty" if tracked files outside bench/results
// differ from it (the results being written are expected to).
func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	c := strings.TrimSpace(string(out))
	st, err := exec.Command("git", "status", "--porcelain", "--untracked-files=no", "--", ".", ":(exclude)bench/results").Output()
	if err == nil && len(bytes.TrimSpace(st)) > 0 {
		c += "-dirty"
	}
	return c
}
