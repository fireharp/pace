//go:build linux

package main

import (
	"net"

	"golang.org/x/sys/unix"
)

// totalMemory returns physical memory in bytes (0 if unknown).
func totalMemory() int64 {
	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err != nil {
		return 0
	}
	return int64(si.Totalram) * int64(si.Unit)
}

// peerPID returns the pid of the process on the other end of a unix socket.
func peerPID(c *net.UnixConn) (int, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var serr error
	err = raw.Control(func(fd uintptr) {
		cred, serr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return int(cred.Pid), nil
}
