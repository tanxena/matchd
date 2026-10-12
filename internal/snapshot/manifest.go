package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

// ReadManifest returns the manifest, or an empty one if there are no
// snapshots yet.
//
// If manifest.json is missing it falls back, in order, to manifest.json.tmp
// and then to scanning the snapshot files themselves. On Windows os.Rename
// is not atomic (its documentation says so), so a crash while the writer
// replaces the manifest could leave neither name in place. The temp file is
// complete and fsynced before the rename, and a .snap file is only ever
// renamed into place after being fsynced, so both are safe to trust
// (decision 010).
func ReadManifest(dataDir string) (Manifest, error) {
	m, found, err := readPublished(dataDir)
	if found || err != nil {
		return m, err
	}
	return scanSnapshots(dataDir)
}

// readPublished reads what the writer last published: manifest.json, or the
// fully written manifest.json.tmp if a crash interrupted the rename. found
// is false if neither exists. The writer uses this, never the directory
// scan: the scan would also see the snapshot file it has just renamed into
// place but not yet listed, and list it twice.
func readPublished(dataDir string) (Manifest, bool, error) {
	m, err := readManifestFile(manifestPath(dataDir))
	if !errors.Is(err, fs.ErrNotExist) {
		return m, err == nil, err
	}
	if m, err := readManifestFile(manifestPath(dataDir) + ".tmp"); err == nil {
		return m, true, nil
	}
	return Manifest{Version: manifestVersion}, false, nil
}

func readManifestFile(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
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

// scanSnapshots rebuilds a manifest from the intact .snap files present,
// keeping the newest Keep. Damaged files are skipped.
func scanSnapshots(dataDir string) (Manifest, error) {
	m := Manifest{Version: manifestVersion}
	entries, err := os.ReadDir(snapshotDir(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	for _, e := range entries { // ReadDir sorts by name, and names are zero-padded seqs
		if !strings.HasSuffix(e.Name(), ".snap") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(snapshotDir(dataDir), e.Name()))
		if err != nil {
			continue
		}
		st, err := Decode(data)
		if err != nil {
			continue
		}
		m.Snapshots = append(m.Snapshots, Entry{Seq: uint64(st.Seq), LastMatchID: st.LastMatchID, File: e.Name()})
	}
	if len(m.Snapshots) > Keep {
		m.Snapshots = m.Snapshots[len(m.Snapshots)-Keep:]
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
