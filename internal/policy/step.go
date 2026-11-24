package policy

import (
	"sort"
	"strings"

	"github.com/fireharp/pace/internal/proto"
)

// Tunables that are not configuration (documented decisions).
const (
	// DefaultETAPerPositionMS is the queue ETA per position when history
	// cannot predict when running jobs end.
	DefaultETAPerPositionMS int64 = 30_000
	// MinETAMS is the smallest ETA / retry_after ever reported.
	MinETAMS int64 = 1_000
	// okHysteresisPermille: a mem_budget WARN clears (OK) only once usage is
	// this far (of budget) below the warn threshold, so a job hovering at the
	// threshold does not flap WARN/OK every sample.
	okHysteresisPermille int64 = 50
	// defaultPriority is used when no class is configured at all.
	defaultPriority = 5
)

// Step advances the policy by one tick. It is pure and deterministic: the
// caller's State is never modified (it is deep-copied), nothing reads the
// clock, randomness or map iteration order, and all arithmetic is integer.
//
// Phases, in order:
//
//  1. intake of wrapper events (arrival order)
//  2. job samples (usage, peak, CPU progress, liveness, reservation growth)
//  3. unmanaged units (tracked units are refreshed or dropped when gone)
//  4. pressure assessment (memory level, disk level, CPU state)
//  5. mode normalisation (see Enforcement.Virtual)
//  6. termination progress (SIGKILL after grace)
//  7. per-target policies: orphaned, mem_budget, wall_clock, stuck
//  8. machine pressure ladder (warn -> throttle -> freeze -> kill; critical
//     kills at once) or recovery (thaw/unthrottle one at a time)
//  9. CPU throttling
//  10. admission (queue order: priority desc, Seq asc; no backfill)
//
// A submit emits ACCEPTED (stream-only) with the resolved need/budget.
// Every emitted event carries V=1 and TS=in.NowMS.
func Step(p Params, s State, in Inputs) (State, Output) {
	ns := cloneState(s)
	c := &stepCtx{
		p:          &p,
		s:          &ns,
		in:         &in,
		now:        in.NowMS,
		enforce:    p.Mode != ModeObserve && p.Mode != ModeWarnOnly,
		observe:    p.Mode == ModeObserve,
		hold:       in.Stale,
		newJobs:    map[string]bool{},
		startedNow: map[string]bool{},
	}
	c.indexSamples()
	c.intake()
	c.applyJobSamples()
	c.mergeUnits()
	c.updatePressure()
	c.buildTargets()
	c.normalizeMode()
	c.progressTerminations()
	c.perTargetPolicies()
	c.pressureLadder()
	c.cpu()
	c.admission()
	c.finish()
	return ns, c.out
}

type stepCtx struct {
	p   *Params
	s   *State
	in  *Inputs
	now int64
	out Output

	enforce bool // stage-3 actions are real
	observe bool // admit everything at once
	hold    bool // Inputs.Stale: start or escalate nothing

	jobSamples  map[string]*JobSample
	unitSamples []UnitSample    // sorted by Key
	newJobs     map[string]bool // submitted this step
	startedNow  map[string]bool // started/attached this step (sample may predate it)

	capacity, avail int64
	level, disk     string
	targets         []*target
}

func (c *stepCtx) emit(e proto.Event) {
	c.markMode(&e)
	c.out.Events = append(c.out.Events, e)
}

// markMode makes a line honest in observe and warn-only mode: one whose
// next= (or "would ...") action the governor will not carry out in this
// mode gets mode=<mode>, and its hint no longer threatens that action.
// next= keeps what enforce mode would do. Admission (next=refuse) is real
// in warn-only mode.
func (c *stepCtx) markMode(e *proto.Event) {
	if c.enforce {
		return
	}
	switch {
	case e.Next != "" && (e.Next != "refuse" || c.observe):
		e.Mode = c.p.Mode
		e.Hint = wouldHint(e.Hint, e.Next)
	case e.Verb == proto.VerbInfo && strings.HasPrefix(e.Msg, "would "):
		e.Mode = c.p.Mode
	}
}

func (c *stepCtx) base(verb string) proto.Event {
	return proto.Event{V: 1, TS: c.now, Verb: verb}
}

