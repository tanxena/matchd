package result

import (
	"fmt"
	"strings"
	"time"
)

// Limits for Assess: what makes a run's numbers untrustworthy. They are in
// the result files only as their consequences (Disturbed), so changing one
// means re-running benchrun; the report never re-judges.
const (
	// MaxOtherCPU: other processes may use at most this many logical CPUs
	// on average during the window, about 4% of this 8-CPU machine. A quiet
	// desktop still has some: the window manager redrawing the console
	// (dwm.exe), the kernel's System process, services. A browser tab
	// playing media was 1-2 CPUs in the smoke runs.
	MaxOtherCPU = 0.35
	// MaxWindowSkew: the measured window may differ from the requested one
	// by at most this fraction (a longer one means the load generator was
	// descheduled or the machine stalled).
	MaxWindowSkew = 0.02
	// MaxCapCancelShare: cap cancels distort the mix; at most this
	// fraction of commands.
	MaxCapCancelShare = 0.005
	// MaxSendLagP99: open loop, the load generator must keep to its
	// schedule: send lag p99 at most this.
	MaxSendLagP99 = 100 * time.Microsecond
	// MinRateShare: open loop, the answered rate must reach this fraction
	// of the target rate.
	MinRateShare = 0.98
)

// Assess returns why a finished run's numbers can't be trusted (nil if
// they can). It doesn't look at Failed: a failed run has no numbers.
func Assess(r *Run) []string {
	var why []string
	add := func(f string, a ...any) { why = append(why, fmt.Sprintf(f, a...)) }

	if !r.PowerBefore.AC || !r.PowerAfter.AC {
		add("on battery power")
	}
	if b, a := r.PowerBefore, r.PowerAfter; b.SchemeGUID != a.SchemeGUID || b.OverlayGUID != a.OverlayGUID || b.AC != a.AC {
		add("power plan or source changed during the run (%s -> %s)", b, a)
	}
	p := r.Pinning
	if p.EngineMask != 0 && p.EngineReadBack != p.EngineMask {
		add("engine affinity reads back as %#x, wanted %#x", p.EngineReadBack, p.EngineMask)
	}
	if p.LoadgenMask != 0 && p.LoadgenReadBack != p.LoadgenMask {
		add("loadgen affinity reads back as %#x, wanted %#x", p.LoadgenReadBack, p.LoadgenMask)
	}
	if len(p.SharedCores) > 0 && p.EngineMask != 0 {
		add("engine and loadgen masks share physical cores %v", p.SharedCores)
	}

	l := r.Load
	if l == nil {
		return why
	}
	if l.CPU.Other > MaxOtherCPU {
		var top []string
		for _, p := range l.CPU.TopOther {
			top = append(top, fmt.Sprintf("%s %.2f", p.Name, p.CPUs))
		}
		add("other processes used %.2f CPUs on average (limit %.2f): %s", l.CPU.Other, MaxOtherCPU, strings.Join(top, ", "))
	}
	if req := float64(l.RequestedNS); req > 0 {
		if skew := float64(l.MeasuredNS)/req - 1; skew > MaxWindowSkew || skew < -MaxWindowSkew {
			add("measured window lasted %v, requested %v", time.Duration(l.MeasuredNS), time.Duration(l.RequestedNS))
		}
	}
	if n := l.Sent.Total(); n > 0 && float64(l.Sent.CapCancel)/float64(n) > MaxCapCancelShare {
		add("%d cap cancels of %d commands: the book outgrew its cap, so the mix was distorted", l.Sent.CapCancel, n)
	}
	if l.Mode == "open" {
		if l.SendLag != nil && time.Duration(l.SendLag.P99NS) > MaxSendLagP99 {
			add("load generator fell behind its schedule: send lag p99 %v (limit %v)", time.Duration(l.SendLag.P99NS), MaxSendLagP99)
		}
		if l.RatePerSec > 0 && l.ThroughputPerSec < MinRateShare*float64(l.RatePerSec) {
			add("answered %.0f/s of the %d/s target: the rate wasn't sustained", l.ThroughputPerSec, l.RatePerSec)
		}
	}
	if l.Engine.SlowConsumers > 0 {
		add("engine disconnected %v slow consumers", l.Engine.SlowConsumers)
	}
	return why
}
