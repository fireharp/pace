// Package attrib attributes the user's processes to governed jobs and to
// unmanaged process groups, every sampling tick. It is deterministic given
// the sequence of process lists (no clock, no map-order dependence) and keeps
// sticky membership keyed by (pid, start time) so that descendants which
// setsid(), re-group or get orphaned to pid 1 still count against their job.
//
// Attribution, per process each tick (self and protected wrapper pids are
// never attributed):
//
//	C. listed in a running job's container (Linux cgroup v2, SetContainers)
//	   -> that job (exact: whatever a process did to its group, session or
//	   environment, the kernel keeps it in the job's cgroup), except that
//	   rule 0 for another job without a container of its own wins: a nested
//	   `pace run` that could not get a container keeps its processes, which
//	   stay in the outer job's cgroup (the outer job's usage is then its
//	   members' sum, not the container's, which counts the inner job too)
//	0. pgid equals a running job's PGID                       -> that job
//	   (strongest evidence; overrides a sticky membership, e.g. a nested
//	   `pace run` whose child was first seen as part of the outer job)
//	1. already a sticky member of a running job              -> that job
//	2. ppid is a member of a job and the parent started no later than the
//	   child                                                 -> parent's job
//	3. pgid is a group led by a member of a job (the leader may have exited:
//	   member-led groups are remembered while the group exists) -> that job
//	4. JobTag (PACE_JOB_ID from the environment) names a running job -> that job.
//	   Proc.JobTag is used when the backend filled it; otherwise the optional
//	   TagReader is asked, only for unattributed non-system, non-zombie
//	   processes that started no earlier than the oldest running job (minus
//	   LeaderStartToleranceMS; an older process cannot belong to any job),
//	   each process at most once, at most MaxTagReads per tick. The parent is
//	   not required to be pid 1: an escaped orphan is re-parented to the
//	   nearest child subreaper (systemd --user on Linux desktops).
//	5. otherwise unmanaged, grouped by pgid
//
// Rules 2-4 are iterated to a fixed point so whole subtrees follow.
// "Running job" = State running with PGID (or PID) set. A job's group is
// considered reused (rule 0 disabled) when a process with pid == PGID exists
// that is not the job's leader: its start time differs from the leader first
// seen for that job, or the leader was already gone when the job was first
// seen (a leader never comes back, so whatever holds its pid later is
// another process). A job this tracker could not have seen start (one that
// is running in the tracker's first Attribute call: a daemon restart) is the
// one case where the leader's start is not known from observation; there the
// first process at pid == PGID must start within LeaderStartToleranceMS of
// the job's StartMS. That comparison mixes the wrapper's clock with the
// backend's start times, which differ after a wall-clock step, so it is used
// for nothing else: a job first seen by a running tracker records its leader
// exactly, whatever the clocks say. Result.JobPIDs lists each job's members
// so that the actuator never signals a process of a reused group.
//
// Job samples: UsedBytes and NProcs over non-zombie members (UsedBytes is the
// container's own usage instead when the job has one: exact, shared pages
// counted once, tmpfs included), Alive = any
// non-zombie member, ExtraPIDs = non-zombie members outside the job's group.
// CPUMS is monotonic: members' CPU (per-process max seen) plus the CPU of
// members that have exited or left the job, and never below the previous
// value. Keeping retired CPU (rather than a max of the live sum) means a job
// whose long-lived member exits while new children burn CPU still shows
// progress to stuck detection.
//
// Units: unmanaged candidates (not members, not self/protected, not System,
// not zombie, inside the scope) grouped by pgid. Some groups are never a
// unit, whatever they hold (their processes still count in
// ScopedUsedBytes): pgid <= 1, the daemon's group (SelfPGIDs, and the
// group of a listed SelfPID), a protected wrapper's group (it is the
// group of the agent or script that ran `pace run`), and the scope root's
// group (with ScopeRootPID set, descendants that share the root's group,
// such as the jobs of a script without job control, are not a unit: only
// groups of their own are). And a terminal's shell (Proc.TermLeader: the
// session leader, or on macOS the login shell under root's login) whose
// own footprint is below MinUnitBytes is never a unit member: the rest of its group still is (enforcement then signals
// those members one by one, never the shell), so a heavy command run in
// the shell's group without job control is governed, but closing the
// user's terminal never is. A TermLeader at or above MinUnitBytes is not a
// shell but what replaced it by exec, and is treated like any process. Key
// "pg:<pgid>:<identityStartMS>", where the identity start is the group
// leader's start time when the leader (pid == pgid) exists, else the start
// remembered for that group (or a tracked unit's StartMS), else the earliest
// member start. The identity is remembered while the group exists, so a key
// does not change when the leader exits. LeaderPID/Comm are the leader's
// when it is a candidate, else the lowest candidate pid's. Exception: in
// process group 0 or 1 (init's group: no job control) every subtree is a
// unit, keyed "pg:<root pid>:<root start>" with PGID 1 (see unitGroup).
//
// Units holds the groups at or above MinUnitBytes (the largest MaxUnits)
// and every tracked one. Smaller untracked groups are SmallUnits (the
// largest MaxUnits of them), and every group below MinUnitBytes is counted
// in SmallGroups/SmallBytes: a swarm of small groups can hold as much as one
// big one, and the daemon offers them to the policy under memory pressure.
// A small group that holds one of pace's own client programs (ClientExes,
// ClientNames: `pace status` run by an agent, the menu bar app) is counted
// but never listed in SmallUnits.
//
// A Tracker is not safe for concurrent use; the daemon calls it from its
// single policy goroutine.
package attrib

