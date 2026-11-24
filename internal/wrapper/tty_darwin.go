//go:build darwin

package wrapper

import (
	"syscall"

	"golang.org/x/sys/unix"
)

const ioctlReadTermios = unix.TIOCGETA

// useCgroupFD: no cgroups on macOS.
func useCgroupFD(*syscall.SysProcAttr, int) bool { return false }

// procStartMS returns the kernel's start time of pid in unix ms (0 if
// unknown). It matches what the platform backend reports as Proc.StartMS.
func procStartMS(pid int) int64 {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || int(kp.Proc.P_pid) != pid {
		return 0
	}
	tv := kp.Proc.P_starttime
	return tv.Sec*1000 + int64(tv.Usec)/1000
}

// p_stat values (xnu bsd/sys/proc.h).
const (
	pStatStop = 4 // SSTOP
	pStatZomb = 5 // SZOMB
)

// procStopped reports whether the kernel has pid stopped (p_stat SSTOP).
func procStopped(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp != nil && int(kp.Proc.P_pid) == pid && kp.Proc.P_stat == pStatStop
}

// groupProcs counts the live (not zombie) members of process group pgid,
// and how many of them the kernel has stopped.
func groupProcs(pgid int) (live, stopped int) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return 0, 0
	}
	for i := range kps {
		if int(kps[i].Eproc.Pgid) != pgid || kps[i].Proc.P_stat == pStatZomb {
			continue
		}
		live++
		if kps[i].Proc.P_stat == pStatStop {
			stopped++
		}
	}
	return live, stopped
}
