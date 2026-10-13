//go:build !windows

package crashtest

import (
	"os/exec"
	"syscall"
)

func prepareInterrupt(*exec.Cmd) {}

// interrupt sends SIGTERM, as a service manager would.
func interrupt(cmd *exec.Cmd) error { return cmd.Process.Signal(syscall.SIGTERM) }