import (
	"sort"
	"strconv"
	"strings"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

// MaxTagReads bounds TagReader calls per tick (each costs ~10-20 µs).
const MaxTagReads = 32

// LeaderStartToleranceMS is how far a job's StartMS (reported by its
// wrapper) may be from the leader's start time as the backend reports it
// and still be the same process. On darwin both come from the kernel and
// match exactly; on Linux the wrapper reports its clock right after the
// spawn while the backend derives the start from btime (whole seconds) and
// clock ticks, so they differ by up to about a second.
const LeaderStartToleranceMS = 3000

// Options configure attribution.
type Options struct {
	// SelfPIDs are never attributed or reported (the daemon itself).
	SelfPIDs []int
	// SelfPGIDs are process groups that are never an unmanaged unit (the
	// daemon's own group). The groups of listed SelfPIDs, of protected
	// wrappers and of ScopeRootPID never are either.
	SelfPGIDs []int
	// ScopeRootPID > 0 restricts unmanaged units (and ScopedUsedBytes) to
	// descendants of this pid (tests). 0 = every process of the user.
	// The root itself is not a candidate. Descendants are remembered, so a
	// descendant reparented to pid 1 stays in scope.
	ScopeRootPID int
	// ProtectNames are process or executable names whose process group (or,
	// in init's group, whose subtree) is never an unmanaged unit, as for the
	// daemon's own processes. A Linux comm is cut to 15 bytes, so a 15-byte
	// comm also matches a longer name it starts.
	ProtectNames []string
	// MinUnitBytes: unmanaged groups below this are not reported in Units
	// (unless already tracked by the policy): they are counted in
	// Result.SmallGroups/SmallBytes and listed in Result.SmallUnits.
	MinUnitBytes int64
	// MaxUnits caps reported Units (largest first; tracked units always
	// kept), and separately SmallUnits (largest first). 0 = no cap.
	MaxUnits int
	// ClientExes (executable paths) and ClientNames (process or executable
	// names) are pace's own client programs: the pace binary itself (`pace
	// status`, `pace watch`) and the menu bar app. A group below
	// MinUnitBytes that holds one is never in SmallUnits, so a status poll
	// is never a swarm victim.
	ClientExes  []string
	ClientNames []string
}

// Result is one tick's attribution.
type Result struct {
	Jobs  []policy.JobSample  // sorted by ID, one per running job with a PID
	Units []policy.UnitSample // sorted by Key
	// SmallUnits are the largest unit candidates below MinUnitBytes that the
	// policy does not track (at most MaxUnits, sorted by Key). They are not
	// in Units: the daemon hands them to the policy only under memory
	// pressure, when a swarm of small groups may be what holds the memory.
	SmallUnits []policy.UnitSample
	// SmallGroups and SmallBytes count every unit candidate group below
	// MinUnitBytes (tracked or not, listed in SmallUnits or not) and their
	// footprint, for status.
	SmallGroups     int
	SmallBytes      int64
	ScopedUsedBytes int64 // footprint of jobs + in-scope unmanaged (excl. system, self, protected)
	TotalUsedBytes  int64 // footprint of every listed process
	NProcs          int
	// InScope holds the pids inside ScopeRootPID's scope; nil when there is
	// no scope (every process of the user is in scope). The actuator uses it
	// so that a unit's group signal never reaches an out-of-scope process
	// that shares the group (e.g. the shell that started a test).
	InScope map[int]bool
	// JobPIDs lists each running job's non-zombie members (sorted), the
	// processes an action for that job may reach. Units carry theirs in
	// UnitSample.PIDs.
	JobPIDs map[string][]int
}

type procKey struct {
	pid   int
	start int64
}

type memberRec struct {
	job string
	cpu int64 // max CPU seen for this process
}

type groupRec struct {
	leaderStart int64
	job         string
}

type leaderRec struct {
	pgid  int
	start int64
	// approx: start is the job's StartMS (a restored job whose leader was not
	// seen yet), so it is compared with LeaderStartToleranceMS instead of
	// exactly.
	approx bool
	// gone: the leader was not listed when the job was first seen; no later
	// process at its pid is the leader.
	gone bool
}

// matches reports whether a process started at start is this leader.
func (lr leaderRec) matches(start int64) bool {
	switch {
	case lr.gone:
		return false
	case !lr.approx:
		return lr.start == start
	}
	d := start - lr.start
	return d >= -LeaderStartToleranceMS && d <= LeaderStartToleranceMS
}

// Container is one job's container this tick (Linux cgroup v2): the
// processes inside it, and its usage when HaveUsage.
type Container struct {
	PIDs      []int
	UsedBytes int64
	HaveUsage bool
}

// Tracker holds sticky membership between ticks.
type Tracker struct {
	opts       Options
	protected  []int
	tags       platform.TagReader
	containers map[string]Container

	members    map[procKey]memberRec
	groups     map[int]groupRec // member-led groups: pgid -> leader start, job
	jobLeader  map[string]leaderRec
	retiredCPU map[string]int64 // CPU of members that left the job
	lastCPU    map[string]int64 // last reported CPUMS (monotonic guard)
	tagCache   map[procKey]string
	scopeKnown map[procKey]bool
	unitIdent  map[int]int64 // pgid -> identity start
	rootPGID   int           // ScopeRootPID's group, last seen (0 = never seen)
	// primed: an Attribute call has completed. Jobs running in the first
	// call were started before this tracker existed (restored state).
	primed bool
}

// New returns a Tracker.
func New(o Options) *Tracker {
	return &Tracker{
		opts:       o,
		members:    map[procKey]memberRec{},
		groups:     map[int]groupRec{},
		jobLeader:  map[string]leaderRec{},
		retiredCPU: map[string]int64{},
		lastCPU:    map[string]int64{},
		tagCache:   map[procKey]string{},
		scopeKnown: map[procKey]bool{},
		unitIdent:  map[int]int64{},
	}
}

// SetOptions replaces options (config reload).
func (t *Tracker) SetOptions(o Options) {
	if o.ScopeRootPID != t.opts.ScopeRootPID {
		t.scopeKnown = map[procKey]bool{}
	}
	t.opts = o
}

// SetProtectedPIDs sets pids (wrappers) that are never units and never victims.
func (t *Tracker) SetProtectedPIDs(pids []int) { t.protected = append([]int(nil), pids...) }

// SetTagReader enables rule 4 lookups through r (nil disables them).
func (t *Tracker) SetTagReader(r platform.TagReader) { t.tags = r }

// SetContainers gives the job containers (by job id) that the following
// Attribute calls use (rule C and the job's usage); nil = none.
func (t *Tracker) SetContainers(m map[string]Container) { t.containers = m }

// UnitKey formats a unit key.
func UnitKey(pgid int, startMS int64) string {
	return "pg:" + strconv.Itoa(pgid) + ":" + strconv.FormatInt(startMS, 10)
}

// activeJob is a running job with a process group.
type activeJob struct {
	id     string
	pg     int
	start  int64 // Job.StartMS (0 = unknown)
	reused bool
}

// Attribute assigns procs to jobs (running jobs with PGID set) and units.
// tracked are units the policy already tracks; they are always reported
// while their group exists.
func (t *Tracker) Attribute(procs []platform.Proc, jobs []policy.Job, tracked []policy.Unit) Result {
	res := Result{NProcs: len(procs)}

	// Deterministic order: by start time, then pid. Duplicate pids keep the
	// newest instance (the older one cannot still exist).
	ps := make([]platform.Proc, len(procs))
	copy(ps, procs)
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].StartMS != ps[j].StartMS {
			return ps[i].StartMS < ps[j].StartMS
		}
		return ps[i].PID < ps[j].PID
	})
	byPID := make(map[int]int, len(ps))
	for i := range ps {
		res.TotalUsedBytes += ps[i].Footprint
		if j, ok := byPID[ps[i].PID]; ok && ps[j].StartMS > ps[i].StartMS {
			continue
		}
		byPID[ps[i].PID] = i
	}
	live := make([]bool, len(ps)) // false for shadowed duplicates
	for _, i := range byPID {
		live[i] = true
	}

	excluded := make(map[int]bool, len(t.opts.SelfPIDs)+len(t.protected))
	for _, p := range t.opts.SelfPIDs {
		excluded[p] = true
	}
	for _, p := range t.protected {
		excluded[p] = true
	}

	// Active jobs, sorted by ID.
	var aj []activeJob
	for _, j := range jobs {
		if j.State != policy.JobRunning {
			continue
		}
		pg := j.PGID
		if pg <= 0 {
			pg = j.PID
		}
		if pg <= 0 {
			continue
		}
		aj = append(aj, activeJob{id: j.ID, pg: pg, start: j.StartMS})
	}
	sort.Slice(aj, func(i, j int) bool { return aj[i].id < aj[j].id })
	jobIdx := make(map[string]int, len(aj))
	jobByPG := make(map[int]int, len(aj))
	for i := range aj {
		if _, dup := jobIdx[aj[i].id]; dup {
			continue
		}
		jobIdx[aj[i].id] = i
		// Group reuse: the process at pid == PGID is not the job's leader
		// (see the package comment). Otherwise whatever process holds the
		// old pid now would be adopted as the leader, and its group
		// signalled as the job's.
		li, listed := byPID[aj[i].pg]
		lr, known := t.jobLeader[aj[i].id]
		if !known || lr.pgid != aj[i].pg {
			switch {
			case listed && !t.primed && aj[i].start > 0:
				// Restored job: the leader's start was never observed; the
				// job's StartMS is the only expectation.
				lr = leaderRec{pgid: aj[i].pg, start: aj[i].start, approx: true}
			case listed:
				lr = leaderRec{pgid: aj[i].pg, start: ps[li].StartMS}
			default:
				lr = leaderRec{pgid: aj[i].pg, gone: true}
			}
		}
		if listed {
			switch {
			case !lr.matches(ps[li].StartMS):
				aj[i].reused = true
			case lr.approx:
				lr = leaderRec{pgid: aj[i].pg, start: ps[li].StartMS}
			}
		}
		t.jobLeader[aj[i].id] = lr
		if _, taken := jobByPG[aj[i].pg]; !taken && !aj[i].reused {
			jobByPG[aj[i].pg] = i
		}
	}
	// Only processes started after the oldest running job can belong to one
	// (rule 4 candidates). A leader start observed by the backend is in the
	// same time frame as every other process's start; the wrapper's StartMS
	// is not (Linux: wall clock at spawn), so it is the fallback only.
	tagFrom := int64(-1)
	for i := range aj {
		s := aj[i].start
		if lr, ok := t.jobLeader[aj[i].id]; ok && !lr.approx && !lr.gone && !aj[i].reused {
			s = lr.start
		}
		if s -= LeaderStartToleranceMS; tagFrom < 0 || s < tagFrom {
			tagFrom = max(s, 0)
		}
	}

	// Remembered member-led groups still valid this tick.
	groups := make(map[int]groupRec, len(t.groups))
	for pg, g := range t.groups {
		if _, ok := jobIdx[g.job]; !ok {
			continue
		}
		if li, ok := byPID[pg]; ok && ps[li].StartMS != g.leaderStart {
			continue // pid reused as a new group leader
		}
		groups[pg] = g
	}

	// Rule C: container membership (job id order: deterministic should a pid
	// ever be listed twice).
	var inCont map[int]int
	if len(t.containers) > 0 {
		inCont = map[int]int{}
		for j := range aj {
			if jobIdx[aj[j].id] != j {
				continue
			}
			for _, pid := range t.containers[aj[j].id].PIDs {
				if _, dup := inCont[pid]; !dup {
					inCont[pid] = j
				}
			}
		}
	}

	assign := make([]int, len(ps)) // index into aj, -1 = none
	shared := map[int]bool{}       // containers holding another job's processes
	for i := range ps {
		assign[i] = -1
		p := &ps[i]
		if !live[i] || excluded[p.PID] {
			continue
		}
		jc, inC := inCont[p.PID]
		jg, inG := jobByPG[p.PGID]
		if inC && inG && jg != jc {
			if _, own := t.containers[aj[jg].id]; !own {
				assign[i] = jg // rule 0 over C: a nested job without a container
				shared[jc] = true
				continue
			}
		}
		if inC {
			assign[i] = jc // rule C
			continue
		}
		if inG {
			assign[i] = jg // rule 0
			continue
		}
		if m, ok := t.members[procKey{p.PID, p.StartMS}]; ok {
			if j, ok := jobIdx[m.job]; ok {
				assign[i] = j // rule 1
			}
		}
	}

	tagReads := 0
	for pass := 0; pass <= len(ps); pass++ {
		changed := false
		for i := range ps {
			if assign[i] >= 0 || !live[i] || excluded[ps[i].PID] {
				continue
			}
			p := &ps[i]
			// Rule 2: parent is a member and started no later.
			if pi, ok := byPID[p.PPID]; ok && p.PPID != p.PID && assign[pi] >= 0 && ps[pi].StartMS <= p.StartMS {
				assign[i] = assign[pi]
				changed = true
				continue
			}
			// Rule 3: group led by a member (present, or remembered while gone).
			if li, ok := byPID[p.PGID]; ok && li != i && assign[li] >= 0 && ps[li].StartMS <= p.StartMS {
				assign[i] = assign[li]
				changed = true
				continue
			}
			if _, leaderPresent := byPID[p.PGID]; !leaderPresent {
				if g, ok := groups[p.PGID]; ok && g.leaderStart <= p.StartMS {
					assign[i] = jobIdx[g.job]
					changed = true
					continue
				}
			}
		}
		if changed {
			continue
		}
		// Rule 4 only once rules 2-3 are exhausted; then iterate again so
		// the tagged process' subtree follows.
		for i := range ps {
			if assign[i] >= 0 || !live[i] || excluded[ps[i].PID] {
				continue
			}
			p := &ps[i]
			tag := p.JobTag
			if tag == "" {
				k := procKey{p.PID, p.StartMS}
				cached, done := t.tagCache[k]
				switch {
				case done:
					tag = cached
				case t.tags != nil && tagReads < MaxTagReads && tagCandidate(p, tagFrom) && !parentCovers(ps, live, excluded, assign, byPID, i, tagFrom):
					tagReads++
					tag = t.tags.JobTag(p.PID)
					t.tagCache[k] = tag
				}
			}
			if tag == "" {
				continue
			}
			if j, ok := jobIdx[tag]; ok {
				assign[i] = j
				changed = true
			}
		}
		if !changed {
			break
		}
	}

	// New membership; retire CPU of members that left or vanished.
	newMembers := make(map[procKey]memberRec, len(t.members))
	jobCPU := make([]int64, len(aj))
	used := make([]int64, len(aj))
	nprocs := make([]int, len(aj))
	extra := make([][]int, len(aj))
	newGroups := make(map[int]groupRec)
	res.JobPIDs = make(map[string][]int, len(aj))
	for i := range ps {
		j := assign[i]
		if j < 0 {
			continue
		}
		p := &ps[i]
		k := procKey{p.PID, p.StartMS}
		cpu := p.CPUMS
		if old, ok := t.members[k]; ok && old.job == aj[j].id && old.cpu > cpu {
			cpu = old.cpu
		}
		newMembers[k] = memberRec{job: aj[j].id, cpu: cpu}
		jobCPU[j] += cpu
		if p.State != 'Z' {
			used[j] += p.Footprint
			nprocs[j]++
			res.JobPIDs[aj[j].id] = append(res.JobPIDs[aj[j].id], p.PID)
			if p.PGID != aj[j].pg {
				extra[j] = append(extra[j], p.PID)
			}
		}
		if p.PID == p.PGID && p.PGID != aj[j].pg {
			newGroups[p.PGID] = groupRec{leaderStart: p.StartMS, job: aj[j].id}
		}
	}
	for k, m := range t.members {
		if nm, ok := newMembers[k]; ok && nm.job == m.job {
			continue
		}
		if _, ok := jobIdx[m.job]; ok {
			t.retiredCPU[m.job] += m.cpu
		}
	}
	t.members = newMembers

	// Keep remembered groups (leader gone) while some member still has that pgid.
	memberPG := make(map[int]bool)
	for i := range ps {
		if assign[i] >= 0 {
			memberPG[ps[i].PGID] = true
		}
	}
	for pg, g := range groups {
		if _, ok := newGroups[pg]; !ok && memberPG[pg] {
			newGroups[pg] = g
		}
	}
	t.groups = newGroups

	// Job samples.
	res.Jobs = make([]policy.JobSample, 0, len(aj))
	for j := range aj {
		if jobIdx[aj[j].id] != j {
			continue // duplicate id
		}
		id := aj[j].id
		cpu := t.retiredCPU[id] + jobCPU[j]
		if prev := t.lastCPU[id]; prev > cpu {
			cpu = prev
		}
		t.lastCPU[id] = cpu
		sort.Ints(extra[j])
		sort.Ints(res.JobPIDs[id])
		if c, ok := t.containers[id]; ok && c.HaveUsage && !shared[j] {
			used[j] = c.UsedBytes
		}
		res.Jobs = append(res.Jobs, policy.JobSample{
			ID: id, UsedBytes: used[j], NProcs: nprocs[j], CPUMS: cpu,
			Alive: nprocs[j] > 0, ExtraPIDs: extra[j],
		})
		res.ScopedUsedBytes += used[j]
	}
	for _, m := range []map[string]int64{t.retiredCPU, t.lastCPU} {
		for id := range m {
			if _, ok := jobIdx[id]; !ok {
				delete(m, id)
			}
		}
	}
	for id := range t.jobLeader {
		if _, ok := jobIdx[id]; !ok {
			delete(t.jobLeader, id)
		}
	}

	// Scope.
	inScope := t.scope(ps, live, byPID)
	if inScope != nil {
		res.InScope = make(map[int]bool)
		for i := range ps {
			if inScope[i] {
				res.InScope[ps[i].PID] = true
			}
		}
	}

	// Tag cache: forget processes that are gone.
	present := make(map[procKey]bool, len(ps))
	for i := range ps {
		if live[i] {
			present[procKey{ps[i].PID, ps[i].StartMS}] = true
		}
	}
	for k := range t.tagCache {
		if !present[k] {
			delete(t.tagCache, k)
		}
	}

	t.units(ps, live, assign, excluded, inScope, byPID, tracked, &res)
	t.primed = true
	return res
}

