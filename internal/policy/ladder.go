package policy

import (
	"sort"
	"syscall"

	"github.com/fireharp/pace/internal/proto"
)

// isPressureTarget: already chosen by the pressure ladder.
func isPressureTarget(t *target) bool {
	return hasWarning(t.enf, proto.PolicyPressure) || memStage(t.enf)
}

// SmallVictimDivisor: an unmanaged group below MinVictimBytes may still be a
// pressure victim (see candidates), but never one below
// MinVictimBytes/SmallVictimDivisor: killing it frees next to nothing.
const SmallVictimDivisor = 8

// candidates returns the possible victims in fixed victim order: alive,
// not already dying, at least MinVictimBytes.
//
// Small unmanaged groups (units below MinVictimBytes, at least
// MinVictimBytes/SmallVictimDivisor) join them only when they make the
// difference. The candidates that rank no later than unmanaged groups
// (over budget, or priority <= UnmanagedPriority) and are not yet counted,
// together with freed (what is already counted as being freed; counted
// says which candidates that includes), must fall short of need, and with
// the uncounted small groups added must reach it: the swarm then covers the
// deficit before any candidate of higher priority would be chosen. A swarm
// of small processes outside pace thus loses before a wrapped job of higher
// priority, while a swarm that cannot spare that job is left alone (it
// would only die with the job, for nothing), and so is a small group
// alone: the small groups must hold at least MinVictimBytes together.
// Within a priority, small groups come after the big ones.
func (c *stepCtx) candidates(need, freed int64, counted func(*target) bool) []*target {
	var out, small []*target
	var smallSum int64
	floor := max(1, c.p.MinVictimBytes/SmallVictimDivisor)
	for _, t := range c.targets {
		if t.dying() {
			continue
		}
		if t.used >= c.p.MinVictimBytes {
			out = append(out, t)
			continue
		}
		if t.unit != nil && t.used >= floor {
			small = append(small, t)
			if !counted(t) {
				smallSum += t.used
			}
		}
	}
	if len(small) > 0 && smallSum >= c.p.MinVictimBytes {
		cover := freed
		for _, t := range out {
			if !counted(t) && (t.overBudget() || t.prio <= c.p.UnmanagedPriority) {
				cover += t.used
			}
		}
		if cover < need && cover+smallSum >= need {
			out = append(out, small...)
		}
	}
	minV := c.p.MinVictimBytes
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ao, bo := a.overBudget(), b.overBudget(); ao == bo && a.prio == b.prio {
			if as, bs := a.used < minV, b.used < minV; as != bs {
				return bs // big before small within a priority
			}
		}
		return victimLess(a, b)
	})
	return out
}

// deficit is how many bytes must be freed to bring avail back above the
// level's threshold plus a margin of half the warn-critical gap (or
// MinVictimBytes when the thresholds coincide). Always >= 1, so at least one
// victim is chosen even when the level comes from the kernel or PSI.
func (c *stepCtx) deficit() int64 {
	warnB, critB := thresholds(c.p, c.capacity)
	margin := (warnB - critB) / 2
	if margin <= 0 {
		margin = c.p.MinVictimBytes
	}
	goal := warnB
	if c.level == proto.LevelCritical {
		goal = critB
	}
	d := goal + margin - c.avail
	if d < 1 {
		d = 1
	}
	return d
}

func (c *stepCtx) pressureEvent(t *target, verb string) proto.Event {
	ev := c.targetEvent(t, verb)
	ev.Policy = proto.PolicyPressure
	ev.Level = c.level
	ev.FreeBytes = proto.I64(nonNeg(c.avail))
	ev.UsedBytes = proto.I64(t.used)
	if t.budget > 0 {
		ev.BudgetBytes = proto.I64(t.budget)
	}
	return ev
}

// ---- 8. pressure ladder ----------------------------------------------------

func (c *stepCtx) pressureLadder() {
	switch c.level {
	case proto.LevelCritical:
		c.critical()
	case proto.LevelWarn:
		c.warnLevel()
	default:
		c.recover()
	}
}

// warnLevel: WARN prospective victims until their usage covers the deficit;
// once a victim's deadline has passed and pressure is still >= warn,
// escalate it one stage per StepMS: throttle -> freeze -> terminate.
func (c *stepCtx) warnLevel() {
	if c.hold {
		return // victims and their stages wait for a fresh sample
	}
	need := c.deficit()
	var freed int64
	for _, t := range c.targets {
		if t.dying() || isPressureTarget(t) {
			freed += t.used
		}
	}
	cands := c.candidates(need, freed, isPressureTarget)
	for _, t := range cands {
		if freed >= need {
			break
		}
		if isPressureTarget(t) {
			continue
		}
		addWarning(t.enf, Warning{Policy: proto.PolicyPressure, IssuedMS: c.now, DeadlineMS: c.now + c.p.WarnDeadlineMS})
		ev := c.pressureEvent(t, proto.VerbWarn)
		ev.Next = "throttle"
		ev.InMS = proto.I64(c.p.WarnDeadlineMS)
		ev.Hint = HintPressureWarn
		c.emit(ev)
		freed += t.used
	}
	// Every chosen victim escalates, even one that has since shrunk below
	// MinVictimBytes: it must not stay frozen while pressure lasts.
	all := append([]*target(nil), c.targets...)
	sort.SliceStable(all, func(i, j int) bool { return victimLess(all[i], all[j]) })
	for _, t := range all {
		if !t.dying() && isPressureTarget(t) {
			c.escalate(t)
		}
	}
}

