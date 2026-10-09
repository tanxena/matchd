// Package gateway accepts TCP connections, turns client frames into engine
// commands, and writes the engine's events back to the right connection
// (decision 009).
//
// Per connection there are exactly two goroutines:
//
//	reader: socket -> decode -> Login handling, or a command on the shared
//	        command channel (the single ordered input to the sequencer)
//	writer: the session's outbound queue (fed by the publisher) and the
//	        reader's control replies -> encode -> socket
//
// The writer is the only goroutine that writes to the socket, so frames never
// interleave.
package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/publisher"
)

// Config holds the gateway's tunables.
type Config struct {
	// Addr is the TCP listen address, e.g. "127.0.0.1:9000" (":0" picks a
	// free port).
	Addr string
	// OutQueue is each session's outbound queue length in events. A client
	// whose queue fills up is disconnected as a slow consumer.
	OutQueue int
	// IdleTimeout disconnects a client that sends nothing (not even a
	// Heartbeat) for this long. 0 disables it.
	IdleTimeout time.Duration
	// WriteTimeout bounds a single flush to the socket. A client that stops
	// reading eventually makes a flush block; after this long the session
	// is closed. 0 disables it.
	WriteTimeout time.Duration
	// SocketWriteBuffer, if > 0, sets the kernel send buffer (SO_SNDBUF) of
	// accepted connections. Only tests need it: a small buffer makes a
	// non-reading client fill its queue quickly.
	SocketWriteBuffer int
}

// DefaultConfig returns production defaults.
func DefaultConfig() Config {
	return Config{
		Addr:         "127.0.0.1:9000",
		OutQueue:     4096,
		IdleTimeout:  60 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
}

// Gateway is the TCP front end.
type Gateway struct {
	cfg  Config
	cmds chan<- engine.Command // shared by all readers; owned (closed) by the caller
	pub  *publisher.Publisher
	log  *slog.Logger

	ln     net.Listener
	closed atomic.Bool // set once by Close; tells Serve that Accept errors are expected

	// mu guards sessions, which Close uses to disconnect everyone. Sessions
	// add themselves when accepted and remove themselves when closed.
	mu       sync.Mutex
	sessions map[*session]struct{}
	// wg counts every reader and writer goroutine, so Close can wait until
	// none is left and the caller can then safely close cmds.
	wg sync.WaitGroup

	accepted atomic.Uint64 // counters for tests now, /metrics in milestone 7
	active   atomic.Int64
}

// New returns a gateway that will send commands on cmds and register
// sessions with pub. Call Listen, then Serve.
func New(cfg Config, cmds chan<- engine.Command, pub *publisher.Publisher, log *slog.Logger) *Gateway {
	if cfg.OutQueue <= 0 {
		cfg.OutQueue = DefaultConfig().OutQueue
	}
	return &Gateway{cfg: cfg, cmds: cmds, pub: pub, log: log, sessions: make(map[*session]struct{})}
}

// Listen opens the listening socket.
func (g *Gateway) Listen() error {
	ln, err := net.Listen("tcp", g.cfg.Addr)
	if err != nil {
		return err
	}
	g.ln = ln
	g.log.Info("gateway listening", "addr", ln.Addr().String())
	return nil
}

// Addr returns the listening address (useful with ":0").
func (g *Gateway) Addr() net.Addr { return g.ln.Addr() }

// Serve accepts connections until Close is called. It returns nil after
// Close, or the error that stopped it.
func (g *Gateway) Serve() error {
	for {
		conn, err := g.ln.Accept()
		if err != nil {
			if g.closed.Load() {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return fmt.Errorf("gateway: accept: %w", err)
		}
		g.start(conn)
	}
}

func (g *Gateway) start(conn net.Conn) {
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true) // small frames must go out now, not after Nagle's delay
		if g.cfg.SocketWriteBuffer > 0 {
			_ = tc.SetWriteBuffer(g.cfg.SocketWriteBuffer)
		}
	}
	s := newSession(g, conn)
	g.mu.Lock()
	if g.closed.Load() { // lost a race with Close: don't start goroutines it won't wait for
		g.mu.Unlock()
		_ = conn.Close()
		return
	}
	g.sessions[s] = struct{}{}
	g.wg.Add(2)
	g.mu.Unlock()
	g.accepted.Add(1)
	g.active.Add(1)
	g.log.Info("connection accepted", "remote", s.remote)
	go s.readLoop()
	go s.writeLoop()
}

func (g *Gateway) forget(s *session) {
	g.mu.Lock()
	delete(g.sessions, s)
	g.mu.Unlock()
	g.active.Add(-1)
}

// Close stops accepting, disconnects every session, and waits for all
// connection goroutines to exit. After Close returns, no goroutine of the
// gateway will send on cmds, so the caller may close it.
//
// Close may be called more than once, and concurrently (the server calls it
// both on a durability failure and on shutdown). Only the first call closes
// the listener; every call waits for the goroutines.
func (g *Gateway) Close() error {
	g.mu.Lock()
	first := g.closed.CompareAndSwap(false, true)
	all := make([]*session, 0, len(g.sessions))
	for s := range g.sessions {
		all = append(all, s)
	}
	g.mu.Unlock()
	var err error
	if first && g.ln != nil {
		err = g.ln.Close()
	}
	for _, s := range all {
		s.close("server shutting down")
	}
	g.wg.Wait()
	return err
}

// Stats is a snapshot of the gateway's counters.
type Stats struct {
	Accepted uint64 // connections accepted since start
	Active   int64  // connections currently open
}

// Stats returns the current counters. Safe from any goroutine.
func (g *Gateway) Stats() Stats {
	return Stats{Accepted: g.accepted.Load(), Active: g.active.Load()}
}