// unitGroup is the unit an unmanaged candidate belongs to: its process group,
// except in process group 0 or 1. That is init's group, where everything a
// PID 1 without job control (a container's entrypoint shell, an agent run as
// PID 1) starts ends up: lumped together it is one unit holding every
// unrelated helper, including the processes of the caller itself, and a
// kill of it (member by member: a group <= 1 is never killpg'd) takes them
// all. There each subtree is a unit of its own instead: the topmost
// candidate ancestor in the same group (started no later), returned as
// -pid. Its key is "pg:<root pid>:<root start>": the root acts as the
// leader of a group of its own, as it would with job control.
func unitGroup(ps []platform.Proc, cand []bool, byPID map[int]int, i int) int {
	pg := ps[i].PGID
	if pg > 1 {
		return pg
	}
	root := i
	for n := 0; n < len(ps); n++ {
		pi, ok := byPID[ps[root].PPID]
		if !ok || pi == root || !cand[pi] || ps[pi].PID <= 1 || ps[pi].PGID != pg || ps[pi].StartMS > ps[root].StartMS {
			break
		}
		root = pi
	}
	return -ps[root].PID
}

// tagCandidate: a process whose PACE_JOB_ID may be read (rule 4).
func tagCandidate(p *platform.Proc, tagFrom int64) bool {
	return tagFrom >= 0 && p.StartMS >= tagFrom && !p.System && p.State != 'Z'
}

