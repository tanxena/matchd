package clock

// rdtsc reads the CPU's timestamp counter (tsc_amd64.s).
func rdtsc() uint64

// cpuid executes CPUID (tsc_amd64.s).
func cpuid(leaf, sub uint32) (eax, ebx, ecx, edx uint32)

// invariantTSC reports whether CPUID advertises an invariant TSC (leaf
// 0x80000007, EDX bit 8): one that ticks at a constant rate regardless of
// frequency scaling and sleep states, and (on such CPUs) is synchronised
// across cores. Without it, TSC differences are not time.
func invariantTSC() bool {
	maxExt, _, _, _ := cpuid(0x80000000, 0)
	if maxExt < 0x80000007 {
		return false
	}
	_, _, _, edx := cpuid(0x80000007, 0)
	return edx&(1<<8) != 0
}
