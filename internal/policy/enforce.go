package policy

import (
	"sort"
	"syscall"

	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
)

// Hints. Each names something the agent can do next; messages with no
// possible action (OK, INFO, THROTTLED, FROZEN, THAWED, orphaned kills)
// carry none.
const (
	HintMemBudgetWarn = "lower parallelism now or the job will be killed"
	HintMemBudgetKill = "retry with lower parallelism or request a larger budget"
	HintPressureWarn  = "free memory or finish soon; you are the next to be stopped"
	HintPressureKill  = "retry when pace status shows headroom"
	HintWallClock     = "split the work or raise --timeout"
	HintStuckWarn     = "the job shows no CPU or output progress; make it progress or stop it, or it will be killed"
	HintStuckKill     = "check why the command hangs (e.g. waiting for input) before retrying"
	HintTooBig        = "lower parallelism or the declared estimate; this machine can never give that much"
	HintTooBigLearned = "earlier runs of this command peaked above what this machine can ever give; declare --mem if this run needs less"
	HintHeadroom      = "retry after retry_after, or lower the estimate or parallelism"
	HintPressureWait  = "retry after retry_after, when memory pressure has eased"
	HintSlots         = "retry after retry_after; this class's concurrency slots are full"
	HintDisk          = "free disk space, then retry"
	HintQueueTimeout  = "retry after retry_after, or raise the queue timeout"
	HintJobWarning    = "retry after retry_after, when current warnings have cleared"
	HintDiskWarn      = "free disk space before it runs out; new jobs will be refused"
	HintDiskCritical  = "free disk space; new jobs are refused until it recovers"
)

// Hints of a unit's mem_budget lines (a process group started outside pace
// run). Nothing retries it and it has no budget of its own to ask for: its
// budget is the operator's unmanaged.budget, and the way to a budget of its
// own is to run it through pace run.
const (
	HintUnitBudgetWarn = "this process was not started by pace run and is over unmanaged.budget; free its memory or stop it, or it will be killed"
	HintUnitBudgetKill = "this process was not started by pace run and exceeded unmanaged.budget; run it through pace run with --mem and --budget, or raise unmanaged.budget"
)

// Hints of WARN lines in observe and warn-only mode, where the action in
// next= is not carried out (the line carries mode=): they say what to do
// without threatening what will not happen.
const (
	HintMemBudgetWarnWould  = "lower parallelism to stay within the budget (this mode only warns: the job will not be killed)"
	HintPressureWarnWould   = "free memory or finish soon (this mode only warns: the job will not be stopped)"
	HintStuckWarnWould      = "the job shows no CPU or output progress; make it progress or stop it (this mode only warns: it will not be killed)"
	HintWallClockWould      = "split the work or raise --timeout (this mode only warns: the job will not be killed)"
	HintDiskWould           = "free disk space before it runs out (this mode only warns: new jobs will not be refused)"
	HintUnitBudgetWarnWould = "this process was not started by pace run and is over unmanaged.budget; free its memory or stop it (this mode only warns: it will not be killed)"
)

// wouldHint is the non-enforce form of an enforce-mode WARN hint.
func wouldHint(h, next string) string {
	switch h {
	case "":
		return ""
	case HintMemBudgetWarn:
		return HintMemBudgetWarnWould
	case HintUnitBudgetWarn:
		return HintUnitBudgetWarnWould
	case HintPressureWarn:
		return HintPressureWarnWould
	case HintStuckWarn:
		return HintStuckWarnWould
	case HintWallClock:
		return HintWallClockWould
	case HintDiskWarn, HintDiskCritical:
		return HintDiskWould
	}
	past := map[string]string{"kill": "killed", "freeze": "frozen", "throttle": "throttled", "refuse": "refused"}[next]
	if past == "" {
		return h
	}
	return h + " (this mode only warns: nothing will be " + past + ")"
}

// target is one enforceable thing: a started governed job or an unmanaged
// process group. Pointers refer into the step's private State copy.
type target struct {
	job  *Job
	unit *Unit
	enf  *Enforcement

	id     string // job ID or unit key
	pid    int
	pgid   int
	extras []int // pids signalled individually in addition to the group
	used   int64
	budget int64
	prio   int
	start  int64
}

func (t *target) overBudget() bool { return t.budget > 0 && t.used > t.budget }

func (t *target) dying() bool {
	return t.enf.Stage == StageTerminating || t.enf.Stage == StageKilled
}

