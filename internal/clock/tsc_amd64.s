#include "textflag.h"

// func rdtsc() uint64
// LFENCE keeps the read from being reordered before earlier instructions,
// so a measurement doesn't start early.
TEXT ·rdtsc(SB),NOSPLIT,$0-8
	LFENCE
	RDTSC
	SHLQ	$32, DX
	ORQ	DX, AX
	MOVQ	AX, ret+0(FP)
	RET

// func cpuid(leaf, sub uint32) (eax, ebx, ecx, edx uint32)
TEXT ·cpuid(SB),NOSPLIT,$0-24
	MOVL	leaf+0(FP), AX
	MOVL	sub+4(FP), CX
	CPUID
	MOVL	AX, eax+8(FP)
	MOVL	BX, ebx+12(FP)
	MOVL	CX, ecx+16(FP)
	MOVL	DX, edx+20(FP)
	RET
