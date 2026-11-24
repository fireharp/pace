package wrapper

// The job container (Linux cgroup v2; platform.JobContainer). A governed
// job's wrapper creates it before the spawn, the child is born inside it
// (CLONE_INTO_CGROUP: no descendant can escape before it is placed) or, where
// the kernel cannot do that, moved into it right after the spawn. The path
// goes to the daemon with the started report; the daemon accounts, caps,
// freezes, throttles and kills through it. After the child's death the
// wrapper asks whether the kernel killed it at the container's hard memory
// limit (reported as ExitRequest.OOMKill, which the policy turns into KILLED
// policy=mem_budget), and removes the container once it is empty.
//
// The daemon decides: its ADMITTED names the container path (its own scope
// of its delegated tree) when it runs jobs in containers, and nothing
// otherwise (macOS; Linux without a delegated cgroup v2 tree: the job is
// governed by its process group and the daemon's status notes say why). A
// path the wrapper cannot use (no write access from the wrapper's own
// cgroup, e.g. a root-owned ssh session scope; PACE_LINUX_CGROUP=0) is
// reported once with an INFO line, and the job runs governed by its process
// group (the daemon's notes list such jobs).

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// jobCgroup is the governed job's container.
type jobCgroup struct {
	c    platform.JobContainer
	id   string
	path string
	fd   int // directory fd for CLONE_INTO_CGROUP; -1 once closed
	// oom/peak: the kernel killed the job at the container's hard limit.
	oom  bool
	peak int64
}

// openContainer creates the job's container at the path ADMITTED gave (no
// path, "" job id or no facility: nil, silently).
func (r *runner) openContainer() *jobCgroup {
	if r.id == "" || r.cgPath == "" || r.o.h.containers == nil {
		return nil // the daemon runs no job containers: its status says so
	}
	if filepath.Base(r.cgPath) != "job-"+r.id || !filepath.IsAbs(r.cgPath) {
		r.containerInfo(fmt.Errorf("container path %q is not this job's", r.cgPath))
		return nil
	}
	c, err := r.o.h.containers(filepath.Dir(r.cgPath))
	if err == nil && c == nil {
		err = errors.New("no container facility")
	}
	if err != nil {
		r.containerInfo(err)
		return nil
	}
	path, err := c.Create(r.id, r.memMax) // the daemon keeps it up to date
	if err != nil {
		r.containerInfo(err)
		return nil
	}
	if filepath.Clean(path) != filepath.Clean(r.cgPath) {
		_ = c.Remove(r.id)
		r.containerInfo(fmt.Errorf("container created at %s, not at the daemon's %s", path, r.cgPath))
		return nil
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		fd = -1 // no CLONE_INTO_CGROUP: attach after the spawn
	}
	return &jobCgroup{c: c, id: r.id, path: path, fd: fd}
}

func (r *runner) containerInfo(err error) {
	r.emit(proto.Event{Verb: proto.VerbInfo, Job: r.id,
		Msg: fmt.Sprintf("no job container (%v): governed by process group", err)})
}

func (cg *jobCgroup) closeFD() {
	if cg != nil && cg.fd >= 0 {
		_ = syscall.Close(cg.fd)
		cg.fd = -1
	}
}

// drop gives up the container after a failed placement.
func (r *runner) dropContainer(err error) {
	cg := r.cg
	r.cg = nil
	cg.closeFD()
	_ = cg.c.Remove(cg.id)
	r.containerInfo(err)
}

// checkOOM records, after the child died of SIGKILL, whether the kernel
// killed it at the container's own hard limit (a machine-wide OOM kill or
// one for an ancestor's limit does not count: see JobContainer.OOMKills).
func (r *runner) checkOOM(ws syscall.WaitStatus) {
	cg := r.cg
	if cg == nil || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		return
	}
	if n, err := cg.c.OOMKills(cg.id); err != nil || n <= 0 {
		return
	}
	cg.oom = true
	if p, err := cg.c.Peak(cg.id); err == nil {
		cg.peak = p
	}
}

// kernelKilled is the KILLED line for a kernel kill at the hard limit when
// the daemon's own (it reports the same) did not arrive: the agent learns
// why either way.
func (r *runner) kernelKilled() proto.Event {
	ev := proto.Event{Verb: proto.VerbKilled, Policy: proto.PolicyMemBudget, Job: r.id, Msg: proto.MsgKernelMemMax}
	if until := r.memWarnUntil.Load(); until > 0 && time.Now().UnixMilli() < until {
		ev.Msg += proto.MsgBeforeWarnDeadline // as the daemon's own line says
	}
	if r.cg != nil && r.cg.peak > 0 {
		ev.UsedBytes = proto.I64(r.cg.peak)
	}
	if r.budget > 0 {
		ev.BudgetBytes = proto.I64(r.budget)
	}
	return ev
}

// startFromBoot places a start time given in clock ticks since boot on the
// wall clock, from simultaneous readings of the wall and boot clocks (Linux
// procStartMS).
func startFromBoot(nowWallMS, nowBootMS int64, ticks uint64, hz int64) int64 {
	return nowWallMS - nowBootMS + int64(ticks)*1000/hz
}

// removeContainer removes the container once nothing is left inside (else
// the daemon removes it when the last descendant has gone).
func (r *runner) removeContainer() {
	cg := r.cg
	if cg == nil {
		return
	}
	cg.closeFD()
	if pids, err := cg.c.PIDs(cg.id); err == nil && len(pids) == 0 {
		_ = cg.c.Remove(cg.id)
	}
}
