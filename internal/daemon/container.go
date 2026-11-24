package daemon

// Linux job containers (platform.JobContainer, cgroup v2).
//
// The wrapper creates a job's container and spawns the child into it, then
// reports its path with the started (or attach) report. The daemon adopts it
// only when the path is the one its own container facility gives for that
// container id (same delegated tree): from then on
//
//   - attribution: every process inside is the job's (rule C), whatever it
//     did to its group, session or environment, and the job's usage is the
//     container's (exact: shared pages once, tmpfs included);
//   - memory.max: budget*(1+tolerance) in enforce mode (enforce.hard_cap),
//     the line above which mem_budget kills at once; the kernel kill is
//     reported by the wrapper (ExitRequest.OOMKill) and becomes KILLED
//     policy=mem_budget; lifted in observe/warn-only mode, when the job ends
//     and on shutdown;
//   - freeze: cgroup.freeze instead of SIGSTOP for the processes inside;
//     members outside (none, unless something moved itself out) still get
//     SIGSTOP. Thawed by cgroup.freeze=0 whenever the state no longer has
//     the job frozen (and on shutdown, and when the job ends);
//   - throttle: cpu.weight/io.weight 1 in addition to the per-thread
//     sched/ioprio throttle, restored with it;
//   - kill: cgroup.kill with every SIGKILL (the policy's signals still go
//     out too);
//   - removal once the job has ended and the container is empty.
//
// A restarted daemon finds the containers of the running jobs it restores by
// job id (and an attaching wrapper reports its path again). Their freeze and
// throttle state is unknown then, so it is treated as set: the first
// reconcile releases what the state does not want. For a job the state calls
// frozen, cgroup.freeze is read back every tick (pace run thaws its container
// when it loses the daemon or forwards a signal): one found thawed is frozen
// again, with the same "frozen again" FROZEN line as a SIGSTOPped job.
//
// The containers live in this daemon's own scope (platform.ContainerScope of
// its socket): another daemon of the same user (a test daemon beside the
// installed one) has a scope of its own, so neither ever lists, releases or
// removes the other's containers. ADMITTED gives the wrapper the job's path,
// so the wrapper never has to work the base out itself.
//
// pace's own processes are never the job's: a wrapper of another governed
// job (a nested pace run) or the daemon itself found inside a container is
// moved out (Evict) before the container is frozen or killed, and every
// tick, so neither cgroup.freeze, cgroup.kill nor the kernel's memory.max
// kill (memory.oom.group) reaches it, as no signal ever does (unsafePID).

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// cgJob is the daemon's record of one job's container.
type cgJob struct {
	cid       string // container id (job id the wrapper created it for)
	capBytes  int64  // memory.max we set; -1 = unknown
	frozen    bool   // cgroup.freeze=1 written (or unknown after a restart)
	throttled bool   // weights lowered
	wKnown    bool   // throttled is what the container has (false after a restart or re-attach)
	pids      []int  // this tick's cgroup.procs
	inside    map[int]bool
	ended     bool // the job left the state: cleanup pending
	// removeFails counts failed removals of an ended job's container;
	// removeAtMS is when the next one may be tried.
	removeFails int
	removeAtMS  int64
	// paceInside: a pace process (wrapper, daemon) could not be moved out,
	// so the container is never frozen or killed as a whole: the job's
	// processes get the per-process signals, which never reach pace's own.
	paceInside bool
}

func (c *cgJob) has(pid int) bool { return c != nil && c.inside[pid] }

// freezes reports whether a freeze of the job reaches pid through
// cgroup.freeze (so no SIGSTOP is sent or recorded for it).
func (c *cgJob) freezes(pid int) bool { return c.has(pid) && !c.paceInside }

// initContainers probes the backend's job containers (once, at start).
func (d *daemon) initContainers() {
	d.cg = map[string]*cgJob{}
	cp, ok := d.backend.(platform.ContainerProvider)
	if !ok {
		return
	}
	c, ok := cp.Containers(platform.ContainerScope(d.sockPath))
	if !ok {
		// Only a backend that can say why (Linux) gets the note.
		if n, ok := d.backend.(platform.ContainerNoter); ok {
			d.contReason = n.ContainersReason()
		}
		return
	}
	d.cont = c
	if wn, ok := c.(interface{ Weights() string }); ok {
		d.contWeights = wn.Weights()
	}
}

