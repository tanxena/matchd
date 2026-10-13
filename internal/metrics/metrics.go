// Package metrics is a small, hand-written Prometheus exporter: counters,
// gauges and fixed-bucket histograms, and the text exposition format
// (decision 017).
//
// Recording is lock-free and allocation-free: a counter is one atomic add, a
// histogram observation is a short scan of its bounds plus two atomic adds,
// so the engine goroutine can record on every command. Every method is safe
// on a nil receiver and does nothing, so a component can hold metrics that
// were never wired up (unit tests).
//
// Rendering (Registry.WriteText) reads the atomics without stopping
// writers, so a scrape is not an instantaneous snapshot of every metric; a
// histogram's buckets, count and sum may disagree by the observations made
// while it was being read. Prometheus scrapes tolerate exactly this.
package metrics

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter only goes up.
type Counter struct{ v atomic.Uint64 }

// Inc adds 1.
func (c *Counter) Inc() {
	if c != nil {
		c.v.Add(1)
	}
}

// Add adds n.
func (c *Counter) Add(n uint64) {
	if c != nil {
		c.v.Add(n)
	}
}

// Load returns the current value (0 for nil).
func (c *Counter) Load() uint64 {
	if c == nil {
		return 0
	}
	return c.v.Load()
}

// Gauge goes up and down.
type Gauge struct{ v atomic.Int64 }

// Set sets the value.
func (g *Gauge) Set(n int64) {
	if g != nil {
		g.v.Store(n)
	}
}

// Add adds n (which may be negative).
func (g *Gauge) Add(n int64) {
	if g != nil {
		g.v.Add(n)
	}
}

// Load returns the current value (0 for nil).
func (g *Gauge) Load() int64 {
	if g == nil {
		return 0
	}
	return g.v.Load()
}

// Histogram counts observations, in nanoseconds, into fixed buckets. Bounds
// are upper bounds, inclusive (Prometheus "le"), in nanoseconds; integers,
// so bucket boundaries are exact. They are rendered in seconds.
type Histogram struct {
	bounds []int64         // ascending, nanoseconds
	counts []atomic.Uint64 // len(bounds)+1; the last is the +Inf overflow bucket
	sum    atomic.Int64    // nanoseconds
}

// NewHistogram returns a histogram with the given upper bounds in
// nanoseconds, which must be positive and strictly ascending.
func NewHistogram(boundsNS []int64) *Histogram {
	for i, b := range boundsNS {
		if b <= 0 || (i > 0 && b <= boundsNS[i-1]) {
			panic(fmt.Sprintf("metrics: histogram bounds must be positive and ascending: %v", boundsNS))
		}
	}
	return &Histogram{bounds: append([]int64(nil), boundsNS...), counts: make([]atomic.Uint64, len(boundsNS)+1)}
}

