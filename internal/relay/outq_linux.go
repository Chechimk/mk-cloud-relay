//go:build linux

package relay

import (
	"syscall"
	"unsafe"
)

// tiocoutq is TIOCOUTQ/SIOCOUTQ: bytes in a TCP socket's send queue that the
// peer has not acknowledged yet.
const tiocoutq = 0x5411

func sendQueueLen(fd uintptr) (int, bool) {
	var n int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, tiocoutq, uintptr(unsafe.Pointer(&n))); errno != 0 {
		return 0, false
	}
	return int(n), true
}
