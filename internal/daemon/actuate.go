package daemon

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// The actuator carries out policy actions on the backend, after hard safety
// checks that hold whatever the policy decided:
//
//   - never pid/pgid <= 1, never the daemon itself or its group, never a
//     wrapper of a governed job; never an OS process or one outside
//     scope_root_pid unless the action is for a job;
//   - an action for a job or unit reaches only processes that attribution
//     placed in that job or unit this tick (Result.JobPIDs,
//     UnitSample.PIDs). A process group that holds anything else (a reused
//     job pgid led by an unrelated process, the scope root or its ancestors,
//     another job's members) is signalled member by member instead of with
//     killpg;
//   - destructive actions (every signal but SIGCONT, and backgrounding) run
//     only on a fresh sample. When the last ListProcs or SampleMachine
//     failed, pids and pressure may be stale: the step gets Inputs.Stale and
//     the policy then starts and escalates nothing, so its stage
//     transitions (and their events) wait for the first fresh step. Should
//     a destructive action still arrive on a stale sample it is held here
//     too (fault actuate_held), as a backstop. Relaxing actions always run.
//
// The actuator also remembers which processes it backgrounded or stopped
// for each target (by pid and start time; persisted in state.json, so a
// restarted daemon can release them too), so that
//
//   - throttling reaches every member of a job or unit, including children
//     forked while it is throttled (they inherit background priority), and
//     every member of a frozen target is stopped, including members found
//     after the freeze;
//   - everything it throttled or froze is restored when the policy releases
//     the target, when the target ends or disappears from the state, and
//     when the daemon stops, whether or not attribution still places those
//     processes in the target.

// heldRec is what the actuator did to one target's processes.
type heldRec struct {
	bg      map[int]int64 // pid -> start ms: backgrounded
	stopped map[int]int64 // pid -> start ms: sent SIGSTOP
	// unseen (not persisted): pids of stopped this daemon sent SIGSTOP and
	// has not yet seen stopped in a current listing (the stop may still be
	// pending, e.g. in uninterruptible sleep). Any other record listed as
	// not stopped was continued by someone else (pace run thawing on a lost
	// daemon, a kill -CONT): reconcileHeld stops it again.
	unseen map[int]bool
	// seedPGID (not persisted): stopped holds only the group leader of a
	// target restored as frozen without records (an older state.json); on
	// the first current listing, the group's stopped members join stopped
	// if the leader is still the same process (expandSeeds).
	seedPGID int
}

// targetKey names an action's target in the ownership maps: "j:<job id>"
// or "u:<unit key>"; "" for an action without a target.
func targetKey(a *policy.Action) string {
	switch {
	case a.Job != "":
		return "j:" + a.Job
	case a.Unit != "":
		return "u:" + a.Unit
	}
	return ""
}

// relaxing actions undo enforcement and are safe on a stale sample.
func relaxing(a *policy.Action) bool {
	switch a.Op {
	case policy.OpForeground:
		return true
	case policy.OpSignal, policy.OpSignalGroup:
		return syscall.Signal(a.Signal) == syscall.SIGCONT
	}
	return false
}

// setAttribution installs one tick's process list and attribution and
// rebuilds the ownership maps the actuator checks against.
func (d *daemon) setAttribution(r attrib.Result, procs []platform.Proc) {
	d.lastAttr = r
	d.lastProcs = procs
	d.procIdx = make(map[int]int, len(procs))
	for i := range procs {
		d.procIdx[procs[i].PID] = i
	}
	d.owner = make(map[int]string, len(procs))
	d.owned = make(map[string][]int, len(r.JobPIDs)+len(r.Units)+len(r.SmallUnits))
	for id, pids := range r.JobPIDs {
		k := "j:" + id
		d.owned[k] = pids
		for _, p := range pids {
			d.owner[p] = k
		}
	}
	for _, us := range [][]policy.UnitSample{r.Units, r.SmallUnits} {
		for i := range us {
			u := &us[i]
			k := "u:" + u.Key
			d.owned[k] = u.PIDs
			for _, p := range u.PIDs {
				d.owner[p] = k
			}
		}
	}
}

