package sysinfo

import (
	"os"
	"testing"
	"time"
)

// The personality values map to the three built-in plans, read straight
// from their well-known GUIDs (hidden plans can still be read).
func TestPersonalities(t *testing.T) {
	skipUnlessWindows(t)
	for g, want := range map[string]string{
		"8c5e7fda-e8bf-4a96-9a85-a6e23a8c635c": "high performance",
		"381b4222-f694-41f0-9685-ff5bb260df2e": "balanced",
		"a1841308-3541-4fab-bc81-f71556f20b4a": "power saver",
	} {
		id := mustGUID(g)
		got := schemePersonality(&id)
		if got == "unknown" {
			t.Logf("%s (%s): not readable on this machine", g, want)
			continue
		}
		if got != want {
			t.Errorf("%s: personality %q, want %q", g, got, want)
		}
	}
}

// The process list's offsets are right: this process is in it, named for
// its executable, with the CPU time GetProcessTimes reports; and interrupt
// time moves.
func TestSnapshotMatchesProcessTimes(t *testing.T) {
	for end := time.Now().Add(200 * time.Millisecond); time.Now().Before(end); { // use some CPU
	}
	before, err := SelfTimes()
	if err != nil {
		t.Fatal(err)
	}
	s, err := ReadSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := SelfTimes()
	me, ok := s.Procs[os.Getpid()]
	if !ok {
		t.Fatalf("pid %d not in the %d-process list", os.Getpid(), len(s.Procs))
	}
	if me.CPU < before.Total() || me.CPU > after.Total() {
		t.Errorf("listed CPU %v, GetProcessTimes says %v..%v", me.CPU, before.Total(), after.Total())
	}
	if me.Name != "sysinfo.test.exe" {
		t.Errorf("listed as %q", me.Name)
	}
	if len(s.Procs) < 20 || s.Procs[4].Name != "System" {
		t.Errorf("%d processes; pid 4 is %q", len(s.Procs), s.Procs[4].Name)
	}
	if s.Interrupts <= 0 {
		t.Errorf("interrupt time %v", s.Interrupts)
	}
	t.Logf("%d processes, interrupt+DPC time since boot %v", len(s.Procs), s.Interrupts)
}

func TestOthers(t *testing.T) {
	a := Snapshot{Procs: map[int]ProcCPU{0: {0, "idle", 100}, 7: {7, "engine", 10}, 9: {9, "chrome", 5}, 11: {11, "old", 1}}}
	b := Snapshot{Procs: map[int]ProcCPU{0: {0, "idle", 900}, 7: {7, "engine", 50}, 9: {9, "chrome", 25}, 12: {12, "new", 3}, 11: {11, "old", 1}}}
	got := Others(a, b, 7)
	if len(got) != 2 || got[0].Name != "chrome" || got[0].CPU != 20 || got[1].Name != "new" || got[1].CPU != 3 {
		t.Fatalf("Others = %+v", got)
	}
}
