package clock

import (
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	qpcProc  = kernel32.NewProc("QueryPerformanceCounter")
	qpfProc  = kernel32.NewProc("QueryPerformanceFrequency")
	qpcFreq  = func() int64 {
		var f int64
		_, _, _ = qpfProc.Call(uintptr(unsafe.Pointer(&f)))
		if f <= 0 {
			panic("clock: QueryPerformanceFrequency failed")
		}
		return f
	}()
)

const fallbackName = "qpc"

var fallbackResolution = time.Duration(1e9 / qpcFreq)

// qpc reads QueryPerformanceCounter. SyscallN with one argument doesn't
// allocate; the cost (about 178 ns here) is the syscall transition.
func qpc() int64 {
	var c int64
	_, _, _ = syscall.SyscallN(qpcProc.Addr(), uintptr(unsafe.Pointer(&c)))
	return c
}

// qpcNS converts QPC ticks to nanoseconds without overflowing: ticks*1e9
// would overflow int64 after about ten days of uptime at 10 MHz.
func qpcNS(t int64) int64 {
	return t/qpcFreq*1e9 + t%qpcFreq*1e9/qpcFreq
}

func fallbackNow() int64 { return qpcNS(qpc()) }

// calibrationWindow is how long calibrate compares the TSC against QPC.
// QPC resolves 100 ns, so each end of the window is uncertain by about that
// much: 2 x 100 ns over 100 ms is 2 ppm.
const calibrationWindow = 100 * time.Millisecond

// calibrate measures the TSC's rate against QPC and returns the conversion,
// or nil to use QPC: when forceQPC is set (no invariant TSC, or a test), or
// the measured rate is implausible.
func calibrate(forceQPC bool) *calibration {
	if forceQPC {
		return nil
	}
	q0, t0 := pair()
	time.Sleep(calibrationWindow)
	q1, t1 := pair()
	if t1 <= t0 || q1 <= q0 {
		return nil
	}
	hz := float64(t1-t0) / (float64(q1-q0) / float64(qpcFreq))
	if hz < 100e6 || hz > 20e9 { // not a CPU clock: something is wrong
		return nil
	}
	return &calibration{
		baseTSC: t0,
		baseNS:  qpcNS(q0), // the TSC timeline continues QPC's
		mult:    uint64(1e9 / hz * (1 << 32)),
		hz:      hz,
	}
}

// pair reads QPC and the TSC at (nearly) the same instant: of several
// tries, it keeps the one whose two QPC reads bracket the TSC read most
// tightly, and uses their midpoint.
func pair() (qpcTicks int64, tscTicks uint64) {
	best := int64(1 << 62)
	for range 16 {
		a := qpc()
		t := rdtsc()
		b := qpc()
		if b-a < best {
			best, qpcTicks, tscTicks = b-a, (a+b)/2, t
		}
	}
	return qpcTicks, tscTicks
}
