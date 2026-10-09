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
	"path/filepath"
	"sync"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/publisher"
	"github.com/tanxena/matchd/internal/sequencer"
	"github.com/tanxena/matchd/internal/wal"
)

// Config configures a server.
type Config struct {
	// DataDir holds the WAL (DataDir/wal). Required.
	DataDir string
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
	durable *durable.Seq
	cmds    chan engine.Command
	done    chan struct{} // closed when the publisher goroutine has finished
	log     *slog.Logger

	seqErr    error // the sequencer's result; written before done is closed
	closeOnce sync.Once
	closeErr  error
}

// Start recovers state from the WAL, ensures the configured instruments
// exist, starts the pipeline, and returns once the server is accepting
// connections.
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

	// Recovery: replay every logged command into a fresh engine. The engine
	// is deterministic, so this rebuilds exactly the state it had. Events
	// are discarded: they were delivered (or not) before the restart.
	eng := engine.New()
	var cmd engine.Command
	var evs []engine.Event
	replay := func(r wal.Record) error {
		if err := engine.DecodeCommand(r.Payload, &cmd); err != nil {
			return err
		}
		cmd.Seq, cmd.Time = engine.Seq(r.Seq), engine.Timestamp(r.Time)
		if cmd.Seq != eng.LastSeq()+1 {
			// No snapshots yet (milestone 5), so the log must start at 1.
			return fmt.Errorf("log starts at seq %d but there is no snapshot to start from", cmd.Seq)
		}
		evs = eng.Step(&cmd, evs[:0])
		return nil
	}
	walDir := filepath.Join(cfg.DataDir, "wal")
	wl, info, err := wal.Open(walDir, cfg.WAL, replay)
	if err != nil {
		return nil, fmt.Errorf("server: recovering %s: %w", walDir, err)
	}
	cfg.Log.Info("recovered from WAL", "dir", walDir, "records", info.Records, "segments", info.Segments,
		"last_seq", info.LastSeq, "torn_bytes", info.TornBytes, "last_order_id", eng.LastOrderID())

	d := durable.New(info.LastSeq)
	seq := sequencer.New(cfg.Clock, wl, cfg.Fsync, d)
	fail := func(err error) (*Server, error) {
		_ = wl.Close()
		return nil, err
	}

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
		pub:     publisher.New(cfg.Log, d),
		eng:     eng,
		wal:     wl,
		durable: d,
		cmds:    make(chan engine.Command, cfg.CommandQueue), // gateway readers -> sequencer
		done:    make(chan struct{}),
		log:     cfg.Log,
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
	go eng.Run(sequenced, events)
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
// publisher before returning. (Snapshot-on-shutdown is milestone 7.) It
// returns the durability error if one occurred.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		gwErr := s.gw.Close() // after this, nothing sends on cmds
		close(s.cmds)
		<-s.done
		walErr := s.wal.Close()
		s.closeErr = errors.Join(s.seqErr, gwErr, walErr)
		s.log.Info("server stopped", "last_seq", s.eng.LastSeq(), "durable_seq", s.durable.Load(), "err", s.closeErr)
	})
	return s.closeErr
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
