// Command engine runs the matching engine server.
//
//	engine -data ./data -fsync always -listen 127.0.0.1:9000 -instrument XYZ:1:1 -instrument ABC:5:100
//
// Each -instrument is SYMBOL:TICK:LOT. On first start they are registered in
// the order given, so the first gets instrument id 1; on later starts they
// must match what the write-ahead log already holds. (A JSON config and the
// HTTP admin endpoint arrive in milestone 7.)
//
// -fsync is always (default), group:<duration> (e.g. group:1ms) or never
// (tests only: acknowledged work can be lost on power failure).
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

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/gateway"
	"github.com/tanxena/matchd/internal/sequencer"
	"github.com/tanxena/matchd/internal/server"
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
	flag.IntVar(&gw.OutQueue, "out-queue", gw.OutQueue, "per-client outbound queue (events); a client that fills it is disconnected")
	flag.DurationVar(&gw.IdleTimeout, "idle-timeout", gw.IdleTimeout, "disconnect clients silent this long (0 = never)")
	flag.DurationVar(&gw.WriteTimeout, "write-timeout", gw.WriteTimeout, "disconnect clients whose socket won't accept data this long (0 = never)")
	flag.Var(&instruments, "instrument", "SYMBOL:TICK:LOT to register (repeatable; ids from 1 in order)")
	dataDir := flag.String("data", "./data", "data directory (the WAL lives in DATA/wal)")
	fsync := flag.String("fsync", "always", "fsync policy: always, group:<duration> (e.g. group:1ms), or never (tests only)")
	debug := flag.Bool("debug", false, "debug logging")
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
	srv, err := server.Start(server.Config{DataDir: *dataDir, Fsync: policy, Gateway: gw, Instruments: instruments, Log: log})
	if err != nil {
		log.Error("start failed", "err", err)
		os.Exit(1)
	}

	// Run until Ctrl-C (SIGINT), SIGTERM, or a durability failure.
	sig := make(chan os.Signal, 1) // signal.Notify needs a buffered channel
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case <-srv.Failed():
		log.Error("durability failure; shutting down without acknowledging further work", "err", srv.Err())
	}
	if err := srv.Close(); err != nil {
		log.Error("shutdown", "err", err)
		os.Exit(1)
	}
}
