// Package server assembles the running system:
//
//	gateway readers --cmds--> sequencer --sequenced--> engine --events--> publisher --> session writers
//	                              |                                         ^
//	                              +--> WAL (disk) --> durable seq ----------+  (events wait for it)
//
// cmd/engine and the end-to-end tests both start it through Start, so the
// tests exercise exactly the wiring that ships.
//
// Beside the TCP gateway there is a small HTTP listener (decision 018):
// /metrics, /healthz, GET/POST /instruments, and GET /debug/latency. Nothing on it touches the
// order path: admin commands go through the sequencer like any other, and
// reads are answered from the publisher's view.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/clock"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/metrics"
	"github.com/tanxena/matchd/internal/publisher"
	"github.com/tanxena/matchd/internal/sequencer"
	"github.com/tanxena/matchd/internal/snapshot"
	"github.com/tanxena/matchd/internal/wal"
)

// Config configures a server.
type Config struct {
	// DataDir holds the WAL, snapshots, manifest and fill log. Required.
	DataDir string
	// Snapshots: after SnapshotEvery commands or every SnapshotInterval,
	// whichever comes first (decision 013). 0 disables that trigger; both 0
	// disables snapshots.
	SnapshotEvery    uint64
	SnapshotInterval time.Duration
	// MDRing is each instrument's market-data resend window, in events.
	// 0 = publisher.DefaultRingSize (65,536; decision 016).
	MDRing int
	// Fsync is the durability policy. Zero value = always.
	Fsync sequencer.Policy
	// WAL options (segment size; tests inject file wrappers here).
	WAL wal.Options

	Gateway     gateway.Config
	Instruments []book.Instrument // ensured, in order, at startup
	Clock       sequencer.Clock   // nil = wall clock
	Log         *slog.Logger      // nil = discard
	// Channel capacities between the stages. 0 = defaults.
	CommandQueue int
	EventQueue   int

	// HTTPAddr is the side HTTP listener for /metrics, /healthz and
	// /instruments, e.g. "127.0.0.1:9100" (decision 018). "" = none.
	HTTPAddr string
	// ShutdownTimeout bounds how long Close lets clients receive what is
	// already queued for them (decision 019). 0 = 5 s.
	ShutdownTimeout time.Duration
	// AdminTimeout bounds how long AddInstrument (POST /instruments) waits
	// for its command to be durable and answered. 0 = 5 s.
	AdminTimeout time.Duration

	// beforeShutdownSnapshot, if set, runs after the final fsync and just
	// before the shutdown snapshot is written. Tests use it to capture the
	// data directory as a crash at that moment would leave it.
	beforeShutdownSnapshot func()
	// noShutdownSnapshot: tests that need an exact number of snapshots.
	noShutdownSnapshot bool
}

// Server states, for /healthz.
const (
	stateRunning int32 = iota
	stateStopping
)

// Server is a running engine.
type Server struct {
	gw      *gateway.Gateway
	pub     *publisher.Publisher
	eng     *engine.Engine
	wal     *wal.Log
	fills   *snapshot.FillLog
	snaps   *snapshot.Writer
	durable *durable.Seq
	cmds    chan engine.Command
	done    chan struct{} // closed when the publisher goroutine has finished
	log     *slog.Logger

	seqErr    error                 // the sequencer's result; written before done is closed
	snapDone  chan struct{}         // closed when the snapshot writer has finished
	snapBack  chan *engine.Snapshot // writer -> engine; Close thaws what the engine didn't
	ticker    *time.Ticker          // snapshot time trigger (nil if disabled)
	closeOnce sync.Once
	closeErr  error

	cfg       Config
	reg       *metrics.Registry
	latency   *metrics.Histogram    // the engine's per-command latency
	fine      *metrics.LogHistogram // the same span, precise to 3.1%, for GET /debug/latency
	httpSrv   *http.Server
	httpLn    net.Listener
	admin     *admin
	state     atomic.Int32
	recovered snapshot.Recovered
	submitMu  sync.RWMutex // guards stopping against submit
	stopping  bool         // no more admin commands (Close has begun)
	clockInfo clock.Info
}