// parentCovers: process i need not be read because its parent decides for
// it: the parent is a member (rule 2 follows) or is itself a rule-4
// candidate (read instead; the environment is inherited).
func parentCovers(ps []platform.Proc, live []bool, excluded map[int]bool, assign []int, byPID map[int]int, i int, tagFrom int64) bool {
	p := &ps[i]
	pi, ok := byPID[p.PPID]
	if !ok || pi == i || !live[pi] || excluded[ps[pi].PID] || ps[pi].StartMS > p.StartMS {
		return false
	}
	return assign[pi] >= 0 || (ps[pi].JobTag == "" && tagCandidate(&ps[pi], tagFrom))
}

// scope returns which processes are in scope (nil = all).
func (t *Tracker) scope(ps []platform.Proc, live []bool, byPID map[int]int) []bool {
	root := t.opts.ScopeRootPID
	if root <= 0 {
		return nil
	}
	in := make([]bool, len(ps))
	for i := range ps {
		if live[i] && ps[i].PID != root && t.scopeKnown[procKey{ps[i].PID, ps[i].StartMS}] {
			in[i] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for i := range ps {
			p := &ps[i]
			if in[i] || !live[i] || p.PID == root {
				continue
			}
			if p.PPID == root {
				if ri, ok := byPID[root]; !ok || ps[ri].StartMS <= p.StartMS {
					in[i], changed = true, true
				}
				continue
			}
			if pi, ok := byPID[p.PPID]; ok && pi != i && in[pi] && ps[pi].StartMS <= p.StartMS {
				in[i], changed = true, true
			}
		}
	}
	known := make(map[procKey]bool)
	for i := range ps {
		if in[i] {
			known[procKey{ps[i].PID, ps[i].StartMS}] = true
		}
	}
	t.scopeKnown = known
	return in
}

// noUnitGroups returns the process groups that are never a unit (see the
// package comment).
func (t *Tracker) noUnitGroups(ps []platform.Proc, live []bool, byPID map[int]int, excluded map[int]bool) map[int]bool {
	out := map[int]bool{}
	for _, pg := range t.opts.SelfPGIDs {
		out[pg] = true
	}
	for i := range ps {
		if live[i] && (excluded[ps[i].PID] || t.namedProtected(&ps[i])) {
			out[ps[i].PGID] = true // the daemon's or a wrapper's group, or a protected name's
		}
	}
	if root := t.opts.ScopeRootPID; root > 0 {
		if ri, ok := byPID[root]; ok && live[ri] {
			t.rootPGID = ps[ri].PGID
		}
		if t.rootPGID > 0 {
			out[t.rootPGID] = true
		}
	} else {
		t.rootPGID = 0
	}
	return out
}

// namedProtected reports whether p's comm or executable name is in
// Options.ProtectNames.
func (t *Tracker) namedProtected(p *platform.Proc) bool {
	if len(t.opts.ProtectNames) == 0 {
		return false
	}
	exe := p.Exe
	if i := strings.LastIndexByte(exe, '/'); i >= 0 {
		exe = exe[i+1:]
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	for _, n := range t.opts.ProtectNames {
		if p.Comm == n || exe == n || (len(p.Comm) == 15 && strings.HasPrefix(n, p.Comm)) {
			return true
		}
	}
	return false
}

// isClient reports whether p is one of pace's own client programs
// (Options.ClientExes, Options.ClientNames).
func (t *Tracker) isClient(p *platform.Proc) bool {
	if len(t.opts.ClientExes) == 0 && len(t.opts.ClientNames) == 0 {
		return false
	}
	exe := strings.TrimSuffix(p.Exe, " (deleted)")
	base := exe
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	for _, e := range t.opts.ClientExes {
		if e != "" && exe == e {
			return true
		}
	}
	for _, n := range t.opts.ClientNames {
		if n != "" && (p.Comm == n || base == n) {
			return true
		}
	}
	return false
}

// initSubtreeRoot is the root of process i's subtree of init's group
// (pgid 0/1): its topmost ancestor in the same group, started no later,
// below pid 1 and the scope root (whose children start subtrees of their
// own). Unlike unitGroup it walks through every process, candidate or not.
func (t *Tracker) initSubtreeRoot(ps []platform.Proc, live []bool, byPID map[int]int, i int) int {
	pg := ps[i].PGID
	root := i
	for n := 0; n < len(ps); n++ {
		pi, ok := byPID[ps[root].PPID]
		if !ok || pi == root || !live[pi] || ps[pi].PID <= 1 || ps[pi].PID == t.opts.ScopeRootPID ||
			ps[pi].PGID != pg || ps[pi].StartMS > ps[root].StartMS {
			break
		}
		root = pi
	}
	return root
}

// noUnitSubtrees is noUnitGroups for init's group, which is split by
// subtree (unitGroup): the subtrees holding the daemon or a wrapper (the
// agent or script that ran pace run, and its helpers) are never a unit,
// as the group of any pace run is not. The result is keyed by root index.
func (t *Tracker) noUnitSubtrees(ps []platform.Proc, live []bool, byPID map[int]int, excluded map[int]bool) map[int]bool {
	var out map[int]bool
	for i := range ps {
		if !live[i] || !(excluded[ps[i].PID] || t.namedProtected(&ps[i])) || ps[i].PGID > 1 || ps[i].PID <= 1 {
			continue
		}
		if out == nil {
			out = map[int]bool{}
		}
		out[t.initSubtreeRoot(ps, live, byPID, i)] = true
	}
	return out
}

type unitAcc struct {
	pgid      int
	used      int64
	cpu       int64
	pids      []int
	minStart  int64
	leaderIdx int  // candidate index of pid == pgid, -1
	lowIdx    int  // candidate with the lowest pid
	client    bool // holds one of pace's own client programs
}

// units fills res.Units, res.SmallUnits, res.SmallGroups and res.SmallBytes,
// and adds the in-scope unmanaged footprint to res.ScopedUsedBytes.
func (t *Tracker) units(ps []platform.Proc, live []bool, assign []int, excluded map[int]bool, inScope []bool,
	byPID map[int]int, tracked []policy.Unit, res *Result) {
	scoped := &res.ScopedUsedBytes
	acc := map[int]*unitAcc{}
	var order []int
	// A protected wrapper's own child, and its process group, is a governed
	// job whose start the policy has not seen yet (the wrapper spawns, then
	// reports the pid; attribution runs before that report is applied). It
	// counts towards scoped usage but is never an unmanaged unit: otherwise a
	// job that allocates fast is killed as "unmanaged" in its first tick,
	// without its KILLED line ever reaching its wrapper.
	prot := make(map[int]bool, len(t.protected))
	for _, p := range t.protected {
		prot[p] = true
	}
	noUnitPG := t.noUnitGroups(ps, live, byPID, excluded)
	noUnitTree := t.noUnitSubtrees(ps, live, byPID, excluded)
	pendingPID := map[int]bool{}
	pendingPG := map[int]bool{}
	for i := range ps {
		p := &ps[i]
		if !live[i] || assign[i] >= 0 || !prot[p.PPID] || prot[p.PID] {
			continue
		}
		if wi, ok := byPID[p.PPID]; ok && ps[wi].StartMS > p.StartMS {
			continue // the parent pid was reused by a newer wrapper
		}
		pendingPID[p.PID] = true
		if p.PGID == p.PID {
			pendingPG[p.PGID] = true
		}
	}
	cand := make([]bool, len(ps))
	for i := range ps {
		p := &ps[i]
		if !live[i] || assign[i] >= 0 || excluded[p.PID] || p.System {
			continue
		}
		if inScope != nil && !inScope[i] {
			continue
		}
		*scoped += p.Footprint
		if p.State == 'Z' || pendingPID[p.PID] || pendingPG[p.PGID] {
			continue
		}
		// Never a unit: pid 1; a group of pace's own or the scope root's (for
		// pgid 0/1, which is split by subtree, the subtree holding the daemon
		// or a wrapper, and the scope root itself, so its children start
		// subtrees of their own); a small terminal shell.
		if p.PID <= 1 || (p.PGID > 1 && noUnitPG[p.PGID]) || (p.PGID <= 1 && p.PID == t.opts.ScopeRootPID) ||
			(p.PGID <= 1 && noUnitTree[t.initSubtreeRoot(ps, live, byPID, i)]) ||
			(p.TermLeader && p.Footprint < t.opts.MinUnitBytes) {
			continue
		}
		cand[i] = true
	}
	for i := range ps {
		if !cand[i] {
			continue
		}
		p := &ps[i]
		g := unitGroup(ps, cand, byPID, i)
		a := acc[g]
		if a == nil {
			a = &unitAcc{pgid: p.PGID, minStart: p.StartMS, leaderIdx: -1, lowIdx: i}
			acc[g] = a
			order = append(order, g)
		}
		a.used += p.Footprint
		a.cpu += p.CPUMS
		if !a.client && t.isClient(p) {
			a.client = true
		}
		a.pids = append(a.pids, p.PID)
		if p.StartMS < a.minStart {
			a.minStart = p.StartMS
		}
		if p.PID == p.PGID || p.PID == -g {
			a.leaderIdx = i
		}
		if p.PID < ps[a.lowIdx].PID {
			a.lowIdx = i
		}
	}
	sort.Ints(order)

	trackedByPG := map[int][]policy.Unit{}
	trackedKeys := map[string]bool{}
	for _, u := range tracked {
		trackedByPG[u.PGID] = append(trackedByPG[u.PGID], u)
		trackedKeys[u.Key] = true
	}

	all := make([]policy.UnitSample, 0, len(order))
	client := make([]bool, 0, len(order)) // parallel to all
	newIdent := make(map[int]int64, len(order))
	for _, pg := range order {
		a := acc[pg]
		if pg < 0 {
			// A subtree of init's group (see unitGroup): keyed by its root
			// process, which is always present (a root that exits leaves
			// its children roots of their own).
			r := ps[a.leaderIdx]
			sort.Ints(a.pids)
			all = append(all, policy.UnitSample{
				Key: UnitKey(r.PID, r.StartMS), PGID: a.pgid, LeaderPID: r.PID, StartMS: r.StartMS,
				Comm: r.Comm, UsedBytes: a.used, NProcs: len(a.pids), CPUMS: a.cpu, PIDs: a.pids,
			})
			client = append(client, a.client)
			continue
		}
		var start int64
		if li, ok := byPID[pg]; ok {
			start = ps[li].StartMS // the real leader defines the group
		} else if s, ok := t.unitIdent[pg]; ok && s <= a.minStart {
			start = s
		} else {
			start = a.minStart
			// After a restart the tracker has no memory; adopt the policy's
			// identity for this group if it is consistent with the members.
			best := int64(-1)
			for _, u := range trackedByPG[pg] {
				if u.StartMS <= a.minStart && u.StartMS > best {
					best = u.StartMS
				}
			}
			if best >= 0 {
				start = best
			}
		}
		newIdent[pg] = start
		rep := a.lowIdx
		if a.leaderIdx >= 0 {
			rep = a.leaderIdx
		}
		sort.Ints(a.pids)
		all = append(all, policy.UnitSample{
			Key: UnitKey(pg, start), PGID: pg, LeaderPID: ps[rep].PID, StartMS: start,
			Comm: ps[rep].Comm, UsedBytes: a.used, NProcs: len(a.pids), CPUMS: a.cpu, PIDs: a.pids,
		})
		client = append(client, a.client)
	}
	t.unitIdent = newIdent

	// Select: big units (largest first, capped), plus tracked ones. Groups
	// below MinUnitBytes are counted in the small summary and the largest
	// untracked ones (capped separately) listed in SmallUnits.
	var idx, sidx []int
	for i := range all {
		if all[i].UsedBytes >= t.opts.MinUnitBytes {
			idx = append(idx, i)
			continue
		}
		res.SmallGroups++
		res.SmallBytes += all[i].UsedBytes
		if !trackedKeys[all[i].Key] && !client[i] {
			sidx = append(sidx, i)
		}
	}
	largest := func(ix []int) []int {
		sort.Slice(ix, func(a, b int) bool {
			ua, ub := &all[ix[a]], &all[ix[b]]
			if ua.UsedBytes != ub.UsedBytes {
				return ua.UsedBytes > ub.UsedBytes
			}
			return ua.Key < ub.Key
		})
		if t.opts.MaxUnits > 0 && len(ix) > t.opts.MaxUnits {
			ix = ix[:t.opts.MaxUnits]
		}
		return ix
	}
	keep := make([]bool, len(all))
	for _, i := range largest(idx) {
		keep[i] = true
	}
	res.Units = nil
	for i := range all {
		if keep[i] || trackedKeys[all[i].Key] {
			res.Units = append(res.Units, all[i])
		}
	}
	sort.Slice(res.Units, func(a, b int) bool { return res.Units[a].Key < res.Units[b].Key })
	res.SmallUnits = nil
	for _, i := range largest(sidx) {
		res.SmallUnits = append(res.SmallUnits, all[i])
	}
	sort.Slice(res.SmallUnits, func(a, b int) bool { return res.SmallUnits[a].Key < res.SmallUnits[b].Key })
}
