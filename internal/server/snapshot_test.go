package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tanxena/matchd/internal/client"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/proto"
	"github.com/tanxena/matchd/internal/snapshot"
	"github.com/tanxena/matchd/internal/wal"
)

// trade sends n orders from two clients around one price, so they rest,
// cross and partially fill, and waits for every answer.
func trade(t *testing.T, s *testServer, n int, clOrdBase uint64) {
	t.Helper()
	a, b := dial(t, s, 1), dial(t, s, 2)
	for i := range n {
		c, side := a, proto.SideBuy
		if i%2 == 1 {
			c, side = b, proto.SideSell
		}
		_ = c.NewOrder(limit(1, clOrdBase+uint64(i), side, int64(100+(i*7)%5-2), int64(1+i%4)))
	}
	drain := func(c *client.Conn, want int) {
		got := 0
		_ = c.SetReadDeadline(time.Now().Add(wait))
		for got < want {
			m, err := c.Next()
			if err != nil {
				t.Fatalf("client %d: %v after %d answers", c.ClientID, err, got)
			}
			if m.Type == proto.TypeAck || m.Type == proto.TypeReject {
				got++
			}
		}
	}
	drain(a, (n+1)/2)
	drain(b, n/2)
	_ = a.Close()
	_ = b.Close()
}

func snapshotConfig(dir string) func(*Config) {
	return func(c *Config) {
		c.DataDir = dir
		c.SnapshotEvery = 100
		c.WAL = wal.Options{SegmentSize: 2048} // many segments, so pruning shows
	}
}

func TestSnapshotsWrittenPrunedAndRecovered(t *testing.T) {
	dir := t.TempDir()
	s1 := start(t, snapshotConfig(dir))
	// Three rounds with pauses: at most one snapshot is in flight, so a
	// burst that finishes within one snapshot's write yields one or two.
	// Three rounds give at least three, so the manifest has to drop one.
	for round := range 3 {
		trade(t, s1, 300, uint64(1+round*1000))
		time.Sleep(150 * time.Millisecond)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	st := s1.Snapshots()
	if st.Written <= snapshot.Keep || st.Failed != 0 {
		t.Fatalf("snapshot writer: %+v; want more than %d written", st, snapshot.Keep)
	}
	t.Logf("snapshot writer: %+v", st)

	m, err := snapshot.ReadManifest(dir)
	if err != nil || len(m.Snapshots) != snapshot.Keep {
		t.Fatalf("manifest %+v %v; want %d snapshots kept", m, err, snapshot.Keep)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "snapshots", "*"))
	if len(files) != snapshot.Keep {
		t.Fatalf("snapshot files %v; unlisted ones must be pruned", files)
	}
	segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*.log"))
	if strings.HasSuffix(segs[0], "00000000000000000001.log") {
		t.Fatalf("no WAL segment was pruned (%d segments, first %s)", len(segs), filepath.Base(segs[0]))
	}

	// replay-check: rebuild the newest snapshot from the older one plus the
	// WAL, and compare every fill-log record with the replay.
	res, err := snapshot.ReplayCheck(dir)
	if err != nil {
		t.Fatalf("replay-check: %v", err)
	}
	if !res.SnapshotCompared || res.FillsChecked == 0 || res.BaseSeq == 0 {
		t.Fatalf("replay-check didn't check enough: %s", res.Report)
	}
	t.Logf("replay-check: %s", res.Report)

	// Restart: snapshot + WAL tail gives exactly the state at shutdown.
	s2 := start(t, snapshotConfig(dir))
	_ = s2.Close()
	if !reflect.DeepEqual(s2.Engine().State(), s1.Engine().State()) {
		t.Fatal("state after recovery from snapshot differs from the state at shutdown")
	}
	waitForLog(t, s2, "snapshot_seq="+strconv.FormatUint(m.Snapshots[1].Seq, 10))

	// And it keeps going: new orders continue the numbering.
	s3 := start(t, snapshotConfig(dir))
	c := dial(t, s3, 3)
	_ = c.NewOrder(limit(1, 1, proto.SideBuy, 1, 1))
	if ack := recv(t, c, 1)[0].Ack; ack.Seq != uint64(s2.Engine().LastSeq())+1 || ack.OrderID != uint64(s2.Engine().LastOrderID())+1 {
		t.Fatalf("after recovery: ack seq %d order %d", ack.Seq, ack.OrderID)
	}
}

