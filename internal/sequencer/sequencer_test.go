package sequencer

import (
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/wal"
)

func TestParsePolicy(t *testing.T) {
	good := map[string]Policy{
		"always":    {Mode: Always, Window: DefaultWindow, MaxBatch: DefaultMaxBatch},
		"never":     {Mode: Never, Window: DefaultWindow, MaxBatch: DefaultMaxBatch},
		"group":     {Mode: Group, Window: time.Millisecond, MaxBatch: DefaultMaxBatch},
		"group:5ms": {Mode: Group, Window: 5 * time.Millisecond, MaxBatch: DefaultMaxBatch},
	}
	for in, want := range good {
		got, err := ParsePolicy(in)
		if err != nil || got != want {
			t.Errorf("ParsePolicy(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "sometimes", "group:", "group:-1ms", "group:fast"} {
		if _, err := ParsePolicy(bad); err == nil {
			t.Errorf("ParsePolicy(%q) accepted", bad)
		}
	}
}

// gateFile wraps a segment file so a test can count fsyncs, hold them until
// released, or make them fail.
type gateFile struct {
	wal.File
	g *gate
}

type gate struct {
	syncs   atomic.Int64
	fail    atomic.Bool
	mu      sync.Mutex    // guards release
	release chan struct{} // if non-nil, Sync waits for it to close
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
		return errors.New("injected fsync failure")
	}
	return f.File.Sync()
}

func (g *gate) hold() chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.release = make(chan struct{})
	return g.release
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.release != nil {
		close(g.release)
		g.release = nil
	}
}

type rig struct {
	g   *gate
	log *wal.Log
	d   *durable.Seq
	s   *Sequencer
	in  chan engine.Command
	out chan engine.Command
	err chan error // Run's result
}

func newRig(t *testing.T, p Policy) *rig {
	t.Helper()
	g := &gate{}
	opts := wal.Options{OpenFile: func(path string) (wal.File, error) {
		f, err := walOpen(path)
		return gateFile{File: f, g: g}, err
	}}
	l, _, err := wal.Open(t.TempDir(), opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	d := durable.New(0)
	var now engine.Timestamp
	r := &rig{g: g, log: l, d: d, s: New(func() engine.Timestamp { now += 10; return now }, l, p, d),
		in: make(chan engine.Command, 10000), out: make(chan engine.Command, 10000), err: make(chan error, 1)}
	go func() { r.err <- r.s.Run(r.in, r.out) }()
	return r
}

func order(i int) engine.Command {
	return engine.Command{Kind: engine.CmdNewOrder, Instrument: 1, Client: 1, ClOrdID: book.ClientOrderID(i),
		Side: book.Buy, Type: book.Limit, TIF: book.GTC, Price: 10, Qty: 1}
}

func recvOut(t *testing.T, r *rig) engine.Command {
	t.Helper()
	select {
	case c := <-r.out:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no command forwarded")
	}
	return engine.Command{}
}

// Under Always, a command is forwarded to the engine only after its fsync:
// with fsync held, nothing comes out.
func TestAlwaysForwardsOnlyAfterFsync(t *testing.T) {
	r := newRig(t, Policy{Mode: Always, MaxBatch: DefaultMaxBatch})
	r.g.hold()
	r.in <- order(1)
	select {
	case c := <-r.out:
		t.Fatalf("command %d forwarded before its fsync completed", c.Seq)
	case <-time.After(200 * time.Millisecond):
	}
	if r.d.Load() != 0 {
		t.Fatal("durable advanced before fsync")
	}
	r.g.open()
	if c := recvOut(t, r); c.Seq != 1 || c.Time != 10 || r.d.Load() < 1 {
		t.Fatalf("got seq %d time %d, durable %d", c.Seq, c.Time, r.d.Load())
	}
	close(r.in)
	if err := <-r.err; err != nil {
		t.Fatal(err)
	}
}

// Under Always, commands already waiting share one fsync.
func TestAlwaysBatchesWaitingCommands(t *testing.T) {
	r := newRig(t, Policy{Mode: Always, MaxBatch: DefaultMaxBatch})
	r.g.hold() // the first command's fsync blocks...
	r.in <- order(0)
	time.Sleep(50 * time.Millisecond) // ...while 999 more queue up behind it
	for i := 1; i < 1000; i++ {
		r.in <- order(i)
	}
	r.g.open()
	for i := range 1000 {
		if c := recvOut(t, r); c.Seq != engine.Seq(i+1) {
			t.Fatalf("command %d has seq %d", i, c.Seq)
		}
	}
	if n := r.g.syncs.Load(); n > 10 {
		t.Fatalf("%d fsyncs for 1000 commands; waiting commands should share fsyncs", n)
	} else {
		t.Logf("1000 commands, %d fsyncs", n)
	}
	close(r.in)
	<-r.err
}

// Under Group, commands are forwarded before their fsync, and durability
// catches up within the window.
func TestGroupForwardsFirstThenSyncs(t *testing.T) {
	r := newRig(t, Policy{Mode: Group, Window: 5 * time.Millisecond, MaxBatch: DefaultMaxBatch})
	r.g.hold()
	r.in <- order(1)
	if c := recvOut(t, r); c.Seq != 1 {
		t.Fatalf("seq %d", c.Seq)
	}
	time.Sleep(50 * time.Millisecond)
	if r.d.Load() != 0 {
		t.Fatal("durable advanced while fsync was held")
	}
	r.g.open()
	deadline := time.Now().Add(5 * time.Second)
	for r.d.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatal("durable never caught up")
		}
		time.Sleep(time.Millisecond)
	}
	close(r.in)
	if err := <-r.err; err != nil {
		t.Fatal(err)
	}
}

