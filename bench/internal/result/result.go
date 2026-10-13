// Package result is the benchmark's file format: one JSON file per run in
// bench/results/ (PRD 7.8). loadgen and clockprobe write the measurement
// part; benchrun wraps it with what was run, where and how; report reads
// the files back to generate the README tables (decision 021).
//
// Every duration is in nanoseconds, and every field name says its unit.
package result

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tanxena/matchd/bench/internal/sysinfo"
	"github.com/tanxena/matchd/internal/metrics"
)

// Schema is the format version; report refuses files with another.
const Schema = 1

// Run is one result file.
type Run struct {
	Schema int    `json:"schema"`
	ID     string `json:"id"`    // unique within the suite, e.g. "r3-fsync-always"
	Suite  string `json:"suite"` // smoke, full, balanced
	Table  string `json:"table"` // fsync, mix, snapshot, balanced, clock
	Config string `json:"config"`
	Round  int    `json:"round"` // 1-based; runs of one round are adjacent
	// Order is the run's position in the whole suite (1-based), so the
	// interleaving can be read back.
	Order     int       `json:"order"`
	StartedAt time.Time `json:"started_at"`

	Commands Commands `json:"commands"`
	Pinning  Pinning  `json:"pinning"`

	Load  *Load  `json:"load,omitempty"`  // load runs
	Clock *Clock `json:"clock,omitempty"` // clock-overhead runs

	Machine     sysinfo.Machine `json:"machine"`
	PowerBefore sysinfo.Power   `json:"power_before"`
	PowerAfter  sysinfo.Power   `json:"power_after"`
	GoVersion   string          `json:"go_version"`
	GitCommit   string          `json:"git_commit"` // with "-dirty" if the tree had changes
	// Disturbed lists why this run's numbers can't be trusted (empty when
	// clean); Failed says why it produced none.
	Disturbed []string `json:"disturbed"`
	Failed    string   `json:"failed,omitempty"`
}

// Commands are the exact command lines, as argv, and the environment
// variables benchrun set. The *Start fields are the equivalent cmd.exe
// lines with `start /affinity`, for reproducing one run by hand.
type Commands struct {
	Benchrun     []string `json:"benchrun"`
	Engine       []string `json:"engine,omitempty"`
	Loadgen      []string `json:"loadgen"` // or clockprobe
	Env          []string `json:"env"`
	EngineStart  string   `json:"engine_start,omitempty"`
	LoadgenStart string   `json:"loadgen_start"`
}

// Pinning is the affinity requested and what the OS reports back.
type Pinning struct {
	EngineMask      uint64 `json:"engine_mask"`
	EngineReadBack  uint64 `json:"engine_read_back"`
	LoadgenMask     uint64 `json:"loadgen_mask"`
	LoadgenReadBack uint64 `json:"loadgen_read_back"`
	// SharedCores lists physical cores with logical CPUs in both masks
	// (from the hardware probe); empty means none shared.
	SharedCores []int `json:"shared_cores"`
}

// Latency summarises a LogSnapshot. Quantiles and the maximum are bucket
// upper bounds: at most 3.125% above the true value, never below it.
type Latency struct {
	Count  uint64  `json:"count"`
	P50NS  int64   `json:"p50_ns"`
	P99NS  int64   `json:"p99_ns"`
	P999NS int64   `json:"p999_ns"`
	MaxNS  int64   `json:"max_ns"`
	MeanNS float64 `json:"mean_ns"` // exact
}

// Summarise computes a Latency.
func Summarise(s metrics.LogSnapshot) Latency {
	return Latency{Count: s.Count(), P50NS: s.QuantileNS(0.5), P99NS: s.QuantileNS(0.99),
		P999NS: s.QuantileNS(0.999), MaxNS: s.MaxNS(), MeanNS: s.MeanNS()}
}

// Load is what loadgen measured over its window.
type Load struct {
	Mode       string `json:"mode"` // closed (a fixed window in flight) or open (a fixed rate)
	Mix        string `json:"mix"`
	Conns      int    `json:"conns"`
	Window     int    `json:"window,omitempty"`       // closed: commands in flight per connection
	RatePerSec int    `json:"rate_per_sec,omitempty"` // open: commands per second, all connections
	Seed       uint64 `json:"seed"`
	Depth      int    `json:"depth"` // resting orders the generator holds the book near

	WarmupNS          int64 `json:"warmup_ns"`   // actual
	MeasuredNS        int64 `json:"measured_ns"` // actual
	RequestedWarmupNS int64 `json:"requested_warmup_ns"`
	RequestedNS       int64 `json:"requested_ns"`

	// Commands sent in the window, by what the generator chose, and their
	// first answers.
	Sent      Counts `json:"sent"`
	Completed uint64 `json:"completed"` // first answers received in the window
	// ThroughputPerSec is Completed over the window: sustained commands
	// (orders and cancels) answered per second.
	ThroughputPerSec float64 `json:"throughput_per_sec"`
	Answers          Answers `json:"answers"`

	// E2E: client send to client receipt of the command's first answer
	// (Ack, Reject or CancelAck), over loopback TCP. Open loop measures
	// from the intended send time, so a late send counts against it.
	E2E     Latency             `json:"e2e"`
	E2EHist metrics.LogSnapshot `json:"e2e_hist"`
	// SendLag (open loop): how late each command was written relative to
	// its schedule, i.e. send-side queueing in the load generator.
	SendLag      *Latency `json:"send_lag,omitempty"`
	Writes       uint64   `json:"writes"`         // write calls in the window
	WindowFullNS int64    `json:"window_full_ns"` // closed: time writers waited for a free slot, summed over connections

	// Core: the engine's matching-core latency (PRD 7.1a) over the window,
	// from GET /debug/latency.
	Core     Latency             `json:"core"`
	CoreHist metrics.LogSnapshot `json:"core_hist"`
	Engine   EngineCounters      `json:"engine"`

	// CPU over the window, in cores (1.0 = one logical CPU busy for the
	// whole window). Other is everything else on the machine.
	CPU CPU `json:"cpu"`

	// BookOrders: resting orders on the instrument at the window's start
	// and end (GET /instruments), so a growing or draining book is visible.
	BookOrdersStart int64 `json:"book_orders_start"`
	BookOrdersEnd   int64 `json:"book_orders_end"`

	Errors []string `json:"errors"`
}

