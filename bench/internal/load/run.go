package load

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/bench/internal/sysinfo"
	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/metrics"
)

// Config is one load run.
type Config struct {
	Addr     string // engine TCP address
	HTTPAddr string // engine HTTP address (/metrics, /debug/latency, /instruments)
	// EnginePID, if set, is the engine process, for its CPU time.
	EnginePID int

	Mix   string
	Conns int // connections, logged in as clients ClientBase..ClientBase+Conns-1
	// Rate > 0: open loop, Rate commands per second over all connections.
	// Rate == 0: closed loop, Window commands in flight per connection.
	Rate   int
	Window int

	Warmup   time.Duration
	Duration time.Duration
	Seed     uint64

	Instrument uint32 // must have tick 1 and lot 1 (checked)
	ClientBase uint32
	// Depth is about how many resting orders the book should hold, across
	// all connections; 0 = 10,000. See Mixes.
	Depth    int
	LiveCap  int           // resting orders per connection before CapCancel
	MaxBatch int           // open loop: most commands per write
	Drain    time.Duration // after the window: how long to wait for answers

	mix Mix
}

// Defaults fills in zero fields.
func (c *Config) Defaults() {
	if c.Conns == 0 {
		c.Conns = 4
	}
	if c.Window == 0 {
		c.Window = 64
	}
	if c.Instrument == 0 {
		c.Instrument = 1
	}
	if c.ClientBase == 0 {
		c.ClientBase = 1
	}
	if c.Depth == 0 {
		c.Depth = 10_000
	}
	if c.LiveCap == 0 {
		c.LiveCap = c.Depth // per connection: never reached while the depth control works
	}
	if c.MaxBatch == 0 {
		c.MaxBatch = 256
	}
	if c.Drain == 0 {
		c.Drain = 30 * time.Second
	}
}

// Phases of a run.
const (
	phaseWarmup int32 = iota
	phaseMeasure
	phaseDone // nothing more is sent
)

type runner struct {
	cfg   Config
	depth *Depth // resting orders across connections
	conns []*conn
	phase atomic.Int32
	stop  chan struct{} // closed at phaseDone or on abort: senders return
	once  sync.Once

	errMu sync.Mutex // guards errs
	errs  []error
}

func (r *runner) measuring() bool { return r.phase.Load() == phaseMeasure }
func (r *runner) stopping() bool  { return r.phase.Load() == phaseDone }

// abort ends the run early after a failure.
func (r *runner) abort(err error) {
	r.errMu.Lock()
	r.errs = append(r.errs, err)
	r.errMu.Unlock()
	r.halt()
}

func (r *runner) halt() {
	r.once.Do(func() {
		r.phase.Store(phaseDone)
		close(r.stop)
	})
}

