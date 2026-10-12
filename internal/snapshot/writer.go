package snapshot

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tanxena/matchd/internal/durable"
	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/fsutil"
	"github.com/tanxena/matchd/internal/wal"
)

// Keep is how many snapshots the manifest retains. Two, so that replay-check
// can rebuild the newest from the older one plus the WAL between them.
const Keep = 2

// Writer is the snapshot goroutine. It receives frozen snapshots from the
// engine, writes them, and hands them back to be thawed.
type Writer struct {
	DataDir string
	WAL     *wal.Log     // segments older than the oldest kept snapshot are removed
	Fills   *FillLog     // likewise (may be nil)
	Durable *durable.Seq // a snapshot is only published once its seq is durable
	Log     *slog.Logger

	// Counters for tests now and /metrics in milestone 7. Written by the
	// writer goroutine; atomics so other goroutines can read them.
	written      atomic.Uint64
	failed       atomic.Uint64
	lastSeq      atomic.Uint64
	lastDuration atomic.Int64 // nanoseconds for the last successful write
}

// Run writes every snapshot from in and returns it on done. It returns when
// in is closed. A failed snapshot is logged and counted, not fatal:
// durability comes from the WAL; a snapshot only shortens recovery.
func (w *Writer) Run(in <-chan *engine.Snapshot, done chan<- *engine.Snapshot) {
	for s := range in {
		start := time.Now()
		if err := w.write(s); err != nil {
			w.failed.Add(1)
			w.Log.Error("snapshot failed", "seq", s.Seq, "err", err)
		} else {
			w.written.Add(1)
			w.lastSeq.Store(uint64(s.Seq))
			w.lastDuration.Store(int64(time.Since(start)))
			w.Log.Info("snapshot written", "seq", s.Seq, "took", time.Since(start))
		}
		done <- s // always: the engine must thaw it either way
	}
}

// errDurabilityFailed: the WAL can no longer make the snapshot's commands
// durable, so the snapshot must not be published.
var errDurabilityFailed = errors.New("snapshot: durability failed before the snapshot's seq was on disk")

func (w *Writer) write(s *engine.Snapshot) error {
	// Publish a snapshot only once every command in it is durable in the
	// WAL. Under group commit the engine runs ahead of the disk; a snapshot
	// published early would, after a power failure, claim commands the WAL
	// lost, and recovery would find a gap between the WAL's end and the
	// snapshot (decision 013). Snapshots are rare, so polling is fine (the
	// durable seq's ping channel belongs to the publisher).
	for w.Durable.Load() < uint64(s.Seq) {
		select {
		case <-w.Durable.Failed():
			return errDurabilityFailed
		case <-time.After(time.Millisecond):
		}
	}

	st := s.State()
	data := Encode(st)
	dir := snapshotDir(w.DataDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	name := fmt.Sprintf("%020d.snap", s.Seq)
	if err := fsutil.WriteFileAtomic(filepath.Join(dir, name), data); err != nil {
		return err
	}

	m, _, err := readPublished(w.DataDir) // not ReadManifest: no directory scan here
	if err != nil {
		return err
	}
	m.Snapshots = append(m.Snapshots, Entry{Seq: uint64(s.Seq), LastMatchID: s.LastMatchID, File: name})
	if len(m.Snapshots) > Keep {
		m.Snapshots = m.Snapshots[len(m.Snapshots)-Keep:]
	}
	if err := WriteManifest(w.DataDir, m); err != nil {
		return err
	}
	w.prune(m)
	return nil
}

// prune removes snapshot files the manifest no longer lists, and WAL and
// fill-log segments the oldest kept snapshot makes unnecessary. Failures
// are logged, not returned: leftovers cost disk, not correctness.
func (w *Writer) prune(m Manifest) {
	listed := map[string]bool{}
	for _, e := range m.Snapshots {
		listed[e.File] = true
	}
	entries, _ := os.ReadDir(snapshotDir(w.DataDir))
	for _, e := range entries {
		name := e.Name()
		isSnapshotFile := strings.HasSuffix(name, ".snap") || strings.HasSuffix(name, ".snap.tmp")
		if listed[name] || !isSnapshotFile {
			continue
		}
		if err := os.Remove(filepath.Join(snapshotDir(w.DataDir), name)); err != nil {
			w.Log.Warn("snapshot: prune", "file", name, "err", err)
		}
	}
	if len(m.Snapshots) < Keep {
		// Never prune with a single snapshot: if that one file turned out
		// to be damaged, the WAL from seq 1 is the only way back. With Keep
		// snapshots, the older one is the fallback and the WAL after it is
		// kept, so recovery survives one damaged snapshot.
		return
	}
	oldest := m.Snapshots[0]
	if w.WAL != nil {
		// Keep every record after the oldest snapshot: replay-check rebuilds
		// the newest snapshot from it (PRD 5.4: a segment is deletable once a
		// snapshot's seq exceeds its last seq).
		if n, err := w.WAL.RemoveSegmentsBelow(oldest.Seq); err != nil {
			w.Log.Warn("snapshot: prune WAL", "err", err)
		} else if n > 0 {
			w.Log.Info("WAL segments removed", "count", n, "below_seq", oldest.Seq)
		}
	}
	if w.Fills != nil {
		if _, err := w.Fills.RemoveBelow(oldest.LastMatchID + 1); err != nil {
			w.Log.Warn("snapshot: prune fill log", "err", err)
		}
	}
}

// Stats is a snapshot of the writer's counters.
type Stats struct {
	Written      uint64
	Failed       uint64
	LastSeq      uint64
	LastDuration time.Duration
}

// Stats returns the current counters. Safe from any goroutine.
func (w *Writer) Stats() Stats {
	return Stats{
		Written: w.written.Load(), Failed: w.failed.Load(),
		LastSeq: w.lastSeq.Load(), LastDuration: time.Duration(w.lastDuration.Load()),
	}
}
