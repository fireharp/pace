package policy

import (
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// ReasonJobWarning is the QUEUED/REFUSED reason used only when
// Params.AdmitPauseOnJobWarn is set and a per-job warning pauses admission.
const ReasonJobWarning = "job_warning"

// maxSuggest caps a suggestion when a class has no estimate and no slots.
const maxSuggest = 64

// Assessment is the machine-level view the daemon publishes in `pace status`.
// It is computed by the same code Step uses for admission.
type Assessment struct {
	CapacityBytes int64 `json:"capacity_bytes"`
	// AvailBytes is Capacity-ScopedUsed under a capacity override, else the
	// kernel's available figure. It may be negative under an override.
	AvailBytes    int64  `json:"avail_bytes"`
	ReserveBytes  int64  `json:"reserve_bytes"`  // the effective reserve (EffectiveReserve)
	ReservedBytes int64  `json:"reserved_bytes"` // sum of unconsumed reservations of running jobs
	HeadroomBytes int64  `json:"headroom_bytes"` // Avail - Reserved - Reserve
	Level         string `json:"level"`          // memory pressure: normal|warn|critical
	DiskLevel     string `json:"disk_level"`     // normal|warn|critical
	CPUPerCore    int64  `json:"cpu_per_core_milli"`
	// Admitting reports whether a new job of DefaultClass would be admitted
	// now. It is false while admission is paused (proto.ReasonDisk,
	// proto.ReasonPressure, ReasonJobWarning), when the default class has
	// no free slot (proto.ReasonSlots), and when there is no headroom or a
	// queued job it would wait behind does not fit (proto.ReasonHeadroom).
	// Reasons says why, most important first. Observe mode always admits.
	Admitting bool     `json:"admitting"`
	Reasons   []string `json:"reasons,omitempty"`
	// Suggested is how many more jobs of DefaultClass could be admitted
	// right now; SuggestedByClass is the same per class: headroom left after
	// the queue's estimates divided by the class estimate, capped by free
	// class slots, 0 while admission is paused (regardless of mode).
	Suggested        int            `json:"suggested"`
	SuggestedByClass map[string]int `json:"suggested_by_class"`
}

func levelRank(l string) int {
	switch l {
	case proto.LevelCritical:
		return 2
	case proto.LevelWarn:
		return 1
	}
	return 0
}

func maxLevel(a, b string) string {
	if levelRank(b) > levelRank(a) {
		return b
	}
	return a
}

// capacityAvail returns capacity C and available memory.
func capacityAvail(p *Params, in *Inputs) (int64, int64) {
	if p.CapacityBytes > 0 {
		return p.CapacityBytes, p.CapacityBytes - in.ScopedUsedBytes
	}
	return in.Machine.TotalBytes, in.Machine.AvailBytes
}

// thresholds returns the warn and critical avail thresholds in bytes.
func thresholds(p *Params, c int64) (warnB, critB int64) {
	if c <= 0 {
		return 0, 0
	}
	return c * p.WarnAvailPermille / 1000, c * p.CritAvailPermille / 1000
}

// memLevel computes the memory pressure level from the inputs alone:
// the max of (avail vs thresholds), the kernel level and PSI (the last two
// only when UseKernelLevel).
func memLevel(p *Params, in *Inputs) string {
	c, avail := capacityAvail(p, in)
	lvl := proto.LevelNormal
	if c > 0 {
		warnB, critB := thresholds(p, c)
		switch {
		case p.CritAvailPermille > 0 && avail < critB:
			lvl = proto.LevelCritical
		case p.WarnAvailPermille > 0 && avail < warnB:
			lvl = proto.LevelWarn
		}
	}
	if p.UseKernelLevel {
		switch {
		case in.Machine.KernelLevel >= 4:
			lvl = maxLevel(lvl, proto.LevelCritical)
		case in.Machine.KernelLevel >= 2:
			lvl = maxLevel(lvl, proto.LevelWarn)
		}
		if p.PSICrit10 > 0 && in.Machine.PSIMemFull10 >= 0 && in.Machine.PSIMemFull10 >= p.PSICrit10 {
			lvl = maxLevel(lvl, proto.LevelCritical)
		}
		if p.PSIWarn10 > 0 && in.Machine.PSIMemSome10 >= 0 && in.Machine.PSIMemSome10 >= p.PSIWarn10 {
			lvl = maxLevel(lvl, proto.LevelWarn)
		}
	}
	return lvl
}

// MemLevel is the memory pressure level a Step over in computes (see
// memLevel). The daemon uses it to decide which unit samples to offer.
func MemLevel(p Params, in Inputs) string { return memLevel(&p, &in) }

// diskLevel compares DiskFreeBytes (-1 = unknown) with the thresholds.
func diskLevel(p *Params, m *platform.Machine) string {
	f := m.DiskFreeBytes
	if f < 0 {
		return proto.LevelNormal
	}
	switch {
	case p.DiskCritFreeBytes > 0 && f < p.DiskCritFreeBytes:
		return proto.LevelCritical
	case p.DiskWarnFreeBytes > 0 && f < p.DiskWarnFreeBytes:
		return proto.LevelWarn
	}
	return proto.LevelNormal
}

// cpuPerCore is load1 per core in milli (-1 if NCPU unknown).
func cpuPerCore(m *platform.Machine) int64 {
	if m.NCPU <= 0 {
		return -1
	}
	return m.Load1Milli / int64(m.NCPU)
}

// unconsumed is the part of a running job's reservation not yet in use.
func unconsumed(j *Job) int64 {
	if j.State != JobRunning {
		return 0
	}
	if d := j.ReservedBytes - j.UsedBytes; d > 0 {
		return d
	}
	return 0
}

func sumUnconsumed(s *State) int64 {
	var t int64
	for i := range s.Jobs {
		t += unconsumed(&s.Jobs[i])
	}
	return t
}

// EffectiveReserve is the memory admission never hands out on a machine of
// capacity c: ReserveBytes, raised to the warn-pressure threshold unless
// AdmitBelowWarn. Without that raise a job admitted into exactly the
// headroom it declared could leave avail below the warn line and be
// throttled, frozen and killed by the pressure ladder although it stayed
// within its estimate and budget.
func EffectiveReserve(p Params, c int64) int64 { return effectiveReserve(&p, c) }

func effectiveReserve(p *Params, c int64) int64 {
	r := p.ReserveBytes
	if p.AdmitBelowWarn || p.WarnAvailPermille <= 0 {
		return r
	}
	if warnB, _ := thresholds(p, c); warnB > r {
		r = warnB
	}
	return r
}

// maxAdmissible is the largest estimate that can ever be admitted on a
// machine of capacity c (0 = unknown capacity, no limit).
func maxAdmissible(p *Params, c int64) int64 {
	if c <= 0 {
		return 0
	}
	return c - effectiveReserve(p, c)
}

func headroom(p *Params, s *State, in *Inputs) int64 {
	c, avail := capacityAvail(p, in)
	return avail - sumUnconsumed(s) - effectiveReserve(p, c)
}

func hasWarning(e *Enforcement, policy string) bool {
	for _, w := range e.Warnings {
		if w.Policy == policy {
			return true
		}
	}
	return false
}

// memStage reports whether e is throttled or frozen on the memory ladder.
func memStage(e *Enforcement) bool {
	return e.Stage == StageThrottled || e.Stage == StageFrozen
}

// pressureActive reports a pressure warning or memory throttle/freeze on any target.
func pressureActive(s *State) bool {
	for i := range s.Jobs {
		e := &s.Jobs[i].Enf
		if hasWarning(e, proto.PolicyPressure) || memStage(e) {
			return true
		}
	}
	for i := range s.Units {
		e := &s.Units[i].Enf
		if hasWarning(e, proto.PolicyPressure) || memStage(e) {
			return true
		}
	}
	return false
}

func jobWarningActive(s *State) bool {
	for i := range s.Jobs {
		for _, w := range s.Jobs[i].Enf.Warnings {
			if w.Policy != proto.PolicyPressure {
				return true
			}
		}
	}
	return false
}

// pauseReasons lists why admission is paused (empty = admitting), ignoring
// the mode. Precisely: disk critical; memory level >= warn; any pressure
// warning, throttle or freeze still outstanding (recovery not finished);
// and, only with AdmitPauseOnJobWarn, any per-job warning.
func pauseReasons(p *Params, s *State, level, disk string) []string {
	var r []string
	if disk == proto.LevelCritical {
		r = append(r, proto.ReasonDisk)
	}
	if levelRank(level) >= 1 || pressureActive(s) {
		r = append(r, proto.ReasonPressure)
	}
	if p.AdmitPauseOnJobWarn && jobWarningActive(s) {
		r = append(r, ReasonJobWarning)
	}
	return r
}

// Assess computes the machine-level view for (p, s, in) without changing
// anything. The daemon calls it with the state returned by the latest Step
// and that step's inputs.
func Assess(p Params, s State, in Inputs) Assessment {
	return assess(&p, &s, &in, memLevel(&p, &in))
}

func assess(p *Params, s *State, in *Inputs, level string) Assessment {
	c, avail := capacityAvail(p, in)
	a := Assessment{
		CapacityBytes: c,
		AvailBytes:    avail,
		ReserveBytes:  effectiveReserve(p, c),
		ReservedBytes: sumUnconsumed(s),
		Level:         level,
		DiskLevel:     diskLevel(p, &in.Machine),
		CPUPerCore:    cpuPerCore(&in.Machine),
	}
	a.HeadroomBytes = avail - a.ReservedBytes - a.ReserveBytes
	reasons := pauseReasons(p, s, a.Level, a.DiskLevel)
	if p.Mode == ModeObserve {
		a.Admitting = true
	} else {
		a.Reasons = reasons
		if len(reasons) == 0 {
			if r := newJobBlocked(p, s, a.HeadroomBytes); r != "" {
				a.Reasons = []string{r}
			}
		}
		a.Admitting = len(a.Reasons) == 0
	}

	// Suggestions: what is left after the queue, per class.
	left := a.HeadroomBytes
	running := map[string]int{}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		switch j.State {
		case JobQueued:
			left -= j.EstimateBytes
		case JobRunning:
			running[j.Class]++
		}
	}
	a.SuggestedByClass = map[string]int{}
	suggest := func(cls ClassParams) int {
		if len(reasons) > 0 || left <= 0 {
			return 0
		}
		est, _ := undeclaredEstimate(p, s, cls, "", c)
		n := maxSuggest
		if est > 0 {
			q := left / est
			if q < int64(n) {
				n = int(q)
			}
		}
		if cls.Slots > 0 {
			free := cls.Slots - running[cls.Name]
			if free < 0 {
				free = 0
			}
			if free < n {
				n = free
			}
		}
		return n
	}
	for _, cls := range p.Classes {
		a.SuggestedByClass[cls.Name] = suggest(cls)
	}
	a.Suggested = suggest(classFor(p, p.DefaultClass))
	return a
}

