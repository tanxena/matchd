package load

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tanxena/matchd/bench/internal/result"
	"github.com/tanxena/matchd/internal/metrics"
)

// scraper reads the engine's HTTP side at the window's edges.
type scraper struct {
	base string
	hc   *http.Client
}

func newScraper(addr string) *scraper {
	return &scraper{base: "http://" + addr, hc: &http.Client{Timeout: 5 * time.Second}}
}

// engineState is everything read from the engine at one moment.
type engineState struct {
	metrics map[string]float64 // series (name plus labels, as rendered) -> value
	latency metrics.LogSnapshot
	orders  int64 // resting orders on the benchmark instrument
}

type instrumentInfo struct {
	ID       uint32 `json:"id"`
	TickSize int64  `json:"tick_size"`
	LotSize  int64  `json:"lot_size"`
	Orders   int64  `json:"orders"`
}

func (s *scraper) get(path string, decode func(io.Reader) error) error {
	resp, err := s.hc.Get(s.base + path)
	if err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	if err := decode(resp.Body); err != nil {
		return fmt.Errorf("GET %s: %w", path, err)
	}
	return nil
}

func (s *scraper) instruments() ([]instrumentInfo, error) {
	var list []instrumentInfo
	err := s.get("/instruments", func(r io.Reader) error { return json.NewDecoder(r).Decode(&list) })
	return list, err
}

// checkInstrument: the generator's prices and quantities assume tick 1 and
// lot 1.
func (s *scraper) checkInstrument(id uint32) error {
	list, err := s.instruments()
	if err != nil {
		return err
	}
	for _, in := range list {
		if in.ID == id {
			if in.TickSize != 1 || in.LotSize != 1 {
				return fmt.Errorf("instrument %d has tick %d and lot %d; the load generator needs 1 and 1", id, in.TickSize, in.LotSize)
			}
			return nil
		}
	}
	return fmt.Errorf("the engine has no instrument %d", id)
}

func (s *scraper) read() (engineState, error) {
	var st engineState
	err := s.get("/metrics", func(r io.Reader) error {
		var err error
		st.metrics, err = parseMetrics(r)
		return err
	})
	if err != nil {
		return st, err
	}
	var lat struct {
		Matching metrics.LogSnapshot `json:"matching"`
	}
	if err := s.get("/debug/latency", func(r io.Reader) error { return json.NewDecoder(r).Decode(&lat) }); err != nil {
		return st, err
	}
	st.latency = lat.Matching
	list, err := s.instruments()
	if err != nil {
		return st, err
	}
	for _, in := range list {
		st.orders += in.Orders
	}
	return st, nil
}

// parseMetrics reads the Prometheus text format: one "series value" per
// line, comments skipped.
func parseMetrics(r io.Reader) (map[string]float64, error) {
	m := map[string]float64{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		i := bytes.LastIndexByte(line, ' ')
		if i < 0 {
			return nil, fmt.Errorf("bad metrics line %q", line)
		}
		v, err := strconv.ParseFloat(string(line[i+1:]), 64)
		if err != nil {
			return nil, fmt.Errorf("bad metrics line %q: %w", line, err)
		}
		m[string(line[:i])] = v
	}
	return m, sc.Err()
}

// sum adds every series of a metric (all label combinations).
func sum(m map[string]float64, name string) float64 {
	var t float64
	for k, v := range m {
		if k == name || strings.HasPrefix(k, name+"{") {
			t += v
		}
	}
	return t
}

// counters returns the engine's counters over the window (after - before),
// and the clock it timed itself with.
func (after engineState) counters(before engineState) result.EngineCounters {
	d := func(name string) float64 { return sum(after.metrics, name) - sum(before.metrics, name) }
	c := result.EngineCounters{
		Commands:        d("matchd_commands_total"),
		Trades:          d("matchd_trades_total"),
		Fsyncs:          d("matchd_wal_fsync_seconds_count"),
		WALBytes:        d("matchd_wal_written_bytes_total"),
		Snapshots:       after.metrics[`matchd_snapshots_total{result="written"}`] - before.metrics[`matchd_snapshots_total{result="written"}`],
		SlowConsumers:   d("matchd_slow_consumer_disconnects_total"),
		ClockReadCostNS: after.metrics["matchd_clock_read_cost_seconds"] * 1e9,
	}
	if c.Fsyncs > 0 {
		c.FsyncMeanNS = d("matchd_wal_fsync_seconds_sum") * 1e9 / c.Fsyncs
	}
	for k := range after.metrics {
		if s, ok := strings.CutPrefix(k, `matchd_clock_info{source="`); ok {
			c.ClockSource = strings.TrimSuffix(s, `"}`)
		}
	}
	return c
}
