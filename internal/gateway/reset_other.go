//go:build !windows

package gateway

import (
	"errors"
	"syscall"
)

// isConnReset reports whether err means the peer reset or aborted the
// connection. See reset_windows.go for why this is per-platform.
func isConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED)
}