// proc returns the last listing's entry for pid.
func (d *daemon) proc(pid int) (*platform.Proc, bool) {
	if i, ok := d.procIdx[pid]; ok && i < len(d.lastProcs) && d.lastProcs[i].PID == pid {
		return &d.lastProcs[i], true
	}
	for i := range d.lastProcs {
		if d.lastProcs[i].PID == pid {
			return &d.lastProcs[i], true
		}
	}
	return nil, false
}

// listed reports whether pid is in the last listing.
func (d *daemon) listed(pid int) bool {
	_, ok := d.proc(pid)
	return ok
}

// startOf returns pid's start time in the last listing (-1 if not listed).
func (d *daemon) startOf(pid int) int64 {
	if p, ok := d.proc(pid); ok {
		return p.StartMS
	}
	return -1
}

func (d *daemon) heldFor(key string) *heldRec {
	h := d.held[key]
	if h == nil {
		h = &heldRec{bg: map[int]int64{}, stopped: map[int]int64{}, unseen: map[int]bool{}}
		d.held[key] = h
	}
	return h
}

// execute carries out actions (see the actuator comment above).
func (d *daemon) execute(actions []policy.Action, now int64) {
	if !d.sampleStale && d.needsDurableState(actions) {
		// A freeze or throttle must survive a daemon crash right after it:
		// the next daemon can only release what state.json says it did,
		// so the processes about to be stopped are recorded first. The
		// record is written and renamed into place, not fsynced (the
		// persister does that afterwards): the signal never waits for the
		// disk, so it lands within the step that decided it and the
		// ladder's next stage is timed from when it took effect.
		d.preRecordStops(actions)
		d.persistNow()
	}
	heldBack := 0
	for i := range actions {
		a := &actions[i]
		if d.sampleStale && !relaxing(a) {
			heldBack++
			continue
		}
		key := targetKey(a)
		sig := syscall.Signal(a.Signal)
		var skip map[int]bool // processes a job container already took care of
		if c := d.cgFor(a.Job); c != nil && (a.Op == policy.OpSignal || a.Op == policy.OpSignalGroup) &&
			(sig == syscall.SIGSTOP || sig == syscall.SIGCONT || sig == syscall.SIGKILL) {
			skip = d.containerAction(a, c, sig)
		}
		switch a.Op {
		case policy.OpSignalGroup:
			d.signalGroup(a, sig, key, skip)
		case policy.OpSignal:
			for _, pid := range a.PIDs {
				if skip[pid] {
					continue
				}
				if why := d.unsafeFor(pid, key); why != "" {
					if why != errNotMember || d.listed(pid) {
						d.refuse(a, "pid", pid, why) // an exited pid is no fault
					}
					continue
				}
				if err := d.backend.Signal(pid, sig); err != nil {
					d.fault("actuate", fmt.Sprintf("signal %d pid %d: %v", a.Signal, pid, err))
					continue
				}
				d.noteSignal(key, sig, pid)
			}
			if sig == syscall.SIGCONT && key != "" {
				d.contHeld(key)
			}
		case policy.OpBackground, policy.OpForeground:
			d.setBackground(a, key, a.Op == policy.OpBackground)
		default:
			d.fault("actuate", "unknown action op "+strconv.Quote(a.Op))
		}
	}
	if heldBack > 0 {
		d.fault("actuate_held", fmt.Sprintf("%d action(s) held: the last process or machine sample failed", heldBack))
	}
}

// needsDurableState: actions that leave processes stopped or deprioritised
// (a SIGSTOP that is not followed by SIGKILL for the same target in this
// batch, or a background action).
func (d *daemon) needsDurableState(actions []policy.Action) bool {
	killed := map[string]bool{}
	for i := range actions {
		if a := &actions[i]; syscall.Signal(a.Signal) == syscall.SIGKILL {
			killed[targetKey(a)] = true
		}
	}
	for i := range actions {
		a := &actions[i]
		if a.Op == policy.OpBackground {
			return true
		}
		if (a.Op == policy.OpSignal || a.Op == policy.OpSignalGroup) && syscall.Signal(a.Signal) == syscall.SIGSTOP && !killed[targetKey(a)] {
			return true
		}
	}
	return false
}