func (c *stepCtx) jobEvent(verb string, j *Job) proto.Event {
	e := c.base(verb)
	e.Job, e.Class, e.PID, e.PGID = j.ID, j.Class, j.PID, j.PGID
	return e
}

func (c *stepCtx) do(virtual bool, acts ...Action) {
	if len(acts) == 0 {
		return
	}
	if virtual {
		c.out.Would = append(c.out.Would, acts...)
	} else {
		c.out.Actions = append(c.out.Actions, acts...)
	}
}

func (c *stepCtx) indexSamples() {
	c.jobSamples = make(map[string]*JobSample, len(c.in.Jobs))
	for i := range c.in.Jobs {
		js := c.in.Jobs[i]
		js.ExtraPIDs = sortedPIDs(js.ExtraPIDs)
		c.jobSamples[js.ID] = &js
	}
	c.unitSamples = make([]UnitSample, len(c.in.Units))
	for i, u := range c.in.Units {
		u.PIDs = sortedPIDs(u.PIDs)
		c.unitSamples[i] = u
	}
	sort.SliceStable(c.unitSamples, func(i, j int) bool { return c.unitSamples[i].Key < c.unitSamples[j].Key })
}

// sortedPIDs returns a sorted, de-duplicated copy without pids <= 1 (never
// signal init or "every process").
func sortedPIDs(in []int) []int {
	var out []int
	for _, p := range in {
		if p > 1 {
			out = append(out, p)
		}
	}
	sort.Ints(out)
	w := 0
	for i, p := range out {
		if i == 0 || p != out[w-1] {
			out[w] = p
			w++
		}
	}
	if w == 0 {
		return nil
	}
	return out[:w]
}

func (c *stepCtx) findJob(id string) int {
	if id == "" {
		return -1
	}
	for i := range c.s.Jobs {
		if c.s.Jobs[i].ID == id {
			return i
		}
	}
	return -1
}

func (c *stepCtx) removeJob(i int) {
	c.s.Jobs = append(c.s.Jobs[:i], c.s.Jobs[i+1:]...)
}

// ---- 1. intake -------------------------------------------------------------

func (c *stepCtx) intake() {
	for _, ev := range c.in.Events {
		switch ev.Kind {
		case EvSubmit:
			c.onSubmit(ev)
		case EvStarted:
			c.onStarted(ev)
		case EvProgress:
			c.onProgress(ev)
		case EvExit:
			c.onExit(ev)
		case EvLost:
			c.onLost(ev)
		case EvAttach:
			c.onAttach(ev)
		}
	}
}

// newJob resolves class defaults, priority, key, estimate and budget.
func (c *stepCtx) newJob(id string, r *proto.SubmitRequest) Job {
	cls := classFor(c.p, r.Class)
	prio := cls.Priority
	if r.Priority != nil {
		prio = *r.Priority
	}
	if prio < 0 {
		prio = 0
	}
	if prio > 9 {
		prio = 9
	}
	key := capKey(r.Key)
	if key == "" {
		key = DeriveKey(r.Argv)
	}
	est := r.EstimateBytes
	learned := false
	if est <= 0 {
		capacity, _ := capacityAvail(c.p, c.in)
		est, learned = undeclaredEstimate(c.p, c.s, cls, key, capacity)
	}
	// Budget: an explicit request wins; otherwise the class budget, raised
	// to a declared estimate (the agent said it needs that much).
	budget := r.BudgetBytes
	if budget <= 0 {
		budget = cls.BudgetBytes
		if r.EstimateBytes > budget && budget > 0 {
			budget = r.EstimateBytes
		}
	}
	wall := r.WallLimitMS
	if wall <= 0 {
		wall = cls.WallLimitMS
	}
	c.s.NextSeq++
	return Job{
		ID:            id,
		Seq:           c.s.NextSeq,
		Class:         cls.Name,
		Key:           key,
		Priority:      prio,
		State:         JobQueued,
		EstimateBytes: est,
		Learned:       learned,
		BudgetBytes:   budget,
		NoWait:        r.NoWait,
		SubmitMS:      c.now,
		WallLimitMS:   wall,
		NoProgressMS:  cls.NoProgressMS,
	}
}