// Start recovers state (latest snapshot, then the WAL after it), ensures
// the configured instruments exist, starts the pipeline, and returns once
// the server is accepting connections.
func Start(cfg Config) (*Server, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("server: DataDir is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Clock == nil {
		cfg.Clock = sequencer.WallClock
	}
	if cfg.Fsync.Mode == 0 {
		cfg.Fsync, _ = sequencer.ParsePolicy("always")
	}
	if cfg.CommandQueue <= 0 {
		cfg.CommandQueue = 4096
	}
	if cfg.EventQueue <= 0 {
		cfg.EventQueue = 65536
	}
	if cfg.WAL.Log == nil {
		cfg.WAL.Log = cfg.Log
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	if cfg.AdminTimeout <= 0 {
		cfg.AdminTimeout = 5 * time.Second
	}
	// The hot-path clock (decision 017): calibrated before anything is timed
	// or stamped.
	ci := clock.Init()
	cfg.Log.Info("clock", "source", ci.Source, "read_cost", ci.ReadCost, "resolution", ci.Resolution,
		"tsc_ghz", fmt.Sprintf("%.4f", ci.TSCHz/1e9))
	reg := metrics.NewRegistry()
	fsyncLatency := reg.Histogram("matchd_wal_fsync_seconds", "Duration of each WAL fsync.", metrics.FsyncBuckets)
	cfg.WAL.FsyncLatency = fsyncLatency

	// Recovery (PRD 5.4): manifest -> latest snapshot -> replay the WAL
	// records after it. The engine is deterministic, so this rebuilds
	// exactly the state it had. Replayed events are discarded: they were
	// delivered (or not) before the restart.
	eng, wl, rec, err := snapshot.Recover(cfg.DataDir, cfg.WAL, cfg.Log)
	if err != nil {
		return nil, fmt.Errorf("server: recovering %s: %w", cfg.DataDir, err)
	}
	cfg.Log.Info("recovered", "snapshot_seq", rec.SnapshotSeq, "wal_replayed", rec.Replayed,
		"wal_segments", rec.WAL.Segments, "last_seq", eng.LastSeq(), "torn_bytes", rec.WAL.TornBytes,
		"last_order_id", eng.LastOrderID(), "last_match_id", eng.LastMatchID())
	fills, err := snapshot.OpenFillLog(cfg.DataDir, cfg.Log)
	if err != nil {
		_ = wl.Close()
		return nil, err
	}
	if fills.LastMatchID() > eng.LastMatchID() {
		// The fill log may lag the engine (unreleased matches), never lead
		// it: a logged fill whose command isn't in the WAL was released
		// before it was durable.
		_ = wl.Close()
		_ = fills.Close()
		return nil, fmt.Errorf("server: fill log reaches match %d but the recovered engine only reaches %d",
			fills.LastMatchID(), eng.LastMatchID())
	}

	d := durable.New(uint64(eng.LastSeq()))
	seq := sequencer.New(cfg.Clock, wl, cfg.Fsync, d)
	fail := func(err error) (*Server, error) {
		_ = wl.Close()
		_ = fills.Close()
		return nil, err
	}
	var evs []engine.Event

	// Configured instruments: present already (from the log) and identical,
	// or added now through the log like any other command.
	for _, in := range cfg.Instruments {
		if id, ok := eng.Lookup(in.Symbol); ok {
			if have := eng.Book(id).Instrument(); have != in {
				return fail(fmt.Errorf("instrument %q: log has %+v, config says %+v", in.Symbol, have, in))
			}
			continue
		}
		c := engine.Command{Kind: engine.CmdAddInstrument, Spec: in}
		if err := seq.LogNow(&c); err != nil {
			return fail(err)
		}
		evs = eng.Step(&c, evs[:0])
		if len(evs) != 1 || evs[0].Kind != engine.EvInstrumentAdded {
			reason := "no event"
			if len(evs) > 0 {
				reason = evs[0].Reason.String()
			}
			return fail(fmt.Errorf("instrument %q rejected: %s", in.Symbol, reason))
		}
		cfg.Log.Info("instrument registered", "symbol", in.Symbol, "id", evs[0].Instrument,
			"tick", in.TickSize, "lot", in.LotSize)
	}

	s := &Server{
		pub:       publisher.New(cfg.Log, d, fills),
		eng:       eng,
		wal:       wl,
		fills:     fills,
		durable:   d,
		cmds:      make(chan engine.Command, cfg.CommandQueue), // gateway readers and admin -> sequencer
		done:      make(chan struct{}),
		log:       cfg.Log,
		cfg:       cfg,
		reg:       reg,
		admin:     newAdmin(),
		recovered: rec,
		clockInfo: ci,
	}
	// Market data starts from the recovered books: each instrument's levels
	// and md_seq, so a subscriber after a restart sees exactly the recovered
	// book and numbering continues where it left off.
	s.pub.SetRingSize(cfg.MDRing)
	for i, is := range eng.State().Instruments {
		bids, asks := levelTotals(is.Bids), levelTotals(is.Asks)
		s.pub.InitMarketData(engine.InstrumentID(i+1), is.Spec, is.MDSeq, uint32(len(is.Bids)+len(is.Asks)), bids, asks)
	}
	// The admin endpoint's commands are answered like a client's: the
	// publisher routes them, once durable, to this reserved id.
	s.pub.Register(gateway.ReservedClientID, s.admin)

	sequenced := make(chan engine.Command, cfg.CommandQueue) // sequencer -> engine
	events := make(chan engine.Event, cfg.EventQueue)        // engine -> publisher

	s.gw = gateway.New(cfg.Gateway, s.cmds, s.pub, cfg.Log)
	if err := s.gw.Listen(); err != nil {
		return fail(err)
	}
	s.registerMetrics() // matchd_wal_fsync_seconds was registered above, before recovery opened the WAL
	if cfg.HTTPAddr != "" {
		if err := s.listenHTTP(cfg.HTTPAddr); err != nil {
			_ = s.gw.Close()
			return fail(err)
		}
	}
	cfg.Log.Info("fsync policy", "policy", cfg.Fsync.String())

	// The pipeline. Each stage closes its output when its input closes (or
	// when it fails), so everything shuts down in order.
	seqDone := make(chan struct{}) // closed after s.seqErr is set
	go func() {
		s.seqErr = seq.Run(s.cmds, sequenced)
		close(seqDone)
	}()
	// Snapshots (decision 013): the engine freezes itself between commands
	// and hands the frozen view to the writer goroutine, which writes it
	// while the engine carries on, then hands it back to be thawed.
	var hooks *engine.SnapshotHooks
	s.snapDone = make(chan struct{})
	if cfg.SnapshotEvery > 0 || cfg.SnapshotInterval > 0 {
		snapOut := make(chan *engine.Snapshot, 1)   // engine -> writer; at most one in flight
		s.snapBack = make(chan *engine.Snapshot, 1) // writer -> engine, for Thaw
		snapBack := s.snapBack
		hooks = &engine.SnapshotHooks{Every: cfg.SnapshotEvery, Out: snapOut, Done: snapBack}
		if cfg.SnapshotInterval > 0 {
			s.ticker = time.NewTicker(cfg.SnapshotInterval)
			hooks.Tick = s.ticker.C
		}
		s.snaps = &snapshot.Writer{DataDir: cfg.DataDir, WAL: wl, Fills: fills, Durable: d, Log: cfg.Log}
		go func() { s.snaps.Run(snapOut, snapBack); close(s.snapDone) }()
		go func() {
			eng.Run(sequenced, events, hooks)
			close(snapOut) // the engine has stopped: no more snapshots
		}()
	} else {
		close(s.snapDone)
		go eng.Run(sequenced, events, nil)
	}
	go func() { s.pub.Run(events); <-seqDone; close(s.done) }()
	go func() {
		if err := s.gw.Serve(); err != nil {
			cfg.Log.Error("gateway stopped", "err", err)
		}
	}()
	if s.httpSrv != nil {
		go func() {
			if err := s.httpSrv.Serve(s.httpLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				cfg.Log.Error("http listener stopped", "err", err)
			}
		}()
	}
	// If durability fails, nothing more can be acknowledged: disconnect
	// everyone now rather than let clients wait for answers that won't come.
	go func() {
		select {
		case <-d.Failed():
			cfg.Log.Error("durability failure: stopping", "err", d.Err())
			_ = s.gw.Close()
		case <-s.done:
		}
	}()
	return s, nil
}

// Addr returns the gateway's listening address.
func (s *Server) Addr() string { return s.gw.Addr().String() }

// Close shuts down gracefully (PRD 6, decision 019), in this order:
//
//  1. Stop accepting: /healthz turns 503; the HTTP listener stops taking
//     requests (in-flight ones finish); the gateway closes its listener and
//     stops every reader, so no further command can be submitted.
//  2. Drain: every command already accepted flows through the sequencer,
//     which writes and fsyncs it (the WAL is durable before anything else
//     happens), then the engine and the publisher, which releases every
//     event.
//  3. Clients receive what is queued for them, for up to ShutdownTimeout;
//     connections still being sent to after that are closed. Meanwhile the
//     shutdown snapshot is written, unless nothing changed since the last
//     one or snapshots are disabled. It only shortens the next start: a
//     crash while it is being written loses nothing, because step 2 already
//     made the WAL durable.
//  4. Close the files.
//
// It returns the durability error if one occurred.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.state.Store(stateStopping)
		var httpErr error
		if s.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
			if err := s.httpSrv.Shutdown(ctx); err != nil {
				_ = s.httpSrv.Close()
				httpErr = fmt.Errorf("http shutdown: %w", err)
			}
			cancel()
		}
		gwErr := s.gw.StopReading() // after this, no reader sends on cmds
		s.submitMu.Lock()
		s.stopping = true // ...and no admin request either
		s.submitMu.Unlock()
		close(s.cmds)
		<-s.done
		<-s.snapDone
		// A snapshot in flight when the engine stopped comes back after the
		// engine goroutine has exited; thaw it here (safe now: nothing else
		// touches the engine), or its books would stay frozen.
		if s.snapBack != nil {
			select {
			case snap := <-s.snapBack:
				s.eng.Thaw(snap)
			default:
			}
		}
		if s.ticker != nil {
			s.ticker.Stop()
		}

		snapDone := make(chan error, 1)
		go func() { snapDone <- s.shutdownSnapshot() }()
		if cut := s.gw.FinishWriters(s.cfg.ShutdownTimeout); cut > 0 {
			s.log.Warn("shutdown: connections closed before receiving everything", "connections", cut)
		}
		gwErr = errors.Join(gwErr, s.gw.Close())
		snapErr := <-snapDone

		walErr := s.wal.Close()
		fillErr := s.fills.Close()
		s.closeErr = errors.Join(s.seqErr, httpErr, gwErr, snapErr, walErr, fillErr)
		s.log.Info("server stopped", "last_seq", s.eng.LastSeq(), "durable_seq", s.durable.Load(), "err", s.closeErr)
	})
	return s.closeErr
}

