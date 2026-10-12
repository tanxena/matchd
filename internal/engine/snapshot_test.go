package engine_test

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
)

// commands generates n sequenced commands (two instruments added first).
func commands(seed uint64, n int) []engine.Command {
	d := newDriverQuiet()
	g := newStreamGen(seed)
	var out []engine.Command
	add := func(c engine.Command) {
		evs := d.stepCmd(c)
		c.Seq, c.Time = d.seq, engine.Timestamp(1_000_000*int64(d.seq))
		out = append(out, c)
		for _, e := range evs {
			if e.Kind == engine.EvAck {
				g.acked = append(g.acked, liveOrder{e.Instrument, e.Client, e.OrderID})
			}
		}
	}
	add(engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "AAA", TickSize: 1, LotSize: 1}})
	add(engine.Command{Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "BBB", TickSize: 1, LotSize: 1}})
	for range n {
		add(g.next())
	}
	return out
}

// newDriverQuiet is newDriver without a *testing.T, for generating streams.
type quietDriver struct {
	e   *engine.Engine
	seq engine.Seq
	out []engine.Event
}

func newDriverQuiet() *quietDriver { return &quietDriver{e: engine.New()} }

func (d *quietDriver) stepCmd(c engine.Command) []engine.Event {
	d.seq++
	c.Seq = d.seq
	c.Time = engine.Timestamp(1_000_000 * int64(d.seq))
	d.out = d.e.Step(&c, d.out[:0])
	return d.out
}

func apply(t *testing.T, e *engine.Engine, cmds []engine.Command) []engine.Event {
	t.Helper()
	var all, buf []engine.Event
	for i := range cmds {
		buf = e.Step(&cmds[i], buf[:0])
		all = append(all, buf...)
	}
	return all
}

func TestStateRestoreRoundTrip(t *testing.T) {
	cmds := commands(11, 5_000)
	e := engine.New()
	apply(t, e, cmds[:3_000])
	st := e.State()
	if len(st.Instruments) != 2 || st.Seq != 3_000 || st.LastMatchID == 0 {
		t.Fatalf("state: %d instruments, seq %d, last match %d", len(st.Instruments), st.Seq, st.LastMatchID)
	}
	r, err := engine.Restore(st)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.State(); !reflect.DeepEqual(got, st) {
		t.Fatal("restored engine's state differs from the state it was restored from")
	}
	// Same next commands, same events: ids, match ids, FIFO order all carry over.
	a := apply(t, e, cmds[3_000:])
	b := apply(t, r, cmds[3_000:])
	if !slices.Equal(a, b) {
		t.Fatalf("restored engine diverges at event %d", firstDiff(a, b))
	}
}

func TestRestoreRejectsBadState(t *testing.T) {
	spec := book.Instrument{Symbol: "AAA", TickSize: 1, LotSize: 1}
	bad := map[string]engine.State{
		"duplicate symbol": {Instruments: []engine.InstrumentState{{Spec: spec}, {Spec: spec}}},
		"order id beyond last": {LastOrderID: 3, Instruments: []engine.InstrumentState{{Spec: spec,
			Bids: []book.RestingOrder{{ID: 9, Client: 1, ClOrdID: 1, Side: book.Buy, Price: 10, Qty: 1}}}}},
		"crossed book": {LastOrderID: 2, Instruments: []engine.InstrumentState{{Spec: spec,
			Bids: []book.RestingOrder{{ID: 1, Client: 1, ClOrdID: 1, Side: book.Buy, Price: 10, Qty: 1}},
			Asks: []book.RestingOrder{{ID: 2, Client: 1, ClOrdID: 2, Side: book.Sell, Price: 10, Qty: 1}}}}},
	}
	for name, st := range bad {
		if _, err := engine.Restore(st); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Run with snapshots: a writer goroutine reads each snapshot while the
// engine keeps going. Every snapshot must equal a fresh replay to its seq,
// and taking snapshots must not change a single event. Run with -race: this
// is where copy-on-write's concurrency is exercised.
func TestRunSnapshotsMatchReplay(t *testing.T) {
	cmds := commands(12, 20_000)

	// Reference: the same commands without snapshots.
	want := apply(t, engine.New(), cmds)

	in := make(chan engine.Command, 256)       // test -> engine goroutine
	out := make(chan engine.Event, 256)        // engine goroutine -> test
	snapOut := make(chan *engine.Snapshot, 1)  // engine -> fake writer
	snapDone := make(chan *engine.Snapshot, 1) // fake writer -> engine
	go engine.New().Run(in, out, &engine.SnapshotHooks{Every: 700, Out: snapOut, Done: snapDone})

	type taken struct {
		seq engine.Seq
		st  engine.State
	}
	snaps := make(chan taken, 1000) // fake writer -> test
	writerDone := make(chan struct{})
	go func() { // the fake snapshot writer
		defer close(writerDone)
		for s := range snapOut {
			time.Sleep(2 * time.Millisecond) // the engine keeps trading meanwhile
			snaps <- taken{s.Seq, s.State()}
			snapDone <- s
		}
	}()
	go func() { // producer
		for _, c := range cmds {
			in <- c
		}
		close(in)
	}()
	var got []engine.Event
	for ev := range out {
		got = append(got, ev)
	}
	close(snapOut) // the engine has returned; no more snapshots
	<-writerDone
	close(snaps)

	if !slices.Equal(got, want) {
		t.Fatalf("snapshots changed the event stream (first difference at %d)", firstDiff(got, want))
	}
	n := 0
	for s := range snaps {
		ref := engine.New()
		apply(t, ref, cmds[:s.seq])
		if st := ref.State(); !reflect.DeepEqual(st, s.st) {
			t.Fatalf("snapshot at seq %d differs from a replay to seq %d", s.seq, s.seq)
		}
		n++
	}
	if n < 5 {
		t.Fatalf("only %d snapshots taken", n)
	}
	t.Logf("%d snapshots, each identical to a replay", n)
}

// Ticks trigger a snapshot only if commands arrived since the last one.
func TestTickTrigger(t *testing.T) {
	in := make(chan engine.Command)
	out := make(chan engine.Event, 16)
	tick := make(chan time.Time)
	snapOut := make(chan *engine.Snapshot, 1)
	snapDone := make(chan *engine.Snapshot, 1)
	go engine.New().Run(in, out, &engine.SnapshotHooks{Tick: tick, Out: snapOut, Done: snapDone})

	tick <- time.Now() // nothing happened yet: no snapshot
	select {
	case <-snapOut:
		t.Fatal("snapshot of an empty, unchanged engine")
	case <-time.After(50 * time.Millisecond):
	}
	in <- engine.Command{Seq: 1, Kind: engine.CmdAddInstrument, Spec: book.Instrument{Symbol: "X", TickSize: 1, LotSize: 1}}
	tick <- time.Now()
	s := <-snapOut
	if s.Seq != 1 {
		t.Fatalf("snapshot seq %d", s.Seq)
	}
	snapDone <- s
	tick <- time.Now() // no commands since: no snapshot
	select {
	case s := <-snapOut:
		t.Fatalf("unchanged engine snapshotted again at %d", s.Seq)
	case <-time.After(50 * time.Millisecond):
	}
	close(in)
	for range out {
	}
}
