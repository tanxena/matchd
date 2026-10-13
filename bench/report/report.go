package main

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/bench/internal/sysinfo"
)

// group is every run of one configuration: one table row.
type group struct {
	Suite, Table, Config string
	Runs                 []*result.Run // the runs whose numbers count
	Excluded             []*result.Run // disturbed or failed
}

// load reads every result file under dir.
func load(dir string) ([]*result.Run, error) {
	var runs []*result.Run
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		r, err := result.Read(path)
		if err != nil {
			return err
		}
		runs = append(runs, r)
		return nil
	})
	if err == nil && len(runs) == 0 {
		err = fmt.Errorf("no result files under %s", dir)
	}
	return runs, err
}

// groups sorts runs into rows. A run counts unless it failed or was
// disturbed: a disturbed run's numbers are never reported, only listed.
// With smoke set (checking the harness, not reporting results) disturbed
// runs count too; failed ones never do.
func groups(runs []*result.Run, smoke bool) map[[3]string]*group {
	out := map[[3]string]*group{}
	for _, r := range runs {
		k := [3]string{r.Suite, r.Table, r.Config}
		g := out[k]
		if g == nil {
			g = &group{Suite: r.Suite, Table: r.Table, Config: r.Config}
			out[k] = g
		}
		if r.Failed != "" || (len(r.Disturbed) > 0 && !smoke) {
			g.Excluded = append(g.Excluded, r)
		} else {
			g.Runs = append(g.Runs, r)
		}
	}
	for _, g := range out {
		byOrder := func(a, b *result.Run) int { return a.Order - b.Order }
		slices.SortFunc(g.Runs, byOrder)
		slices.SortFunc(g.Excluded, byOrder)
	}
	return out
}