// shutdownSnapshot writes a final snapshot of the stopped engine, so the
// next start replays (almost) no WAL. It is skipped when snapshots are
// disabled, when durability failed (the engine may then hold commands the
// WAL doesn't), or when nothing changed since the last snapshot. A failure
// is reported but loses nothing: the WAL is already durable.
func (s *Server) shutdownSnapshot() error {
	if s.snaps == nil || s.durable.Err() != nil || s.cfg.noShutdownSnapshot {
		return nil
	}
	seq := uint64(s.eng.LastSeq())
	if seq <= max(s.recovered.SnapshotSeq, s.snaps.Stats().LastSeq) {
		s.log.Info("shutdown snapshot skipped: nothing changed since the last one", "seq", seq)
		return nil
	}
	if s.cfg.beforeShutdownSnapshot != nil {
		s.cfg.beforeShutdownSnapshot()
	}
	// The engine goroutine has exited, so this goroutine may freeze it.
	snap := s.eng.Freeze()
	err := s.snaps.WriteNow(snap)
	s.eng.Thaw(snap)
	if err != nil {
		return fmt.Errorf("shutdown snapshot: %w", err)
	}
	return nil
}

// submit hands an admin command to the sequencer. It returns false once
// Close has begun or durability has failed.
func (s *Server) submit(c engine.Command) bool {
	s.submitMu.RLock()
	defer s.submitMu.RUnlock()
	if s.stopping {
		return false
	}
	select {
	case s.cmds <- c:
		return true
	case <-s.durable.Failed():
		return false
	}
}