// containerNote is the status note about job containers ("" on platforms
// without them).
func (d *daemon) containerNote() string {
	switch {
	case d.cont != nil:
		capNote := "memory.max = budget + tolerance in enforce mode"
		if !d.cfg.HardCap {
			capNote = "no memory.max (enforce.hard_cap = false)"
		}
		thr := "throttle is per-thread sched/ioprio only (no cpu/io controller delegated to the job containers)"
		if d.contWeights != "" {
			// Measured: with the cpu controller on, a job cgroup is a
			// scheduling entity, so per-thread policy and weights rank it
			// among the pace jobs only; a cpu.max quota is what holds against
			// processes outside pace.
			thr = "throttle sets " + d.contWeights + " on the job's container besides per-thread sched/ioprio; " +
				"weights and per-thread priority rank it only against the other pace jobs"
			if !strings.Contains(d.contWeights, "cpu.max") {
				thr += " (weak against processes outside pace: PACE_LINUX_THROTTLE_CPU_PCT=0 disabled the cpu.max quota)"
			}
		}
		note := "linux: each job runs in a cgroup v2 container: exact job memory, " + capNote +
			", freeze with cgroup.freeze, kill with cgroup.kill; " + thr
		if ids := d.uncontained(); len(ids) > 0 {
			note += fmt.Sprintf("; %d running job(s) without a container, governed by process group "+
				"(their pace run could not place the child in it, e.g. started from a root-owned session scope; its INFO line says why): %s",
				len(ids), strings.Join(ids, " "))
		}
		return note
	case d.contReason != "":
		return "linux: no cgroup v2 job containers (" + d.contReason + "): job memory is the sum of its processes' footprints, " +
			"freeze is SIGSTOP, no kernel memory cap; delegate a cgroup subtree to the user (systemd Delegate=yes) or set PACE_CGROUP_BASE"
	}
	return ""
}

// uncontained lists the running jobs whose wrapper has started the child
// without a container this daemon adopted (at most 8 ids).
func (d *daemon) uncontained() []string {
	var ids []string
	for i := range d.st.Jobs {
		j := &d.st.Jobs[i]
		if j.State != policy.JobRunning || j.PID <= 0 || d.cgFor(j.ID) != nil {
			continue
		}
		if len(ids) == 8 {
			ids = append(ids, "...")
			break
		}
		ids = append(ids, j.ID)
	}
	return ids
}

// noteCgroup adopts the container a started/attach report names (loop
// goroutine, as events arrive, so the next sample already uses it).
func (d *daemon) noteCgroup(ev policy.InEvent) {
	if d.cont == nil || ev.Cgroup == "" || ev.Job == "" || (ev.Kind != policy.EvStarted && ev.Kind != policy.EvAttach) {
		return
	}
	cid, ok := strings.CutPrefix(filepath.Base(ev.Cgroup), "job-")
	want, err := d.cont.Path(cid)
	if !ok || err != nil || want != filepath.Clean(ev.Cgroup) {
		d.fault("container", fmt.Sprintf("job %s: wrapper container %q is not under this daemon's base; governed by process group", ev.Job, ev.Cgroup))
		return
	}
	if _, err := d.cont.PIDs(cid); err != nil {
		d.fault("container", fmt.Sprintf("job %s: container %s: %v", ev.Job, cid, err))
		return
	}
	if old := d.cg[ev.Job]; old != nil && old.cid == cid {
		if old.ended { // an unknown job's container, now re-attached
			old.ended, old.capBytes = false, -1
		}
		if ev.Kind == policy.EvAttach {
			// The wrapper lifted the throttle and the cap when it lost its
			// stream (thawLost): what this daemon last wrote says nothing.
			old.capBytes, old.wKnown = -1, false
		}
		return
	}
	// A re-attach after a restart: freeze/throttle state unknown.
	d.cg[ev.Job] = &cgJob{cid: cid, capBytes: -1, frozen: ev.Kind == policy.EvAttach, throttled: ev.Kind == policy.EvAttach,
		wKnown: ev.Kind != policy.EvAttach}
}

