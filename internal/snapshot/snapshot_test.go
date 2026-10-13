package snapshot

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/tanxena/matchd/internal/book"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/wal"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func sampleState() State {
	return State{
		Seq: 42, LastOrderID: 9, LastMatchID: 5,
		Instruments: []engine.InstrumentState{
			{Spec: book.Instrument{Symbol: "XYZ", TickSize: 1, LotSize: 1},
				Bids: []book.RestingOrder{
					{ID: 3, Client: 1, ClOrdID: 30, Side: book.Buy, Price: 100, Qty: 4},
					{ID: 7, Client: 2, ClOrdID: 70, Side: book.Buy, Price: 100, Qty: 1},
					{ID: 2, Client: 1, ClOrdID: 20, Side: book.Buy, Price: 99, Qty: 9},
				},
				Asks: []book.RestingOrder{{ID: 9, Client: 3, ClOrdID: 90, Side: book.Sell, Price: 101, Qty: 2}}},
			{Spec: book.Instrument{Symbol: "EMPTY", TickSize: 5, LotSize: 100}}, // no orders: nil sides
		},
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	st := sampleState()
	got, err := Decode(Encode(st))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, st) {
		t.Fatalf("round trip:\n got  %+v\n want %+v", got, st)
	}
	// And the decoded state restores into a working engine.
	if _, err := engine.Restore(got); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsDamage(t *testing.T) {
	good := Encode(sampleState())
	for i := range good { // every single-byte flip is caught by the checksum or the header
		bad := append([]byte(nil), good...)
		bad[i] ^= 0x01
		if _, err := Decode(bad); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("flipped byte %d not detected: %v", i, err)
		}
	}
	for _, n := range []int{0, 7, 20, len(good) - 1} {
		if _, err := Decode(good[:n]); !errors.Is(err, ErrCorruptSnapshot) {
			t.Fatalf("truncated to %d bytes not detected", n)
		}
	}
}

func TestManifest(t *testing.T) {
	dir := t.TempDir()
	m, err := ReadManifest(dir)
	if err != nil || len(m.Snapshots) != 0 {
		t.Fatalf("missing manifest: %+v %v", m, err)
	}
	want := Manifest{Snapshots: []Entry{{Seq: 10, LastMatchID: 2, File: "a.snap"}, {Seq: 20, LastMatchID: 4, File: "b.snap"}}}
	if err := WriteManifest(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(dir)
	if err != nil || !reflect.DeepEqual(got.Snapshots, want.Snapshots) {
		t.Fatalf("got %+v %v", got, err)
	}
	if err := os.WriteFile(manifestPath(dir), []byte(`{"version":1,"snapshots":[{"seq":20},{"seq":10}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir); err == nil {
		t.Fatal("out-of-order manifest accepted")
	}
}

func TestReadSnapshotChecksManifestAgreement(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(snapshotDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snapshotDir(dir), "s.snap"), Encode(sampleState()), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(dir, Entry{Seq: 42, LastMatchID: 5, File: "s.snap"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSnapshot(dir, Entry{Seq: 43, LastMatchID: 5, File: "s.snap"}); !errors.Is(err, ErrCorruptSnapshot) {
		t.Fatalf("seq disagreement not detected: %v", err)
	}
}

func fillPair(match uint64, seq engine.Seq) (engine.Event, engine.Event) {
	taker := engine.Event{Seq: seq, Time: engine.Timestamp(seq) * 10, Kind: engine.EvFill, Instrument: 1, Client: 2, ClOrdID: 20,
		OrderID: 8, Side: book.Buy, Price: 100, Qty: 3, Leaves: 1, MatchID: match, Role: engine.RoleTaker}
	maker := engine.Event{Seq: seq, Time: engine.Timestamp(seq) * 10, Kind: engine.EvFill, Instrument: 1, Client: 1, ClOrdID: 10,
		OrderID: 4, Side: book.Sell, Price: 100, Qty: 3, Leaves: 0, MatchID: match, Role: engine.RoleMaker}
	return taker, maker
}

func TestFillLog(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenFillLog(dir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	var want []Match
	for _, id := range []uint64{1, 2, 5, 9} { // gaps: unreleased matches are simply absent
		tk, mk := fillPair(id, engine.Seq(id*3))
		ack := engine.Event{Kind: engine.EvAck, Seq: tk.Seq}
		f.Released(&ack) // not a fill: ignored
		f.Released(&tk)
		f.Released(&mk)
		want = append(want, MatchOf(tk, mk))
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	got, _, err := ReadFillLog(dir)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v\nwant %+v", got, err, want)
	}
	// Reopening continues after the last recorded match.
	f2, err := OpenFillLog(dir, quiet)
	if err != nil || f2.LastMatchID() != 9 {
		t.Fatalf("reopen: last %d, %v", f2.LastMatchID(), err)
	}
	_ = f2.Close()
}

// syncCounter wraps a WAL file and counts Sync calls.
type syncCounter struct {
	wal.File
	n *atomic.Int64
}

func (s syncCounter) Sync() error { s.n.Add(1); return s.File.Sync() }

// Review F-006: the fill log is never fsynced, not even when a segment
// fills and the next one starts (the publisher goroutine writes it and must
// never wait on a disk).
func TestFillLogNeverFsyncs(t *testing.T) {
	dir := t.TempDir()
	var syncs atomic.Int64
	f, err := openFillLog(dir, quiet, wal.Options{SegmentSize: 4 << 10, OpenFile: func(path string) (wal.File, error) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		return syncCounter{File: file, n: &syncs}, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 500; id++ { // 500 x 85-byte records: about 10 segments of 4 KB
		tk, mk := fillPair(id, engine.Seq(id))
		f.Released(&tk)
		f.Released(&mk)
		if err := f.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	_ = f.Close()
	segs, _ := filepath.Glob(filepath.Join(fillDir(dir), "*.log"))
	if len(segs) < 5 {
		t.Fatalf("only %d segments: the test didn't roll", len(segs))
	}
	if n := syncs.Load(); n != 0 {
		t.Fatalf("the fill log fsynced %d times across %d segments", n, len(segs))
	}
}

// ...and a fill log damaged beyond a torn tail is moved aside, with a
// warning, instead of stopping the server from starting.
func TestDamagedFillLogIsMovedAside(t *testing.T) {
	dir := t.TempDir()
	f, err := OpenFillLog(dir, quiet)
	if err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 50; id++ {
		tk, mk := fillPair(id, engine.Seq(id))
		f.Released(&tk)
		f.Released(&mk)
	}
	if err := f.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	segs, _ := filepath.Glob(filepath.Join(fillDir(dir), "*.log"))
	data, err := os.ReadFile(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xff // a record in the middle, not a torn tail
	if err := os.WriteFile(segs[0], data, 0o644); err != nil {
		t.Fatal(err)
	}

	f2, err := OpenFillLog(dir, quiet)
	if err != nil {
		t.Fatalf("a damaged fill log stopped the open: %v", err)
	}
	defer func() { _ = f2.Close() }()
	if f2.LastMatchID() != 0 {
		t.Fatalf("fresh fill log starts at match %d", f2.LastMatchID())
	}
	aside, _ := filepath.Glob(fillDir(dir) + ".damaged-*")
	if len(aside) != 1 {
		t.Fatalf("damaged fill log not kept aside: %v", aside)
	}
}
