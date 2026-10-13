//go:build !amd64

package clock

// Without the amd64 TSC read, Now always uses the fallback source.

func rdtsc() uint64 { panic("clock: no TSC on this architecture") }

func invariantTSC() bool { return false }