// check enforces PRD 7.5: every row needs at least minRuns counted runs,
// and (unless allowed) no run may have been excluded.
// minRuns applies to every suite unless perSuite names it.
func check(gs map[[3]string]*group, minRuns int, perSuite map[string]int, allowExcluded bool) error {
	var problems []string
	for _, g := range sortedGroups(gs) {
		need := minRuns
		if n, ok := perSuite[g.Suite]; ok {
			need = n
		}
		if len(g.Runs) < need {
			problems = append(problems, fmt.Sprintf("%s/%s/%s: %d counted runs, need %d", g.Suite, g.Table, g.Config, len(g.Runs), need))
		}
		if !allowExcluded {
			for _, r := range g.Excluded {
				problems = append(problems, fmt.Sprintf("%s/%s: %s", r.Suite, r.ID, why(r)))
			}
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("results not fit to report (-allow-disturbed reports the rest, listing these):\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

func why(r *result.Run) string {
	if r.Failed != "" {
		return "failed: " + r.Failed
	}
	return "disturbed: " + strings.Join(r.Disturbed, "; ")
}

func sortedGroups(gs map[[3]string]*group) []*group {
	var out []*group
	for _, g := range gs {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Suite != b.Suite {
			return a.Suite < b.Suite
		}
		if a.Table != b.Table {
			return a.Table < b.Table
		}
		return firstOrder(a) < firstOrder(b)
	})
	return out
}

func firstOrder(g *group) int {
	all := append(append([]*result.Run{}, g.Runs...), g.Excluded...)
	o := 1 << 30
	for _, r := range all {
		o = min(o, r.Order)
	}
	return o
}

// ---------------------------------------------------------------------------
// Statistics across runs: median and min-max (PRD 7.5), never the best run.

type stat struct{ med, lo, hi float64 }

func stats(vals []float64) stat {
	if len(vals) == 0 {
		return stat{}
	}
	v := slices.Clone(vals)
	slices.Sort(v)
	m := v[len(v)/2]
	if len(v)%2 == 0 {
		m = (v[len(v)/2-1] + v[len(v)/2]) / 2
	}
	return stat{med: m, lo: v[0], hi: v[len(v)-1]}
}

func over(runs []*result.Run, f func(*result.Run) float64) stat {
	var v []float64
	for _, r := range runs {
		v = append(v, f(r))
	}
	return stats(v)
}

// Formatting. Latencies are given in microseconds (the README's unit,
// decision 017), with three significant figures at most.
func usNum(ns float64) string {
	u := ns / 1e3
	switch {
	case u >= 100:
		return fmt.Sprintf("%.0f", u)
	case u >= 10:
		return fmt.Sprintf("%.1f", u)
	case u >= 1:
		return fmt.Sprintf("%.2f", u)
	}
	return fmt.Sprintf("%.3f", u)
}

func (s stat) us() string {
	if s.lo == s.hi {
		return usNum(s.med)
	}
	return fmt.Sprintf("%s (%s–%s)", usNum(s.med), usNum(s.lo), usNum(s.hi))
}

func (s stat) num(f string) string {
	if fmt.Sprintf(f, s.lo) == fmt.Sprintf(f, s.hi) {
		return fmt.Sprintf(f, s.med)
	}
	return fmt.Sprintf(f+" ("+f+"–"+f+")", s.med, s.lo, s.hi)
}

func (s stat) count() string { return thousands(s.med) }

// big is a large number with its range, thousands separated.
func (s stat) big() string {
	if thousands(s.lo) == thousands(s.hi) {
		return thousands(s.med)
	}
	return thousands(s.med) + " (" + thousands(s.lo) + "–" + thousands(s.hi) + ")"
}

func thousands(v float64) string {
	n := int64(v + 0.5)
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}

func runsCell(g *group) string {
	if len(g.Excluded) == 0 {
		return fmt.Sprint(len(g.Runs))
	}
	return fmt.Sprintf("**n=%d** of %d†", len(g.Runs), len(g.Runs)+len(g.Excluded))
}

// ---------------------------------------------------------------------------
// The document.

type doc struct {
	asserted []string
	b        strings.Builder
	gs       map[[3]string]*group
	runs     []*result.Run
	excl     []*result.Run
	suites   []string
}

func (d *doc) p(f string, a ...any) { fmt.Fprintf(&d.b, f, a...) }

func (d *doc) group(suite, table, config string) *group { return d.gs[[3]string{suite, table, config}] }

// tableGroups are a table's rows in plan order.
func (d *doc) tableGroups(suite, table string) []*group {
	var out []*group
	for _, g := range sortedGroups(d.gs) {
		if g.Suite == suite && g.Table == table {
			out = append(out, g)
		}
	}
	return out
}

// mainSuite is the suite the main tables come from: full if present
// (smoke when checking the harness).
func (d *doc) mainSuite() string {
	for _, s := range []string{"full", "smoke"} {
		if slices.Contains(d.suites, s) {
			return s
		}
	}
	return d.suites[0]
}

// render generates the section. asserted are statements about the
// environment the harness can't measure (a Defender exclusion), given by
// whoever ran the suites; the hardware line labels them as such.
func render(runs []*result.Run, smoke bool, asserted ...string) string {
	d := &doc{gs: groups(runs, smoke), runs: runs, asserted: asserted}
	if smoke {
		d.p("> **Smoke test of the harness, not benchmark results:** short runs, any power plan, and disturbed runs counted.\n\n")
	}
	seen := map[string]bool{}
	for _, r := range runs {
		if !seen[r.Suite] {
			seen[r.Suite] = true
			d.suites = append(d.suites, r.Suite)
		}
		if r.Failed != "" || (len(r.Disturbed) > 0 && !smoke) {
			d.excl = append(d.excl, r)
		}
	}
	sort.Strings(d.suites)
	main := d.mainSuite()

	d.hardware(main)
	d.clockTable()
	d.mixTables(main)
	d.fsyncTable(main)
	d.snapshotTable(main)
	d.balancedTable(main)
	d.excluded()
	d.commands(main)
	return d.b.String()
}

// first returns any counted run of a suite (for the hardware statement).
func (d *doc) first(suite string) *result.Run {
	var best *result.Run
	for _, r := range d.runs {
		if r.Suite == suite && (best == nil || r.Order < best.Order) {
			best = r
		}
	}
	return best
}

// firstLoadRun is a suite's first load run: the pinning and protocol come
// from one (a clock run pins only itself and has no warmup).
func (d *doc) firstLoadRun(suite string) *result.Run {
	var best *result.Run
	for _, r := range d.runs {
		if r.Suite == suite && r.Load != nil && (best == nil || r.Order < best.Order) {
			best = r
		}
	}
	return best
}

func (d *doc) hardware(main string) {
	r := d.first(main)
	m := r.Machine
	l3 := ""
	for _, c := range m.Caches {
		if c.Level == 3 {
			l3 = fmt.Sprintf(", %d MB L3 shared by logical CPUs %s", c.Bytes>>20, cpuRange(c.Logical))
		}
	}
	// One line with everything a reader needs to judge the numbers (PRD
	// 7.8): every part comes from the result files, except what the
	// harness can't measure, which is labelled as stated by the operator.
	parts := []string{fmt.Sprintf("%s, %d cores / %d threads%s, %.1f GB RAM", m.CPU, len(m.Cores), m.Logical, l3, float64(m.RAMBytes)/(1<<30)),
		m.OS, r.GoVersion}
	power := func(s string) string {
		f := d.first(s)
		if f == nil {
			return ""
		}
		ac := "on AC"
		if !f.PowerBefore.AC {
			ac = "**on battery**"
		}
		mode := ""
		if f.PowerBefore.Overlay != "" && f.PowerBefore.Overlay != "none" {
			mode = ", power mode " + f.PowerBefore.Overlay
		}
		return fmt.Sprintf("%s plan%s %s", f.PowerBefore.SchemeName, mode, ac)
	}
	pw := power(main)
	for _, s := range d.suites {
		if s != main {
			pw += fmt.Sprintf(" (the %s suite: %s)", s, power(s))
		}
	}
	parts = append(parts, pw)
	var pinning string
	if lr := d.firstLoadRun(main); lr != nil && lr.Pinning.EngineMask != 0 {
		p := lr.Pinning
		shared := "no shared physical core, verified from the OS's processor topology"
		if len(p.SharedCores) > 0 {
			shared = fmt.Sprintf("**sharing physical cores %v**", p.SharedCores)
		}
		pinning = fmt.Sprintf("engine pinned to CPUs %s and load generator to %s (%s; they share the L3)",
			cpuRange(sysinfo.MaskCPUs(p.EngineMask)), cpuRange(sysinfo.MaskCPUs(p.LoadgenMask)), shared)
		parts = append(parts, pinning)
	}
	for _, a := range d.asserted {
		parts = append(parts, a+" (stated by the operator; the harness can't check it)")
	}
	parts = append(parts, "nothing else running (measured: see below)")
	d.p("**Hardware and environment** (stated once; every table below ran on it): %s.\n\n", strings.Join(parts, "; "))

	// Nothing else running: measured, not asserted.
	var others []float64
	worst := ""
	worstV := -1.0
	for _, r := range d.runs {
		if r.Load == nil || r.Failed != "" || len(r.Disturbed) > 0 {
			continue
		}
		others = append(others, r.Load.CPU.Other)
		if r.Load.CPU.Other > worstV {
			worstV = r.Load.CPU.Other
			worst = r.ID
			if len(r.Load.CPU.TopOther) > 0 {
				worst += ", mostly " + r.Load.CPU.TopOther[0].Name
			}
		}
	}
	if len(others) > 0 {
		s := stats(others)
		d.p("**Nothing else running,** as measured: other processes used %.2f logical CPUs on average per measured window (median across runs), %.2f at most (%s); a run over %.2f is excluded as disturbed.\n\n",
			s.med, s.hi, worst, result.MaxOtherCPU)
	}
	if pinning != "" {
		d.p("**Pinning** with the process affinity mask (as `start /affinity` sets it), read back after setting.\n\n")
	}

	lr := d.firstLoadRun(main)
	if lr == nil {
		return
	}
	if t := lr; t.Load != nil {
		d.p("**Protocol** (PRD 7): a fresh engine per run; %v warmup discarded, %v measured; each configuration run %s times, interleaved round by round with the other configurations, with a cooldown between runs. Cells are the **median (min–max) across runs**, never the best run. Commit `%s`.\n\n",
			time.Duration(t.Load.RequestedWarmupNS), time.Duration(t.Load.RequestedNS), runsPerConfig(d, main), shortCommit(t.GitCommit))
	}
}

func runsPerConfig(d *doc, suite string) string {
	n := 0
	for _, g := range d.gs {
		if g.Suite == suite {
			n = max(n, len(g.Runs)+len(g.Excluded))
		}
	}
	return fmt.Sprint(n)
}

// shortCommit abbreviates a full hash, keeping a "-dirty" suffix.
func shortCommit(c string) string {
	if len(c) >= 40 {
		return c[:12] + c[40:]
	}
	return c
}

func cpuRange(cpus []int) string {
	if len(cpus) == 0 {
		return "none"
	}
	contiguous := true
	for i := 1; i < len(cpus); i++ {
		contiguous = contiguous && cpus[i] == cpus[i-1]+1
	}
	if contiguous && len(cpus) > 1 {
		return fmt.Sprintf("%d–%d", cpus[0], cpus[len(cpus)-1])
	}
	s := make([]string, len(cpus))
	for i, c := range cpus {
		s[i] = fmt.Sprint(c)
	}
	return strings.Join(s, ", ")
}

func (d *doc) clockTable() {
	d.p("#### Clock overhead\n\n")
	d.p("Back-to-back reads of `internal/clock`, the engine's hot-path clock (decision 017), by `bench/clockprobe` pinned to the engine's CPUs: the cost each read adds to a timed span, and what timing zero work measures.\n\n")
	d.p("| suite (plan) | source | cost per read (ns) | resolution (ns) | zero-work span p50 (ns) | p99.9 (ns) | max (µs) | reads per run | runs |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, s := range d.suites {
		for _, g := range d.tableGroups(s, "clock") {
			if len(g.Runs) == 0 {
				continue
			}
			c := g.Runs[0].Clock
			d.p("| %s (%s) | %s | %s | %s | %s | %s | %s | %s | %s |\n", s, g.Runs[0].PowerBefore.Personality, c.Source,
				over(g.Runs, func(r *result.Run) float64 { return r.Clock.CostNS }).num("%.1f"),
				over(g.Runs, func(r *result.Run) float64 { return float64(r.Clock.Resolution) }).num("%.0f"),
				over(g.Runs, func(r *result.Run) float64 { return float64(r.Clock.Steps.P50NS) }).num("%.0f"),
				over(g.Runs, func(r *result.Run) float64 { return float64(r.Clock.Steps.P999NS) }).num("%.0f"),
				over(g.Runs, func(r *result.Run) float64 { return float64(r.Clock.Steps.MaxNS) }).us(),
				thousands(float64(c.Reads)), runsCell(g))
		}
	}
	d.p("\n")
}

// coreFootnote is the footnote every core-latency table carries (PRD 7.1,
// decision 017): which clock timed it and what a read costs.
func (d *doc) coreFootnote(runs []*result.Run) {
	if len(runs) == 0 || runs[0].Load == nil {
		return
	}
	e := runs[0].Load.Engine
	cost := over(runs, func(r *result.Run) float64 { return r.Load.Engine.ClockReadCostNS })
	d.p("\n¹ Timed inside the engine with `internal/clock` (source `%s`; one read costs %s ns as the engine measured it at startup, see the clock-overhead table): from taking the command off the engine's input channel to sending its last event, which includes one clock read in between. Quantiles and max come from a log-linear histogram (`GET /debug/latency`) and are at most 3.1%% above the true value, never below.\n\n",
		e.ClockSource, cost.num("%.0f"))
}

func (d *doc) mixTables(main string) {
	gs := d.tableGroups(main, "mix")
	if len(gs) == 0 {
		return
	}
	l := firstLoad(gs)
	d.p("#### Matching-core latency by order mix\n\n")
	var all0 []*result.Run
	for _, g := range gs {
		all0 = append(all0, g.Runs...)
	}
	book := over(all0, func(r *result.Run) float64 { return float64(r.Load.BookOrdersEnd) })
	d.p("PRD 7.1(a). Open loop at %s commands/s (all connections), fsync `group:1ms`, snapshots off; the load generator holds the book near %s resting orders (%s at the end of the windows, across mixes and runs). Microseconds.\n\n",
		thousands(float64(l.RatePerSec)), thousands(float64(l.Depth)), book.num("%.0f"))
	d.p("| mix | count per run | p50 | p99 | p99.9 | max | mean | runs |\n|---|---|---|---|---|---|---|---|\n")
	var all []*result.Run
	for _, g := range gs {
		all = append(all, g.Runs...)
		lat := func(f func(result.Latency) float64) stat {
			return over(g.Runs, func(r *result.Run) float64 { return f(r.Load.Core) })
		}
		d.p("| %s¹ | %s | %s | %s | %s | %s | %s | %s |\n", g.Config,
			lat(func(x result.Latency) float64 { return float64(x.Count) }).count(),
			lat(func(x result.Latency) float64 { return float64(x.P50NS) }).us(),
			lat(func(x result.Latency) float64 { return float64(x.P99NS) }).us(),
			lat(func(x result.Latency) float64 { return float64(x.P999NS) }).us(),
			lat(func(x result.Latency) float64 { return float64(x.MaxNS) }).us(),
			lat(func(x result.Latency) float64 { return x.MeanNS }).us(), runsCell(g))
	}
	d.coreFootnote(all)

	d.p("#### End-to-end latency by order mix\n\n")
	d.p("PRD 7.1(b), the same runs: client send to client receipt of the command's first answer (Ack, Reject or CancelAck) over loopback TCP, measured from each command's *scheduled* send time, so the load generator falling behind counts against it. Microseconds. Every answer waits for its WAL fsync (`group:1ms`).\n\n")
	d.p("| mix | count per run | p50 | p99 | p99.9 | max | send lag p99 | fsync mean (µs) | engine CPU | loadgen CPU | runs |\n|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, g := range gs {
		d.e2eRow(g, true)
	}
	d.p("\nCPU columns are logical CPUs busy on average over the window. The load generator spins on the clock to keep its schedule, so it always uses at least one.\n\n")
}

func firstLoad(gs []*group) *result.Load {
	for _, g := range gs {
		if len(g.Runs) > 0 {
			return g.Runs[0].Load
		}
		if len(g.Excluded) > 0 && g.Excluded[0].Load != nil {
			return g.Excluded[0].Load
		}
	}
	return &result.Load{}
}

func (d *doc) e2eRow(g *group, lag bool) {
	lat := func(f func(result.Latency) float64) stat {
		return over(g.Runs, func(r *result.Run) float64 { return f(r.Load.E2E) })
	}
	d.p("| %s | %s | %s | %s | %s | %s |", g.Config,
		lat(func(x result.Latency) float64 { return float64(x.Count) }).count(),
		lat(func(x result.Latency) float64 { return float64(x.P50NS) }).us(),
		lat(func(x result.Latency) float64 { return float64(x.P99NS) }).us(),
		lat(func(x result.Latency) float64 { return float64(x.P999NS) }).us(),
		lat(func(x result.Latency) float64 { return float64(x.MaxNS) }).us())
	if lag {
		d.p(" %s |", over(g.Runs, func(r *result.Run) float64 {
			if r.Load.SendLag == nil {
				return 0
			}
			return float64(r.Load.SendLag.P99NS)
		}).us())
		d.p(" %s |", over(g.Runs, func(r *result.Run) float64 { return r.Load.Engine.FsyncMeanNS / 1e3 }).num("%.0f"))
	}
	d.p(" %s | %s | %s |\n",
		over(g.Runs, func(r *result.Run) float64 { return r.Load.CPU.Engine }).num("%.2f"),
		over(g.Runs, func(r *result.Run) float64 { return r.Load.CPU.Loadgen }).num("%.2f"), runsCell(g))
}

// latCells are the cells PRD 7.1 requires for every reported latency:
// the count (per run), p50, p99, p99.9 and max, each median (min-max)
// across the runs.
func latCells(runs []*result.Run, sel func(*result.Run) result.Latency) []string {
	lat := func(f func(result.Latency) float64) stat {
		return over(runs, func(r *result.Run) float64 { return f(sel(r)) })
	}
	return []string{
		lat(func(x result.Latency) float64 { return float64(x.Count) }).count(),
		lat(func(x result.Latency) float64 { return float64(x.P50NS) }).us(),
		lat(func(x result.Latency) float64 { return float64(x.P99NS) }).us(),
		lat(func(x result.Latency) float64 { return float64(x.P999NS) }).us(),
		lat(func(x result.Latency) float64 { return float64(x.MaxNS) }).us(),
	}
}

func core(r *result.Run) result.Latency { return r.Load.Core }
func e2e(r *result.Run) result.Latency  { return r.Load.E2E }

// row writes one table row from its cells.
func (d *doc) row(cells ...string) { d.p("| %s |\n", strings.Join(cells, " | ")) }

func (d *doc) fsyncTable(main string) {
	gs := d.tableGroups(main, "fsync")
	if len(gs) == 0 {
		return
	}
	l := firstLoad(gs)
	d.p("#### Throughput and end-to-end latency by fsync policy\n\n")
	d.p("PRD 7.2 and 7.6. Closed loop: %d connections × %d commands in flight, resting mix, snapshots off. Throughput is commands (orders and cancels) answered per second over the window; end-to-end latency here is at saturation, so it is mostly queueing (in flight ÷ throughput). Matching-core latency doesn't depend on the fsync policy (PRD 7.1), so it isn't given per policy. Latency in microseconds.\n\n", l.Conns, l.Window)
	d.p("| fsync | commands/s | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max | fsyncs/s | commands per fsync | fsync mean (µs) | engine CPU | loadgen CPU | runs |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, g := range gs {
		perSec := func(r *result.Run) float64 { return r.Load.Engine.Fsyncs / (float64(r.Load.MeasuredNS) / 1e9) }
		perFsync := over(g.Runs, func(r *result.Run) float64 {
			if r.Load.Engine.Fsyncs == 0 {
				return 0
			}
			return r.Load.Engine.Commands / r.Load.Engine.Fsyncs
		})
		pf := perFsync.num("%.0f")
		fps := over(g.Runs, perSec).num("%.0f")
		mean := over(g.Runs, func(r *result.Run) float64 { return r.Load.Engine.FsyncMeanNS / 1e3 }).num("%.0f")
		if g.Config == "never" {
			// No fsync on the commit path. The few fsyncs counted are WAL
			// segment rolls (64 MiB each), not comparable per command or
			// in time with the other rows (review F-012).
			pf, fps, mean = "–", "–", "–"
		}
		cells := append([]string{"`" + g.Config + "`", over(g.Runs, func(r *result.Run) float64 { return r.Load.ThroughputPerSec }).big()},
			latCells(g.Runs, e2e)...)
		d.row(append(cells, fps, pf, mean,
			over(g.Runs, func(r *result.Run) float64 { return r.Load.CPU.Engine }).num("%.2f"),
			over(g.Runs, func(r *result.Run) float64 { return r.Load.CPU.Loadgen }).num("%.2f"), runsCell(g))...)
	}
	d.p("\n`never` is for tests only: acknowledged work can be lost on power failure (decision 012). It has no fsync on the commit path, so its fsync columns are blank. The only fsyncs it does are WAL segment rolls, each flushing a 64 MiB segment.\n\n")
}

func (d *doc) snapshotTable(main string) {
	gs := d.tableGroups(main, "snapshot")
	if len(gs) == 0 {
		return
	}
	l := firstLoad(gs)
	d.p("#### Snapshots: every 10 s vs none\n\n")
	d.p("PRD 5.4: the copy-on-write snapshot's effect on the tail. Resting mix, open loop at %s commands/s, fsync `group:1ms`, snapshots on and off alternating. Microseconds.\n\n", thousands(float64(l.RatePerSec)))
	d.p("| snapshots | core count per run¹ | core p50 | core p99 | core p99.9 | core max | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max | snapshots per run | runs |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	var all []*result.Run
	for _, g := range gs {
		all = append(all, g.Runs...)
		cells := append([]string{strings.TrimPrefix(g.Config, "snapshots-")}, latCells(g.Runs, core)...)
		cells = append(cells, latCells(g.Runs, e2e)...)
		d.row(append(cells, over(g.Runs, func(r *result.Run) float64 { return r.Load.Engine.Snapshots }).num("%.0f"), runsCell(g))...)
	}
	d.coreFootnote(all)
}

func (d *doc) balancedTable(main string) {
	var bal []*group
	for _, s := range d.suites {
		bal = append(bal, d.tableGroups(s, "balanced")...)
	}
	ref := d.group(main, "mix", "resting")
	if len(bal) == 0 || ref == nil {
		return
	}
	d.p("#### The same resting row under the Balanced plan\n\n")
	d.p("What a reader running this on a laptop with default power settings would see: the mix table's resting row, and the same configuration run under the Balanced plan (on its own, after the main suite: a plan can't be switched mid-suite). Microseconds.\n\n")
	d.p("| plan | core count per run¹ | core p50 | core p99 | core p99.9 | core max | e2e count per run | e2e p50 | e2e p99 | e2e p99.9 | e2e max | clock read (ns) | runs |\n|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	row := func(label string, g *group, clock *group) {
		ck := "–"
		if clock != nil && len(clock.Runs) > 0 {
			ck = over(clock.Runs, func(r *result.Run) float64 { return r.Clock.CostNS }).num("%.1f")
		}
		cells := append([]string{label}, latCells(g.Runs, core)...)
		cells = append(cells, latCells(g.Runs, e2e)...)
		d.row(append(cells, ck, runsCell(g))...)
	}
	label := func(g *group) string {
		if len(g.Runs) > 0 {
			p := g.Runs[0].PowerBefore
			l := p.SchemeName
			if p.Overlay != "" && p.Overlay != "none" {
				l += ", " + p.Overlay
			}
			return l
		}
		return g.Suite
	}
	row(label(ref)+" (mix table)", ref, d.group(main, "clock", "clock"))
	for _, g := range bal {
		row(label(g), g, d.group(g.Suite, "clock", "clock"))
	}
	d.p("\n")
}

func (d *doc) excluded() {
	if len(d.excl) == 0 {
		return
	}
	d.p("† Runs left out of the numbers (failed, or disturbed by the checks in `bench/internal/result/assess.go`):\n\n")
	slices.SortFunc(d.excl, func(a, b *result.Run) int { return strings.Compare(a.Suite+a.ID, b.Suite+b.ID) })
	for _, r := range d.excl {
		d.p("- `%s/%s`: %s\n", r.Suite, r.ID, why(r))
	}
	d.p("\n")
}

// commands lists each row's exact command lines (PRD 7.8): the cmd.exe
// equivalents of what benchrun started, from the first run of the row.
func (d *doc) commands(main string) {
	d.p("<details><summary>Exact commands for every row</summary>\n\n")
	d.p("Each row's runs were started by `%s`, which pins each process with the same affinity mask `start /affinity` takes. To repeat one run by hand from cmd.exe, start the engine, then the load generator with `-engine-pid` set to the engine's process id (or 0 to skip its CPU time):\n\n",
		strings.Join(d.first(main).Commands.Benchrun, " "))
	for _, g := range sortedGroups(d.gs) {
		r := firstAny(g)
		if r == nil {
			continue
		}
		d.p("**%s / %s / %s**\n\n```\n", g.Suite, g.Table, g.Config)
		if r.Commands.EngineStart != "" {
			d.p("%s\n", r.Commands.EngineStart)
		}
		d.p("%s\n```\n\n", r.Commands.LoadgenStart)
	}
	d.p("</details>\n")
}

func firstAny(g *group) *result.Run {
	if len(g.Runs) > 0 {
		return g.Runs[0]
	}
	if len(g.Excluded) > 0 {
		return g.Excluded[0]
	}
	return nil
}
