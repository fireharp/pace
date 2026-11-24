package policy

import (
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

func TestJobNotLeadingItsGroupIsSignalledPerPID(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("j", proto.SubmitRequest{})
	m.tick(100)
	// Wrapper reports a pgid that is not the job's own (no Setpgid): the
	// group may contain the wrapper or the agent, so it must never be killpg'd.
	m.events = append(m.events, InEvent{Kind: EvStarted, Job: "j", PID: 4000, PGID: 3000, StartMS: m.now})
	m.samples["j"] = &JobSample{ID: "j", Alive: true, UsedBytes: 5 * GiB, ExtraPIDs: []int{4001}}
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "j")
	if hasAct(o.Actions, "j", OpSignalGroup, 0) {
		t.Fatalf("killpg on a foreign group: %+v", o.Actions)
	}
	a := acts(o.Actions, "j", OpSignal, sigTERM)
	if len(a) != 1 || len(a[0].PIDs) != 2 || a[0].PIDs[0] != 4000 || a[0].PIDs[1] != 4001 {
		t.Fatalf("per-pid TERM: %+v", o.Actions)
	}
}

func TestUnsignallableTargetsAreIgnored(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("j", proto.SubmitRequest{})
	m.tick(100)
	m.events = append(m.events, InEvent{Kind: EvStarted, Job: "j", PID: 1, PGID: 1, StartMS: m.now})
	m.samples["j"] = &JobSample{ID: "j", Alive: true, UsedBytes: 5 * GiB}
	m.units["pg:0:0"] = &UnitSample{Key: "pg:0:0", UsedBytes: 4 * GiB, PIDs: []int{0, 1}}
	m.setFree(0)
	o := m.tick(100)
	eq(t, "no kills", len(evs(o, proto.VerbKilled)), 0)
	eq(t, "no actions", len(o.Actions), 0)
}

func TestShrunkVictimStillEscalates(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(2300 * MiB)
	m.tick(100)
	m.run(200, 100) // throttled
	eq(t, "throttled", m.job("young").Enf.Stage, StageThrottled)
	m.use("young", 10*MiB) // below MinVictimBytes now
	m.setFree(2300 * MiB)
	o := m.run(25, 100)
	mustEv(t, o, proto.VerbFrozen, "young")
}

func TestUnsortedPersistedUnits(t *testing.T) {
	m := newSim(t, testParams())
	m.s.Units = []Unit{
		{Key: "pg:9:1", Enf: Enforcement{Stage: StageFrozen}},
		{Key: "pg:5:1", Enf: Enforcement{Stage: StageThrottled}},
	}
	m.unit("pg:5:1", 5, T0, GiB)
	m.unit("pg:9:1", 9, T0, GiB)
	m.units["pg:9:1"].PIDs = append(m.units["pg:9:1"].PIDs, 9) // duplicate pid
	m.tick(100)
	eq(t, "kept stage", m.unitState("pg:9:1").Enf.Stage, StageFrozen)
	eq(t, "kept stage", m.unitState("pg:5:1").Enf.Stage, StageThrottled)
}

func TestDuplicateUnitSampleKeys(t *testing.T) {
	p := testParams()
	in := Inputs{NowMS: T0, ScopedUsedBytes: 15 * GiB, Units: []UnitSample{
		{Key: "pg:7:1", PGID: 7, LeaderPID: 7, UsedBytes: GiB, PIDs: []int{7}},
		{Key: "pg:7:1", PGID: 7, LeaderPID: 7, UsedBytes: GiB, PIDs: []int{7}},
	}}
	in.Machine.DiskFreeBytes = -1
	ns, out := Step(p, State{}, in)
	eq(t, "one unit", len(ns.Units), 1)
	eq(t, "one kill", len(evs(out, proto.VerbKilled)), 1)
}

func TestRefuseHintsAndThresholdEdges(t *testing.T) {
	for reason, want := range map[string]string{
		proto.ReasonHeadroom: HintHeadroom, proto.ReasonPressure: HintPressureWait, proto.ReasonSlots: HintSlots,
		proto.ReasonDisk: HintDisk, ReasonJobWarning: HintJobWarning, "other": "",
	} {
		eq(t, reason, refuseHint(reason), want)
	}
	p := testParams()
	w, c := thresholds(&p, 0)
	eq(t, "no capacity", w+c, int64(0))
	// Coinciding thresholds: the margin falls back to MinVictimBytes.
	p.WarnAvailPermille, p.CritAvailPermille = 100, 100
	cx := &stepCtx{p: &p, capacity: 10 * GiB, avail: GiB - 1, level: proto.LevelCritical}
	eq(t, "deficit", cx.deficit(), 1+p.MinVictimBytes)
}

func TestStartedWithoutStartTimeAndExitWhileQueued(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("a", proto.SubmitRequest{})
	m.tick(100)
	m.events = append(m.events, InEvent{Kind: EvStarted, Job: "a", PID: 100, PGID: 100})
	m.tick(100)
	eq(t, "start defaults to now", m.job("a").StartMS, m.now)
	m.setFree(2*GiB + 1)
	m.submit("q", proto.SubmitRequest{})
	m.tick(100)
	m.exit("q", 0)
	o := m.tick(100)
	eq(t, "silent", len(o.Events), 0)
	if m.job("q") != nil || len(m.s.History) != 0 {
		t.Fatal("queued exit must just remove the job")
	}
}

func TestUnmanagedThrottleRecovery(t *testing.T) {
	// A unit on the memory ladder is unthrottled even while CPU is high
	// (units are never CPU-throttled).
	m := newSim(t, testParams())
	m.unit("pg:900:1", 900, T0, 2*GiB)
	m.mach.Load1Milli = 8 * 3_000
	m.setFree(2300 * MiB)
	m.tick(100)
	m.run(200, 100)
	eq(t, "throttled", m.unitState("pg:900:1").Enf.Stage, StageThrottled)
	m.setFree(10 * GiB)
	o := m.run(60, 100)
	mustEv(t, o, proto.VerbUnthrottled, "pg:900:1")
	if !hasAct(o.Actions, "pg:900:1", OpForeground, 0) {
		t.Fatalf("foreground: %+v", o.Actions)
	}
	if m.unitState("pg:900:1") != nil {
		t.Fatal("recovered unit should no longer be tracked")
	}
}