// preRecordStops records, before they are signalled, the processes the
// SIGSTOP actions will reach (as noteSignal does after the signal).
func (d *daemon) preRecordStops(actions []policy.Action) {
	for i := range actions {
		d.recordStops(&actions[i], false)
	}
}

// recordStops records the processes a SIGSTOP action will stop. Those in
// the job's container are left out (cgroup.freeze, not SIGSTOP) unless
// inContainer (the freeze fell back to SIGSTOP). It reports whether it
// recorded any.
func (d *daemon) recordStops(a *policy.Action, inContainer bool) bool {
	key := targetKey(a)
	if key == "" || syscall.Signal(a.Signal) != syscall.SIGSTOP {
		return false
	}
	var pids []int
	switch a.Op {
	case policy.OpSignal:
		pids = a.PIDs
	case policy.OpSignalGroup:
		for j := range d.lastProcs {
			if p := &d.lastProcs[j]; p.PGID == a.PGID && p.State != 'Z' {
				pids = append(pids, p.PID)
			}
		}
	}
	c := d.cgFor(a.Job)
	n := 0
	for _, pid := range pids {
		if (inContainer || !c.freezes(pid)) && d.unsafeFor(pid, key) == "" {
			d.noteSignal(key, syscall.SIGSTOP, pid)
			n++
		}
	}
	return n > 0
}

func (d *daemon) refuse(a *policy.Action, what string, id int, why string) {
	target := a.Job
	if target == "" {
		target = a.Unit
	}
	d.fault("unsafe_action", fmt.Sprintf("refused %s %s %d (%s) for %q: %s", a.Op, what, id, signalName(a.Signal), target, why))
}

func signalName(s int) string {
	return "sig" + strconv.Itoa(s)
}

// errNotMember is unsafeFor's answer for a process outside the target.
const errNotMember = "not a member of the target"

// unsafeFor returns why pid must not be touched by an action for target
// key, or "".
func (d *daemon) unsafeFor(pid int, key string) string {
	if why := d.unsafePID(pid, len(key) > 2 && key[:2] == "j:"); why != "" {
		return why
	}
	if key != "" && d.owner[pid] != key {
		return errNotMember
	}
	return ""
}

// unsafePID returns why pid must not be touched, or "".
func (d *daemon) unsafePID(pid int, forJob bool) string {
	switch {
	case pid <= 1:
		return "pid <= 1"
	case pid == d.selfPID:
		return "governor itself"
	case d.isProtected(pid):
		return "wrapper of a governed job"
	case !forJob && d.isSystem(pid):
		return "operating-system process"
	case !forJob && d.lastAttr.InScope != nil && !d.lastAttr.InScope[pid]:
		return "outside scope_root_pid"
	}
	return ""
}

func (d *daemon) isProtected(pid int) bool {
	for _, p := range d.actProt {
		if p == pid {
			return true
		}
	}
	return false
}

func (d *daemon) isSystem(pid int) bool {
	if p, ok := d.proc(pid); ok {
		return p.System
	}
	return false
}

// members returns the pids of process group pgid from the last listing.
func (d *daemon) members(pgid int) []int {
	var out []int
	for i := range d.lastProcs {
		if d.lastProcs[i].PGID == pgid {
			out = append(out, d.lastProcs[i].PID)
		}
	}
	return out
}