func (c *stepCtx) escalate(t *target) {
	e := t.enf
	wi := findWarning(e, proto.PolicyPressure)
	if wi < 0 && memStage(e) {
		// Recovery already told this target OK but had not stepped it all
		// the way down when pressure came back: warn it again, with a fresh
		// deadline, before escalating it any further.
		addWarning(e, Warning{Policy: proto.PolicyPressure, IssuedMS: c.now, DeadlineMS: c.now + c.p.WarnDeadlineMS})
		ev := c.pressureEvent(t, proto.VerbWarn)
		ev.Next = "freeze"
		if e.Stage == StageFrozen {
			ev.Next = "kill"
		}
		ev.InMS = proto.I64(c.p.WarnDeadlineMS)
		ev.Hint = HintPressureWarn
		c.emit(ev)
		return
	}
	if wi >= 0 && c.now < e.Warnings[wi].DeadlineMS {
		return
	}
	if c.now < e.NextStepMS {
		return
	}
	virtual := !c.enforce
	switch e.Stage {
	case StageNone:
		if !e.CPUThrottle {
			c.do(virtual, c.bgAct(t, OpBackground, proto.PolicyPressure)...)
		}
		ev := c.pressureEvent(t, proto.VerbThrottled)
		ev.Next = "freeze"
		ev.InMS = proto.I64(c.p.StepMS)
		c.emitStage(ev, virtual, "would throttle")
		e.Stage = StageThrottled
	case StageThrottled:
		c.do(virtual, c.sigActs(t, syscall.SIGSTOP, proto.PolicyPressure)...)
		ev := c.pressureEvent(t, proto.VerbFrozen)
		ev.Next = "kill"
		ev.InMS = proto.I64(c.p.StepMS)
		c.emitStage(ev, virtual, "would freeze (SIGSTOP)")
		e.Stage = StageFrozen
	case StageFrozen:
		c.kill(t, proto.PolicyPressure, HintPressureKill, func(ev *proto.Event) {
			ev.FreeBytes = proto.I64(nonNeg(c.avail))
		})
		return
	default:
		return
	}
	e.StageMS = c.now
	e.NextStepMS = c.now + c.p.StepMS
	e.Virtual = virtual
}

// emitStage emits ev, or an INFO "would ..." in its place when virtual.
func (c *stepCtx) emitStage(ev proto.Event, virtual bool, would string) {
	if virtual {
		ev.Verb = proto.VerbInfo
		ev.Msg = would
		ev.Hint = ""
	}
	c.emit(ev)
}

// critical: WARN and kill in the same step. Victims are taken in order
// until their usage (plus everything already terminating) covers the
// deficit; a new round may start only every CriticalStepMS.
func (c *stepCtx) critical() {
	if c.hold || c.now < c.s.Pressure.CritNextMS {
		return
	}
	need := c.deficit()
	var freed int64
	for _, t := range c.targets {
		if t.dying() {
			freed += t.used
		}
	}
	chose := false
	for _, t := range c.candidates(need, freed, func(*target) bool { return false }) {
		if freed >= need {
			break
		}
		if !hasWarning(t.enf, proto.PolicyPressure) {
			ev := c.pressureEvent(t, proto.VerbWarn)
			ev.Next = "kill"
			ev.InMS = proto.I64(0)
			ev.Hint = HintPressureWarn
			c.emit(ev)
		}
		c.kill(t, proto.PolicyPressure, HintPressureKill, func(ev *proto.Event) {
			ev.FreeBytes = proto.I64(nonNeg(c.avail))
		})
		freed += t.used
		chose = true
	}
	if chose {
		c.s.Pressure.CritNextMS = c.now + c.p.CriticalStepMS
	}
}