// Counts are commands sent, by generator choice.
type Counts struct {
	Passive  uint64 `json:"passive"`  // limit GTC away from mid: rests
	Crossing uint64 `json:"crossing"` // limit IOC through mid: trades, never rests
	Cancel   uint64 `json:"cancel"`   // cancel of one of the connection's resting orders
	// CapCancel: cancels sent in place of a passive order because the
	// connection already had LiveCap resting orders (should be ~0; if not,
	// the mix was distorted and the run is flagged).
	CapCancel uint64 `json:"cap_cancel"`
	// NoTarget: a cancel was due but the connection had nothing resting, so
	// a passive order was sent instead.
	NoTarget uint64 `json:"no_target"`
}

// Total is every command sent.
func (c Counts) Total() uint64 { return c.Passive + c.Crossing + c.Cancel + c.CapCancel }

// Answers are what came back in the window.
type Answers struct {
	Acks        uint64            `json:"acks"`
	Rejects     map[string]uint64 `json:"rejects"`     // by reason
	CancelAcks  uint64            `json:"cancel_acks"` // requested cancels confirmed
	IOCUnfilled uint64            `json:"ioc_unfilled"`
	Fills       uint64            `json:"fills"` // fill messages, taker and maker
}

// EngineCounters are /metrics deltas over the window, plus the clock the
// engine timed itself with.
type EngineCounters struct {
	Commands      float64 `json:"commands"`
	Trades        float64 `json:"trades"`
	Fsyncs        float64 `json:"fsyncs"`
	FsyncMeanNS   float64 `json:"fsync_mean_ns"`
	WALBytes      float64 `json:"wal_bytes"`
	Snapshots     float64 `json:"snapshots"`
	SlowConsumers float64 `json:"slow_consumers"`
	// FsyncStalls: fsyncs over the engine's stall threshold (25 ms), and
	// FsyncMaxNS the upper bound of the slowest fsync's /metrics bucket.
	// Recorded since review F-013; zero in older result files.
	FsyncStalls     float64 `json:"fsync_stalls"`
	FsyncMaxNS      float64 `json:"fsync_max_ns"`
	ClockSource     string  `json:"clock_source"`
	ClockReadCostNS float64 `json:"clock_read_cost_ns"`
}

// CPU is CPU use over the window, in cores.
type CPU struct {
	Engine  float64 `json:"engine"`
	Loadgen float64 `json:"loadgen"`
	// Other: every other process (from the OS's process list), the
	// "nothing else running" check. TopOther names the largest.
	Other    float64   `json:"other"`
	TopOther []ProcUse `json:"top_other"`
	// Interrupts: interrupt and DPC time, which belongs to no process
	// (much of it is the loopback network stack carrying the benchmark's
	// own traffic), so it is neither the engine's nor "other".
	Interrupts float64 `json:"interrupts"`
	// MachineBusy: all non-idle time on the machine.
	MachineBusy float64 `json:"machine_busy"`
	Logical     int     `json:"logical"` // logical CPUs on the machine
}

// ProcUse is one process's CPU use over the window, in cores.
type ProcUse struct {
	PID  int     `json:"pid"`
	Name string  `json:"name"`
	CPUs float64 `json:"cpus"`
}

// Clock is a clock-overhead measurement (PRD 7.1): back-to-back reads of
// internal/clock, the engine's hot-path clock.
type Clock struct {
	Source     string  `json:"source"`
	TSCHz      float64 `json:"tsc_hz"`
	Reads      int     `json:"reads"`
	CostNS     float64 `json:"cost_ns"`       // mean time per read over the whole loop
	Resolution int64   `json:"resolution_ns"` // smallest non-zero step seen
	Zero       uint64  `json:"zero_steps"`    // back-to-back reads that returned the same value
	// Steps: the distribution of the difference between consecutive
	// reads (what a timed span of zero work measures).
	Steps     Latency             `json:"steps"`
	StepsHist metrics.LogSnapshot `json:"steps_hist"`
	// InitCostNS: the cost clock.Init measured at startup, as the engine
	// logs it and exports it in /metrics.
	InitCostNS int64 `json:"init_cost_ns"`
	// Other: other processes' CPU use (in logical CPUs) while the probe
	// ran, the same "nothing else running" check as load runs (review
	// F-014); TopOther names the largest. Missing (0, nil) in older files.
	Other    float64   `json:"other_cpus"`
	TopOther []ProcUse `json:"top_other"`
}

// Write writes v as indented JSON.
func Write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Encode writes v to w as indented JSON.
func Encode(w io.Writer, v any) error {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

// Read reads a result file, checking its schema.
func Read(path string) (*Run, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if r.Schema != Schema {
		return nil, fmt.Errorf("%s: schema %d, want %d", path, r.Schema, Schema)
	}
	return &r, nil
}