// victimLess is the fixed victim order: over budget first, then lowest
// priority, then youngest (latest start), then id; jobs before units on a
// full tie.
func victimLess(a, b *target) bool {
	if ao, bo := a.overBudget(), b.overBudget(); ao != bo {
		return ao
	}
	if a.prio != b.prio {
		return a.prio < b.prio
	}
	if a.start != b.start {
		return a.start > b.start
	}
	if a.id != b.id {
		return a.id < b.id
	}
	return a.job != nil && b.job == nil
}

func (c *stepCtx) buildTargets() {
	c.targets = nil
	for i := range c.s.Jobs {
		j := &c.s.Jobs[i]
		if !c.jobAlive(j) {
			continue
		}
		t := &target{job: j, enf: &j.Enf, id: j.ID, pid: j.PID, pgid: j.PGID,
			used: j.UsedBytes, budget: j.BudgetBytes, prio: j.Priority, start: j.StartMS}
		// killpg only when the job leads its own group (the wrapper's
		// Setpgid). Otherwise the group may hold the wrapper or the agent
		// itself, so the job's pid and extras are signalled one by one.
		if t.pgid <= 1 || t.pgid != t.pid {
			t.pgid = 0
		}
		if js, ok := c.jobSamples[j.ID]; ok {
			t.extras = js.ExtraPIDs
		}
		if t.pgid == 0 && t.pid > 1 {
			t.extras = sortedPIDs(append([]int{t.pid}, t.extras...))
		}
		if t.pgid == 0 && len(t.extras) == 0 {
			continue // nothing that can be signalled
		}
		c.targets = append(c.targets, t)
	}
	for i := range c.s.Units {
		u := &c.s.Units[i]
		t := &target{unit: u, enf: &u.Enf, id: u.Key, pid: u.LeaderPID, pgid: u.PGID,
			used: u.UsedBytes, budget: c.p.UnmanagedBudgetBytes, prio: c.p.UnmanagedPriority, start: u.StartMS}
		if t.pgid <= 1 {
			t.pgid = 0
		}
		if us := c.unitSample(u.Key); us != nil {
			leaderIn := false
			for _, p := range us.PIDs {
				if p == u.LeaderPID {
					leaderIn = true
				}
			}
			// killpg reaches the whole group; members are signalled one by
			// one too when the leader is gone or there is no usable group.
			if !leaderIn || u.LeaderPID <= 1 || t.pgid == 0 {
				t.extras = us.PIDs
			}
		}
		if t.pgid == 0 && len(t.extras) == 0 {
			continue // nothing that can be signalled
		}
		c.targets = append(c.targets, t)
	}
}

func (c *stepCtx) targetEvent(t *target, verb string) proto.Event {
	e := c.base(verb)
	if t.job != nil {
		e.Job, e.Class = t.job.ID, t.job.Class
	} else {
		e.Unit, e.Comm = proto.UnitRef(t.unit.Key), t.unit.Comm
	}
	e.PID, e.PGID = t.pid, t.pgid
	return e
}

func (t *target) ref(a Action) Action {
	if t.job != nil {
		a.Job = t.job.ID
	} else {
		a.Unit = t.unit.Key
	}
	return a
}

// sigActs signals the whole target: killpg on the group (never pgid <= 1)
// plus each extra pid individually.
func (c *stepCtx) sigActs(t *target, sig syscall.Signal, policy string) []Action {
	var acts []Action
	if t.pgid > 1 {
		acts = append(acts, t.ref(Action{Op: OpSignalGroup, PGID: t.pgid, Signal: int(sig), Policy: policy}))
	}
	if len(t.extras) > 0 {
		acts = append(acts, t.ref(Action{Op: OpSignal, PIDs: append([]int(nil), t.extras...), Signal: int(sig), Policy: policy}))
	}
	return acts
}

// bgAct is OpBackground/OpForeground for every member of the group (the
// daemon expands PGID from its process list) plus the listed pids.
func (c *stepCtx) bgAct(t *target, op, policy string) []Action {
	pids := t.extras
	if t.pid > 1 {
		pids = sortedPIDs(append([]int{t.pid}, t.extras...))
	}
	if t.pgid <= 1 && len(pids) == 0 {
		return nil
	}
	return []Action{t.ref(Action{Op: op, PGID: t.pgid, PIDs: pids, Policy: policy})}
}

func findWarning(e *Enforcement, policy string) int {
	for i, w := range e.Warnings {
		if w.Policy == policy {
			return i
		}
	}
	return -1
}