func (c *stepCtx) onSubmit(ev InEvent) {
	if ev.Submit == nil || ev.Job == "" || c.findJob(ev.Job) >= 0 {
		return // malformed or duplicate
	}
	j := c.newJob(ev.Job, ev.Submit)
	qt := ev.Submit.QueueTimeoutMS
	if qt <= 0 {
		qt = c.p.DefaultQueueTimeoutMS
	}
	if qt > 0 {
		j.QueueDeadlineMS = c.now + qt
	}
	c.s.Jobs = append(c.s.Jobs, j)
	c.newJobs[j.ID] = true
	e := c.jobEvent(proto.VerbAccepted, &j)
	e.NeedBytes = proto.I64(j.EstimateBytes)
	if j.BudgetBytes > 0 {
		e.BudgetBytes = proto.I64(j.BudgetBytes)
	}
	e.Priority = proto.Int(j.Priority)
	c.emit(e)
	if r := ev.Submit.Class; r != "" && r != j.Class {
		// A typo in --class must not pass silently: say which class governs it.
		info := c.jobEvent(proto.VerbInfo, &j)
		info.Msg = "unknown class " + r + ", governed as " + j.Class + " (classes: " + classNames(c.p) + ")"
		c.emit(info)
	}
}

// classNames lists the configured class names, space separated.
func classNames(p *Params) string {
	var b strings.Builder
	for i, cl := range p.Classes {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(cl.Name)
	}
	return b.String()
}

func (c *stepCtx) onStarted(ev InEvent) {
	i := c.findJob(ev.Job)
	if i < 0 {
		return
	}
	j := &c.s.Jobs[i]
	if j.State == JobQueued {
		// The process exists although it was never admitted (wrapper bug or
		// fail-open race): govern it rather than ignore it.
		j.State = JobRunning
		j.AdmitMS = c.now
		if j.ReservedBytes < j.EstimateBytes {
			j.ReservedBytes = j.EstimateBytes
		}
	}
	j.PID, j.PGID = ev.PID, ev.PGID
	j.StartMS = ev.StartMS
	if j.StartMS <= 0 {
		j.StartMS = c.now
	}
	j.LastProgressMS = c.now
	c.startedNow[j.ID] = true
}

func (c *stepCtx) onProgress(ev InEvent) {
	i := c.findJob(ev.Job)
	if i < 0 {
		return
	}
	j := &c.s.Jobs[i]
	if ev.OutputBytes > j.OutputBytes {
		j.OutputBytes = ev.OutputBytes
		j.LastProgressMS = c.now
	}
}

func (c *stepCtx) onExit(ev InEvent) {
	i := c.findJob(ev.Job)
	if i < 0 {
		return
	}
	j := c.s.Jobs[i]
	if ev.OOMKill && j.State == JobRunning && (j.Enf.KillPolicy == "" || j.Enf.Virtual) {
		c.kernelKilled(&j, ev)
	}
	// Learn only from runs that ended on their own (or were killed only
	// virtually): a policy kill truncates the peak and would bias estimates.
	if j.State == JobRunning && j.StartMS > 0 && j.PeakBytes > 0 && (j.Enf.KillPolicy == "" || j.Enf.Virtual) {
		dur := c.now - j.StartMS
		if dur < 0 {
			dur = 0
		}
		// The class history is the fallback for commands never seen before,
		// so it learns one sample per command: the first recorded run of
		// each key. One heavy command re-run often must not set everyone's
		// estimate; it takes HistoryMinSamples different commands.
		if j.Key == "" || findHistory(c.s, histKeyPrefix+j.Key) < 0 {
			recordHistory(c.p, c.s, histClassPrefix+j.Class, j.PeakBytes, dur, c.now)
		}
		if j.Key != "" {
			recordHistory(c.p, c.s, histKeyPrefix+j.Key, j.PeakBytes, dur, c.now)
		}
	}
	c.removeJob(i)
}

