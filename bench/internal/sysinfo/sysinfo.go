// Package sysinfo is the benchmark's window onto the operating system:
// process and system CPU time, CPU affinity, graceful interrupts, the power
// plan, and a description of the hardware (decision 021).
//
// Only Windows is implemented: the benchmark tables are Windows-only
// (README). Elsewhere every function returns ErrUnsupported, so the bench
// programs still build (make ci cross-compiles them for Linux).
package sysinfo

import (
	"errors"
	"fmt"
	"math/bits"
	"os/exec"
	"strings"
	"time"
)

// ErrUnsupported is returned on platforms the benchmark doesn't run on.
var ErrUnsupported = errors.New("sysinfo: not supported on this platform")

// CPUTimes is CPU time used by a process, or by the whole machine.
type CPUTimes struct {
	Kernel time.Duration
	User   time.Duration
}

// Total is kernel plus user time.
func (t CPUTimes) Total() time.Duration { return t.Kernel + t.User }

// SystemTimes is the machine's CPU time summed over all logical CPUs since
// boot. Busy is everything that isn't the idle loop.
type SystemTimes struct {
	Idle time.Duration
	Busy time.Duration
}

// Power describes the power state at one moment.
type Power struct {
	// AC: on mains power. Battery: percent charge, -1 if unknown or none.
	AC      bool `json:"ac"`
	Battery int  `json:"battery_percent"`
	// The active power plan, and its personality (what kind of plan it
	// is, which survives renaming or duplicating it): "high performance",
	// "balanced", "power saver", or "unknown".
	SchemeGUID  string `json:"scheme_guid"`
	SchemeName  string `json:"scheme_name"`
	Personality string `json:"personality"`
	// The Windows 10/11 power-mode overlay ("best performance", ...) on top
	// of the plan; "none" if not set, "" if the API is missing.
	OverlayGUID string `json:"overlay_guid"`
	Overlay     string `json:"overlay"`
}

// String is a one-line summary for logs and the hardware statement.
func (p Power) String() string {
	src := "battery"
	if p.AC {
		src = "AC power"
	}
	s := fmt.Sprintf("%s, %q plan (%s)", src, p.SchemeName, p.Personality)
	if p.Overlay != "" && p.Overlay != "none" {
		s += ", power mode " + p.Overlay
	}
	return s
}

// Core is one physical core and the logical CPUs (hyperthreads) on it.
type Core struct {
	Logical []int `json:"logical"`
}

// Cache is a cache level shared by a set of logical CPUs.
type Cache struct {
	Level   int    `json:"level"`
	Type    string `json:"type"` // unified, data, instruction
	Bytes   int64  `json:"bytes"`
	Logical []int  `json:"logical"`
}

// Machine describes the hardware and OS.
type Machine struct {
	CPU      string  `json:"cpu"`
	Logical  int     `json:"logical_cpus"`
	Cores    []Core  `json:"cores"`
	Packages int     `json:"packages"`
	Caches   []Cache `json:"caches"`
	RAMBytes uint64  `json:"ram_bytes"`
	OS       string  `json:"os"`
}

// SharedCores returns the physical cores that have logical CPUs in both
// masks: if it's empty, two processes pinned to a and b never share a core
// (they can still share caches outside the core, such as L3).
func (m Machine) SharedCores(a, b uint64) []int {
	var shared []int
	for i, c := range m.Cores {
		inA, inB := false, false
		for _, l := range c.Logical {
			inA = inA || a&(1<<l) != 0
			inB = inB || b&(1<<l) != 0
		}
		if inA && inB {
			shared = append(shared, i)
		}
	}
	return shared
}

// MaskCPUs lists the logical CPUs in an affinity mask.
func MaskCPUs(mask uint64) []int {
	var cpus []int
	for mask != 0 {
		i := bits.TrailingZeros64(mask)
		cpus = append(cpus, i)
		mask &^= 1 << i
	}
	return cpus
}

// ParseMask reads an affinity mask in the hex form `start /affinity` takes
// ("0F", "0xF0").
func ParseMask(s string) (uint64, error) {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "0x")
	var m uint64
	if _, err := fmt.Sscanf(s, "%x", &m); err != nil || m == 0 {
		return 0, fmt.Errorf("bad affinity mask %q (want hex, e.g. 0F)", s)
	}
	return m, nil
}

// Interruptible prepares cmd so Interrupt can stop it gracefully: on
// Windows, a process group of its own (CTRL_BREAK goes to a group).
func Interruptible(cmd *exec.Cmd) { interruptible(cmd) }

// ProcCPU is one process's CPU time.
type ProcCPU struct {
	PID  int           `json:"pid"`
	Name string        `json:"name"`
	CPU  time.Duration `json:"cpu_ns"`
}

// Snapshot is every process's CPU time, and the time all CPUs spent in
// interrupts and deferred procedure calls (DPCs), at one moment. Interrupt
// and DPC time (the network stack's, for one) belongs to no process, so it
// is kept apart from "other processes".
type Snapshot struct {
	Procs      map[int]ProcCPU
	Interrupts time.Duration // interrupt + DPC time, summed over CPUs
}

// Others returns the CPU time processes other than those in skip used
// between a and b, largest first. A process absent from a (started during
// the window) counts from zero. The idle process (pid 0) is never counted.
func Others(a, b Snapshot, skip ...int) []ProcCPU {
	var out []ProcCPU
next:
	for pid, p := range b.Procs {
		if pid == 0 {
			continue
		}
		for _, s := range skip {
			if pid == s {
				continue next
			}
		}
		if prev, ok := a.Procs[pid]; ok && prev.Name == p.Name {
			p.CPU -= prev.CPU
		}
		if p.CPU > 0 {
			out = append(out, p)
		}
	}
	sortProcs(out)
	return out
}

func sortProcs(p []ProcCPU) {
	for i := 1; i < len(p); i++ { // insertion sort: a few hundred entries, once per window
		for j := i; j > 0 && p[j].CPU > p[j-1].CPU; j-- {
			p[j], p[j-1] = p[j-1], p[j]
		}
	}
}
