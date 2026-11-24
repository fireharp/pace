package daemon

import "golang.org/x/sys/unix"

// getPeerCred uses SO_PEERCRED.
func getPeerCred(fd int) (peerCred, error) {
	u, err := unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
	if err != nil {
		return peerCred{}, err
	}
	return peerCred{Known: true, UID: int(u.Uid), PID: int(u.Pid)}, nil
}