// signalGroup signals a process group. A group that contains a process the
// action must not reach is signalled member by member instead, skipping
// those, so enforcement still happens without collateral damage. Members in
// skip (a job container froze them) are left out the same way.
func (d *daemon) signalGroup(a *policy.Action, sig syscall.Signal, key string, skip map[int]bool) {
	if a.PGID <= 1 {
		d.refuse(a, "pgid", a.PGID, "pgid <= 1")
		return
	}
	if a.PGID == d.selfPGID {
		d.refuse(a, "pgid", a.PGID, "governor's own process group")
		return
	}
	split := false
	for i := range d.lastProcs {
		p := &d.lastProcs[i]
		if p.PGID == a.PGID && p.State != 'Z' && (skip[p.PID] || d.unsafeFor(p.PID, key) != "") {
			split = true
			break
		}
	}
	if !split {
		if err := d.backend.SignalGroup(a.PGID, sig); err != nil {
			d.fault("actuate", fmt.Sprintf("signal %d pgid %d: %v", a.Signal, a.PGID, err))
		} else {
			for i := range d.lastProcs {
				if p := &d.lastProcs[i]; p.PGID == a.PGID && p.State != 'Z' {
					d.noteSignal(key, sig, p.PID)
				}
			}
		}
		if sig == syscall.SIGCONT && key != "" {
			d.contHeld(key)
		}
		return
	}
	var outside []int
	for i := range d.lastProcs {
		p := &d.lastProcs[i]
		if p.PGID != a.PGID || p.State == 'Z' || skip[p.PID] {
			continue
		}
		if why := d.unsafeFor(p.PID, key); why != "" {
			if why == errNotMember {
				outside = append(outside, p.PID)
			} else {
				d.refuse(a, "pid", p.PID, why+" (member of pgid "+strconv.Itoa(a.PGID)+")")
			}
			continue
		}
		if err := d.backend.Signal(p.PID, sig); err != nil {
			d.fault("actuate", fmt.Sprintf("signal %d pid %d: %v", a.Signal, p.PID, err))
			continue
		}
		d.noteSignal(key, sig, p.PID)
	}
	// A job's own group holding a process that is not the job's (its pgid
	// is now led by an unrelated process) is worth a fault; a unit sharing
	// its group with out-of-scope or job processes is ordinary.
	if len(outside) > 0 && a.Job != "" {
		d.fault("group_split", fmt.Sprintf("pgid %d of job %q holds non-members %v: signalled members only", a.PGID, a.Job, outside))
	}
	if sig == syscall.SIGCONT && key != "" {
		d.contHeld(key)
	}
}

// noteSignal remembers stopped processes per target, to thaw them later.
func (d *daemon) noteSignal(key string, sig syscall.Signal, pid int) {
	if key == "" || sig != syscall.SIGSTOP {
		return
	}
	if st := d.startOf(pid); st >= 0 {
		h := d.heldFor(key)
		h.stopped[pid] = st
		h.unseen[pid] = true
	}
}

// contHeld sends SIGCONT to the remembered stopped processes of key that
// the policy's SIGCONT did not cover (still listed with the same start),
// then forgets them.
func (d *daemon) contHeld(key string) {
	h := d.held[key]
	if h == nil {
		return
	}
	d.releaseStopped(h)
}

// releaseStopped thaws the remembered stopped processes still listed with
// the same start, and forgets them. A record whose process is not in the
// listing is forgotten only when the listing is current (the process is
// gone); with no current listing it is kept for a later release.
func (d *daemon) releaseStopped(h *heldRec) {
	for _, pid := range sortedPIDs(h.stopped) {
		if d.sameProc(pid, h.stopped[pid]) {
			if d.unsafePID(pid, true) == "" {
				if err := d.backend.Signal(pid, syscall.SIGCONT); err != nil {
					d.fault("actuate", fmt.Sprintf("thaw pid %d: %v", pid, err))
				}
			}
		} else if !d.procsFresh {
			continue
		}
		delete(h.stopped, pid)
		delete(h.unseen, pid)
	}
}

// continuedWhileFrozen reports whether a recorded stopped process of a
// frozen target is listed (in a current listing) as running: someone else
// continued it. A stop this daemon sent and has not yet seen take effect
// does not count; seeing it stopped clears that.
func (d *daemon) continuedWhileFrozen(h *heldRec, pid int) bool {
	if !d.procsFresh {
		return false
	}
	p, ok := d.proc(pid)
	if !ok {
		return false
	}
	switch p.State {
	case 'T':
		delete(h.unseen, pid)
		return false
	case 'Z':
		return false
	}
	return !h.unseen[pid]
}

