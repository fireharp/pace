package daemon

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// Assessment is the "inform" stage: the figures published in status and
// metrics. The policy package owns the decisions; this only describes.
type Assessment struct {
	CapacityBytes        int64
	AvailBytes           int64
	ReserveBytes         int64
	ReservedBytes        int64 // sum of unconsumed reservations of running jobs
	HeadroomBytes        int64 // what a new job may use now (>= 0)
	Level                string
	Admitting            bool
	NotAdmittingReasons  []string
	SuggestedConcurrency int
	SuggestedByClass     map[string]int
}

// AssessFunc computes an Assessment for the state after a step.
type AssessFunc func(p policy.Params, s policy.State, in policy.Inputs) Assessment

// maxSuggested caps suggested concurrency (a huge headroom divided by a
// small estimate is not a useful number for an agent).
const maxSuggested = 64

// DefaultAssess adapts policy.Assess, the same code Step uses for
// admission, so `pace status` can never disagree with admission decisions.
func DefaultAssess(p policy.Params, s policy.State, in policy.Inputs) Assessment {
	pa := policy.Assess(p, s, in)
	a := Assessment{
		CapacityBytes:        pa.CapacityBytes,
		AvailBytes:           max(0, pa.AvailBytes),
		ReserveBytes:         pa.ReserveBytes,
		ReservedBytes:        pa.ReservedBytes,
		HeadroomBytes:        max(0, pa.HeadroomBytes),
		Level:                displayLevel(pa.Level, s.Pressure.Level),
		Admitting:            pa.Admitting,
		NotAdmittingReasons:  pa.Reasons,
		SuggestedConcurrency: pa.Suggested,
		SuggestedByClass:     pa.SuggestedByClass,
	}
	// No headroom is not a pause (a small job may still fit a reservation
	// slack), but agents should see it as a reason not to start more work.
	if p.Mode != policy.ModeObserve && a.HeadroomBytes <= 0 && !slices.Contains(a.NotAdmittingReasons, proto.ReasonHeadroom) {
		a.NotAdmittingReasons = append(append([]string(nil), a.NotAdmittingReasons...), proto.ReasonHeadroom)
		a.Admitting = false
	}
	return a
}

// displayLevel is the worse of the instantaneous level and the level the
// policy holds (hysteresis), which is what admission actually acts on.
func displayLevel(now, held string) string {
	rank := func(l string) int {
		switch l {
		case proto.LevelCritical:
			return 2
		case proto.LevelWarn:
			return 1
		}
		return 0
	}
	if rank(held) > rank(now) {
		return held
	}
	if now == "" {
		return proto.LevelNormal
	}
	return now
}

// snapshot is what the loop publishes after each step for handlers.
type snapshot struct {
	status     *proto.Status // immutable once published
	totalBytes int64
	level      int
	queued     int
	running    int
	done       int
}

// publish builds and atomically publishes the status snapshot.
func (d *daemon) publish(now int64, p policy.Params, s policy.State, in policy.Inputs, a Assessment) {
	st := &proto.Status{
		V: 1, TS: now, Version: d.o.Version, Platform: d.backend.Name(), Mode: p.Mode,
		CapacityBytes: a.CapacityBytes, AvailBytes: a.AvailBytes, ReserveBytes: a.ReserveBytes,
		ReservedBytes: a.ReservedBytes, HeadroomBytes: a.HeadroomBytes, Pressure: a.Level,
		KernelLevel: in.Machine.KernelLevel, SwapUsedBytes: in.Machine.SwapUsed,
		Load1Milli: in.Machine.Load1Milli, NCPU: in.Machine.NCPU, DiskFreeBytes: in.Machine.DiskFreeBytes,
		Admitting: a.Admitting, NotAdmittingReasons: a.NotAdmittingReasons,
		SuggestedConcurrency: a.SuggestedConcurrency, SuggestedByClass: a.SuggestedByClass,
	}
	st.Running, st.Queued = d.reg.statusesOf(s.Jobs)
	st.Recent = d.reg.recent(maxRecentJobs)
	st.Unmanaged = topUnits(p, s, in)
	if d.lastAttr.SmallGroups > 0 {
		st.UnmanagedBelowMinVictim = &proto.SmallUnitsStatus{
			Groups: d.lastAttr.SmallGroups, UsedBytes: d.lastAttr.SmallBytes, MinVictimBytes: p.MinVictimBytes,
		}
	}
	st.Warnings = d.activeWarnings(now, s)
	st.RecentAlerts = d.recentAlertsAt(now)
	st.Faults = d.faults.active(time.Now().UnixMilli())
	st.Notes = d.platformNotes()

	q, r, dn := d.reg.counts()
	d.snap.Store(&snapshot{status: st, totalBytes: in.Machine.TotalBytes, level: levelNum(a.Level), queued: q, running: r, done: dn})
}

