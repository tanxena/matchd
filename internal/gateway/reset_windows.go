package gateway

import (
	"errors"
	"syscall"
)

// isConnReset reports whether err means the peer reset or aborted the
// connection. On Windows, Winsock reports these as WSAECONNRESET (10054) and
// WSAECONNABORTED (10053), which are not equal to syscall.ECONNRESET there,
// so a portable errors.Is(err, syscall.ECONNRESET) misses them (decision 010).
func isConnReset(err error) bool {
	return errors.Is(err, syscall.Errno(10054)) || errors.Is(err, syscall.Errno(10053))
}