// releaseBG restores the remembered backgrounded processes (as
// releaseStopped).
func (d *daemon) releaseBG(h *heldRec) {
	for _, pid := range sortedPIDs(h.bg) {
		if d.sameProc(pid, h.bg[pid]) {
			if d.unsafePID(pid, true) == "" {
				if err := d.backend.SetBackground(pid, false); err != nil {
					d.fault("actuate", fmt.Sprintf("foreground pid %d: %v", pid, err))
				}
			}
		} else if !d.procsFresh {
			continue
		}
		delete(h.bg, pid)
	}
}

// sameProc: pid is listed with this start time (not gone, not reused).
func (d *daemon) sameProc(pid int, start int64) bool {
	return d.startOf(pid) == start
}

func sortedPIDs(m map[int]int64) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// setBackground backgrounds or restores a target: the listed pids plus
// every current member of the target (or of the group, for an action
// without a target); a restore also covers every process backgrounded for
// it earlier that still exists (children forked while throttled).
func (d *daemon) setBackground(a *policy.Action, key string, on bool) {
	explicit := map[int]bool{}
	var pids []int
	add := func(pid int) {
		for _, p := range pids {
			if p == pid {
				return
			}
		}
		pids = append(pids, pid)
	}
	for _, p := range a.PIDs {
		explicit[p] = true
		add(p)
	}
	switch {
	case key != "":
		for _, p := range d.owned[key] {
			add(p)
		}
	case a.PGID > 1:
		for _, p := range d.members(a.PGID) {
			add(p)
		}
	}
	var h *heldRec
	if key != "" {
		h = d.heldFor(key)
	}
	for _, pid := range pids {
		if why := d.unsafeFor(pid, key); why != "" {
			if why != errNotMember || (explicit[pid] && d.listed(pid)) {
				d.refuse(a, "pid", pid, why)
			}
			continue
		}
		if err := d.backend.SetBackground(pid, on); err != nil {
			d.fault("actuate", fmt.Sprintf("%s pid %d: %v", a.Op, pid, err))
			continue
		}
		if h != nil {
			if on {
				if st := d.startOf(pid); st >= 0 {
					h.bg[pid] = st
				}
			} else {
				delete(h.bg, pid)
			}
		}
	}
	if !on && h != nil {
		d.releaseBG(h)
	}
}

// targetModes classifies the state's real (not virtual) enforcement per
// target key.
type targetMode struct{ bg, frozen, dying bool }

func targetModes(s *policy.State) map[string]targetMode {
	out := make(map[string]targetMode, len(s.Jobs)+len(s.Units))
	add := func(k string, e *policy.Enforcement) {
		if e.Virtual {
			out[k] = targetMode{}
			return
		}
		out[k] = targetMode{
			bg:     e.Stage == policy.StageThrottled || e.Stage == policy.StageFrozen || e.CPUThrottle,
			frozen: e.Stage == policy.StageFrozen,
			dying:  e.Stage == policy.StageTerminating || e.Stage == policy.StageKilled,
		}
	}
	for i := range s.Jobs {
		add("j:"+s.Jobs[i].ID, &s.Jobs[i].Enf)
	}
	for i := range s.Units {
		add("u:"+s.Units[i].Key, &s.Units[i].Enf)
	}
	return out
}

