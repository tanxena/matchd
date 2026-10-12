package snapshot

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/wal"
)

func walDir(dataDir string) string { return filepath.Join(dataDir, "wal") }

// Recovered describes how the engine was rebuilt.
type Recovered struct {
	SnapshotSeq uint64 // 0 if no snapshot was used
	WAL         wal.RecoveryInfo
	Replayed    int // WAL records applied after the snapshot
}

// LoadLatest returns the newest listed snapshot that reads back intact. If
// the newest is damaged it falls back to the older one, logging why; the WAL
// is kept from the oldest listed snapshot on, so the older one is always
// still usable. ok is false if the manifest lists no usable snapshot; the
// caller then replays the WAL from an empty engine, which works as long as
// the WAL still starts at seq 1 (it does until two snapshots exist, because
// the writer only prunes once Keep snapshots are listed).
func LoadLatest(dataDir string, log *slog.Logger) (st State, e Entry, ok bool, err error) {
	m, err := ReadManifest(dataDir)
	if err != nil {
		return State{}, Entry{}, false, err
	}
	for i := len(m.Snapshots) - 1; i >= 0; i-- {
		e := m.Snapshots[i]
		st, err := ReadSnapshot(dataDir, e)
		if err == nil {
			return st, e, true, nil
		}
		log.Error("snapshot unusable, trying an older one", "file", e.File, "err", err)
	}
	if len(m.Snapshots) > 0 {
		log.Error("every listed snapshot is unusable; recovering from the WAL alone")
	}
	return State{}, Entry{}, false, nil
}

// Recover rebuilds the engine (PRD 5.4): load the manifest, load the latest
// snapshot, replay every WAL record after it, and return the engine and the
// WAL opened for appending. With no snapshot, the WAL must start at seq 1.
func Recover(dataDir string, walOpts wal.Options, log *slog.Logger) (*engine.Engine, *wal.Log, Recovered, error) {
	var rec Recovered
	st, entry, ok, err := LoadLatest(dataDir, log)
	if err != nil {
		return nil, nil, rec, err
	}
	eng := engine.New()
	if ok {
		if eng, err = engine.Restore(st); err != nil {
			return nil, nil, rec, fmt.Errorf("snapshot %s: %w", entry.File, err)
		}
		rec.SnapshotSeq = entry.Seq
	}

	var cmd engine.Command
	var evs []engine.Event
	replay := func(r wal.Record) error {
		if r.Seq <= rec.SnapshotSeq {
			return nil // already in the snapshot
		}
		if err := engine.DecodeCommand(r.Payload, &cmd); err != nil {
			return err
		}
		cmd.Seq, cmd.Time = engine.Seq(r.Seq), engine.Timestamp(r.Time)
		if cmd.Seq != eng.LastSeq()+1 {
			return fmt.Errorf("WAL record %d does not follow the recovered state at seq %d (missing records?)", cmd.Seq, eng.LastSeq())
		}
		evs = eng.Step(&cmd, evs[:0])
		rec.Replayed++
		return nil
	}
	wl, info, err := wal.Open(walDir(dataDir), walOpts, replay)
	if err != nil {
		return nil, nil, rec, err
	}
	rec.WAL = info
	if ok && info.LastSeq < rec.SnapshotSeq {
		_ = wl.Close()
		// A snapshot is only published once its seq is durable in the WAL,
		// so the WAL ending before it means records were lost.
		return nil, nil, rec, fmt.Errorf("WAL ends at seq %d, before snapshot seq %d", info.LastSeq, rec.SnapshotSeq)
	}
	return eng, wl, rec, nil
}
