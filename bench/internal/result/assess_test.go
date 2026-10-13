package result

import (
	"strings"
	"testing"
	"time"

	"github.com/tanxena/matchd/bench/internal/sysinfo"
)

func cleanRun() *Run {
	p := sysinfo.Power{AC: true, SchemeGUID: "g", Personality: "high performance", OverlayGUID: "o"}
	return &Run{
		PowerBefore: p, PowerAfter: p,
		Pinning: Pinning{EngineMask: 0x0F, EngineReadBack: 0x0F, LoadgenMask: 0xF0, LoadgenReadBack: 0xF0},
		Load: &Load{Mode: "open", RatePerSec: 20000, ThroughputPerSec: 20000, RequestedNS: 60e9, MeasuredNS: 60e9,
			Sent: Counts{Passive: 1_000_000}, SendLag: &Latency{P99NS: 20_000}, CPU: CPU{Other: 0.05}},
	}
}

// Each rule, on its own, marks a run disturbed; a clean run passes.
func TestAssess(t *testing.T) {
	if why := Assess(cleanRun()); len(why) != 0 {
		t.Fatalf("clean run judged disturbed: %v", why)
	}
	for name, tc := range map[string]struct {
		spoil func(*Run)
		want  string
	}{
		"battery":       {func(r *Run) { r.PowerBefore.AC, r.PowerAfter.AC = false, false }, "battery"},
		"plan changed":  {func(r *Run) { r.PowerAfter.SchemeGUID = "other" }, "power plan or source changed"},
		"mode changed":  {func(r *Run) { r.PowerAfter.OverlayGUID = "other" }, "power plan or source changed"},
		"affinity":      {func(r *Run) { r.Pinning.EngineReadBack = 0xFF }, "engine affinity"},
		"shared core":   {func(r *Run) { r.Pinning.SharedCores = []int{1} }, "share physical cores"},
		"other procs":   {func(r *Run) { r.Load.CPU = CPU{Other: 0.9, TopOther: []ProcUse{{Name: "chrome.exe", CPUs: 0.8}}} }, "chrome.exe 0.80"},
		"long window":   {func(r *Run) { r.Load.MeasuredNS = 62e9 }, "measured window lasted"},
		"cap cancels":   {func(r *Run) { r.Load.Sent.CapCancel = 10_000 }, "cap cancels"},
		"send lag":      {func(r *Run) { r.Load.SendLag.P99NS = int64(2 * time.Millisecond) }, "fell behind its schedule"},
		"rate":          {func(r *Run) { r.Load.ThroughputPerSec = 15000 }, "wasn't sustained"},
		"slow consumer": {func(r *Run) { r.Load.Engine.SlowConsumers = 1 }, "slow consumers"},
	} {
		r := cleanRun()
		tc.spoil(r)
		why := Assess(r)
		if len(why) != 1 || !strings.Contains(why[0], tc.want) {
			t.Errorf("%s: %v, want one reason containing %q", name, why, tc.want)
		}
	}
	// Clock runs get the other-processes check too (review F-014).
	ck := &Run{PowerBefore: cleanRun().PowerBefore, PowerAfter: cleanRun().PowerAfter,
		Clock: &Clock{Other: 0.9, TopOther: []ProcUse{{Name: "MsMpEng.exe", CPUs: 0.87}}}}
	if why := Assess(ck); len(why) != 1 || !strings.Contains(why[0], "MsMpEng.exe 0.87") {
		t.Errorf("busy clock run: %v", why)
	}
	ck.Clock.Other = 0.05
	if why := Assess(ck); len(why) != 0 {
		t.Errorf("quiet clock run judged disturbed: %v", why)
	}
	// A closed-loop run has no schedule to keep.
	r := cleanRun()
	r.Load.Mode, r.Load.SendLag, r.Load.ThroughputPerSec = "closed", nil, 1
	if why := Assess(r); len(why) != 0 {
		t.Errorf("closed loop judged on open-loop rules: %v", why)
	}
}