// kernelKilled reports a job the kernel OOM-killed at its container's hard
// limit. The daemon sets that limit (enforce mode only) at budget *
// (1 + tolerance), the line above which mem_budget kills at once, so a job
// that jumps past it dies in the kernel before any sample sees it: it gets
// the KILLED line mem_budget would have given it (on its stream, which the
// wrapper still reads while it waits for a KILLED after a signal death),
// and its run is not learned from.
func (c *stepCtx) kernelKilled(j *Job, ev InEvent) {
	e := c.jobEvent(proto.VerbKilled, j)
	e.Policy = proto.PolicyMemBudget
	e.Level = c.s.Pressure.Level
	j.PeakBytes = max(j.PeakBytes, ev.PeakBytes) // not learned from (KillPolicy below)
	e.UsedBytes = proto.I64(max(j.UsedBytes, j.PeakBytes))
	if j.BudgetBytes > 0 {
		e.BudgetBytes = proto.I64(j.BudgetBytes)
	}
	e.Hint = HintMemBudgetKill
	e.Msg = proto.MsgKernelMemMax
	if warnPending(&j.Enf, c.now) {
		// The hard limit is the tolerance line: a WARN's deadline holds
		// only below it, so say why the kill came before that deadline.
		e.Msg += proto.MsgBeforeWarnDeadline
	}
	c.emit(e)
	j.Enf.KillPolicy = proto.PolicyMemBudget
	j.Enf.Virtual = false
}

func (c *stepCtx) onLost(ev InEvent) {
	i := c.findJob(ev.Job)
	if i < 0 {
		return
	}
	j := &c.s.Jobs[i]
	if j.State == JobQueued || j.PID == 0 {
		// Nothing was spawned: dequeue / release silently.
		c.removeJob(i)
		return
	}
	if j.WrapperLostMS == 0 {
		j.WrapperLostMS = c.now
	}
}

func (c *stepCtx) onAttach(ev InEvent) {
	i := c.findJob(ev.Job)
	if i < 0 {
		if ev.Job == "" {
			return
		}
		req := proto.SubmitRequest{}
		if ev.Submit != nil {
			req = *ev.Submit
		}
		j := c.newJob(ev.Job, &req)
		j.State = JobRunning
		j.AdmitMS = c.now
		j.ReservedBytes = j.EstimateBytes
		j.SubmitMS = ev.StartMS
		if j.SubmitMS <= 0 {
			j.SubmitMS = c.now
		}
		c.s.Jobs = append(c.s.Jobs, j)
		i = len(c.s.Jobs) - 1
	}
	j := &c.s.Jobs[i]
	if j.State == JobQueued {
		j.State = JobRunning
		j.AdmitMS = c.now
		if j.ReservedBytes < j.EstimateBytes {
			j.ReservedBytes = j.EstimateBytes
		}
	}
	if ev.PID > 0 {
		j.PID = ev.PID
	}
	if ev.PGID > 0 {
		j.PGID = ev.PGID
	}
	if ev.StartMS > 0 {
		j.StartMS = ev.StartMS
	} else if j.StartMS == 0 && j.PID > 0 {
		j.StartMS = c.now
	}
	if ev.OutputBytes > j.OutputBytes {
		j.OutputBytes = ev.OutputBytes
	}
	j.LastProgressMS = c.now
	j.WrapperLostMS = 0
	c.startedNow[j.ID] = true
}

// ---- 2. job samples --------------------------------------------------------

// jobAlive: a running, started job whose group still has members (a missing
// sample means "unknown", treated as alive).
func (c *stepCtx) jobAlive(j *Job) bool {
	if j.State != JobRunning || j.PID == 0 {
		return false
	}
	if js, ok := c.jobSamples[j.ID]; ok && !js.Alive && !c.startedNow[j.ID] {
		return false
	}
	return true
}

func (c *stepCtx) applyJobSamples() {
	for i := 0; i < len(c.s.Jobs); i++ {
		j := &c.s.Jobs[i]
		if j.State != JobRunning {
			continue
		}
		// A "not alive" sample taken before this step's started/attach event
		// is stale and ignored.
		if js, ok := c.jobSamples[j.ID]; ok && j.PID != 0 && (js.Alive || !c.startedNow[j.ID]) {
			if js.Alive {
				j.UsedBytes = js.UsedBytes
				j.NProcs = js.NProcs
				if js.UsedBytes > j.PeakBytes {
					j.PeakBytes = js.UsedBytes
				}
				if js.CPUMS > j.CPUMS {
					j.CPUMS = js.CPUMS
					j.LastProgressMS = c.now
				}
			} else {
				// Group empty. The reservation is kept (conservative) until the
				// wrapper reports the exit, which follows within milliseconds.
				j.UsedBytes, j.NProcs = 0, 0
				if j.WrapperLostMS != 0 {
					// Group empty and nobody to report the exit: done.
					c.removeJob(i)
					i--
					continue
				}
			}
		}
		// Reservation converges on measured peak and never shrinks.
		r := j.EstimateBytes
		if m := withMargin(j.PeakBytes, c.p.MarginPermille); m > r {
			r = m
		}
		if r > j.ReservedBytes {
			j.ReservedBytes = r
		}
	}
}

