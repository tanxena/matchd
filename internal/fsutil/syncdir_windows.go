package wal

// syncDir is a no-op on Windows. Go's os.File.Sync on a directory handle
// fails there (FlushFileBuffers needs a handle opened for writing, and
// directories can't be opened that way through os.Open), and there is no
// Windows equivalent of fsyncing a directory. NTFS journals metadata changes
// such as file creation, which is what we rely on; this is weaker than the
// Linux guarantee and can't be tested locally (decision 010, decision 011).
func syncDir(string) error { return nil }