// HTTPAddr returns the HTTP listener's address, or "" if there is none.
func (s *Server) HTTPAddr() string {
	if s.httpLn == nil {
		return ""
	}
	return s.httpLn.Addr().String()
}

// Metrics returns the registry /metrics renders.
func (s *Server) Metrics() *metrics.Registry { return s.reg }

// Snapshots returns the snapshot writer's counters (zero if disabled).
func (s *Server) Snapshots() snapshot.Stats {
	if s.snaps == nil {
		return snapshot.Stats{}
	}
	return s.snaps.Stats()
}

// Failed is closed if durability fails; Err then says why.
func (s *Server) Failed() <-chan struct{} { return s.durable.Failed() }

// Err returns the durability failure, if any.
func (s *Server) Err() error { return s.durable.Err() }

// Gateway returns the gateway (for its counters).
func (s *Server) Gateway() *gateway.Gateway { return s.gw }

// Publisher returns the publisher (for its counters).
func (s *Server) Publisher() *publisher.Publisher { return s.pub }

// Engine returns the engine. It belongs to the engine goroutine while the
// server runs: only use it after Close has returned.
func (s *Server) Engine() *engine.Engine { return s.eng }

// levelTotals sums resting quantity per price.
func levelTotals(orders []book.RestingOrder) map[book.Price]book.Qty {
	m := make(map[book.Price]book.Qty)
	for _, r := range orders {
		m[r.Price] += r.Qty
	}
	return m
}
