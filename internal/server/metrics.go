package server

import (
	"math"
	"time"

	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/metrics"
	"github.com/tanxena/matchd/internal/publisher"
)

// registerMetrics creates every /metrics series (PRD 5.8, decision 017) and
// wires the ones recorded on hot paths into their components. Counts a
// component already keeps (gateway, publisher, snapshot writer, WAL) are
// read at scrape time rather than duplicated.
func (s *Server) registerMetrics() {
	r := s.reg

	// Matching latency: recorded by the engine goroutine, per command, from
	// taking it off the channel to sending its last event.
	s.latency = r.Histogram("matchd_matching_latency_seconds",
		"Time for one command in the engine: from receiving it to sending its last event.",
		metrics.MatchingBuckets)
	step := r.Histogram("matchd_matching_step_seconds",
		"Time for the matching step alone (engine.Step), without handing the events on.",
		metrics.MatchingBuckets)
	s.eng.SetLatency(s.latency, step)
	// The same span in a log-linear histogram, served as JSON at
	// GET /debug/latency rather than in /metrics: the benchmark reads its
	// quantiles and maximum to within 3.1% (decision 021).
	s.fine = metrics.NewLogHistogram()
	s.eng.SetFineLatency(s.fine)

	// Commands and fills: counted by the publisher on release, so only
	// durable commands are ever counted.
	pm := &publisher.Metrics{
		Fills:  r.Counter("matchd_fills_total", "Fill events released, one per party per match."),
		Trades: r.Counter("matchd_trades_total", "Matches executed."),
	}
	const help = "Commands executed, by type and outcome (counted once durable)."
	for kind, name := range map[engine.CommandKind]string{
		engine.CmdNewOrder: "new_order", engine.CmdCancel: "cancel", engine.CmdAddInstrument: "add_instrument",
	} {
		pm.Commands[kind][0] = r.Counter("matchd_commands_total", help, "type", name, "outcome", "accepted")
		pm.Commands[kind][1] = r.Counter("matchd_commands_total", help, "type", name, "outcome", "rejected")
	}
	s.pub.SetMetrics(pm)

	// WAL (matchd_wal_fsync_seconds is registered in Start, before recovery
	// opens the WAL).
	r.GaugeFunc("matchd_wal_fsync_in_progress_seconds",
		"How long the fsync now running has taken so far; 0 when none is. A stuck disk shows here before the fsync returns.",
		func() float64 { return s.wal.SyncInProgress().Seconds() })
	r.CounterFunc("matchd_wal_written_bytes_total", "Bytes written to WAL segments since start.", s.wal.Written)
	r.GaugeFunc("matchd_durable_seq", "Highest command seq known to be on stable storage.",
		func() float64 { return float64(s.durable.Load()) })

	// Snapshots.
	r.GaugeFunc("matchd_snapshot_age_seconds",
		"Seconds since the newest snapshot was written; +Inf if there is none.", s.snapshotAge)
	r.CounterFunc("matchd_snapshots_total", "Snapshots written since start.", func() uint64 { return s.Snapshots().Written }, "result", "written")
	r.CounterFunc("matchd_snapshots_total", "Snapshots written since start.", func() uint64 { return s.Snapshots().Failed }, "result", "failed")

	// Connections and publishing.
	r.GaugeFunc("matchd_connections", "Client connections open now.", func() float64 { return float64(s.gw.Stats().Active) })
	r.CounterFunc("matchd_connections_accepted_total", "Client connections accepted since start.", func() uint64 { return s.gw.Stats().Accepted })
	r.CounterFunc("matchd_slow_consumer_disconnects_total",
		"Sessions disconnected as slow consumers: outbound queue over its soft limit for longer than the write timeout, at its hard limit, or a socket write blocked longer than the write timeout (decision 022).",
		func() uint64 { return s.pub.Stats().SlowConsumers + s.gw.Stats().SlowWrites })
	r.GaugeFunc("matchd_events_held", "Events waiting for their command's fsync.", func() float64 { return float64(s.pub.Stats().Held) })

	// Market data.
	r.CounterFunc("matchd_md_subscriptions_total", "Market-data subscriptions served (each sends a snapshot).", func() uint64 { return s.pub.Stats().Subscriptions })
	r.CounterFunc("matchd_md_resends_total", "Market-data Resend requests received (gap recoveries).", func() uint64 { return s.pub.Stats().Resends })
	r.CounterFunc("matchd_md_resend_unavailable_total", "Resend requests older than the ring buffer (client must re-snapshot).", func() uint64 { return s.pub.Stats().ResendMisses })

	// The clock everything above was timed with (decision 017).
	r.GaugeFunc("matchd_clock_info", "The hot-path clock's source (value always 1).", func() float64 { return 1 }, "source", s.clockInfo.Source)
	r.GaugeFunc("matchd_clock_read_cost_seconds", "Measured cost of one hot-path clock read.", func() float64 { return s.clockInfo.ReadCost.Seconds() })
}

// snapshotAge is the time since the newest snapshot (written by this
// process, or the one recovery started from), or +Inf if none exists:
// "never" should trip an age alert, not hide from it.
func (s *Server) snapshotAge() float64 {
	at := s.Snapshots().LastAt
	if at.IsZero() {
		at = s.recovered.SnapshotAt
	}
	if at.IsZero() {
		return math.Inf(1)
	}
	return time.Since(at).Seconds()
}