// ---- 3. units --------------------------------------------------------------

func (c *stepCtx) mergeUnits() {
	old := c.s.Units // private copy; sorted defensively (persisted state)
	sort.SliceStable(old, func(i, j int) bool { return old[i].Key < old[j].Key })
	var units []Unit
	for _, us := range c.unitSamples {
		u := Unit{Key: us.Key}
		k := sort.Search(len(old), func(i int) bool { return old[i].Key >= us.Key })
		if k < len(old) && old[k].Key == us.Key {
			u = old[k]
		}
		if len(units) > 0 && units[len(units)-1].Key == us.Key {
			continue // duplicate sample key
		}
		u.PGID, u.LeaderPID, u.StartMS, u.Comm = us.PGID, us.LeaderPID, us.StartMS, us.Comm
		u.UsedBytes, u.NProcs = us.UsedBytes, us.NProcs
		units = append(units, u)
	}
	// Tracked units absent from the sample have no members left: dropped.
	c.s.Units = units
}

func (c *stepCtx) unitSample(key string) *UnitSample {
	k := sort.Search(len(c.unitSamples), func(i int) bool { return c.unitSamples[i].Key >= key })
	if k < len(c.unitSamples) && c.unitSamples[k].Key == key {
		return &c.unitSamples[k]
	}
	return nil
}

// ---- 4. pressure -------------------------------------------------------------

func (c *stepCtx) updatePressure() {
	c.capacity, c.avail = capacityAvail(c.p, c.in)
	c.level = memLevel(c.p, c.in)
	c.disk = diskLevel(c.p, &c.in.Machine)
	pr := &c.s.Pressure
	prev := pr.Level
	if c.level != prev {
		pr.Level = c.level
		pr.SinceMS = c.now
		if prev != "" || c.level != proto.LevelNormal {
			e := c.base(proto.VerbInfo)
			e.Level = c.level
			e.FreeBytes = proto.I64(nonNeg(c.avail))
			e.Msg = "memory pressure " + c.level
			c.emit(e)
		}
	}
	if c.level == proto.LevelNormal {
		if prev != proto.LevelNormal {
			pr.NormalSinceMS = c.now
		}
	} else {
		pr.NormalSinceMS = 0
	}
	c.updateDisk()
}

func (c *stepCtx) updateDisk() {
	pr := &c.s.Pressure
	cur := c.disk
	if cur == proto.LevelNormal {
		cur = ""
	}
	if cur == pr.DiskLow {
		return
	}
	pr.DiskLow = cur
	free := c.in.Machine.DiskFreeBytes
	if cur == "" {
		e := c.base(proto.VerbOK)
		e.Policy = proto.PolicyDisk
		e.Level = proto.LevelNormal
		e.FreeBytes = proto.I64(free)
		c.emit(e)
		return
	}
	e := c.base(proto.VerbWarn)
	e.Policy = proto.PolicyDisk
	e.Level = cur
	e.FreeBytes = proto.I64(free)
	e.Next = "refuse"
	if cur == proto.LevelCritical {
		e.Hint = HintDiskCritical
	} else {
		e.Hint = HintDiskWarn
	}
	c.emit(e)
}

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// ---- finish ------------------------------------------------------------------

func (c *stepCtx) finish() {
	// Drop units that need no tracking (no warning, no stage).
	units := c.s.Units[:0]
	for _, u := range c.s.Units {
		if len(u.Enf.Warnings) > 0 || u.Enf.Stage != StageNone || u.Enf.CPUThrottle {
			units = append(units, u)
		}
	}
	if len(units) == 0 {
		c.s.Units = nil
	} else {
		c.s.Units = units
	}
	sort.SliceStable(c.s.Jobs, func(i, j int) bool { return c.s.Jobs[i].Seq < c.s.Jobs[j].Seq })
	if len(c.s.Jobs) == 0 {
		c.s.Jobs = nil
	}
	c.s.LastMS = c.now
}