// platformNotes are the backend's standing limits, for status.
func (d *daemon) platformNotes() []string {
	var out []string
	if n := d.containerNote(); n != "" {
		out = append(out, n)
	}
	if tn, ok := d.backend.(platform.ThrottleNoter); ok {
		if n := tn.ThrottleNote(d.contCPUMax()); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// throttleCaveat is the backend's short note for a THROTTLED line of job
// (or a unit: job "") ("" for a strong, reversible throttle, as on macOS).
// A job in an adopted container is also capped by its cpu.max quota, when
// the container part of the throttle has one.
func (d *daemon) throttleCaveat(job string) string {
	if tc, ok := d.backend.(platform.ThrottleCaveater); ok {
		return tc.ThrottleCaveat(d.contCPUMax() && d.cgFor(job) != nil)
	}
	return ""
}

// contCPUMax: the job containers' throttle includes a cpu.max quota.
func (d *daemon) contCPUMax() bool {
	return d.cont != nil && strings.Contains(d.contWeights, "cpu.max")
}

// publishBootSnapshot serves a minimal status before the first sample.
func (d *daemon) publishBootSnapshot() {
	st := &proto.Status{
		V: 1, TS: d.now(), Version: d.o.Version, Platform: d.backend.Name(), Mode: d.cfg.Params.Mode,
		Pressure: proto.LevelNormal, Running: []proto.JobStatus{}, Queued: []proto.JobStatus{},
		SuggestedByClass: map[string]int{},
		Faults:           append([]string{"no sample yet"}, d.faults.active(time.Now().UnixMilli())...),
		Notes:            d.platformNotes(),
	}
	d.snap.Store(&snapshot{status: st})
}

// topUnits lists the largest unmanaged process groups.
func topUnits(p policy.Params, s policy.State, in policy.Inputs) []proto.UnitStatus {
	if len(in.Units) == 0 {
		return nil
	}
	idx := make([]int, len(in.Units))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool {
		ua, ub := &in.Units[idx[a]], &in.Units[idx[b]]
		if ua.UsedBytes != ub.UsedBytes {
			return ua.UsedBytes > ub.UsedBytes
		}
		return ua.Key < ub.Key
	})
	if len(idx) > topUnmanaged {
		idx = idx[:topUnmanaged]
	}
	out := make([]proto.UnitStatus, 0, len(idx))
	for _, i := range idx {
		u := &in.Units[i]
		us := proto.UnitStatus{
			Key: proto.UnitRef(u.Key), PGID: u.PGID, LeaderPID: u.LeaderPID, Comm: u.Comm, NProcs: u.NProcs,
			UsedBytes: u.UsedBytes, StartTS: u.StartMS, Priority: p.UnmanagedPriority,
		}
		if k := sort.Search(len(s.Units), func(k int) bool { return s.Units[k].Key >= u.Key }); k < len(s.Units) && s.Units[k].Key == u.Key && !s.Units[k].Enf.Virtual {
			us.Stage = s.Units[k].Enf.Stage
		}
		out = append(out, us)
	}
	return out
}

// activeWarnings lists warnings still outstanding in the policy state,
// using the WARN event that announced each one when we have it. Entries for
// warnings that cleared are forgotten.
func (d *daemon) activeWarnings(now int64, s policy.State) []proto.Event {
	var out []proto.Event
	live := map[string]bool{}
	add := func(job, unit, class string, w policy.Warning, enf *policy.Enforcement) {
		k := warnKey(job, unit, w.Policy)
		live[k] = true
		e, ok := d.lastWarn[k]
		if !ok {
			e = proto.Event{V: 1, TS: w.IssuedMS, Verb: proto.VerbWarn, Job: job, Unit: unit, Class: class, Policy: w.Policy}
		}
		if w.DeadlineMS > 0 {
			e.InMS = proto.I64(max(0, w.DeadlineMS-now))
		}
		if next, at, ok := pressureNext(w, enf, now); ok {
			// The warning's deadline passed and the ladder moved on: what
			// follows now is the next rung, at its step time (the same next
			// and in the latest THROTTLED or FROZEN line gave).
			e.Next, e.InMS = next, proto.I64(max(0, at-now))
		}
		out = append(out, e)
	}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		for _, w := range j.Enf.Warnings {
			add(j.ID, "", j.Class, w, &j.Enf)
		}
	}
	for i := range s.Units {
		u := &s.Units[i]
		for _, w := range u.Enf.Warnings {
			add("", proto.UnitRef(u.Key), "", w, &u.Enf)
		}
	}
	// Machine-level warnings (no job, no unit) last while the condition that
	// raised them holds: disk while the policy holds the disk low, anything
	// else while memory pressure is up. (A matching OK also drops one; see
	// fanout.)
	keys := make([]string, 0)
	for k, e := range d.lastWarn {
		if e.Job == "" && e.Unit == "" && machineWarnLive(&e, s) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		live[k] = true
		out = append(out, d.lastWarn[k])
	}
	for k := range d.lastWarn {
		if !live[k] {
			delete(d.lastWarn, k)
		}
	}
	return out
}

// pressureNext is the next step of a pressure warning whose deadline has
// passed while the target climbs the ladder: freeze after the throttle,
// kill after the freeze, at the target's next step time.
func pressureNext(w policy.Warning, e *policy.Enforcement, now int64) (next string, atMS int64, ok bool) {
	if w.Policy != proto.PolicyPressure || now < w.DeadlineMS {
		return "", 0, false
	}
	switch e.Stage {
	case policy.StageThrottled:
		return "freeze", e.NextStepMS, true
	case policy.StageFrozen:
		return "kill", e.NextStepMS, true
	}
	return "", 0, false
}

// machineWarnLive reports whether a machine-level WARN still applies.
func machineWarnLive(e *proto.Event, s policy.State) bool {
	if e.Policy == proto.PolicyDisk {
		return s.Pressure.DiskLow != ""
	}
	return s.Pressure.Level != "" && s.Pressure.Level != proto.LevelNormal
}