func addWarning(e *Enforcement, w Warning) {
	if i := findWarning(e, w.Policy); i >= 0 {
		e.Warnings[i] = w
		return
	}
	e.Warnings = append(e.Warnings, w)
	sort.SliceStable(e.Warnings, func(i, j int) bool { return e.Warnings[i].Policy < e.Warnings[j].Policy })
}

func removeWarning(e *Enforcement, policy string) {
	i := findWarning(e, policy)
	if i < 0 {
		return
	}
	e.Warnings = append(e.Warnings[:i], e.Warnings[i+1:]...)
	if len(e.Warnings) == 0 {
		e.Warnings = nil
	}
}

func (c *stepCtx) killGrace() int64 {
	if c.level == proto.LevelCritical {
		return c.p.CriticalGraceMS
	}
	return c.p.GraceMS
}

// ---- 5. mode normalisation -------------------------------------------------

// normalizeMode keeps real and virtual enforcement apart across a mode
// change (config reload). In enforce mode, virtual stages are forgotten so
// the target is judged afresh for real. In observe/warn-only, real
// throttles and freezes are released at once (these are relaxing actions
// and always real); a real termination already announced completes.
func (c *stepCtx) normalizeMode() {
	for _, t := range c.targets {
		e := t.enf
		if c.enforce {
			if e.Virtual {
				e.Stage, e.StageMS, e.NextStepMS, e.KillPolicy = StageNone, 0, 0, ""
				e.CPUThrottle, e.Virtual = false, false
			}
			continue
		}
		if e.Virtual || t.dying() {
			continue
		}
		if e.Stage == StageFrozen {
			c.do(false, c.sigActs(t, syscall.SIGCONT, proto.PolicyPressure)...)
			ev := c.targetEvent(t, proto.VerbThawed)
			ev.Policy = proto.PolicyPressure
			c.emit(ev)
			if t.job != nil {
				t.job.LastProgressMS = c.now
			}
		}
		if e.Stage == StageThrottled || e.Stage == StageFrozen || e.CPUThrottle {
			pol := proto.PolicyPressure
			if e.CPUThrottle && !memStage(e) {
				pol = proto.PolicyCPU
			}
			c.do(false, c.bgAct(t, OpForeground, pol)...)
			ev := c.targetEvent(t, proto.VerbUnthrottled)
			ev.Policy = pol
			c.emit(ev)
		}
		e.Stage, e.StageMS, e.NextStepMS, e.CPUThrottle = StageNone, 0, 0, false
	}
}

// ---- 6. termination progress -----------------------------------------------

func (c *stepCtx) progressTerminations() {
	if c.hold {
		return // SIGKILL after grace waits for a fresh process list
	}
	for _, t := range c.targets {
		e := t.enf
		if !t.dying() {
			continue
		}
		if e.Virtual && e.Stage == StageKilled {
			continue // the virtual kill is fully logged; nothing to repeat
		}
		if c.level == proto.LevelCritical && e.Stage == StageTerminating {
			if d := e.StageMS + c.p.CriticalGraceMS; d < e.NextStepMS {
				e.NextStepMS = d
			}
		}
		if c.now < e.NextStepMS {
			continue
		}
		// Grace over and still alive: stop everything (no more forks), then
		// SIGKILL. Repeated every grace period while members remain.
		acts := c.sigActs(t, syscall.SIGSTOP, e.KillPolicy)
		acts = append(acts, c.sigActs(t, syscall.SIGKILL, e.KillPolicy)...)
		c.do(e.Virtual, acts...)
		if e.Virtual {
			ev := c.targetEvent(t, proto.VerbInfo)
			ev.Policy = e.KillPolicy
			ev.Msg = "would force-kill (SIGKILL) after grace"
			c.emit(ev)
		}
		if e.Stage == StageTerminating {
			e.Stage = StageKilled
			e.StageMS = c.now
		}
		g := c.p.GraceMS
		if g <= 0 {
			g = 1000
		}
		e.NextStepMS = c.now + g
	}
}

