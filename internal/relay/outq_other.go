//go:build !linux

package relay

func sendQueueLen(fd uintptr) (int, bool) { return 0, false }