// newJobBlocked says why a new job of DefaultClass submitted now would not
// be admitted although admission is not paused ("" = it would be, if its
// estimate fits the headroom): the class has no free slot, there is no
// headroom at all, or a queued job of at least its priority (which it
// would queue behind: there is no backfill) does not fit. It replays
// admission's queue order on the state after the step.
func newJobBlocked(p *Params, s *State, hr int64) string {
	def := classFor(p, p.DefaultClass)
	running := map[string]int{}
	for i := range s.Jobs {
		if s.Jobs[i].State == JobRunning {
			running[s.Jobs[i].Class]++
		}
	}
	if def.Slots > 0 && running[def.Name] >= def.Slots {
		return proto.ReasonSlots
	}
	if hr <= 0 {
		return proto.ReasonHeadroom
	}
	for _, i := range queueOrder(s) {
		j := &s.Jobs[i]
		if cls := classFor(p, j.Class); cls.Slots > 0 && running[j.Class] >= cls.Slots {
			continue // skipped by admission; does not block jobs behind it
		}
		if j.EstimateBytes > hr {
			if j.Priority >= def.Priority {
				return proto.ReasonHeadroom
			}
			break // a new job would be ordered ahead of it
		}
		hr -= j.EstimateBytes
		running[j.Class]++
	}
	return ""
}