// kill starts termination: SIGTERM to the group and extras (SIGCONT too if
// frozen) and the KILLED event, at TERM time. SIGKILL follows after grace.
// On a stale sample nothing happens (it reports false): the kill is decided
// again on the next fresh step.
func (c *stepCtx) kill(t *target, policy string, hint string, decorate func(*proto.Event)) bool {
	if c.hold {
		return false
	}
	virtual := !c.enforce
	e := t.enf
	acts := c.sigActs(t, syscall.SIGTERM, policy)
	if e.Stage == StageFrozen {
		acts = append(acts, c.sigActs(t, syscall.SIGCONT, policy)...)
	}
	c.do(virtual, acts...)

	ev := c.targetEvent(t, proto.VerbKilled)
	ev.Policy = policy
	ev.Level = c.level
	ev.UsedBytes = proto.I64(t.used)
	if t.budget > 0 {
		ev.BudgetBytes = proto.I64(t.budget)
	}
	if decorate != nil {
		decorate(&ev)
	}
	if virtual {
		ev.Verb = proto.VerbInfo
		ev.Msg = "would kill (SIGTERM, then SIGKILL after grace)"
	} else {
		ev.Hint = hint
	}
	c.emit(ev)

	e.Warnings = nil
	e.Stage = StageTerminating
	e.StageMS = c.now
	e.NextStepMS = c.now + c.killGrace()
	e.KillPolicy = policy
	e.Virtual = virtual
	return true
}

// ---- 7. per-target policies ------------------------------------------------

func (c *stepCtx) perTargetPolicies() {
	for _, t := range c.targets {
		if t.dying() {
			continue
		}
		if t.job != nil {
			j := t.job
			if j.WrapperLostMS > 0 && c.now-j.WrapperLostMS >= c.p.OrphanGraceMS {
				// Nobody can receive a warning: kill at once, no hint.
				c.kill(t, proto.PolicyOrphaned, "", nil)
				continue
			}
		}
		if t.job == nil && t.used < c.p.MinVictimBytes && findWarning(t.enf, proto.PolicyMemBudget) < 0 {
			// A unit below min_victim is never a new mem_budget victim: the
			// daemon offers such units only under memory pressure, and only
			// the pressure ladder may act on them, as a swarm (candidates).
			// One warned while it was larger still gets its OK or kill.
			continue
		}
		if c.memBudget(t) {
			continue
		}
		if t.job == nil {
			continue
		}
		if c.wallClock(t) {
			continue
		}
		c.stuck(t)
	}
}

func (c *stepCtx) budgetEvent(t *target, verb string) proto.Event {
	ev := c.targetEvent(t, verb)
	ev.Policy = proto.PolicyMemBudget
	ev.UsedBytes = proto.I64(t.used)
	ev.BudgetBytes = proto.I64(t.budget)
	return ev
}

// memBudget: WARN at BudgetWarnPermille of budget (deadline WarnDeadlineMS),
// kill at once above budget*(1+tolerance), kill at the deadline if still
// above budget, OK once usage falls okHysteresis below the warn threshold.
// Returns true if the target was killed.
func (c *stepCtx) memBudget(t *target) bool {
	b := t.budget
	if b <= 0 {
		removeWarning(t.enf, proto.PolicyMemBudget)
		return false
	}
	tol := b + b*c.p.TolerancePermille/1000
	warnAt := b
	if c.p.BudgetWarnPermille > 0 {
		warnAt = b * c.p.BudgetWarnPermille / 1000
	}
	okBelow := warnAt - b*okHysteresisPermille/1000
	warnHint, killHint := HintMemBudgetWarn, HintMemBudgetKill
	if t.job == nil {
		warnHint, killHint = HintUnitBudgetWarn, HintUnitBudgetKill
	}
	wi := findWarning(t.enf, proto.PolicyMemBudget)
	if t.used > tol {
		if c.hold {
			return false
		}
		if !c.enforce && wi < 0 {
			// Observe / warn-only: the kill is only logged ("would kill"),
			// so the WARN is the agent's one actionable signal. Without it a
			// job that jumps straight past the tolerance band is never
			// warned at all. (In enforce mode the KILLED line and its hint
			// carry the same information at once.)
			ev := c.budgetEvent(t, proto.VerbWarn)
			ev.Level = c.level
			ev.Next = "kill"
			ev.InMS = proto.I64(0)
			ev.Hint = warnHint
			c.emit(ev)
		}
		// Say why the kill comes now: a WARN may have announced a later
		// deadline (next=kill in=...), which holds only inside the
		// tolerance band.
		msg := tolMsg(tol, warnPending(t.enf, c.now))
		return c.kill(t, proto.PolicyMemBudget, killHint, func(ev *proto.Event) { ev.Msg = msg })
	}
	if t.used >= warnAt {
		if wi < 0 {
			addWarning(t.enf, Warning{Policy: proto.PolicyMemBudget, IssuedMS: c.now, DeadlineMS: c.now + c.p.WarnDeadlineMS})
			wi = findWarning(t.enf, proto.PolicyMemBudget)
			ev := c.budgetEvent(t, proto.VerbWarn)
			ev.Level = c.level
			ev.Next = "kill"
			ev.InMS = proto.I64(c.p.WarnDeadlineMS)
			ev.Hint = warnHint
			c.emit(ev)
		}
		if c.now >= t.enf.Warnings[wi].DeadlineMS && t.used > b {
			return c.kill(t, proto.PolicyMemBudget, killHint, nil)
		}
		return false
	}
	if wi >= 0 && t.used < okBelow {
		removeWarning(t.enf, proto.PolicyMemBudget)
		c.emit(c.budgetEvent(t, proto.VerbOK))
	}
	return false
}

