// Package server assembles the running system:
//
//	gateway readers --cmds--> sequencer --sequenced--> engine --events--> publisher --> session writers
//
// cmd/engine and the end-to-end tests both start it through Start, so the
// tests exercise exactly the wiring that ships.
package server

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/publisher"
	"github.com/tanxena/matchd/internal/sequencer"
)

// Config configures a server.
type Config struct {
	Gateway     gateway.Config
	Instruments []book.Instrument // registered, in order, before accepting connections
	Clock       sequencer.Clock   // nil = wall clock
	Log         *slog.Logger      // nil = discard
	// Channel capacities between the stages. 0 = defaults.
	CommandQueue int
	EventQueue   int
}

// Server is a running engine.
type Server struct {
	gw   *gateway.Gateway
	pub  *publisher.Publisher
	eng  *engine.Engine
	cmds chan engine.Command
	done chan struct{} // closed when the publisher goroutine has drained everything
	log  *slog.Logger
}

// Start registers the instruments, starts the pipeline goroutines and the
// listener, and returns once the server is accepting connections.
func Start(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Clock == nil {
		cfg.Clock = sequencer.WallClock
	}
	if cfg.CommandQueue <= 0 {
		cfg.CommandQueue = 4096
	}
	if cfg.EventQueue <= 0 {
		cfg.EventQueue = 65536
	}

	seq := sequencer.New(cfg.Clock)
	eng := engine.New()

	// Instruments are ordinary sequenced commands. Applying them here,
	// synchronously and before any goroutine starts, lets startup fail
	// cleanly on a bad config instead of logging a reject nobody reads.
	var evs []engine.Event
	for _, in := range cfg.Instruments {
		c := engine.Command{Kind: engine.CmdAddInstrument, Spec: in}
		seq.Stamp(&c)
		evs = eng.Step(&c, evs[:0])
		if len(evs) != 1 || evs[0].Kind != engine.EvInstrumentAdded {
			reason := "no event"
			if len(evs) > 0 {
				reason = evs[0].Reason.String()
			}
			return nil, fmt.Errorf("instrument %q rejected: %s", in.Symbol, reason)
		}
		cfg.Log.Info("instrument registered", "symbol", in.Symbol, "id", evs[0].Instrument,
			"tick", in.TickSize, "lot", in.LotSize)
	}

	s := &Server{
		pub:  publisher.New(cfg.Log),
		eng:  eng,
		cmds: make(chan engine.Command, cfg.CommandQueue), // gateway readers -> sequencer
		done: make(chan struct{}),
		log:  cfg.Log,
	}
	sequenced := make(chan engine.Command, cfg.CommandQueue) // sequencer -> engine
	events := make(chan engine.Event, cfg.EventQueue)        // engine -> publisher

	s.gw = gateway.New(cfg.Gateway, s.cmds, s.pub, cfg.Log)
	if err := s.gw.Listen(); err != nil {
		return nil, err
	}

	// The pipeline. Each stage closes its output when its input closes, so
	// closing cmds (in Close) shuts everything down in order.
	go seq.Run(s.cmds, sequenced)
	go eng.Run(sequenced, events)
	go func() { s.pub.Run(events); close(s.done) }()
	go func() {
		if err := s.gw.Serve(); err != nil {
			cfg.Log.Error("gateway stopped", "err", err)
		}
	}()
	return s, nil
}

// Addr returns the gateway's listening address.
func (s *Server) Addr() string { return s.gw.Addr().String() }

// Close stops accepting and disconnects clients, then lets every command
// already accepted flow through the sequencer, engine and publisher before
// returning. (Full graceful shutdown with fsync and snapshot is milestone 7.)
func (s *Server) Close() error {
	err := s.gw.Close() // after this, nothing sends on cmds
	close(s.cmds)
	<-s.done
	s.log.Info("server stopped", "last_seq", s.eng.LastSeq())
	return err
}

// Gateway returns the gateway (for its counters).
func (s *Server) Gateway() *gateway.Gateway { return s.gw }

// Publisher returns the publisher (for its counters).
func (s *Server) Publisher() *publisher.Publisher { return s.pub }

// Engine returns the engine. It belongs to the engine goroutine while the
// server runs: only use it after Close has returned.
func (s *Server) Engine() *engine.Engine { return s.eng }