// Run drives the engine: warmup, then the measured window, then a drain.
// The returned Load is filled in even when err is non-nil, as far as the
// run got.
func Run(cfg Config) (*result.Load, error) {
	cfg.Defaults()
	mix, ok := Mixes[cfg.Mix]
	if !ok {
		return nil, fmt.Errorf("unknown mix %q (want one of: %s)", cfg.Mix, MixNames())
	}
	cfg.mix = mix
	if cfg.Rate < 0 || cfg.Conns <= 0 || cfg.Window <= 0 {
		return nil, errors.New("rate must be >= 0, conns and window > 0")
	}
	clock.Init()
	eng := newScraper(cfg.HTTPAddr)
	if err := eng.checkInstrument(cfg.Instrument); err != nil {
		return nil, err
	}

	r := &runner{cfg: cfg, depth: NewDepth(cfg.Depth), stop: make(chan struct{})}
	for i := range cfg.Conns {
		c, err := client.Dial(cfg.Addr, cfg.ClientBase+uint32(i), 5*time.Second)
		if err != nil {
			for _, k := range r.conns {
				_ = k.c.Close()
			}
			return nil, fmt.Errorf("connection %d: %w", i, err)
		}
		r.conns = append(r.conns, newConn(r, i, c))
	}
	var readers, senders sync.WaitGroup
	for _, k := range r.conns {
		readers.Add(1)
		go func() { defer readers.Done(); k.read() }()
	}
	if cfg.Rate > 0 {
		senders.Add(1)
		go func() { defer senders.Done(); r.pace() }()
	} else {
		for _, k := range r.conns {
			senders.Add(1)
			go func() { defer senders.Done(); k.writeClosed() }()
		}
	}

	out := &result.Load{
		Mode: "closed", Mix: cfg.Mix, Conns: cfg.Conns, Window: cfg.Window, Seed: cfg.Seed, Depth: cfg.Depth,
		RequestedWarmupNS: int64(cfg.Warmup), RequestedNS: int64(cfg.Duration), Errors: []string{},
		Answers: result.Answers{Rejects: map[string]uint64{}},
	}
	if cfg.Rate > 0 {
		out.Mode, out.RatePerSec, out.Window = "open", cfg.Rate, 0
	}

	t0 := clock.Now()
	sleepUnlessStopped(r, cfg.Warmup)
	// Window start: CPU times, then the engine's counters, then the flip,
	// at once. Window end: stop counting and sending at once, then CPU
	// times and the engine's counters. The engine's window then encloses
	// ours by one HTTP scrape (about a millisecond) at the start, and at
	// the end by the commands still in flight when sending stopped. Until
	// review F-018 a full process-list query sat between the first scrape
	// and the flip, and sending went on until after the second scrape: the
	// engine counted 100-220 more commands per run, 5-18 ms of traffic.
	cpu0, errCPU := readCPU(cfg.EnginePID)
	before, errBefore := eng.read()
	r.phase.CompareAndSwap(phaseWarmup, phaseMeasure)
	start := clock.Now()
	out.WarmupNS = start - t0

	sleepUnlessStopped(r, cfg.Duration)
	r.phase.CompareAndSwap(phaseMeasure, phaseDone) // stop counting...
	end := clock.Now()
	r.halt() // ...and stop sending
	cpu1, errCPU2 := readCPU(cfg.EnginePID)
	after, errAfter := eng.read()
	out.MeasuredNS = end - start

	// Drain: wait for every answer, so the engine has nothing queued for
	// connections about to close.
	senders.Wait()
	for deadline := time.Now().Add(cfg.Drain); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		n := 0
		for _, k := range r.conns {
			n += k.inFlight()
		}
		if n == 0 {
			break
		}
	}
	for _, k := range r.conns {
		_ = k.c.Close()
	}
	readers.Wait()

	// Assemble.
	var e2e, lag metrics.LogSnapshot
	for _, k := range r.conns {
		k.mu.Lock()
		out.Sent = addCounts(out.Sent, k.sent)
		out.Completed += k.completed
		out.Writes += k.writes
		out.WindowFullNS += k.fullNS
		out.Answers = addAnswers(out.Answers, k.answers)
		if n := len(k.pend) - k.head; n > 0 {
			out.Errors = append(out.Errors, fmt.Sprintf("conn %d: %d commands still unanswered after the drain", k.idx, n))
		}
		k.mu.Unlock()
		e2e = e2e.Add(k.e2e.Snapshot())
		lag = lag.Add(k.sendLag.Snapshot())
	}
	out.E2EHist, out.E2E = e2e, result.Summarise(e2e)
	if cfg.Rate > 0 {
		l := result.Summarise(lag)
		out.SendLag = &l
	}
	if out.MeasuredNS > 0 {
		out.ThroughputPerSec = float64(out.Completed) / (float64(out.MeasuredNS) / 1e9)
	}
	for _, err := range []error{errBefore, errAfter, errCPU, errCPU2} {
		if err != nil {
			out.Errors = append(out.Errors, err.Error())
		}
	}
	if errBefore == nil && errAfter == nil {
		out.CoreHist = after.latency.Sub(before.latency)
		out.Core = result.Summarise(out.CoreHist)
		out.Engine = after.counters(before)
		out.BookOrdersStart, out.BookOrdersEnd = before.orders, after.orders
	}
	if errCPU == nil && errCPU2 == nil {
		out.CPU = cpuUse(cpu0, cpu1, out.MeasuredNS)
	}
	r.errMu.Lock()
	for _, err := range r.errs {
		out.Errors = append(out.Errors, err.Error())
	}
	failed := len(r.errs) > 0
	r.errMu.Unlock()
	if failed {
		return out, fmt.Errorf("run failed: %s", out.Errors[len(out.Errors)-1])
	}
	return out, nil
}