// recover: after RecoverHoldMS of normal pressure, clear pressure warnings
// (OK), then step targets down one at a time, one per StepMS: frozen ones
// are thawed first (to throttled), then throttled ones unthrottled; in
// reverse victim order, so the youngest / least important come back last.
func (c *stepCtx) recover() {
	pr := &c.s.Pressure
	if c.now-pr.NormalSinceMS < c.p.RecoverHoldMS {
		return
	}
	for _, t := range c.targets {
		if hasWarning(t.enf, proto.PolicyPressure) {
			removeWarning(t.enf, proto.PolicyPressure)
			ev := c.pressureEvent(t, proto.VerbOK)
			c.emit(ev)
		}
	}
	if c.now < pr.RecoverNextMS {
		return
	}
	pick := func(stage string) *target {
		var best *target
		for _, t := range c.targets {
			if t.enf.Stage != stage {
				continue
			}
			if best == nil || victimLess(best, t) {
				best = t
			}
		}
		return best
	}
	if t := pick(StageFrozen); t != nil {
		virtual := t.enf.Virtual
		c.do(virtual, c.sigActs(t, syscall.SIGCONT, proto.PolicyPressure)...)
		ev := c.pressureEvent(t, proto.VerbThawed)
		c.emitStage(ev, virtual, "would thaw (SIGCONT)")
		t.enf.Stage = StageThrottled
		t.enf.StageMS = c.now
		t.enf.NextStepMS = c.now + c.p.StepMS
		if t.job != nil {
			t.job.LastProgressMS = c.now // frozen time is not "stuck" time
		}
		pr.RecoverNextMS = c.now + c.p.StepMS
		return
	}
	if t := pick(StageThrottled); t != nil {
		c.unthrottleMem(t)
		pr.RecoverNextMS = c.now + c.p.StepMS
	}
}

func (c *stepCtx) unthrottleMem(t *target) {
	e := t.enf
	virtual := e.Virtual
	e.Stage, e.StageMS, e.NextStepMS = StageNone, 0, 0
	if t.job != nil && c.s.Pressure.CPUHigh {
		// CPU is still high: keep it in the background as a CPU throttle;
		// UNTHROTTLED is sent when the CPU throttle ends.
		e.CPUThrottle = true
		return
	}
	if e.CPUThrottle {
		return // still backgrounded for CPU; UNTHROTTLED comes when that ends
	}
	c.do(virtual, c.bgAct(t, OpForeground, proto.PolicyPressure)...)
	ev := c.pressureEvent(t, proto.VerbUnthrottled)
	c.emitStage(ev, virtual, "would unthrottle")
	e.Virtual = false
}

// ---- 9. CPU ------------------------------------------------------------------

// cpu: load per core >= CPUWarnPerCoreMilli marks CPU high; while high, one
// more governed job (lowest priority, then youngest) is backgrounded per
// StepMS. When load stays below CPUOKPerCoreMilli for RecoverHoldMS, every
// CPU-throttled job is restored at once. Unmanaged groups are never
// CPU-throttled (they are the user's interactive apps; memory is what
// stalls the machine).
func (c *stepCtx) cpu() {
	pr := &c.s.Pressure
	per := cpuPerCore(&c.in.Machine)
	enabled := c.p.CPUWarnPerCoreMilli > 0 && per >= 0
	okAt := c.p.CPUOKPerCoreMilli
	if okAt <= 0 || okAt > c.p.CPUWarnPerCoreMilli {
		okAt = c.p.CPUWarnPerCoreMilli
	}
	switch {
	case !enabled:
		pr.CPUHigh, pr.CPUOKSinceMS = false, 0
	case per >= c.p.CPUWarnPerCoreMilli:
		pr.CPUHigh, pr.CPUOKSinceMS = true, 0
	case per < okAt:
		if pr.CPUOKSinceMS == 0 {
			pr.CPUOKSinceMS = c.now
		}
		if pr.CPUHigh && c.now-pr.CPUOKSinceMS >= c.p.RecoverHoldMS {
			pr.CPUHigh = false
		}
	default:
		pr.CPUOKSinceMS = 0 // hysteresis band: keep the current state
	}

	if pr.CPUHigh {
		if c.hold || c.now < pr.CPUNextMS {
			return
		}
		var best *target
		for _, t := range c.targets {
			if t.job == nil || t.enf.CPUThrottle || t.enf.Stage != StageNone {
				continue
			}
			if best == nil || cpuLess(t, best) {
				best = t
			}
		}
		if best == nil {
			return
		}
		virtual := !c.enforce
		c.do(virtual, c.bgAct(best, OpBackground, proto.PolicyCPU)...)
		ev := c.targetEvent(best, proto.VerbThrottled)
		ev.Policy = proto.PolicyCPU
		c.emitStage(ev, virtual, "would throttle (cpu)")
		best.enf.CPUThrottle = true
		best.enf.Virtual = virtual
		pr.CPUNextMS = c.now + c.p.StepMS
		return
	}
	for _, t := range c.targets {
		e := t.enf
		if !e.CPUThrottle {
			continue
		}
		e.CPUThrottle = false
		if t.dying() || memStage(e) {
			continue // still backgrounded by the memory ladder / dying
		}
		virtual := e.Virtual
		c.do(virtual, c.bgAct(t, OpForeground, proto.PolicyCPU)...)
		ev := c.targetEvent(t, proto.VerbUnthrottled)
		ev.Policy = proto.PolicyCPU
		c.emitStage(ev, virtual, "would unthrottle (cpu)")
		if e.Stage == StageNone {
			e.Virtual = false
		}
	}
}

// cpuLess orders CPU-throttle candidates: lowest priority, youngest, id.
func cpuLess(a, b *target) bool {
	if a.prio != b.prio {
		return a.prio < b.prio
	}
	if a.start != b.start {
		return a.start > b.start
	}
	return a.id < b.id
}