// reconcileHeld runs after each step's actions. Throttled targets get their
// new members backgrounded (children forked while throttled, members found
// later); frozen targets get every member not yet stopped stopped (members
// found after the freeze; a target restored from an older state.json
// without records; members someone else continued, such as pace run thawing
// its job when it lost the daemon: those come with a FROZEN line, returned
// for the step's events), so a target the state calls frozen is; and whatever
// was backgrounded or stopped for a target the state no longer throttles
// or freezes (released, ended, gone) is restored, unless the target is
// being killed.
func (d *daemon) reconcileHeld() (refrozen []proto.Event) {
	modes := targetModes(&d.st)
	if d.procsFresh {
		d.expandSeeds()
	}
	if !d.sampleStale {
		keys := make([]string, 0, len(modes))
		for k := range modes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		type stop struct {
			key string
			pid int
		}
		var stops []stop
		save := false
		for _, k := range keys {
			m := modes[k]
			if !m.bg && !m.frozen {
				continue
			}
			h := d.heldFor(k)
			// A job container freezes its members itself (syncContainers),
			// including those found after the freeze.
			var cont *cgJob
			if strings.HasPrefix(k, "j:") {
				cont = d.cgFor(k[2:])
			}
			again := false
			for _, pid := range d.owned[k] {
				st := d.startOf(pid)
				if st < 0 || d.unsafeFor(pid, k) != "" {
					continue
				}
				if old, ok := h.stopped[pid]; m.frozen && !cont.freezes(pid) && (!ok || old != st) {
					h.stopped[pid] = st // recorded before the signal (persisted below)
					h.unseen[pid] = true
					stops = append(stops, stop{k, pid})
					save = true
				} else if m.frozen && !cont.freezes(pid) && d.continuedWhileFrozen(h, pid) {
					h.unseen[pid] = true
					stops = append(stops, stop{k, pid})
					again = true
				}
				if m.bg {
					if old, ok := h.bg[pid]; ok && old == st {
						continue
					}
					if err := d.backend.SetBackground(pid, true); err != nil {
						d.fault("actuate", fmt.Sprintf("background pid %d: %v", pid, err))
						continue
					}
					h.bg[pid] = st
				}
			}
			if again {
				refrozen = append(refrozen, d.refrozeEvent(k))
			}
		}
		if save {
			d.persistNow()
		}
		if len(stops) > 0 {
			for _, s := range stops {
				if err := d.backend.Signal(s.pid, syscall.SIGSTOP); err != nil {
					d.fault("actuate", fmt.Sprintf("freeze pid %d: %v", s.pid, err))
					delete(d.held[s.key].stopped, s.pid)
				}
			}
		}
	}
	for k, h := range d.held {
		m := modes[k]
		if !m.dying {
			if !m.bg {
				d.releaseBG(h)
			}
			if !m.frozen {
				d.releaseStopped(h)
			}
		}
		if len(h.bg) == 0 && len(h.stopped) == 0 {
			delete(d.held, k)
		}
	}
	return refrozen
}

// MsgRefrozen is the msg of the FROZEN line the daemon sends when it stops
// a frozen target again that someone else had continued.
const MsgRefrozen = "frozen again: it was continued while frozen"

// refrozeEvent is the FROZEN line for target key frozen again.
func (d *daemon) refrozeEvent(key string) proto.Event {
	ev := proto.Event{Verb: proto.VerbFrozen, Policy: proto.PolicyPressure, Level: d.st.Pressure.Level, Msg: MsgRefrozen}
	id := key[min(2, len(key)):]
	if strings.HasPrefix(key, "j:") {
		ev.Job = id
		for i := range d.st.Jobs {
			if j := &d.st.Jobs[i]; j.ID == id {
				ev.Class, ev.PID, ev.PGID = j.Class, j.PID, j.PGID
			}
		}
		return ev
	}
	ev.Unit = proto.UnitRef(id)
	for i := range d.st.Units {
		if u := &d.st.Units[i]; u.Key == id {
			ev.Comm, ev.PID, ev.PGID = u.Comm, u.LeaderPID, u.PGID
		}
	}
	return ev
}

// seedHeld runs after a state load: a target the state calls frozen for
// which no stopped processes were recorded (a state.json from before the
// records existed) gets its group leader recorded, with the start time the
// state knows, so that it is thawed if it leaves the state (dropped by
// attribution, its group reused, its job ended) while still stopped.
func (d *daemon) seedHeld() {
	seed := func(key string, e *policy.Enforcement, leader, pgid int, start int64) {
		if e.Virtual || e.Stage != policy.StageFrozen || leader <= 1 || pgid <= 1 || leader != pgid || start <= 0 {
			return
		}
		if h := d.held[key]; h != nil && len(h.stopped) > 0 {
			return
		}
		h := d.heldFor(key)
		h.stopped[leader] = start
		h.seedPGID = pgid
	}
	for i := range d.st.Jobs {
		j := &d.st.Jobs[i]
		seed("j:"+j.ID, &j.Enf, j.PID, j.PGID, j.StartMS)
	}
	for i := range d.st.Units {
		u := &d.st.Units[i]
		seed("u:"+u.Key, &u.Enf, u.LeaderPID, u.PGID, u.StartMS)
	}
}

