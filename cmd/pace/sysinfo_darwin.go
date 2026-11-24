//go:build darwin

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// totalMemory returns physical memory in bytes (0 if unknown).
func totalMemory() int64 {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(v)
}

// peerPID returns the pid of the process on the other end of a unix socket.
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	pid, serr := 0, error(nil)
	err = raw.Control(func(fd uintptr) {
		pid, serr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	})
	if err != nil {
		return 0, err
	}
	return pid, serr
}
