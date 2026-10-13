//go:build !windows

package sysinfo

import (
	"os/exec"
	"syscall"
)

// The benchmark runs on Windows only (README); these stubs let the bench
// programs build elsewhere.

func ProcessTimes(int) (CPUTimes, error)    { return CPUTimes{}, ErrUnsupported }
func SelfTimes() (CPUTimes, error)          { return CPUTimes{}, ErrUnsupported }
func ReadSystemTimes() (SystemTimes, error) { return SystemTimes{}, ErrUnsupported }
func SetAffinity(int, uint64) error         { return ErrUnsupported }
func Affinity(int) (uint64, error)          { return 0, ErrUnsupported }
func DiskFree(string) (uint64, error)       { return 0, ErrUnsupported }
func ReadPower() (Power, error)             { return Power{}, ErrUnsupported }
func ReadMachine() (Machine, error)         { return Machine{}, ErrUnsupported }

func interruptible(*exec.Cmd) {}

// Interrupt sends SIGINT, which the engine handles like Ctrl-C.
func Interrupt(pid int) error         { return syscall.Kill(pid, syscall.SIGINT) }
func ReadSnapshot() (Snapshot, error) { return Snapshot{}, ErrUnsupported }
