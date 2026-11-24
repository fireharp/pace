package wrapper

import (
	"os"
	"strconv"
	"strings"
)

// groupHasComm reports whether a member of process group pgid other than
// its leader runs a program named comm (test helper).
func groupHasComm(pgid int, comm string) bool {
	names, _ := os.ReadDir("/proc")
	for _, n := range names {
		pid, err := strconv.Atoi(n.Name())
		if err != nil || pid == pgid {
			continue
		}
		b, err := os.ReadFile("/proc/" + n.Name() + "/stat")
		if err != nil {
			continue
		}
		if _, pg, ok := statState(b); !ok || pg != pgid {
			continue
		}
		if c, err := os.ReadFile("/proc/" + n.Name() + "/comm"); err == nil && strings.TrimSpace(string(c)) == comm {
			return true
		}
	}
	return false
}
