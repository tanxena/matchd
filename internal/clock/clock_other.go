//go:build !windows

package clock

import "time"

// Outside Windows, Go's monotonic clock already has nanosecond resolution
// (Linux: clock_gettime through the vDSO, about 20 ns), so it is used as is.

const fallbackName = "monotonic"

const fallbackResolution = time.Nanosecond

var origin = time.Now()

func fallbackNow() int64 { return int64(time.Since(origin)) }

func calibrate(bool) *calibration { return nil }
