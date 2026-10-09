package server

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/sequencer"
	"github.com/tanxena/matchd/internal/wal"
)

// PRD 5.4 / 5.4(a): acks are gated on fsync. These tests put a wrapper
// around every WAL segment file that counts fsyncs and can hold them (block
// until released) or fail them, and watch what clients receive.

type gate struct {
	syncs   atomic.Int64
	fail    atomic.Bool
	mu      sync.Mutex    // guards release
	release chan struct{} // non-nil: Sync blocks until it is closed
}

func (g *gate) hold() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release = make(chan struct{})
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
}

type gateFile struct {
	wal.File
	g *gate
}

func (f gateFile) Sync() error {
	f.g.mu.Lock()
	r := f.g.release
	f.g.mu.Unlock()
	if r != nil {
		<-r
	}
	f.g.syncs.Add(1)
	if f.g.fail.Load() {
		return errors.New("injected fsync failure (EIO)")
	}
	return f.File.Sync()
}

func gatedWAL(g *gate) wal.Options {
	return wal.Options{OpenFile: func(path string) (wal.File, error) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		return gateFile{File: f, g: g}, err
	}}
}

func policy(t *testing.T, s string) sequencer.Policy {
	t.Helper()
	p, err := sequencer.ParsePolicy(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// noMessage asserts nothing arrives for d.
func noMessage(t *testing.T, c *client.Conn, d time.Duration, why string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(d))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	if m, err := c.Next(); err == nil {
		t.Fatalf("%s: client %d received %v", why, c.ClientID, m)
	}
}

func TestAckGatedOnFsync(t *testing.T) {
	for _, pol := range []string{"always", "group:1ms"} {
		t.Run(pol, func(t *testing.T) {
			g := &gate{}
			s := start(t, func(c *Config) { c.Fsync = policy(t, pol); c.WAL = gatedWAL(g) })
			cl := dial(t, s, 1)

			g.hold()
			_ = cl.NewOrder(limit(1, 1, proto.SideBuy, 100, 5))
			noMessage(t, cl, 300*time.Millisecond, "fsync held")
			if pol != "always" {
				// Group commit applies first: the engine has produced the
				// Ack, and the publisher is holding it.
				if held := s.Publisher().Stats().Held; held == 0 {
					t.Fatal("group: no events held while fsync is blocked")
				}
			}
			g.open()
			if m := recv(t, cl, 1)[0]; m.Type != proto.TypeAck {
				t.Fatalf("after fsync: %v", m)
			}
		})
	}
}

// The test PRD 5.4 asks for: a failed fsync must mean no ack and no fill
// for the affected batch, for either party.
func TestFsyncFailureEmitsNothing(t *testing.T) {
	for _, pol := range []string{"always", "group:1ms"} {
		t.Run(pol, func(t *testing.T) {
			g := &gate{}
			s := start(t, func(c *Config) { c.Fsync = policy(t, pol); c.WAL = gatedWAL(g) })
			maker, taker := dial(t, s, 1), dial(t, s, 2)

			_ = maker.NewOrder(limit(1, 1, proto.SideSell, 100, 5))
			if m := recv(t, maker, 1)[0]; m.Type != proto.TypeAck {
				t.Fatalf("maker: %v", m) // durable before the failure: acked
			}

			g.fail.Store(true)
			_ = taker.NewOrder(limit(1, 1, proto.SideBuy, 100, 5)) // would fill against the maker

			select {
			case <-s.Failed():
			case <-time.After(5 * time.Second):
				t.Fatal("server did not report the fsync failure")
			}
			// Both connections are closed by the server; before that, no
			// Ack for the taker and no Fill for either side may arrive.
			for _, c := range []*client.Conn{maker, taker} {
				_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
				for {
					m, err := c.Next()
					if err != nil {
						break // closed: good
					}
					t.Fatalf("client %d received %v after the batch's fsync failed", c.ClientID, m)
				}
			}
			if err := s.Close(); err == nil {
				t.Fatal("Close returned nil after a durability failure")
			}
			st := s.Publisher().Stats()
			t.Logf("%s: publisher withheld %d events", pol, st.Withheld)
			if pol != "always" && st.Withheld == 0 {
				t.Fatal("group: the engine applied the batch, so its events must have been withheld")
			}
			waitForLog(t, s, "durability failure")
		})
	}
}

// Fsync counts: under always, each sequential round trip costs an fsync;
// under group, a burst of pipelined orders shares a few.
func TestFsyncCounts(t *testing.T) {
	t.Run("always sequential", func(t *testing.T) {
		g := &gate{}
		s := start(t, func(c *Config) { c.Fsync = policy(t, "always"); c.WAL = gatedWAL(g) })
		cl := dial(t, s, 1)
		before := g.syncs.Load()
		for i := range 20 {
			_ = cl.NewOrder(limit(1, uint64(i+1), proto.SideBuy, 50, 1))
			recv(t, cl, 1)
		}
		if n := g.syncs.Load() - before; n != 20 {
			t.Fatalf("%d fsyncs for 20 sequential orders under always, want 20", n)
		}
	})
	t.Run("group burst", func(t *testing.T) {
		g := &gate{}
		s := start(t, func(c *Config) { c.Fsync = policy(t, "group:2ms"); c.WAL = gatedWAL(g) })
		cl := dial(t, s, 1)
		before := g.syncs.Load()
		for i := range 500 {
			_ = cl.NewOrder(limit(1, uint64(i+1), proto.SideBuy, 50, 1))
		}
		recv(t, cl, 500)
		n := g.syncs.Load() - before
		t.Logf("500 pipelined orders, %d fsyncs", n)
		if n >= 100 {
			t.Fatalf("%d fsyncs for a 500-order burst under group commit", n)
		}
	})
}

// snapshotState captures everything replay must reproduce.
func snapshotState(e *engine.Engine) string {
	out := fmt.Sprintf("last_seq=%d last_order=%d instruments=%d\n", e.LastSeq(), e.LastOrderID(), e.NumInstruments())
	for id := engine.InstrumentID(1); int(id) <= e.NumInstruments(); id++ {
		b := e.Book(id)
		out += fmt.Sprintf("%s bids=%v asks=%v\n", b.Instrument().Symbol, b.Orders(book.Buy, nil), b.Orders(book.Sell, nil))
	}
	return out
}

func TestRestartReplaysToIdenticalState(t *testing.T) {
	dir := t.TempDir()
	cfg := func(c *Config) { c.DataDir = dir }

	s1 := start(t, cfg)
	a, b := dial(t, s1, 1), dial(t, s1, 2)
	for i := range 30 { // a mix: rests, crosses, partials
		side := proto.SideBuy
		if i%3 == 0 {
			side = proto.SideSell
		}
		_ = a.NewOrder(limit(1, uint64(i+1), side, int64(100+i%5), int64(1+i%4)))
		_ = b.NewOrder(limit(2, uint64(i+1), proto.SideSell, int64(200+i%7), 2))
	}
	time.Sleep(200 * time.Millisecond)
	_ = a.Cancel(proto.Cancel{Instrument: 1, OrderID: 3})
	_ = b.Cancel(proto.Cancel{Instrument: 2, OrderID: 2})
	time.Sleep(200 * time.Millisecond)
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotState(s1.Engine())

	s2 := start(t, cfg) // same instruments: already in the log, so no new records
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	if after := snapshotState(s2.Engine()); after != before {
		t.Fatalf("state after replay differs:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// The next command continues the sequence and the order ids.
	s3 := start(t, cfg)
	c := dial(t, s3, 3)
	_ = c.NewOrder(limit(1, 1, proto.SideBuy, 1, 1))
	ack := recv(t, c, 1)[0].Ack
	if ack.Seq != uint64(s2.Engine().LastSeq())+1 || ack.OrderID != uint64(s2.Engine().LastOrderID())+1 {
		t.Fatalf("after restart: ack seq %d order %d; last were seq %d order %d",
			ack.Seq, ack.OrderID, s2.Engine().LastSeq(), s2.Engine().LastOrderID())
	}
	t.Logf("replayed state:\n%s", before)
}

func TestRestartInstrumentMismatch(t *testing.T) {
	dir := t.TempDir()
	s1 := start(t, func(c *Config) { c.DataDir = dir })
	_ = s1.Close()
	_, err := Start(Config{DataDir: dir, Instruments: []book.Instrument{{Symbol: "XYZ", TickSize: 5, LotSize: 1}}})
	if err == nil {
		t.Fatal("restart with a changed tick size was accepted")
	}
	t.Logf("rejected as expected: %v", err)
}

// A torn final record (a crash mid-write) is truncated at restart; earlier
// commands survive and the sequence continues from the last intact record.
func TestRestartAfterTornTail(t *testing.T) {
	dir := t.TempDir()
	s1 := start(t, func(c *Config) { c.DataDir = dir })
	cl := dial(t, s1, 1)
	for i := range 5 {
		_ = cl.NewOrder(limit(1, uint64(i+1), proto.SideBuy, 50, 1))
		recv(t, cl, 1)
	}
	_ = s1.Close()

	segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*.log"))
	last := segs[len(segs)-1]
	data, _ := os.ReadFile(last)
	if err := os.WriteFile(last, data[:len(data)-10], 0o644); err != nil { // tear the last record
		t.Fatal(err)
	}

	s2 := start(t, func(c *Config) { c.DataDir = dir })
	_ = s2.Close()
	if got := s2.Engine().LastOrderID(); got != 4 {
		t.Fatalf("after torn tail: last order id %d, want 4 (order 5's record was torn)", got)
	}
	waitForLog(t, s2, "truncating torn tail")
}
