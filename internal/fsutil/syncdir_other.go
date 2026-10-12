//go:build !windows

package wal

import "os"

// syncDir fsyncs a directory so that a newly created file's directory entry
// is durable. On Linux, fsyncing a file makes its contents durable but not
// necessarily its name in the directory; without this, a crash right after
// creating a segment could lose the whole file.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
