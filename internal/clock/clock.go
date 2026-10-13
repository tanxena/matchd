// Package clock is the only clock on the hot path (decision 017): the
// engine's per-command latency, WAL fsync latency, and the sequencer's
// command timestamps all read it, and nothing on those paths calls time.Now.
//
// Why it exists: on Windows, Go's time.Now advances in steps of the system
// timer tick (measured: about 515 us here), so it cannot time a matching
// step of about 0.3 us; back-to-back readings are almost always equal, and
// the rest jump by a whole tick. Linux reads a nanosecond clock through the
// vDSO for about 20 ns, so there the package simply uses Go's monotonic
// clock. On Windows it uses:
//
//   - the CPU's timestamp counter (RDTSC, about 12 ns per read here), when
//     CPUID says it is invariant (constant rate, unaffected by frequency
//     scaling or sleep states), calibrated against QueryPerformanceCounter;
//   - otherwise QueryPerformanceCounter itself: 100 ns resolution, but about
//     178 ns per read from Go, because every call crosses the syscall
//     boundary.
//
// Call Init once before relying on Now (server.Start does). Before Init, or
// if calibration fails, Now uses the fallback source, so it is always
// correct, only possibly slower.
package clock

import (
	"math/bits"
	"sync"
	"sync/atomic"
	"time"
)

// Info describes the source Now reads.
type Info struct {
	Source     string        // "tsc", "qpc" or "monotonic"
	TSCHz      float64       // calibrated TSC frequency (tsc only)
	ReadCost   time.Duration // measured cost of one Now() call
	Resolution time.Duration // smallest step the source can show
}

// calibration converts TSC ticks to nanoseconds on the fallback source's
// timeline: ns = baseNS + (tsc - baseTSC) * mult >> 32.
type calibration struct {
	baseTSC uint64
	baseNS  int64
	mult    uint64 // nanoseconds per tick, as 32.32 fixed point
	hz      float64
}

func (c *calibration) ns(tsc uint64) int64 {
	hi, lo := bits.Mul64(tsc-c.baseTSC, c.mult)
	return c.baseNS + int64(hi<<32|lo>>32)
}

var (
	tsc      atomic.Pointer[calibration] // nil: use the fallback source
	initOnce sync.Once
	info     Info
	// Wall time at Init, and Now at the same moment, for WallNanos.
	wallBase int64
	monoBase int64
)

// Now returns monotonic nanoseconds from an arbitrary origin. It does not
// allocate. Differences between two calls are elapsed time.
func Now() int64 {
	if c := tsc.Load(); c != nil {
		return c.ns(rdtsc())
	}
	return fallbackNow()
}

// WallNanos returns the wall-clock time in nanoseconds since the Unix
// epoch, at Now's resolution: the wall time read once at Init plus the
// monotonic time since. It doesn't follow later adjustments of the system
// clock (NTP steps); command timestamps only need to be ordered and
// precise, and replay uses the logged values, never the clock.
func WallNanos() int64 {
	if wallBase == 0 {
		return time.Now().UnixNano() // before Init
	}
	return wallBase + (Now() - monoBase)
}

// Init picks and calibrates the source, once; later calls do nothing. It
// takes about a tenth of a second on Windows (the calibration window).
func Init() Info {
	initOnce.Do(func() { setup(invariantTSC()) })
	return info
}

// setup chooses the source as if the CPU did (or didn't) have an invariant
// TSC. Init uses the real answer; tests force the fallback through here.
func setup(invariant bool) {
	c := calibrate(!invariant)
	tsc.Store(c) // nil selects the fallback
	info = describe(c)
	monoBase = Now()
	wallBase = time.Now().UnixNano()
}

// Source reports what Now reads. Call after Init.
func Source() Info { return info }

// describe measures the chosen source's read cost and resolution.
func describe(c *calibration) Info {
	in := Info{Source: fallbackName, Resolution: fallbackResolution}
	if c != nil {
		in.Source, in.TSCHz = "tsc", c.hz
		in.Resolution = time.Duration(max(1, int64(1e9/c.hz)))
	}
	// Best of three passes: the first runs right after the calibration
	// sleep, while the CPU may still be clocking up. Each pass is averaged
	// over milliseconds, so time.Now's coarseness doesn't matter.
	const n = 200_000
	in.ReadCost = time.Hour
	var sink int64
	for range 3 {
		start := time.Now()
		for range n {
			sink += Now()
		}
		in.ReadCost = min(in.ReadCost, time.Since(start)/n)
	}
	_ = sink
	return in
}
