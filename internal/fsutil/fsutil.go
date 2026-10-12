// Package fsutil holds the small file-system operations durability depends
// on, with their per-platform differences in one place (decision 010).
package fsutil

import (
	"fmt"
	"os"
	"path/filepath"
)

// WriteFileAtomic replaces path with data so that a crash at any moment
// leaves either the old file or the new one, never a mix:
//
//  1. write data to path.tmp and fsync it (the bytes are durable),
//  2. rename path.tmp over path (the switch is a single directory update),
//  3. fsync the directory (the rename itself is durable; Linux only, see
//     SyncDir).
//
// A crash before step 2 leaves a stray path.tmp, which readers ignore and
// the next write replaces.
func WriteFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return SyncDir(filepath.Dir(path))
}