// expandSeeds (on a current listing): for each seeded record whose leader
// is still the same process (pid and start time) and still leads its
// group, the group's stopped members are recorded too, so a release thaws
// the whole group. A leader that is gone or reused ends the seed: then
// nothing of that group is ever signalled (its pid or pgid may be another
// process's now).
func (d *daemon) expandSeeds() {
	for k, h := range d.held {
		pg := h.seedPGID
		if pg <= 0 {
			continue
		}
		h.seedPGID = 0
		start, ok := h.stopped[pg]
		if !ok {
			continue
		}
		forJob := len(k) > 2 && k[:2] == "j:"
		lp, listed := d.proc(pg)
		same := listed && lp.StartMS == start
		if forJob && listed && !same {
			// A job's StartMS is its wrapper's report: on Linux it is off
			// by up to a second from the backend's start (attrib's
			// leader tolerance).
			dt := lp.StartMS - start
			same = dt >= -attrib.LeaderStartToleranceMS && dt <= attrib.LeaderStartToleranceMS
		}
		if !same || lp.PGID != pg {
			delete(h.stopped, pg) // gone or reused: never signalled
			continue
		}
		start = lp.StartMS
		h.stopped[pg] = start
		for i := range d.lastProcs {
			p := &d.lastProcs[i]
			if p.PGID == pg && p.State == 'T' && p.StartMS >= start && d.unsafePID(p.PID, forJob) == "" {
				h.stopped[p.PID] = p.StartMS
			}
		}
	}
}

// releaseAll restores everything the actuator throttled or froze, and
// resets those stages in the state: on shutdown no process is left stopped
// or deprioritised without a governor to release it, and the next daemon
// judges every target afresh. It reports what it released.
func (d *daemon) releaseAll() string {
	keys := make([]string, 0, len(d.held))
	for k := range d.held {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	released := d.releaseContainers()
	for _, k := range keys {
		h := d.held[k]
		if len(h.bg) > 0 || len(h.stopped) > 0 {
			released = append(released, k)
		}
		d.releaseBG(h)
		d.releaseStopped(h)
		if len(h.bg) == 0 && len(h.stopped) == 0 {
			delete(d.held, k) // what is left (no current listing) is persisted for the next daemon
		}
	}

	// The state's frozen and throttled targets: their current members too
	// (covers targets restored from state.json before any reconcile), and
	// the stages are reset.
	st := cloneState(d.st)
	changed := false
	reset := func(k string, e *policy.Enforcement) {
		if e.Virtual || !(e.Stage == policy.StageThrottled || e.Stage == policy.StageFrozen || e.CPUThrottle) {
			return
		}
		for _, pid := range d.owned[k] {
			if d.unsafeFor(pid, k) != "" {
				continue
			}
			if e.Stage == policy.StageFrozen {
				_ = d.backend.Signal(pid, syscall.SIGCONT)
			}
			_ = d.backend.SetBackground(pid, false)
		}
		if len(released) == 0 || released[len(released)-1] != k {
			released = append(released, k)
		}
		changed = true
		e.Stage, e.StageMS, e.NextStepMS, e.CPUThrottle = policy.StageNone, 0, 0, false
	}
	for i := range st.Jobs {
		reset("j:"+st.Jobs[i].ID, &st.Jobs[i].Enf)
	}
	for i := range st.Units {
		reset("u:"+st.Units[i].Key, &st.Units[i].Enf)
	}
	if changed {
		d.st = st
	}
	if len(released) == 0 {
		return ""
	}
	sort.Strings(released)
	released = dedupStrings(released)
	return fmt.Sprintf("released throttles/freezes of %v", released)
}

func dedupStrings(s []string) []string {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
