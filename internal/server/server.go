// Package server assembles the running system:
//
//	gateway readers --cmds--> sequencer --sequenced--> engine --events--> publisher --> session writers
//	                              |                                         ^
//	                              +--> WAL (disk) --> durable seq ----------+  (events wait for it)
//
// cmd/engine and the end-to-end tests both start it through Start, so the
// tests exercise exactly the wiring that ships.
package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/gateway"
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
}

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
		pub:     publisher.New(cfg.Log, d, fills),
		eng:     eng,
		wal:     wl,
		fills:   fills,
		durable: d,
		cmds:    make(chan engine.Command, cfg.CommandQueue), // gateway readers -> sequencer
		done:    make(chan struct{}),
		log:     cfg.Log,
	}
	// Market data starts from the recovered books: each instrument's levels
	// and md_seq, so a subscriber after a restart sees exactly the recovered
	// book and numbering continues where it left off.
	s.pub.SetRingSize(cfg.MDRing)
	for i, is := range eng.State().Instruments {
		bids, asks := levelTotals(is.Bids), levelTotals(is.Asks)
		s.pub.InitMarketData(engine.InstrumentID(i+1), is.MDSeq, bids, asks)
	}

	sequenced := make(chan engine.Command, cfg.CommandQueue) // sequencer -> engine
	events := make(chan engine.Event, cfg.EventQueue)        // engine -> publisher

	s.gw = gateway.New(cfg.Gateway, s.cmds, s.pub, cfg.Log)
	if err := s.gw.Listen(); err != nil {
		return fail(err)
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

// Close stops accepting and disconnects clients, then lets every command
// already accepted flow through the sequencer (final fsync), engine and
// publisher, waits for an in-progress snapshot, and closes the files.
// (Snapshot-on-shutdown is milestone 7.) It returns the durability error if
// one occurred.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		gwErr := s.gw.Close() // after this, nothing sends on cmds
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
		walErr := s.wal.Close()
		fillErr := s.fills.Close()
		s.closeErr = errors.Join(s.seqErr, gwErr, walErr, fillErr)
		s.log.Info("server stopped", "last_seq", s.eng.LastSeq(), "durable_seq", s.durable.Load(), "err", s.closeErr)
	})
	return s.closeErr
}

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