// replay-check must actually catch damage, or it proves nothing.
func TestReplayCheckDetectsDamage(t *testing.T) {
	setup := func(t *testing.T) string {
		dir := t.TempDir()
		s := start(t, snapshotConfig(dir))
		trade(t, s, 600, 1)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := snapshot.ReplayCheck(dir); err != nil {
			t.Fatalf("clean data dir fails replay-check: %v", err)
		}
		return dir
	}

	t.Run("snapshot content changed", func(t *testing.T) {
		dir := setup(t)
		m, _ := snapshot.ReadManifest(dir)
		newest := m.Snapshots[len(m.Snapshots)-1]
		st, err := snapshot.ReadSnapshot(dir, newest)
		if err != nil {
			t.Fatal(err)
		}
		for i := range st.Instruments { // change one resting quantity, keep the file valid
			if len(st.Instruments[i].Bids) > 0 {
				st.Instruments[i].Bids[0].Qty++
				break
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "snapshots", newest.File), snapshot.Encode(st), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := snapshot.ReplayCheck(dir); err == nil || !strings.Contains(err.Error(), "differs from snapshot") {
			t.Fatalf("tampered snapshot: %v", err)
		}
	})
	t.Run("fill the replay never produced", func(t *testing.T) {
		dir := setup(t)
		f, err := snapshot.OpenFillLog(dir, nil)
		if err != nil {
			t.Fatal(err)
		}
		id := f.LastMatchID() + 1000
		tk := engine.Event{Kind: engine.EvFill, Role: engine.RoleTaker, MatchID: id, Seq: 1}
		mk := engine.Event{Kind: engine.EvFill, Role: engine.RoleMaker, MatchID: id, Seq: 1}
		f.Released(&tk)
		f.Released(&mk)
		_ = f.Close()
		if _, err := snapshot.ReplayCheck(dir); err == nil || !strings.Contains(err.Error(), "never produced") {
			t.Fatalf("fabricated fill: %v", err)
		}
	})
	t.Run("WAL ends before the newest snapshot", func(t *testing.T) {
		dir := setup(t)
		segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*.log"))
		if err := os.Remove(segs[len(segs)-1]); err != nil { // lose the newest records
			t.Fatal(err)
		}
		if _, err := snapshot.ReplayCheck(dir); err == nil {
			t.Fatal("missing WAL tail not detected")
		}
	})
}

func corruptNewestSnapshot(t *testing.T, dir string) {
	t.Helper()
	m, _ := snapshot.ReadManifest(dir)
	if len(m.Snapshots) == 0 {
		t.Fatal("no snapshot to corrupt")
	}
	newest := filepath.Join(dir, "snapshots", m.Snapshots[len(m.Snapshots)-1].File)
	data, _ := os.ReadFile(newest)
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(newest, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryFallsBackToOlderSnapshot(t *testing.T) {
	dir := t.TempDir()
	s1 := start(t, snapshotConfig(dir))
	for round := range 3 { // several snapshots, so the WAL gets pruned
		trade(t, s1, 300, uint64(1+round*1000))
		time.Sleep(150 * time.Millisecond)
	}
	_ = s1.Close()
	corruptNewestSnapshot(t, dir)
	s2 := start(t, snapshotConfig(dir))
	_ = s2.Close()
	if !reflect.DeepEqual(s2.Engine().State(), s1.Engine().State()) {
		t.Fatal("state differs after falling back to the older snapshot")
	}
	waitForLog(t, s2, "snapshot unusable, trying an older one")
}

// With a single snapshot nothing is pruned, so even if that snapshot is
// damaged the WAL from seq 1 rebuilds everything.
func TestRecoveryFromWALWhenOnlySnapshotDamaged(t *testing.T) {
	dir := t.TempDir()
	s1 := start(t, func(c *Config) {
		c.DataDir = dir
		c.SnapshotEvery = 150                  // rounds of 100, 100, 50 commands: exactly one snapshot
		c.WAL = wal.Options{SegmentSize: 2048} // each round's batch lands in its own segment
	})
	for round, n := range []int{100, 100, 50} {
		trade(t, s1, n, uint64(1+round*1000))
		time.Sleep(100 * time.Millisecond)
	}
	_ = s1.Close()
	if m, _ := snapshot.ReadManifest(dir); len(m.Snapshots) != 1 {
		t.Fatalf("want exactly 1 snapshot, have %d", len(m.Snapshots))
	}
	if segs, _ := filepath.Glob(filepath.Join(dir, "wal", "*.log")); len(segs) < 3 {
		t.Fatalf("only %d WAL segments; the test needs several", len(segs))
	}
	corruptNewestSnapshot(t, dir)
	s2 := start(t, func(c *Config) { c.DataDir = dir })
	_ = s2.Close()
	if !reflect.DeepEqual(s2.Engine().State(), s1.Engine().State()) {
		t.Fatal("state differs after recovering from the WAL alone")
	}
	waitForLog(t, s2, "recovering from the WAL alone")
}

// Under group commit the engine runs ahead of the disk; a snapshot must not
// be published until its seq is durable.
func TestSnapshotWaitsForDurability(t *testing.T) {
	g := &gate{}
	dir := t.TempDir()
	s := start(t, func(c *Config) {
		c.DataDir = dir
		c.Fsync = policy(t, "group:1ms")
		c.WAL = gatedWAL(g)
		c.SnapshotEvery = 5
	})
	cl := dial(t, s, 1)
	g.hold()
	t.Cleanup(g.open) // runs before the server's Close: a failing test must not hang on a held fsync
	for i := range 10 {
		_ = cl.NewOrder(limit(1, uint64(i+1), proto.SideBuy, 50, 1))
	}
	time.Sleep(300 * time.Millisecond) // the engine has applied them and frozen a snapshot
	if m, _ := snapshot.ReadManifest(dir); len(m.Snapshots) != 0 {
		t.Fatalf("snapshot published while its commands weren't durable: %+v", m)
	}
	g.open()
	recv(t, cl, 10)
	_ = s.Close()
	if m, _ := snapshot.ReadManifest(dir); len(m.Snapshots) == 0 {
		t.Fatal("no snapshot after durability caught up")
	}
}
