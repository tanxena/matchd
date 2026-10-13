package crashtest

import (
	"os/exec"
	"syscall"
)

// On Windows a console program gets Ctrl-C/Ctrl-Break, which Go delivers as
// os.Interrupt. The child gets its own process group, so the event can be
// sent to it alone.
func prepareInterrupt(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

var generateCtrlEvent = syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")

// interrupt sends Ctrl-Break to the child's process group. It fails if
// this process has no console to share (then the test is skipped).
func interrupt(cmd *exec.Cmd) error {
	const ctrlBreakEvent = 1
	if r, _, err := generateCtrlEvent.Call(ctrlBreakEvent, uintptr(cmd.Process.Pid)); r == 0 {
		return err
	}
	return nil
}
