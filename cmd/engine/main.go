// Command engine runs the matching engine server.
//
//	engine -data ./data -fsync always -listen 127.0.0.1:9000 -http 127.0.0.1:9100 -instrument XYZ:1:1
//
// Each -instrument is SYMBOL:TICK:LOT. On first start they are registered in
// the order given, so the first gets instrument id 1; on later starts they
// must match what the write-ahead log already holds. More can be added while
// running with POST /instruments (or engctl add-instrument).
//
// The HTTP listener (-http, "" to disable) serves /metrics, /healthz and
// /instruments (decision 018). It has no authentication: keep it on a
// local address.
//
// SIGINT/SIGTERM shut down gracefully (decision 019): stop accepting, drain
// and fsync, let clients receive their answers for up to -shutdown-timeout,
// snapshot, exit. A second signal exits at once; nothing acknowledged is
// lost, the next start just replays more of the WAL.
//
// -fsync is always (default), group:<duration> (e.g. group:1ms) or never
// (tests only: acknowledged work can be lost on power failure).
//
// Snapshots are taken every -snapshot-every commands or every
// -snapshot-interval, whichever comes first (0 disables a trigger).
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/sequencer"
	"github.com/tanxena/matchd/internal/server"
	"github.com/tanxena/matchd/internal/wal"
)

// instrumentFlags collects repeated -instrument values.
type instrumentFlags []book.Instrument

func (f *instrumentFlags) String() string { return fmt.Sprint(*f) }

func (f *instrumentFlags) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) != 3 {
		return errors.New("want SYMBOL:TICK:LOT")
	}
	tick, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return fmt.Errorf("tick: %w", err)
	}
	lot, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return fmt.Errorf("lot: %w", err)
	}
	in := book.Instrument{Symbol: parts[0], TickSize: tick, LotSize: lot}
	if err := in.Validate(); err != nil {
		return err
	}
	*f = append(*f, in)
	return nil
}

func main() {
	gw := gateway.DefaultConfig()
	var instruments instrumentFlags
	flag.StringVar(&gw.Addr, "listen", gw.Addr, "TCP address for client connections")
	flag.IntVar(&gw.OutQueue, "out-queue", gw.OutQueue, "per-client outbound queue soft limit (events); a client above it for longer than -write-timeout is disconnected")
	flag.DurationVar(&gw.IdleTimeout, "idle-timeout", gw.IdleTimeout, "disconnect clients silent this long (0 = never)")
	flag.DurationVar(&gw.WriteTimeout, "write-timeout", gw.WriteTimeout, "disconnect clients whose socket won't accept data this long (0 = never)")
	flag.Var(&instruments, "instrument", "SYMBOL:TICK:LOT to register (repeatable; ids from 1 in order)")
	dataDir := flag.String("data", "./data", "data directory (the WAL lives in DATA/wal)")
	fsync := flag.String("fsync", "always", "fsync policy: always, group:<duration> (e.g. group:1ms), or never (tests only)")
	snapEvery := flag.Uint64("snapshot-every", 1_000_000, "snapshot after this many commands (0 = no command trigger)")
	snapInterval := flag.Duration("snapshot-interval", 10*time.Second, "snapshot at least this often while commands arrive (0 = no time trigger)")
	fsyncStall := flag.Duration("fsync-stall", wal.DefaultStallThreshold, "an fsync slower than this counts as a durability stall (metric and warning)")
	segBytes := flag.Int64("wal-segment-bytes", 64<<20, "start a new WAL segment after this many bytes")
	httpAddr := flag.String("http", "127.0.0.1:9100", `HTTP address for /metrics, /healthz and /instruments ("" = off)`)
	shutdownTimeout := flag.Duration("shutdown-timeout", 5*time.Second, "on shutdown, how long clients get to receive what is queued for them")
	debug := flag.Bool("debug", false, "debug logging (one line per command)")
	flag.Parse()
	policy, err := sequencer.ParsePolicy(*fsync)
	if err != nil {
		fmt.Fprintln(os.Stderr, "engine:", err)
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if len(instruments) == 0 {
		log.Warn("no -instrument given; every order will be rejected with UnknownInstrument")
	}

	if policy.Mode == sequencer.Never {
		log.Warn("fsync=never: acknowledged orders can be lost on power failure; for tests only")
	}
	srv, err := server.Start(server.Config{
		DataDir: *dataDir, Fsync: policy, Gateway: gw, Instruments: instruments, Log: log,
		SnapshotEvery: *snapEvery, SnapshotInterval: *snapInterval,
		WAL:      wal.Options{SegmentSize: *segBytes, StallThreshold: *fsyncStall},
		HTTPAddr: *httpAddr, ShutdownTimeout: *shutdownTimeout,
	})
	if err != nil {
		log.Error("start failed", "err", err)
		os.Exit(1)
	}

	// Run until Ctrl-C (SIGINT), SIGTERM, or a durability failure.
	sig := make(chan os.Signal, 2) // signal.Notify needs a buffered channel
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String(), "timeout", *shutdownTimeout)
	case <-srv.Failed():
		log.Error("durability failure; shutting down without acknowledging further work", "err", srv.Err())
	}
	go func() {
		s := <-sig
		log.Warn("second signal: exiting now; the next start replays the WAL", "signal", s.String())
		os.Exit(1)
	}()
	if err := srv.Close(); err != nil {
		log.Error("shutdown", "err", err)
		os.Exit(1)
	}
}