// Under Group, once MaxBatch commands are unsynced the syncer is kicked and
// doesn't wait out the window; and a burst shares few fsyncs.
func TestGroupBurstAndKick(t *testing.T) {
	// A kick taken as the syncer's wake-up. The sequencer always marks the
	// log dirty before it kicks, and when both signals are waiting Go's
	// select takes either at random. The syncer used to treat a kick taken
	// first as just "data is waiting" and then wait out the window, so the
	// burst below failed about 1 run in 100. A kick alone takes that branch
	// every time.
	l, _, err := wal.Open(t.TempDir(), wal.Options{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	l.Append(1, 10, []byte{1})
	if err := l.Flush(); err != nil {
		t.Fatal(err)
	}
	d := durable.New(0)
	g := startGroupSyncer(l, d, time.Hour) // the window never expires
	g.kick()
	deadline := time.Now().Add(5 * time.Second)
	for d.Load() < 1 {
		if time.Now().After(deadline) {
			g.stop()
			t.Fatal("kicked syncer waited out its 1h window instead of syncing")
		}
		time.Sleep(time.Millisecond)
	}
	g.stop()

	r := newRig(t, Policy{Mode: Group, Window: time.Hour, MaxBatch: 100}) // the window never expires
	for i := range 1000 {
		r.in <- order(i)
	}
	for range 1000 {
		recvOut(t, r)
	}
	deadline = time.Now().Add(5 * time.Second)
	for r.d.Load() < 900 { // kicks every 100 unsynced; at most the last <100 wait for the window
		if time.Now().After(deadline) {
			t.Fatalf("durable stuck at %d with a 1h window: kicks didn't happen", r.d.Load())
		}
		time.Sleep(time.Millisecond)
	}
	close(r.in)
	if err := <-r.err; err != nil || r.d.Load() != 1000 {
		t.Fatalf("after close: durable %d, err %v (final fsync must cover everything)", r.d.Load(), err)
	}
}

// A failed fsync under Always: the batch is never forwarded, durability is
// marked failed, Run returns the error.
func TestAlwaysFsyncFailure(t *testing.T) {
	r := newRig(t, Policy{Mode: Always, MaxBatch: DefaultMaxBatch})
	r.in <- order(1)
	recvOut(t, r)
	r.g.fail.Store(true)
	r.in <- order(2)
	if err := <-r.err; err == nil {
		t.Fatal("Run did not return the fsync error")
	}
	if c, ok := <-r.out; ok {
		t.Fatalf("command %d forwarded despite failed fsync", c.Seq)
	}
	if r.d.Load() != 1 || r.d.Err() == nil {
		t.Fatalf("durable %d err %v", r.d.Load(), r.d.Err())
	}
}

// A failed fsync under Group: durability never advances past the last good
// fsync, and the sequencer stops.
func TestGroupFsyncFailure(t *testing.T) {
	r := newRig(t, Policy{Mode: Group, Window: time.Millisecond, MaxBatch: DefaultMaxBatch})
	r.in <- order(1)
	recvOut(t, r)
	for r.d.Load() < 1 {
		time.Sleep(time.Millisecond)
	}
	r.g.fail.Store(true)
	r.in <- order(2)
	recvOut(t, r) // forwarded (group applies before fsync)...
	select {
	case err := <-r.err:
		if err == nil {
			t.Fatal("Run returned nil after fsync failure")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("sequencer kept running after the syncer's fsync failed")
	}
	if r.d.Load() != 1 {
		t.Fatalf("...but must never become durable: durable = %d", r.d.Load())
	}
}

func TestNeverAdvancesWithoutFsync(t *testing.T) {
	r := newRig(t, Policy{Mode: Never, MaxBatch: DefaultMaxBatch})
	for i := range 10 {
		r.in <- order(i)
	}
	for range 10 {
		recvOut(t, r)
	}
	close(r.in)
	<-r.err
	if r.d.Load() != 10 || r.g.syncs.Load() != 0 {
		t.Fatalf("durable %d, fsyncs %d", r.d.Load(), r.g.syncs.Load())
	}
}

func walOpen(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}
