package daemon

import "golang.org/x/sys/unix"

// getPeerCred uses LOCAL_PEERCRED (uid) and LOCAL_PEERPID (pid).
func getPeerCred(fd int) (peerCred, error) {
	x, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	if err != nil {
		return peerCred{}, err
	}
	pc := peerCred{Known: true, UID: int(x.Uid)}
	if pid, err := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID); err == nil {
		pc.PID = pid
	}
	return pc, nil
}
