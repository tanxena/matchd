package fsutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	for _, content := range []string{"first", "second, longer than the first", "3"} {
		if err := WriteFileAtomic(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("got %q, %v; want %q", got, err, content)
		}
		if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("temp file left behind: %v", err)
		}
	}
}

// A stray .tmp from an earlier crash doesn't get in the way.
func TestWriteFileAtomicOverStaleTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.json")
	if err := os.WriteFile(path+".tmp", []byte("half-written garbage from a crash"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("ok")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "ok" {
		t.Fatalf("got %q", got)
	}
}

func TestSyncDir(t *testing.T) {
	if err := SyncDir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
}
