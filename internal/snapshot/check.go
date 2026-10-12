package snapshot

import (
	"fmt"
	"reflect"

	"github.com/tanxena/matchd/internal/engine"
	"github.com/tanxena/matchd/internal/wal"
)

// Report is what replay-check found.
type Report struct {
	BaseSeq          uint64 // replay started here (0 = from an empty engine)
	SnapshotSeq      uint64 // the newest snapshot, compared with the replay (0 = none)
	SnapshotCompared bool
	WALFirst         uint64
	WALLast          uint64
	WALTornBytes     int64 // a torn tail the running engine would truncate
	Replayed         int   // WAL records replayed
	Matches          int   // matches produced by the replay
	FillsChecked     int   // fill-log records compared with the replay
	FillsBeforeBase  int   // fill-log records older than the replay's base (not checkable)
	FillLogTornBytes int64
}

func (r Report) String() string {
	return fmt.Sprintf("replayed WAL %d..%d (%d records) from base %d; snapshot %d compared=%v; %d matches; fill log: %d checked, %d before base",
		r.WALFirst, r.WALLast, r.Replayed, r.BaseSeq, r.SnapshotSeq, r.SnapshotCompared, r.Matches, r.FillsChecked, r.FillsBeforeBase)
}

// CheckResult carries the replay's outcome for callers that verify more
// (the crash harness compares client-received fills against Matches).
type CheckResult struct {
	Report
	End     State            // the engine state after replaying the whole WAL
	Matches map[uint64]Match // every match the replay produced, by id
}

// ReplayCheck runs the WAL through a fresh engine and compares the result
// against the engine's own snapshot and fill log (PRD 5.4). It only reads
// the data directory. The engine must not be running.
//
//   - Base: an empty engine if the WAL still starts at seq 1 (the strongest
//     check: all of history), otherwise the oldest listed snapshot.
//   - Replaying the WAL from the base, the state at the newest snapshot's seq
//     must equal that snapshot exactly.
//   - Every fill-log record after the base must equal the match the replay
//     produced with the same id. Matches missing from the fill log are
//     allowed (released after the last fill-log write, or never released),
//     so this catches wrong and duplicated fills, not missing ones.
func ReplayCheck(dataDir string) (CheckResult, error) {
	var res CheckResult
	m, err := ReadManifest(dataDir)
	if err != nil {
		return res, err
	}

	// Find where the WAL starts without replaying anything yet.
	_, info, err := wal.Open(walDir(dataDir), wal.Options{ReadOnly: true}, nil)
	if err != nil {
		return res, fmt.Errorf("WAL: %w", err)
	}
	res.WALFirst, res.WALLast, res.WALTornBytes = info.FirstSeq, info.LastSeq, info.TornBytes

	eng := engine.New()
	var baseMatch uint64
	switch {
	case info.FirstSeq <= 1:
		// Replay all of history.
	case len(m.Snapshots) > 0:
		base := m.Snapshots[0]
		st, err := ReadSnapshot(dataDir, base)
		if err != nil {
			return res, fmt.Errorf("base snapshot: %w", err)
		}
		if eng, err = engine.Restore(st); err != nil {
			return res, fmt.Errorf("base snapshot: %w", err)
		}
		res.BaseSeq, baseMatch = base.Seq, base.LastMatchID
	default:
		return res, fmt.Errorf("WAL starts at seq %d but no snapshot covers what came before", info.FirstSeq)
	}

	var newest *Entry
	var want State
	if n := len(m.Snapshots); n > 0 && m.Snapshots[n-1].Seq > res.BaseSeq {
		newest = &m.Snapshots[n-1]
		if want, err = ReadSnapshot(dataDir, *newest); err != nil {
			return res, fmt.Errorf("newest snapshot: %w", err)
		}
		res.SnapshotSeq = newest.Seq
	}

	res.Matches = map[uint64]Match{}
	var cmd engine.Command
	var evs []engine.Event
	var taker engine.Event
	compare := func() error {
		if newest != nil && uint64(eng.LastSeq()) == newest.Seq {
			if got := eng.State(); !reflect.DeepEqual(got, want) {
				return fmt.Errorf("replay at seq %d differs from snapshot %s", newest.Seq, newest.File)
			}
			res.SnapshotCompared = true
		}
		return nil
	}
	if err := compare(); err != nil { // the base may itself be the newest
		return res, err
	}
	_, _, err = wal.Open(walDir(dataDir), wal.Options{ReadOnly: true}, func(r wal.Record) error {
		if r.Seq <= res.BaseSeq {
			return nil
		}
		if err := engine.DecodeCommand(r.Payload, &cmd); err != nil {
			return err
		}
		cmd.Seq, cmd.Time = engine.Seq(r.Seq), engine.Timestamp(r.Time)
		if cmd.Seq != eng.LastSeq()+1 {
			return fmt.Errorf("WAL record %d does not follow seq %d", cmd.Seq, eng.LastSeq())
		}
		evs = eng.Step(&cmd, evs[:0])
		res.Replayed++
		for _, e := range evs {
			if e.Kind != engine.EvFill {
				continue
			}
			if e.Role == engine.RoleTaker {
				taker = e
			} else {
				res.Matches[e.MatchID] = MatchOf(taker, e)
			}
		}
		return compare()
	})
	if err != nil {
		return res, err
	}
	if newest != nil && !res.SnapshotCompared {
		return res, fmt.Errorf("WAL ends at seq %d, before the newest snapshot (seq %d)", info.LastSeq, newest.Seq)
	}
	res.Report.Matches = len(res.Matches)
	res.End = eng.State()

	fills, finfo, err := ReadFillLog(dataDir)
	if err != nil {
		return res, fmt.Errorf("fill log: %w", err)
	}
	res.FillLogTornBytes = finfo.TornBytes
	for _, f := range fills {
		if f.MatchID <= baseMatch {
			res.FillsBeforeBase++
			continue
		}
		got, ok := res.Matches[f.MatchID]
		if !ok {
			// A released fill whose command isn't in the WAL: an ack escaped
			// before durability, or the WAL lost acknowledged data.
			return res, fmt.Errorf("fill log has match %d (command seq %d), which the WAL replay never produced", f.MatchID, f.Seq)
		}
		if got != f {
			return res, fmt.Errorf("fill log match %d differs from the replay:\n log    %+v\n replay %+v", f.MatchID, f, got)
		}
		res.FillsChecked++
	}
	return res, nil
}
