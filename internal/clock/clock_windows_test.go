package clock

import (
	"testing"
	"time"
)

// driftBoundPPM is the stated bound on calibration error: TSC-derived
// elapsed time must agree with QPC to within 50 parts per million (0.015 ns
// on a 300 ns command; 150 us over the 3 s window). Calibrating over 100 ms
// with QPC's 100 ns resolution should give a few ppm.
const driftBoundPPM = 50

func TestCalibrationDriftAgainstQPC(t *testing.T) {
	if !invariantTSC() {
		t.Skip("no invariant TSC: Now uses QPC, nothing to drift")
	}
	Init()
	if tsc.Load() == nil {
		t.Fatal("invariant TSC, but calibration was rejected")
	}
	q0, _ := pair()
	n0 := Now() // TSC-derived
	time.Sleep(3 * time.Second)
	q1, _ := pair()
	n1 := Now()
	byQPC := float64(qpcNS(q1) - qpcNS(q0))
	byTSC := float64(n1 - n0)
	ppm := (byTSC - byQPC) / byQPC * 1e6
	t.Logf("over %.3f s: TSC %.0f ns, QPC %.0f ns, drift %.2f ppm (bound %d)", byQPC/1e9, byTSC, byQPC, ppm, driftBoundPPM)
	if ppm > driftBoundPPM || ppm < -driftBoundPPM {
		t.Fatalf("calibration drift %.2f ppm exceeds %d", ppm, driftBoundPPM)
	}
	// The TSC timeline continues QPC's, so a switch between sources (Init
	// running while something measures) can't produce a jump.
	if d := Now() - fallbackNow(); d < -50_000 || d > 50_000 {
		t.Fatalf("TSC and QPC timelines are %d ns apart", d)
	}
}

// Forcing the no-invariant-TSC path: the clock falls back to QPC and still
// works, just at QPC's cost.
func TestForcedQPCFallback(t *testing.T) {
	Init()
	saved, savedInfo := tsc.Load(), info
	defer func() { tsc.Store(saved); info = savedInfo }()

	setup(false) // as if CPUID said "not invariant"
	in := Source()
	if in.Source != "qpc" || tsc.Load() != nil {
		t.Fatalf("forced fallback chose %+v", in)
	}
	t.Logf("fallback: %s, read cost %v, resolution %v", in.Source, in.ReadCost, in.Resolution)
	a := Now()
	time.Sleep(50 * time.Millisecond)
	b := Now()
	if d := time.Duration(b - a); d < 45*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("QPC measured %v over a 50 ms sleep", d)
	}
	if n := testing.AllocsPerRun(100, func() { _ = Now() }); n != 0 {
		t.Fatalf("QPC path allocates %v", n)
	}
}