// restoreContainers finds the containers of the running jobs restored from
// state.json, by job id.
func (d *daemon) restoreContainers() {
	if d.cont == nil {
		return
	}
	for i := range d.st.Jobs {
		j := &d.st.Jobs[i]
		if j.State != policy.JobRunning || d.cg[j.ID] != nil {
			continue
		}
		if _, err := d.cont.PIDs(j.ID); err != nil {
			continue
		}
		d.cg[j.ID] = &cgJob{cid: j.ID, capBytes: -1, frozen: true, throttled: true}
	}
	// Containers of jobs this daemon does not know (state.json lost, a job
	// ended while no daemon ran) are released and removed once empty, like
	// those of ended jobs; a wrapper that re-attaches reports its own again.
	ids, _ := d.cont.Jobs()
	for _, id := range ids {
		if d.cg[id] == nil {
			d.cg[id] = &cgJob{cid: id, capBytes: -1, frozen: true, throttled: true, ended: true}
		}
	}
}

// runningJob returns the running job with id from the state.
func (d *daemon) runningJob(id string) *policy.Job {
	for i := range d.st.Jobs {
		if j := &d.st.Jobs[i]; j.ID == id && j.State == policy.JobRunning {
			return j
		}
	}
	return nil
}

func isGone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOENT)
}

// feedContainers reads every adopted container of a running job (members
// and usage) right after the process listing and hands them to the
// tracker.
func (d *daemon) feedContainers() {
	tr, ok := d.tracker.(interface {
		SetContainers(map[string]attrib.Container)
	})
	if d.cont == nil || !ok {
		return
	}
	m := make(map[string]attrib.Container, len(d.cg))
	for _, id := range d.cgIDs() {
		c := d.cg[id]
		c.pids, c.inside = nil, nil
		if c.ended || d.runningJob(id) == nil {
			continue
		}
		pids, err := d.cont.PIDs(c.cid)
		if err != nil {
			if isGone(err) {
				delete(d.cg, id) // removed behind our back: back to process groups
			} else {
				d.fault("container", fmt.Sprintf("job %s: %v", id, err))
			}
			continue
		}
		pids = d.evictOwn(id, c, pids)
		c.pids = pids
		c.inside = make(map[int]bool, len(pids))
		for _, p := range pids {
			c.inside[p] = true
		}
		u, uerr := d.cont.Usage(c.cid)
		m[id] = attrib.Container{PIDs: pids, UsedBytes: u, HaveUsage: uerr == nil}
	}
	tr.SetContainers(m)
}

// ownProcess: a process that belongs to pace, never to a job (the daemon,
// a wrapper of a governed job).
func (d *daemon) ownProcess(pid int) bool {
	return pid == d.selfPID || d.reg.isWrapper(pid)
}

// evictOwn moves pace's own processes out of a job's container and returns
// the pids that stay (the job's). One that cannot be moved stays listed
// (attribution still leaves it out), is a fault, and sets c.paceInside.
func (d *daemon) evictOwn(id string, c *cgJob, pids []int) []int {
	out := pids[:0:0]
	c.paceInside = false
	for _, p := range pids {
		if !d.ownProcess(p) {
			out = append(out, p)
			continue
		}
		if err := d.cont.Evict(p); err != nil {
			d.fault("container", fmt.Sprintf("job %s: pace process %d inside its container: %v (no container-wide freeze or kill)", id, p, err))
			out = append(out, p)
			c.paceInside = true
		}
	}
	return out
}

// evictBefore re-reads a container right before a freeze or kill (a wrapper
// may have started inside since the tick's listing) and moves pace's own
// processes out. It reports whether any of them is still inside.
func (d *daemon) evictBefore(id string, c *cgJob) bool {
	if pids, err := d.cont.PIDs(c.cid); err == nil {
		d.evictOwn(id, c, pids)
	}
	return c.paceInside
}

