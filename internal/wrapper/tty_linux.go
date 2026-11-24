//go:build linux

package wrapper

import (
	"bytes"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const ioctlReadTermios = unix.TCGETS

// useCgroupFD makes the spawn create the child inside the cgroup directory
// fd (clone3 with CLONE_INTO_CGROUP, Linux >= 5.7).
func useCgroupFD(sys *syscall.SysProcAttr, fd int) bool {
	sys.UseCgroupFD = true
	sys.CgroupFD = fd
	return true
}

// clkTck is USER_HZ, fixed at 100 by the Linux ABI on every architecture.
const clkTck = 100

// procStartMS returns pid's start time on the wall clock in unix ms (0 if
// unknown): /proc/<pid>/stat field 22 (clock ticks since boot, on
// CLOCK_BOOTTIME) placed on the wall clock through CLOCK_BOOTTIME now.
// /proc/stat btime is not used: it is whole seconds, truncated, so a start
// derived from it is up to 1 s early, and the job's wall-clock limit (the
// policy measures it from this start) fired up to 1 s before the limit.
// Within that second it agrees with the platform backend's Proc.StartMS
// (btime based), which is all attribution needs from it (docs/ARCHITECTURE.md:
// the leader of a restored job is matched within 3 s).
func procStartMS(pid int) int64 {
	if pid <= 0 {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	f := bytes.Fields(b[i+1:]) // f[0] is field 3 (state)
	if len(f) < 20 {
		return 0
	}
	ticks, err := strconv.ParseUint(string(f[19]), 10, 64)
	if err != nil {
		return 0
	}
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0
	}
	return startFromBoot(time.Now().UnixMilli(), ts.Nano()/1e6, ticks, clkTck)
}

// procStopped reports whether the kernel has pid stopped (/proc/<pid>/stat
// state T, or t when traced).
func procStopped(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	st, _, ok := statState(b)
	return ok && (st == 'T' || st == 't')
}

// statState parses a /proc/<pid>/stat line: the state and the process group
// (fields 3 and 5; the command name before them may hold any byte).
func statState(b []byte) (state byte, pgrp int, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return 0, 0, false
	}
	f := bytes.Fields(b[i+2:])
	if len(f) < 3 || len(f[0]) != 1 {
		return 0, 0, false
	}
	pg, err := strconv.Atoi(string(f[2]))
	if err != nil {
		return 0, 0, false
	}
	return f[0][0], pg, true
}

// groupProcs counts the live (not zombie) members of process group pgid,
// and how many of them the kernel has stopped (state T, or t when traced).
func groupProcs(pgid int) (live, stopped int) {
	d, err := os.Open("/proc")
	if err != nil {
		return 0, 0
	}
	names, _ := d.Readdirnames(-1)
	d.Close()
	for _, n := range names {
		if n == "" || n[0] < '0' || n[0] > '9' {
			continue
		}
		b, err := os.ReadFile("/proc/" + n + "/stat")
		if err != nil {
			continue // gone meanwhile
		}
		st, pg, ok := statState(b)
		if !ok || pg != pgid || st == 'Z' || st == 'X' {
			continue
		}
		live++
		if st == 'T' || st == 't' {
			stopped++
		}
	}
	return live, stopped
}