// Observe records one observation of ns nanoseconds.
func (h *Histogram) Observe(ns int64) {
	if h == nil {
		return
	}
	// A linear scan: bucket lists are short (about a dozen) and most
	// observations land in the first few, so this beats a binary search.
	i := 0
	for i < len(h.bounds) && ns > h.bounds[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sum.Add(ns)
}

// HistogramSnapshot is a histogram's state at one moment: per-bucket (not
// cumulative) counts, the last being the overflow bucket.
type HistogramSnapshot struct {
	Bounds []int64 // nanoseconds
	Counts []uint64
	SumNS  int64
}

// Snapshot reads the histogram.
func (h *Histogram) Snapshot() HistogramSnapshot {
	if h == nil {
		return HistogramSnapshot{}
	}
	s := HistogramSnapshot{Bounds: h.bounds, Counts: make([]uint64, len(h.counts)), SumNS: h.sum.Load()}
	for i := range h.counts {
		s.Counts[i] = h.counts[i].Load()
	}
	return s
}

// Count is the total number of observations.
func (s HistogramSnapshot) Count() uint64 {
	var n uint64
	for _, c := range s.Counts {
		n += c
	}
	return n
}

// Sub returns the observations made between earlier and s (the same
// histogram read twice).
func (s HistogramSnapshot) Sub(earlier HistogramSnapshot) HistogramSnapshot {
	out := HistogramSnapshot{Bounds: s.Bounds, Counts: make([]uint64, len(s.Counts)), SumNS: s.SumNS - earlier.SumNS}
	for i := range s.Counts {
		out.Counts[i] = s.Counts[i]
		if i < len(earlier.Counts) {
			out.Counts[i] -= earlier.Counts[i]
		}
	}
	return out
}

// QuantileBucket returns the index of the bucket holding quantile q (0..1)
// of the observations: the bucket a p99 "is in". -1 if there are none.
func (s HistogramSnapshot) QuantileBucket(q float64) int {
	total := s.Count()
	if total == 0 {
		return -1
	}
	rank := uint64(math.Ceil(q * float64(total)))
	if rank == 0 {
		rank = 1
	}
	var seen uint64
	for i, c := range s.Counts {
		seen += c
		if seen >= rank {
			return i
		}
	}
	return len(s.Counts) - 1
}

// Micros converts microsecond bucket boundaries (as decided and documented,
// decision 017) to the nanoseconds a Histogram stores.
func Micros(us ...float64) []int64 {
	out := make([]int64, len(us))
	for i, u := range us {
		out[i] = int64(math.Round(u * 1000))
	}
	return out
}

// Bucket boundaries in microseconds, chosen with the user (decision 017).
// The benchmark (milestone 9) uses the same lists, so its tables and
// /metrics agree; /metrics shows them in seconds.
var (
	// MatchingBuckets: one command through the engine. Matching is about a
	// third of a microsecond, so the low end is finer than a microsecond.
	MatchingBuckets = Micros(0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 5000)
	// FsyncBuckets: one WAL fsync, from a fast SSD to a stalled disk.
	FsyncBuckets = Micros(50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 1000000)
)

// Registry holds named metrics and renders them in the Prometheus text
// format. Registration happens at startup; WriteText may run concurrently
// with recording.
type Registry struct {
	mu   sync.Mutex
	fams map[string]*family
}

type family struct {
	name, help, typ string
	series          []*series
}

type series struct {
	labels  []string // name, value, name, value...
	counter *Counter
	gauge   *Gauge
	hist    *Histogram
	value   func() float64 // CounterFunc/GaugeFunc
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{fams: map[string]*family{}} }

func (r *Registry) add(name, help, typ string, s *series) {
	if len(s.labels)%2 != 0 {
		panic("metrics: labels must be name/value pairs: " + name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f := r.fams[name]
	if f == nil {
		f = &family{name: name, help: help, typ: typ}
		r.fams[name] = f
	} else if f.typ != typ {
		panic(fmt.Sprintf("metrics: %s registered as %s and %s", name, f.typ, typ))
	}
	f.series = append(f.series, s)
}

// Counter registers and returns a new counter. labels are name/value pairs.
func (r *Registry) Counter(name, help string, labels ...string) *Counter {
	c := &Counter{}
	r.add(name, help, "counter", &series{labels: labels, counter: c})
	return c
}

// Gauge registers and returns a new gauge.
func (r *Registry) Gauge(name, help string, labels ...string) *Gauge {
	g := &Gauge{}
	r.add(name, help, "gauge", &series{labels: labels, gauge: g})
	return g
}

// Histogram registers and returns a new histogram with bounds in
// nanoseconds; its name should end in _seconds.
func (r *Registry) Histogram(name, help string, boundsNS []int64, labels ...string) *Histogram {
	h := NewHistogram(boundsNS)
	r.add(name, help, "histogram", &series{labels: labels, hist: h})
	return h
}

// CounterFunc registers a counter whose value is read from f at each scrape,
// for counts a component already keeps.
func (r *Registry) CounterFunc(name, help string, f func() uint64, labels ...string) {
	r.add(name, help, "counter", &series{labels: labels, value: func() float64 { return float64(f()) }})
}

// GaugeFunc registers a gauge read from f at each scrape.
func (r *Registry) GaugeFunc(name, help string, f func() float64, labels ...string) {
	r.add(name, help, "gauge", &series{labels: labels, value: f})
}

// ContentType is the media type of WriteText's output.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// WriteText renders every metric in the Prometheus text exposition format,
// families sorted by name.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	fams := make([]*family, 0, len(r.fams))
	for _, f := range r.fams {
		fams = append(fams, f)
	}
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })

	// Rendered into memory, then written at once: a scrape is a few KB, and
	// this leaves a single write error to report.
	var b bytes.Buffer
	for _, f := range fams {
		b.WriteString("# HELP " + f.name + " " + escapeHelp(f.help) + "\n# TYPE " + f.name + " " + f.typ + "\n")
		for _, s := range f.series {
			switch {
			case s.counter != nil:
				writeSample(&b, f.name, s.labels, "", "", strconv.FormatUint(s.counter.Load(), 10))
			case s.gauge != nil:
				writeSample(&b, f.name, s.labels, "", "", strconv.FormatInt(s.gauge.Load(), 10))
			case s.value != nil:
				writeSample(&b, f.name, s.labels, "", "", formatFloat(s.value()))
			case s.hist != nil:
				snap := s.hist.Snapshot()
				var cum uint64
				for i, bound := range snap.Bounds {
					cum += snap.Counts[i]
					writeSample(&b, f.name+"_bucket", s.labels, "le", formatFloat(float64(bound)/1e9), strconv.FormatUint(cum, 10))
				}
				cum += snap.Counts[len(snap.Counts)-1]
				writeSample(&b, f.name+"_bucket", s.labels, "le", "+Inf", strconv.FormatUint(cum, 10))
				writeSample(&b, f.name+"_sum", s.labels, "", "", formatFloat(float64(snap.SumNS)/1e9))
				writeSample(&b, f.name+"_count", s.labels, "", "", strconv.FormatUint(cum, 10))
			}
		}
	}
	_, err := w.Write(b.Bytes())
	return err
}

// writeSample writes one line: name{labels,extra="v"} value.
func writeSample(b *bytes.Buffer, name string, labels []string, extraName, extraValue, value string) {
	b.WriteString(name)
	if len(labels) > 0 || extraName != "" {
		b.WriteByte('{')
		sep := ""
		for i := 0; i < len(labels); i += 2 {
			b.WriteString(sep + labels[i] + `="` + escapeLabel(labels[i+1]) + `"`)
			sep = ","
		}
		if extraName != "" {
			b.WriteString(sep + extraName + `="` + extraValue + `"`)
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(value)
	b.WriteByte('\n')
}

// formatFloat renders a sample value or le bound: the shortest form that
// parses back to the same float64 (250 ns is "2.5e-07"), and Prometheus's
// spellings of the special values.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var (
	helpEscaper  = strings.NewReplacer(`\`, `\\`, "\n", `\n`)
	labelEscaper = strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`)
)

func escapeHelp(s string) string  { return helpEscaper.Replace(s) }
func escapeLabel(s string) string { return labelEscaper.Replace(s) }
