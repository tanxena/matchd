package sysinfo

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func skipUnlessWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the benchmark's OS layer is Windows-only")
	}
}

func TestMachine(t *testing.T) {
	skipUnlessWindows(t)
	m, err := ReadMachine()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s; %d logical CPUs on %d cores, %d package(s); %.1f GB; %s", m.CPU, m.Logical, len(m.Cores), m.Packages, float64(m.RAMBytes)/(1<<30), m.OS)
	for i, c := range m.Cores {
		t.Logf("core %d: logical %v", i, c.Logical)
	}
	for _, c := range m.Caches {
		t.Logf("L%d %s %d KB: logical %v", c.Level, c.Type, c.Bytes>>10, c.Logical)
	}
	if m.Logical != runtime.NumCPU() {
		t.Errorf("%d logical CPUs from the cores, runtime.NumCPU says %d", m.Logical, runtime.NumCPU())
	}
	if m.CPU == "" || m.RAMBytes == 0 || m.OS == "" || len(m.Cores) == 0 {
		t.Errorf("incomplete: %+v", m)
	}
	// Every logical CPU is on exactly one core.
	seen := map[int]bool{}
	for _, c := range m.Cores {
		for _, l := range c.Logical {
			if seen[l] {
				t.Errorf("logical CPU %d on two cores", l)
			}
			seen[l] = true
		}
	}
}

func TestSharedCores(t *testing.T) {
	m := Machine{Cores: []Core{{Logical: []int{0, 1}}, {Logical: []int{2, 3}}, {Logical: []int{4, 5}}, {Logical: []int{6, 7}}}}
	if s := m.SharedCores(0x0F, 0xF0); len(s) != 0 {
		t.Errorf("0x0F/0xF0 share cores %v", s)
	}
	if s := m.SharedCores(0x07, 0xF8); len(s) != 1 || s[0] != 1 {
		t.Errorf("0x07/0xF8: shared %v, want [1]", s)
	}
	// Hyperthreads numbered the other way: 0..3 then their siblings 4..7.
	m2 := Machine{Cores: []Core{{Logical: []int{0, 4}}, {Logical: []int{1, 5}}, {Logical: []int{2, 6}}, {Logical: []int{3, 7}}}}
	if s := m2.SharedCores(0x0F, 0xF0); len(s) != 4 {
		t.Errorf("sibling-split masks share %v, want all 4 cores", s)
	}
}

func TestParseMask(t *testing.T) {
	for in, want := range map[string]uint64{"0F": 0x0F, "0xF0": 0xF0, "f0": 0xF0, " 3 ": 3} {
		if got, err := ParseMask(in); err != nil || got != want {
			t.Errorf("ParseMask(%q) = %#x, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "xyz"} {
		if _, err := ParseMask(bad); err == nil {
			t.Errorf("ParseMask(%q) accepted", bad)
		}
	}
	if got := MaskCPUs(0xF0); len(got) != 4 || got[0] != 4 || got[3] != 7 {
		t.Errorf("MaskCPUs(0xF0) = %v", got)
	}
}

// Pin a child process, read the mask back, and see its CPU time.
func TestAffinityAndTimes(t *testing.T) {
	skipUnlessWindows(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperSpin$")
	cmd.Env = append(os.Environ(), "SYSINFO_SPIN=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := SetAffinity(cmd.Process.Pid, 0x2); err != nil {
		t.Fatal(err)
	}
	got, err := Affinity(cmd.Process.Pid)
	if err != nil || got != 0x2 {
		t.Fatalf("affinity read back %#x, %v; want 0x2", got, err)
	}
	time.Sleep(300 * time.Millisecond)
	ct, err := ProcessTimes(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if ct.Total() < 50*time.Millisecond {
		t.Errorf("spinning child used only %v CPU", ct.Total())
	}
	self, err := SelfTimes()
	if err != nil || self.Total() <= 0 {
		t.Errorf("self times %v, %v", self, err)
	}
	s1, err := ReadSystemTimes()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	s2, _ := ReadSystemTimes()
	// Wall time × CPUs = busy + idle, within timer granularity.
	total := (s2.Busy - s1.Busy) + (s2.Idle - s1.Idle)
	if want := 100 * time.Millisecond * time.Duration(runtime.NumCPU()); total < want/2 || total > want*2 {
		t.Errorf("system time advanced %v over 100 ms on %d CPUs", total, runtime.NumCPU())
	}
}

func TestHelperSpin(t *testing.T) {
	if os.Getenv("SYSINFO_SPIN") == "" {
		t.Skip("helper process")
	}
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); {
	}
}

func TestPower(t *testing.T) {
	skipUnlessWindows(t)
	p, err := ReadPower()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s; scheme %s, overlay %s", p, p.SchemeGUID, p.OverlayGUID)
	if p.SchemeGUID == "" || p.SchemeName == "" || p.SchemeName == "?" {
		t.Errorf("no active scheme: %+v", p)
	}
}