func (d *daemon) cgIDs() []string {
	ids := make([]string, 0, len(d.cg))
	for id := range d.cg {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// cgFor returns the adopted container of a running job (nil if none).
func (d *daemon) cgFor(job string) *cgJob {
	if job == "" || d.cont == nil {
		return nil
	}
	c := d.cg[job]
	if c == nil || c.ended {
		return nil
	}
	return c
}

// hardCap is the memory.max a running job's container should have now.
func (d *daemon) hardCap(j *policy.Job) int64 {
	p := &d.cfg.Params
	if !d.cfg.HardCap || p.Mode != policy.ModeEnforce || j.BudgetBytes <= 0 {
		return 0
	}
	return j.BudgetBytes + j.BudgetBytes*p.TolerancePermille/1000
}

// decorateAdmitted gives each ADMITTED its job's container path (this
// daemon's scope: the wrapper creates exactly that) and hard cap, so the
// wrapper creates the container with the limit already in place: a job
// that allocates fast would otherwise be past it before the daemon adopts
// the container (one tick after the started report). Both fields are the
// job stream's alone (proto.Event.WithoutStreamFields).
func (d *daemon) decorateAdmitted(evs []proto.Event) {
	if d.cont == nil {
		return
	}
	for i := range evs {
		e := &evs[i]
		if e.Verb != proto.VerbAdmitted || e.Job == "" {
			continue
		}
		j := d.runningJob(e.Job)
		if j == nil {
			continue
		}
		if p, err := d.cont.Path(j.ID); err == nil {
			e.Cgroup = p
		}
		if c := d.hardCap(j); c > 0 {
			e.MemMaxBytes = proto.I64(c)
		}
	}
}

// syncContainers runs after each step's actions: caps follow budget and
// mode; freeze and weights follow the state (see reconcileContainer); the
// containers of ended jobs are released and removed once empty. It returns
// the FROZEN lines for frozen jobs it found thawed and froze again.
func (d *daemon) syncContainers() (refrozen []proto.Event) {
	if d.cont == nil || len(d.cg) == 0 {
		return nil
	}
	modes := targetModes(&d.st)
	for _, id := range d.cgIDs() {
		c := d.cg[id]
		j := d.runningJob(id)
		if j == nil {
			c.ended = true
		}
		if c.ended {
			d.endContainer(id, c)
			continue
		}
		if want := d.hardCap(j); want != c.capBytes {
			if err := d.cont.SetMemMax(c.cid, want); err != nil {
				d.fault("container", fmt.Sprintf("job %s: memory.max: %v", id, err))
			} else {
				c.capBytes = want
			}
		}
		if !d.sampleStale && d.reconcileContainer(id, c, modes["j:"+id]) {
			refrozen = append(refrozen, d.refrozeEvent("j:"+id))
		}
	}
	return refrozen
}

// reconcileContainer makes the container's freeze and weights what the
// state says (a dying job is left to the kill sequence). The freeze is
// read back from the kernel for a job the state calls frozen: pace run
// thaws its container when it loses the daemon or forwards a signal, and
// after a restart or re-attach what this daemon last wrote says nothing.
// A container found thawed is frozen again (reported true: the caller
// sends the "frozen again" FROZEN line).
func (d *daemon) reconcileContainer(id string, c *cgJob, m targetMode) (refroze bool) {
	if m.dying {
		return false
	}
	thawed := false
	if m.frozen && c.frozen && !c.paceInside {
		if on, err := d.cont.Frozen(c.cid); err == nil && !on {
			c.frozen, thawed = false, true
		}
	}
	if m.frozen && !c.frozen && d.evictBefore(id, c) {
		// reconcileHeld stops the members by signal instead (freezes).
	} else if m.frozen != c.frozen {
		if err := d.cont.Freeze(c.cid, m.frozen); err != nil {
			d.fault("container", fmt.Sprintf("job %s: freeze=%v: %v", id, m.frozen, err))
		} else {
			c.frozen = m.frozen
			refroze = thawed && m.frozen
		}
	}
	if m.bg != c.throttled || !c.wKnown {
		if _, err := d.cont.SetThrottle(c.cid, m.bg); err != nil {
			d.fault("container", fmt.Sprintf("job %s: weights: %v", id, err))
		} else {
			c.throttled, c.wKnown = m.bg, true
		}
	}
	return refroze
}

// endContainer releases an ended job's container (thaw, weights, cap: what
// is left inside is no longer the job's) and removes it once empty (no
// process in it or in a cgroup the job created below it). The removal is a
// single attempt, never a wait on the loop; one that fails (a sub-cgroup
// that cannot be removed) is retried with a growing delay, up to a minute.
func (d *daemon) endContainer(id string, c *cgJob) {
	pids, err := d.cont.PIDs(c.cid)
	if err != nil {
		delete(d.cg, id) // gone (the wrapper removed it)
		return
	}
	d.releaseContainer(c)
	if len(pids) > 0 {
		return // descendants still inside: try again next tick
	}
	now := d.nowMS()
	if now < c.removeAtMS {
		return
	}
	if err := d.cont.RemoveNow(c.cid); err != nil {
		c.removeFails++
		c.removeAtMS = now + min(removeBackoffMS<<min(c.removeFails-1, 8), removeBackoffMaxMS)
		d.fault("container", fmt.Sprintf("job %s: remove: %v", id, err))
		return
	}
	delete(d.cg, id)
}

// The retry delays of a container removal that failed: 500 ms, doubling,
// at most a minute.
const (
	removeBackoffMS    int64 = 500
	removeBackoffMaxMS int64 = 60_000
)

// releaseContainer undoes freeze, weights and cap (shutdown, job end).
func (d *daemon) releaseContainer(c *cgJob) {
	if c.frozen {
		if err := d.cont.Freeze(c.cid, false); err == nil {
			c.frozen = false
		}
	}
	if c.throttled || !c.wKnown {
		if _, err := d.cont.SetThrottle(c.cid, false); err == nil {
			c.throttled, c.wKnown = false, true
		}
	}
	if c.capBytes != 0 {
		if err := d.cont.SetMemMax(c.cid, 0); err == nil {
			c.capBytes = 0
		}
	}
}

// releaseContainers runs on shutdown: no container stays frozen, throttled
// or capped without a governor (the next daemon re-applies what its state
// wants).
func (d *daemon) releaseContainers() []string {
	var out []string
	for _, id := range d.cgIDs() {
		c := d.cg[id]
		if c.frozen || c.throttled {
			out = append(out, "j:"+id)
		}
		d.releaseContainer(c)
	}
	return out
}

// closeContainers removes this daemon's scope when nothing is left in it
// (shutdown; a scope still holding job containers stays for the next
// daemon on this socket).
func (d *daemon) closeContainers() {
	if cl, ok := d.cont.(interface{ Close() }); ok {
		cl.Close()
	}
}

// containerAction carries out a SIGSTOP, SIGCONT or SIGKILL for a job with a
// container through the container first. It returns the pids the ordinary
// signal path must then leave out: after a freeze, every process inside
// (frozen by the kernel, never SIGSTOPped); otherwise none (a SIGCONT or
// SIGKILL on top is harmless).
//
// A freeze or kill of the whole container needs pace's own processes out of
// it first (evictBefore); one that cannot be moved leaves the job to the
// per-process signals, which never reach it.
func (d *daemon) containerAction(a *policy.Action, c *cgJob, sig syscall.Signal) (skip map[int]bool) {
	switch sig {
	case syscall.SIGSTOP:
		if d.evictBefore(a.Job, c) {
			d.recordFallbackStops(a) // frozen with SIGSTOP instead
			return nil
		}
		if err := d.cont.Freeze(c.cid, true); err != nil {
			d.fault("container", fmt.Sprintf("job %s: freeze: %v (falling back to SIGSTOP)", a.Job, err))
			d.recordFallbackStops(a)
			return nil
		}
		c.frozen = true
		return c.inside
	case syscall.SIGCONT:
		if err := d.cont.Freeze(c.cid, false); err != nil {
			d.fault("container", fmt.Sprintf("job %s: thaw: %v", a.Job, err))
		} else {
			c.frozen = false
		}
	case syscall.SIGKILL:
		if d.evictBefore(a.Job, c) {
			return nil // killed by signal only
		}
		if err := d.cont.Kill(c.cid); err != nil {
			d.fault("container", fmt.Sprintf("job %s: cgroup.kill: %v", a.Job, err))
		}
	}
	return nil
}

// recordFallbackStops: a container freeze that falls back to SIGSTOP
// stops the processes inside, which preRecordStops left out (it expected
// cgroup.freeze). They are recorded and persisted before the signals, as
// every other stop, so shutdown and a restarted daemon continue them.
func (d *daemon) recordFallbackStops(a *policy.Action) {
	if d.recordStops(a, true) && !d.sampleStale {
		d.persistNow()
	}
}