func sleepUnlessStopped(r *runner, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-r.stop:
	}
}

// pace is the open-loop sender: one goroutine that spins on the clock and,
// for each connection, writes every command whose scheduled time has come,
// in one write. Spinning costs the load generator one busy CPU (on its own
// cores, recorded in its CPU figure), but a sleeping sender would wake
// late by the OS timer's granularity and that lateness would land in the
// latency it measures. Each command's scheduled time is kept, and latency
// is measured from it, so falling behind can't hide (no coordinated
// omission); how far behind it fell is recorded as send lag.
func (r *runner) pace() {
	n := len(r.conns)
	interval := float64(time.Second) * float64(n) / float64(r.cfg.Rate) // per connection
	start := clock.Now()
	next := make([]float64, n)
	for i := range next {
		next[i] = float64(start) + interval*float64(i)/float64(n) // staggered
	}
	due := make([]int64, 0, r.cfg.MaxBatch)
	for spins := 0; ; spins++ {
		select {
		case <-r.stop:
			return
		default:
		}
		now := clock.Now()
		for i, k := range r.conns {
			due = due[:0]
			for next[i] <= float64(now) && len(due) < r.cfg.MaxBatch {
				due = append(due, int64(next[i]))
				next[i] += interval
			}
			if len(due) == 0 {
				continue
			}
			if err := k.sendDue(due); err != nil {
				k.fail(fmt.Errorf("conn %d: write: %w", k.idx, err))
				return
			}
		}
		if spins%1024 == 0 {
			runtime.Gosched() // let the readers in if they share this thread's CPU
		}
	}
}

func addCounts(a, b result.Counts) result.Counts {
	a.Passive += b.Passive
	a.Crossing += b.Crossing
	a.Cancel += b.Cancel
	a.CapCancel += b.CapCancel
	a.NoTarget += b.NoTarget
	return a
}

func addAnswers(a, b result.Answers) result.Answers {
	a.Acks += b.Acks
	a.CancelAcks += b.CancelAcks
	a.IOCUnfilled += b.IOCUnfilled
	a.Fills += b.Fills
	for k, v := range b.Rejects {
		a.Rejects[k] += v
	}
	return a
}

// cpuSample is CPU time at one moment: this process, the engine, every
// other process, and the machine as a whole.
type cpuSample struct {
	engine, self sysinfo.CPUTimes
	system       sysinfo.SystemTimes
	procs        sysinfo.Snapshot
	enginePID    int
}

func readCPU(enginePID int) (cpuSample, error) {
	s := cpuSample{enginePID: enginePID}
	var err error
	if s.self, err = sysinfo.SelfTimes(); err != nil {
		return s, err
	}
	if s.system, err = sysinfo.ReadSystemTimes(); err != nil {
		return s, err
	}
	if s.procs, err = sysinfo.ReadSnapshot(); err != nil {
		return s, err
	}
	if enginePID > 0 {
		if s.engine, err = sysinfo.ProcessTimes(enginePID); err != nil {
			return s, err
		}
	}
	return s, nil
}

// cpuUse converts two samples windowNS apart into cores used.
func cpuUse(a, b cpuSample, windowNS int64) result.CPU {
	w := float64(windowNS)
	c := result.CPU{Logical: runtime.NumCPU()}
	if w <= 0 {
		return c
	}
	c.Loadgen = float64(b.self.Total()-a.self.Total()) / w
	if b.enginePID > 0 {
		c.Engine = float64(b.engine.Total()-a.engine.Total()) / w
	}
	c.MachineBusy = float64(b.system.Busy-a.system.Busy) / w
	c.Interrupts = float64(b.procs.Interrupts-a.procs.Interrupts) / w
	for i, p := range sysinfo.Others(a.procs, b.procs, os.Getpid(), b.enginePID) {
		use := float64(p.CPU) / w
		c.Other += use
		if i < 5 {
			c.TopOther = append(c.TopOther, result.ProcUse{PID: p.PID, Name: p.Name, CPUs: use})
		}
	}
	return c
}
