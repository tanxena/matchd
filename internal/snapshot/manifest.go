package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tanxena/matchd/internal/fsutil"
)

// Entry is one snapshot listed in the manifest.
type Entry struct {
	Seq         uint64 `json:"seq"`           // last command included
	LastMatchID uint64 `json:"last_match_id"` // fills up to here are covered by this snapshot
	File        string `json:"file"`          // name inside <data>/snapshots
}

// Manifest lists the snapshots that are complete and durable, oldest first.
// A snapshot file exists before it is listed; only listed snapshots are used
// for recovery, so a crash while writing one is harmless.
type Manifest struct {
	Version   int     `json:"version"`
	Snapshots []Entry `json:"snapshots"`
}

const manifestVersion = 1

func manifestPath(dataDir string) string { return filepath.Join(dataDir, "manifest.json") }
func snapshotDir(dataDir string) string  { return filepath.Join(dataDir, "snapshots") }

// ReadManifest returns the manifest, or an empty one if none exists yet.
func ReadManifest(dataDir string) (Manifest, error) {
	data, err := os.ReadFile(manifestPath(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return Manifest{Version: manifestVersion}, nil
	}
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("snapshot: manifest: %w", err)
	}
	if m.Version != manifestVersion {
		return Manifest{}, fmt.Errorf("snapshot: manifest version %d, want %d", m.Version, manifestVersion)
	}
	for i := 1; i < len(m.Snapshots); i++ {
		if m.Snapshots[i].Seq <= m.Snapshots[i-1].Seq {
			return Manifest{}, fmt.Errorf("snapshot: manifest entries out of order at %d", i)
		}
	}
	return m, nil
}

// WriteManifest replaces the manifest atomically.
func WriteManifest(dataDir string, m Manifest) error {
	m.Version = manifestVersion
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(manifestPath(dataDir), append(data, '\n'))
}

// ReadSnapshot reads and decodes one snapshot file listed in the manifest.
func ReadSnapshot(dataDir string, e Entry) (State, error) {
	data, err := os.ReadFile(filepath.Join(snapshotDir(dataDir), e.File))
	if err != nil {
		return State{}, err
	}
	st, err := Decode(data)
	if err != nil {
		return State{}, err
	}
	if uint64(st.Seq) != e.Seq || st.LastMatchID != e.LastMatchID {
		return State{}, fmt.Errorf("%w: %s holds seq %d / match %d, manifest says %d / %d",
			ErrCorruptSnapshot, e.File, st.Seq, st.LastMatchID, e.Seq, e.LastMatchID)
	}
	return st, nil
}
