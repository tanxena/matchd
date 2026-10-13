package metrics

import (
	"bytes"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestTextFormat(t *testing.T) {
	r := NewRegistry()
	acc := r.Counter("matchd_commands_total", "Commands by type and outcome.", "type", "new_order", "outcome", "accepted")
	rej := r.Counter("matchd_commands_total", "Commands by type and outcome.", "type", "new_order", "outcome", "rejected")
	g := r.Gauge("matchd_connections", "Open client connections.")
	r.GaugeFunc("matchd_snapshot_age_seconds", "Seconds since the last snapshot.", func() float64 { return 1.5 })
	r.CounterFunc("matchd_weird_total", "Help with a \\ backslash\nand a newline.", func() uint64 { return 7 }, "label", "quote\" back\\slash")
	h := r.Histogram("matchd_matching_latency_seconds", "Engine latency per command.", Micros(0.25, 1, 5))
	acc.Add(3)
	rej.Inc()
	g.Set(2)
	h.Observe(100)  // <= 250ns
	h.Observe(250)  // exactly on a bound: le is inclusive
	h.Observe(251)  // next bucket
	h.Observe(9000) // overflow

	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP matchd_commands_total Commands by type and outcome.
# TYPE matchd_commands_total counter
matchd_commands_total{type="new_order",outcome="accepted"} 3
matchd_commands_total{type="new_order",outcome="rejected"} 1
# HELP matchd_connections Open client connections.
# TYPE matchd_connections gauge
matchd_connections 2
# HELP matchd_matching_latency_seconds Engine latency per command.
# TYPE matchd_matching_latency_seconds histogram
matchd_matching_latency_seconds_bucket{le="2.5e-07"} 2
matchd_matching_latency_seconds_bucket{le="1e-06"} 3
matchd_matching_latency_seconds_bucket{le="5e-06"} 3
matchd_matching_latency_seconds_bucket{le="+Inf"} 4
matchd_matching_latency_seconds_sum 9.601e-06
matchd_matching_latency_seconds_count 4
# HELP matchd_snapshot_age_seconds Seconds since the last snapshot.
# TYPE matchd_snapshot_age_seconds gauge
matchd_snapshot_age_seconds 1.5
# HELP matchd_weird_total Help with a \\ backslash\nand a newline.
# TYPE matchd_weird_total counter
matchd_weird_total{label="quote\" back\\slash"} 7
`
	if b.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// The decided boundaries (microseconds) must survive the trip to /metrics:
// stored exactly as integer nanoseconds, rendered in seconds, and parsed by
// a scraper back to the same value. 0.25 us = 250 ns = "2.5e-07".
func TestBoundsRoundTrip(t *testing.T) {
	for name, us := range map[string][]float64{
		"matching": {0.25, 0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 5000},
		"fsync":    {50, 100, 250, 500, 1000, 2500, 5000, 10000, 25000, 50000, 100000, 1000000},
	} {
		got := map[string][]int64{"matching": MatchingBuckets, "fsync": FsyncBuckets}[name]
		if len(got) != len(us) {
			t.Fatalf("%s: %d bounds, want %d", name, len(got), len(us))
		}
		for i, u := range us {
			if got[i] != int64(u*1000) {
				t.Errorf("%s bound %d: %d ns, want %v us", name, i, got[i], u)
			}
			s := formatFloat(float64(got[i]) / 1e9)
			back, err := strconv.ParseFloat(s, 64)
			if err != nil || back != u/1e6 || int64(math.Round(back*1e9)) != got[i] {
				t.Errorf("%s bound %v us renders as %q, which parses to %v", name, u, s, back)
			}
		}
	}
	if s := formatFloat(250e-9); s != "2.5e-07" {
		t.Errorf("250 ns renders as %q", s)
	}
}

func TestQuantileBucket(t *testing.T) {
	h := NewHistogram(Micros(1, 10, 100))
	if h.Snapshot().QuantileBucket(0.99) != -1 {
		t.Fatal("empty histogram has a quantile")
	}
	for range 98 {
		h.Observe(500) // bucket 0
	}
	h.Observe(5000)    // bucket 1
	h.Observe(500_000) // overflow
	s := h.Snapshot()
	if got := s.QuantileBucket(0.98); got != 0 {
		t.Errorf("p98 bucket %d, want 0", got)
	}
	if got := s.QuantileBucket(0.99); got != 1 {
		t.Errorf("p99 bucket %d, want 1", got)
	}
	if got := s.QuantileBucket(1); got != 3 {
		t.Errorf("max bucket %d, want 3 (overflow)", got)
	}
	before := s
	h.Observe(500)
	if d := h.Snapshot().Sub(before); d.Count() != 1 || d.Counts[0] != 1 || d.SumNS != 500 {
		t.Errorf("Sub = %+v", d)
	}
}

// The engine records on every command: recording must not allocate.
func TestRecordingDoesNotAllocate(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "c")
	g := r.Gauge("g", "g")
	h := r.Histogram("h_seconds", "h", MatchingBuckets)
	var nilH *Histogram
	if n := testing.AllocsPerRun(1000, func() {
		c.Inc()
		g.Add(1)
		h.Observe(1234)
		nilH.Observe(1)
	}); n != 0 {
		t.Fatalf("%v allocations per recording", n)
	}
}

func TestNilMetricsAreNoOps(t *testing.T) {
	var c *Counter
	var g *Gauge
	var h *Histogram
	c.Inc()
	c.Add(2)
	g.Set(1)
	g.Add(1)
	h.Observe(1)
	if c.Load() != 0 || g.Load() != 0 || h.Snapshot().Count() != 0 {
		t.Fatal("nil metrics hold values")
	}
}

func TestBadRegistrations(t *testing.T) {
	for name, f := range map[string]func(){
		"odd labels":      func() { NewRegistry().Counter("x", "x", "lonely") },
		"type clash":      func() { r := NewRegistry(); r.Counter("x", "x"); r.Gauge("x", "x") },
		"unsorted bounds": func() { NewHistogram([]int64{5, 3}) },
		"zero bound":      func() { NewHistogram([]int64{0, 3}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			f()
		}()
	}
}

// Recording from many goroutines while scraping: run with -race. Every
// observation is counted, and a scrape's _count always equals its +Inf
// bucket.
func TestConcurrentRecordAndScrape(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("c_total", "c")
	h := r.Histogram("h_seconds", "h", MatchingBuckets)
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10000 {
				c.Inc()
				h.Observe(int64(w*1000 + i))
			}
		}()
	}
	stop := make(chan struct{})
	scraped := make(chan error, 1)
	go func() {
		for {
			select {
			case <-stop:
				scraped <- nil
				return
			default:
			}
			var b bytes.Buffer
			if err := r.WriteText(&b); err != nil {
				scraped <- err
				return
			}
			var inf, count string
			for _, line := range strings.Split(b.String(), "\n") {
				if v, ok := strings.CutPrefix(line, `h_seconds_bucket{le="+Inf"} `); ok {
					inf = v
				}
				if v, ok := strings.CutPrefix(line, "h_seconds_count "); ok {
					count = v
				}
			}
			if inf != count {
				t.Errorf("+Inf bucket %s but count %s", inf, count)
			}
		}
	}()
	wg.Wait()
	close(stop)
	if err := <-scraped; err != nil {
		t.Fatal(err)
	}
	if c.Load() != 40000 || h.Snapshot().Count() != 40000 {
		t.Fatalf("counter %d, histogram %d; want 40000", c.Load(), h.Snapshot().Count())
	}
}

func BenchmarkObserve(b *testing.B) {
	h := NewHistogram(MatchingBuckets)
	for i := 0; b.Loop(); i++ {
		h.Observe(int64(i & 4095))
	}
}