// tolMsg is the msg of a mem_budget kill past the tolerance line tol (the
// budget plus tolerance), which comes at once, without waiting for a WARN's
// deadline. early: a WARN announced a deadline that has not passed yet.
func tolMsg(tol int64, early bool) string {
	m := "used is over budget + tolerance (" + units.FormatBytes(tol) + "): killed at once"
	if early {
		m += proto.MsgBeforeWarnDeadline
	}
	return m
}

// warnPending: a mem_budget WARN announced a kill deadline that has not
// passed at now.
func warnPending(e *Enforcement, now int64) bool {
	wi := findWarning(e, proto.PolicyMemBudget)
	return wi >= 0 && now < e.Warnings[wi].DeadlineMS
}

// wallClock: WARN at limit-WarnDeadline, kill at the limit.
func (c *stepCtx) wallClock(t *target) bool {
	j := t.job
	limit := j.WallLimitMS
	if limit <= 0 || j.StartMS <= 0 {
		return false
	}
	elapsed := c.now - j.StartMS
	dec := func(ev *proto.Event) {
		ev.ElapsedMS = proto.I64(elapsed)
		ev.LimitMS = proto.I64(limit)
	}
	if elapsed >= limit {
		return c.kill(t, proto.PolicyWallClock, HintWallClock, dec)
	}
	warnAt := limit - c.p.WarnDeadlineMS
	if elapsed >= warnAt && findWarning(t.enf, proto.PolicyWallClock) < 0 {
		addWarning(t.enf, Warning{Policy: proto.PolicyWallClock, IssuedMS: c.now, DeadlineMS: j.StartMS + limit})
		ev := c.targetEvent(t, proto.VerbWarn)
		ev.Policy = proto.PolicyWallClock
		dec(&ev)
		ev.Next = "kill"
		ev.InMS = proto.I64(limit - elapsed)
		ev.Hint = HintWallClock
		c.emit(ev)
	}
	return false
}

// stuck: no CPU and no output progress for NoProgressMS -> WARN; any
// progress clears it (OK); still none at the deadline -> kill. Not
// evaluated while frozen (the governor stopped it) or on a stale sample
// (CPU progress is not measured then).
func (c *stepCtx) stuck(t *target) bool {
	j := t.job
	limit := j.NoProgressMS
	if limit <= 0 || t.enf.Stage == StageFrozen || c.hold {
		return false
	}
	idle := c.now - j.LastProgressMS
	dec := func(ev *proto.Event) {
		ev.IdleMS = proto.I64(idle)
		ev.LimitMS = proto.I64(limit)
	}
	if wi := findWarning(t.enf, proto.PolicyStuck); wi >= 0 {
		w := t.enf.Warnings[wi]
		if j.LastProgressMS > w.IssuedMS {
			removeWarning(t.enf, proto.PolicyStuck)
			ev := c.targetEvent(t, proto.VerbOK)
			ev.Policy = proto.PolicyStuck
			c.emit(ev)
			return false
		}
		if c.now >= w.DeadlineMS {
			return c.kill(t, proto.PolicyStuck, HintStuckKill, dec)
		}
		return false
	}
	if idle >= limit {
		addWarning(t.enf, Warning{Policy: proto.PolicyStuck, IssuedMS: c.now, DeadlineMS: c.now + c.p.WarnDeadlineMS})
		ev := c.targetEvent(t, proto.VerbWarn)
		ev.Policy = proto.PolicyStuck
		dec(&ev)
		ev.Next = "kill"
		ev.InMS = proto.I64(c.p.WarnDeadlineMS)
		ev.Hint = HintStuckWarn
		c.emit(ev)
		if c.p.WarnDeadlineMS <= 0 {
			return c.kill(t, proto.PolicyStuck, HintStuckKill, dec)
		}
	}
	return false
}
