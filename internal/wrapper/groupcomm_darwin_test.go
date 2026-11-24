package wrapper

import (
	"bytes"

	"golang.org/x/sys/unix"
)

// groupHasComm reports whether a member of process group pgid other than
// its leader runs a program named comm (test helper).
func groupHasComm(pgid int, comm string) bool {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false
	}
	for i := range kps {
		p := &kps[i]
		if int(p.Proc.P_pid) == pgid || int(p.Eproc.Pgid) != pgid {
			continue
		}
		name := p.Proc.P_comm[:]
		if j := bytes.IndexByte(name, 0); j >= 0 {
			name = name[:j]
		}
		if string(name) == comm {
			return true
		}
	}
	return false
}
