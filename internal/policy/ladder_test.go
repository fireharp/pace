package policy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// twoJobs: "old" (pid 100) and "young" (pid 200), both priority 5, 1 GiB each.
func twoJobs(t *testing.T, mode string) *sim {
	p := testParams()
	p.Mode = mode
	m := newSim(t, p)
	m.admitStart("old", proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: 8 * GiB}, 100, GiB)
	m.tick(1000)
	m.admitStart("young", proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: 8 * GiB}, 200, GiB)
	return m
}

// tickUntil ticks by dt until cond(out) holds; fails after max ticks.
func (m *sim) tickUntil(dt int64, max int, cond func(Output) bool) Output {
	m.t.Helper()
	for i := 0; i < max; i++ {
		if o := m.tick(dt); cond(o) {
			return o
		}
	}
	m.t.Fatalf("condition not reached after %d ticks", max)
	return Output{}
}

func TestPressureWarnEscalationTiming(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(2300 * MiB) // warn (< 2.4G); deficit 3.04G-2.3G = 0.74G -> one victim
	o := m.tick(100)
	t0 := m.now
	info := evs(o, proto.VerbInfo)
	if len(info) != 1 || info[0].Level != proto.LevelWarn || info[0].Job != "" {
		t.Fatalf("level-change INFO:\n%s", dumpEvents(o))
	}
	w := mustEv(t, o, proto.VerbWarn, "young")
	noEv(t, o, proto.VerbWarn, "old")
	eq(t, "policy", w.Policy, proto.PolicyPressure)
	eq(t, "level", w.Level, proto.LevelWarn)
	eq(t, "next", w.Next, "throttle")
	eq(t, "in", i64(w.InMS), int64(20_000))
	eq(t, "free", i64(w.FreeBytes), 2300*MiB)
	eq(t, "hint", w.Hint, HintPressureWarn)
	eq(t, "no actions yet", len(o.Actions), 0)

	o = m.tickUntil(100, 300, func(o Output) bool { return len(o.Events) > 0 || len(o.Actions) > 0 })
	eq(t, "throttle at deadline", m.now-t0, int64(20_000))
	th := mustEv(t, o, proto.VerbThrottled, "young")
	eq(t, "next", th.Next, "freeze")
	eq(t, "in", i64(th.InMS), int64(2_000))
	bg := acts(o.Actions, "young", OpBackground, 0)
	if len(bg) != 1 || bg[0].PGID != 200 || bg[0].Policy != proto.PolicyPressure {
		t.Fatalf("background action: %+v", o.Actions)
	}

	o = m.tickUntil(100, 300, func(o Output) bool { return len(o.Events) > 0 || len(o.Actions) > 0 })
	eq(t, "freeze after step", m.now-t0, int64(22_000))
	mustEv(t, o, proto.VerbFrozen, "young")
	if !hasAct(o.Actions, "young", OpSignalGroup, sigSTOP) {
		t.Fatalf("no SIGSTOP: %+v", o.Actions)
	}

	o = m.tickUntil(100, 300, func(o Output) bool { return len(o.Events) > 0 || len(o.Actions) > 0 })
	eq(t, "kill after step", m.now-t0, int64(24_000))
	k := mustEv(t, o, proto.VerbKilled, "young")
	eq(t, "policy", k.Policy, proto.PolicyPressure)
	eq(t, "hint", k.Hint, HintPressureKill)
	if !hasAct(o.Actions, "young", OpSignalGroup, sigTERM) || !hasAct(o.Actions, "young", OpSignalGroup, sigCONT) {
		t.Fatalf("frozen job must get SIGTERM and SIGCONT: %+v", o.Actions)
	}
	// TERM before CONT so the stopped process handles TERM on resume.
	if o.Actions[0].Signal != sigTERM {
		t.Fatalf("SIGTERM must come first: %+v", o.Actions)
	}

	o = m.tickUntil(100, 300, func(o Output) bool { return len(o.Actions) > 0 })
	eq(t, "SIGKILL after grace", m.now-t0, int64(29_000))
	if !hasAct(o.Actions, "young", OpSignalGroup, sigKILL) {
		t.Fatalf("no SIGKILL: %+v", o.Actions)
	}
	if m.job("old").Enf.Stage != StageNone || len(m.job("old").Enf.Warnings) != 0 {
		t.Fatalf("old should be untouched: %+v", m.job("old").Enf)
	}
}

func TestPressureEscalationStopsWhenNormal(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(2300 * MiB)
	m.tick(100)
	m.run(200, 100) // 20s: throttled
	eq(t, "throttled", m.job("young").Enf.Stage, StageThrottled)
	m.setFree(8 * GiB)
	o := m.run(40, 100) // 4s: below hold -> nothing
	eq(t, "nothing during hold", len(o.Actions), 0)
	noEv(t, o, proto.VerbFrozen, "young")
	o = m.run(11, 100) // hold reached
	mustEv(t, o, proto.VerbOK, "young")
	u := mustEv(t, o, proto.VerbUnthrottled, "young")
	eq(t, "policy", u.Policy, proto.PolicyPressure)
	if !hasAct(o.Actions, "young", OpForeground, 0) {
		t.Fatalf("no foreground: %+v", o.Actions)
	}
	eq(t, "stage", m.job("young").Enf.Stage, StageNone)
}

func TestWarnLevelPicksVictimsUntilDeficitCovered(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(1900 * MiB) // deficit 3.04G-1.9G = ~1.14G -> both
	o := m.tick(100)
	mustEv(t, o, proto.VerbWarn, "young")
	mustEv(t, o, proto.VerbWarn, "old")
	// Already-warned victims are not re-warned.
	o = m.tick(100)
	eq(t, "no rewarn", len(evs(o, proto.VerbWarn)), 0)
}

func TestKernelWarnPicksAtLeastOneVictim(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.p.UseKernelLevel = true
	m.mach.KernelLevel = 2
	o := m.tick(100)
	mustEv(t, o, proto.VerbWarn, "young")
	noEv(t, o, proto.VerbWarn, "old")
}

func TestCriticalWarnAndKillSameStep(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(1 * GiB) // critical (< 1.12G); deficit 1.76G-1G=0.76G -> one victim
	o := m.tick(100)
	w := mustEv(t, o, proto.VerbWarn, "young")
	eq(t, "level", w.Level, proto.LevelCritical)
	eq(t, "next", w.Next, "kill")
	eq(t, "in", i64(w.InMS), int64(0))
	k := mustEv(t, o, proto.VerbKilled, "young")
	eq(t, "kill policy", k.Policy, proto.PolicyPressure)
	eq(t, "kill level", k.Level, proto.LevelCritical)
	if !hasAct(o.Actions, "young", OpSignalGroup, sigTERM) {
		t.Fatalf("no SIGTERM: %+v", o.Actions)
	}
	noEv(t, o, proto.VerbKilled, "old")
	// Critical grace (1s) then SIGKILL.
	o = m.tickUntil(100, 50, func(o Output) bool { return hasAct(o.Actions, "young", OpSignalGroup, sigKILL) })
	eq(t, "critical grace", m.now-k.TS, int64(1_000))
}

func TestCriticalPacingAndMultipleVictims(t *testing.T) {
	t.Run("several victims in one round", func(t *testing.T) {
		m := twoJobs(t, ModeEnforce)
		m.setFree(0) // deficit 1.76G -> both
		o := m.tick(100)
		mustEv(t, o, proto.VerbKilled, "young")
		mustEv(t, o, proto.VerbKilled, "old")
	})
	t.Run("next round only after CriticalStepMS", func(t *testing.T) {
		m := twoJobs(t, ModeEnforce)
		m.setFree(1 * GiB)
		o := m.tick(100)
		t0 := m.now
		mustEv(t, o, proto.VerbKilled, "young")
		m.base += 800 * MiB // deficit grows beyond what young frees
		o = m.tickUntil(100, 20, func(o Output) bool { return evFor(o, proto.VerbKilled, "old") != nil })
		eq(t, "paced", m.now-t0, int64(500))
	})
	t.Run("terminating warn-level victim gets critical grace", func(t *testing.T) {
		m := twoJobs(t, ModeEnforce)
		m.use("young", 9*GiB) // over its 8G budget*1.1? 9 > 8.8 -> mem_budget kill, grace 5s
		o := m.tick(100)
		mustEv(t, o, proto.VerbKilled, "young")
		t0 := m.now
		m.setFree(500 * MiB) // now critical
		o = m.tickUntil(100, 60, func(o Output) bool { return hasAct(o.Actions, "young", OpSignalGroup, sigKILL) })
		eq(t, "shortened grace", m.now-t0, int64(1_000))
	})
	t.Run("no candidates -> nothing", func(t *testing.T) {
		m := newSim(t, testParams())
		m.admitStart("tiny", proto.SubmitRequest{}, 100, 10*MiB) // below MinVictimBytes
		m.setFree(100 * MiB)
		o := m.tick(100)
		eq(t, "no kill", len(evs(o, proto.VerbKilled)), 0)
	})
}

func TestVictimOrderExhaustive(t *testing.T) {
	mk := func(id string, over bool, prio int, start int64, unit bool) *target {
		t := &target{id: id, prio: prio, start: start, used: 2 * GiB, budget: 4 * GiB, enf: &Enforcement{}}
		if over {
			t.used = 5 * GiB
		}
		if unit {
			t.unit = &Unit{Key: id}
		} else {
			t.job = &Job{ID: id}
		}
		return t
	}
	want := []*target{
		mk("over-hi", true, 9, 100, false),
		mk("low-young", false, 1, 500, false),
		mk("low-old", false, 1, 100, false),
		mk("mid-a", false, 5, 300, false),
		mk("mid-a", false, 5, 300, true), // same id: job before unit
		mk("mid-b", false, 5, 300, false),
	}
	// Every permutation sorts to the same order.
	var perm func(k int)
	n := 0
	cur := append([]*target(nil), want...)
	perm = func(k int) {
		if k == len(cur) {
			n++
			got := append([]*target(nil), cur...)
			sortTargets(got)
			for i := range got {
				if got[i] != want[i] {
					t.Fatalf("permutation %d: position %d is %s(unit=%v), want %s", n, i, got[i].id, got[i].unit != nil, want[i].id)
				}
			}
			return
		}
		for i := k; i < len(cur); i++ {
			cur[k], cur[i] = cur[i], cur[k]
			perm(k + 1)
			cur[k], cur[i] = cur[i], cur[k]
		}
	}
	perm(0)
	eq(t, "permutations", n, 720)
	// Irreflexive.
	for _, a := range want {
		if victimLess(a, a) {
			t.Fatalf("victimLess(%s,%s) is true", a.id, a.id)
		}
	}
}

func sortTargets(ts []*target) {
	for i := 1; i < len(ts); i++ {
		for j := i; j > 0 && victimLess(ts[j], ts[j-1]); j-- {
			ts[j], ts[j-1] = ts[j-1], ts[j]
		}
	}
}

func TestUnmanagedVictimsPreferredByPriority(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.unit("pg:900:1", 900, T0-1_000_000, 1*GiB) // old but priority 2
	m.unit("pg:901:1", 901, T0, 10*MiB)          // below MinVictimBytes
	m.tick(100)
	m.setFree(1 * GiB)
	o := m.tick(100)
	k := mustEv(t, o, proto.VerbKilled, "pg:900:1")
	eq(t, "no job on unit event", k.Job, "")
	// The contract id is pg:<pgid>; the start-time suffix stays internal.
	eq(t, "unit id", k.Unit, "pg:900")
	if !strings.Contains(k.Line(), " unit=pg:900 ") {
		t.Fatalf("line %q lacks unit=pg:900", k.Line())
	}
	eq(t, "comm", k.Comm, "upg:900:1")
	eq(t, "pgid", k.PGID, 900)
	noEv(t, o, proto.VerbKilled, "young")
	noEv(t, o, proto.VerbKilled, "pg:901:1")
	if !hasAct(o.Actions, "pg:900:1", OpSignalGroup, sigTERM) {
		t.Fatalf("unit not signalled: %+v", o.Actions)
	}
	if hasAct(o.Actions, "pg:900:1", OpSignal, sigTERM) {
		t.Fatal("leader present: no per-pid signals expected")
	}
	if u := m.unitState("pg:900:1"); u == nil || u.Enf.Stage != StageTerminating {
		t.Fatalf("unit not tracked: %+v", m.s.Units)
	}
	if m.unitState("pg:901:1") != nil {
		t.Fatal("untouched unit should not be tracked")
	}
	// Unit gone: tracking dropped.
	delete(m.units, "pg:900:1")
	m.tick(100)
	if m.unitState("pg:900:1") != nil {
		t.Fatal("vanished unit still tracked")
	}
}

func TestUnitLeaderGoneSignalsMembers(t *testing.T) {
	m := newSim(t, testParams())
	m.units["pg:900:1"] = &UnitSample{Key: "pg:900:1", PGID: 900, LeaderPID: 900, StartMS: T0, UsedBytes: 3 * GiB, PIDs: []int{905, 903}}
	m.units["pg:0:1"] = &UnitSample{Key: "pg:0:1", PGID: 0, LeaderPID: 0, StartMS: T0, UsedBytes: 1 * GiB, PIDs: []int{1, 950}}
	m.setFree(0)
	o := m.tick(100)
	a := acts(o.Actions, "pg:900:1", OpSignal, sigTERM)
	if len(a) != 1 || fmt.Sprint(a[0].PIDs) != "[903 905]" {
		t.Fatalf("member signals: %+v", o.Actions)
	}
	if hasAct(o.Actions, "pg:0:1", OpSignalGroup, 0) {
		t.Fatal("signal_group to pgid 0")
	}
	b := acts(o.Actions, "pg:0:1", OpSignal, sigTERM)
	if len(b) != 1 || fmt.Sprint(b[0].PIDs) != "[950]" {
		t.Fatalf("pgid-0 unit signals: %+v", o.Actions)
	}
}

func TestUnitBudget(t *testing.T) {
	p := testParams()
	p.UnmanagedBudgetBytes = 2 * GiB
	m := newSim(t, p)
	m.unit("pg:900:1", 900, T0, 1900*MiB)
	o := m.tick(100)
	w := mustEv(t, o, proto.VerbWarn, "pg:900:1")
	eq(t, "policy", w.Policy, proto.PolicyMemBudget)
	// A unit was never run through pace: it has no job to retry and no
	// budget to request, so it gets its own hints, not the job's.
	eq(t, "warn hint", w.Hint, HintUnitBudgetWarn)
	m.units["pg:900:1"].UsedBytes = 3 * GiB
	o = m.tick(100)
	k := mustEv(t, o, proto.VerbKilled, "pg:900:1")
	eq(t, "kill hint", k.Hint, HintUnitBudgetKill)
	eq(t, "kill msg", k.Msg, "used is over budget + tolerance (2.2G): killed at once, before the WARN's deadline")
}

func TestUnitBudgetHints(t *testing.T) {
	for _, h := range []string{HintUnitBudgetWarn, HintUnitBudgetKill, HintUnitBudgetWarnWould} {
		for _, job := range []string{"the job", "retry", "request a larger budget"} {
			if strings.Contains(h, job) {
				t.Errorf("unit hint %q says %q", h, job)
			}
		}
	}
	// Deadline kill of a unit (over budget, inside tolerance).
	p := testParams()
	p.UnmanagedBudgetBytes = 2 * GiB
	m := newSim(t, p)
	m.unit("pg:900:1", 900, T0, 2*GiB+10*MiB)
	mustEv(t, m.tick(100), proto.VerbWarn, "pg:900:1")
	o := m.tickUntil(100, 300, func(o Output) bool { return len(o.Events) > 0 })
	k := mustEv(t, o, proto.VerbKilled, "pg:900:1")
	eq(t, "deadline kill hint", k.Hint, HintUnitBudgetKill)
	eq(t, "deadline kill msg", k.Msg, "")
	// Observe mode: the WARN's hint does not threaten.
	p.Mode = ModeObserve
	m = newSim(t, p)
	m.unit("pg:900:1", 900, T0, 1900*MiB)
	w := mustEv(t, m.tick(100), proto.VerbWarn, "pg:900:1")
	eq(t, "observe hint", w.Hint, HintUnitBudgetWarnWould)
}

func TestOverBudgetVictimFirst(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.admitStart("hog", proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: 1500 * MiB, Priority: intp(9)}, 300, 1600*MiB)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "hog")
	noEv(t, o, proto.VerbKilled, "young")
}

func freezeBoth(t *testing.T, mode string) *sim {
	m := twoJobs(t, mode)
	m.setFree(1900 * MiB) // warn, both victims
	m.tick(100)
	m.run(220, 100) // 20s warn + 2s throttle -> frozen
	for _, id := range []string{"old", "young"} {
		if m.job(id).Enf.Stage != StageFrozen {
			t.Fatalf("%s stage %q", id, m.job(id).Enf.Stage)
		}
	}
	return m
}

func TestRecoveryThawsOneByOne(t *testing.T) {
	m := freezeBoth(t, ModeEnforce)
	m.setFree(10 * GiB)
	o := m.run(50, 100) // the first normal tick starts the 5s hold
	eq(t, "hold", len(o.Actions), 0)
	o = m.tick(100)
	t0 := m.now
	mustEv(t, o, proto.VerbOK, "old")
	mustEv(t, o, proto.VerbOK, "young")
	// The oldest comes back first, the youngest last.
	mustEv(t, o, proto.VerbThawed, "old")
	noEv(t, o, proto.VerbThawed, "young")
	if !hasAct(o.Actions, "old", OpSignalGroup, sigCONT) {
		t.Fatalf("no SIGCONT: %+v", o.Actions)
	}
	var order []string
	for i := 0; i < 100 && (m.job("young").Enf.Stage != StageNone || m.job("old").Enf.Stage != StageNone); i++ {
		o = m.tick(100)
		for _, e := range o.Events {
			order = append(order, fmt.Sprintf("%d:%s:%s", (m.now-t0)/1000, e.Verb, e.Job))
		}
	}
	eq(t, "order", fmt.Sprint(order), "[2:THAWED:young 4:UNTHROTTLED:old 6:UNTHROTTLED:young]")
	// Admission resumes only now.
	m.submit("n", proto.SubmitRequest{})
	mustEv(t, m.tick(100), proto.VerbAdmitted, "n")
}

func TestThawResetsStuckClock(t *testing.T) {
	p := testParams()
	p.Classes[1].NoProgressMS = 10_000
	m := newSim(t, p)
	m.admitStart("j", proto.SubmitRequest{}, 100, GiB)
	m.setFree(2300 * MiB)
	for i := 0; i < 230; i++ { // keep it busy until frozen
		m.cpu("j", 1)
		m.tick(100)
	}
	eq(t, "frozen", m.job("j").Enf.Stage, StageFrozen)
	m.setFree(10 * GiB)
	o := m.run(60, 100) // frozen 6s+ without progress: never stuck
	noEv(t, o, proto.VerbWarn, "j")
	mustEv(t, o, proto.VerbThawed, "j")
	eq(t, "progress reset", m.job("j").LastProgressMS > m.now-2_000, true)
}

func TestWarnOnlyProducesWouldNotActions(t *testing.T) {
	m := twoJobs(t, ModeWarnOnly)
	m.setFree(2300 * MiB)
	all := m.run(400, 100)
	mustEv(t, all, proto.VerbWarn, "young")
	for _, v := range []string{proto.VerbThrottled, proto.VerbFrozen, proto.VerbKilled} {
		if len(evs(all, v)) != 0 {
			t.Fatalf("%s emitted in warn-only", v)
		}
	}
	var msgs []string
	for _, e := range evs(all, proto.VerbInfo) {
		if e.Job == "young" {
			msgs = append(msgs, e.Msg)
		}
	}
	eq(t, "would sequence", fmt.Sprint(msgs),
		"[would throttle would freeze (SIGSTOP) would kill (SIGTERM, then SIGKILL after grace) would force-kill (SIGKILL) after grace]")
	for _, want := range []struct {
		op  string
		sig int
	}{{OpBackground, 0}, {OpSignalGroup, sigSTOP}, {OpSignalGroup, sigTERM}, {OpSignalGroup, sigCONT}, {OpSignalGroup, sigKILL}} {
		if !hasAct(all.Would, "young", want.op, want.sig) {
			t.Fatalf("missing would %s %d", want.op, want.sig)
		}
	}
	eq(t, "no real actions", len(all.Actions), 0)
	// The virtual kill is not repeated.
	o := m.run(100, 100)
	eq(t, "quiet afterwards", len(o.Would), 0)
	// It exits on its own: history is learned.
	m.exit("young", 0)
	m.tick(100)
	if findHistory(&m.s, "class:default") < 0 {
		t.Fatal("virtually-killed job's peak not learned")
	}
}

func TestWarnOnlyRecoveryIsVirtual(t *testing.T) {
	m := freezeBoth(t, ModeWarnOnly)
	m.setFree(10 * GiB)
	o := m.run(120, 100)
	eq(t, "no real actions", len(o.Actions), 0)
	if !hasAct(o.Would, "old", OpSignalGroup, sigCONT) || !hasAct(o.Would, "young", OpForeground, 0) {
		t.Fatalf("virtual recovery: %s", dumpEvents(o))
	}
	eq(t, "stages cleared", m.job("old").Enf.Stage+m.job("young").Enf.Stage, "")
	eq(t, "virtual cleared", m.job("old").Enf.Virtual || m.job("young").Enf.Virtual, false)
}

func TestObserveCriticalIsVirtual(t *testing.T) {
	m := twoJobs(t, ModeObserve)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbWarn, "young")
	info := mustEv(t, o, proto.VerbInfo, "young")
	eq(t, "msg", info.Msg, "would kill (SIGTERM, then SIGKILL after grace)")
	eq(t, "hint", info.Hint, "")
	if !hasAct(o.Would, "young", OpSignalGroup, sigTERM) {
		t.Fatalf("would: %+v", o.Would)
	}
}

func TestModeSwitches(t *testing.T) {
	t.Run("enforce -> warn-only releases real freezes", func(t *testing.T) {
		m := freezeBoth(t, ModeEnforce)
		m.strict = false
		m.p.Mode = ModeWarnOnly
		o := m.tick(100)
		for _, id := range []string{"old", "young"} {
			mustEv(t, o, proto.VerbThawed, id)
			mustEv(t, o, proto.VerbUnthrottled, id)
			if !hasAct(o.Actions, id, OpSignalGroup, sigCONT) || !hasAct(o.Actions, id, OpForeground, 0) {
				t.Fatalf("real release for %s: %+v", id, o.Actions)
			}
		}
	})
	t.Run("warn-only -> enforce forgets virtual stages", func(t *testing.T) {
		m := freezeBoth(t, ModeWarnOnly)
		m.p.Mode = ModeEnforce
		o := m.tick(100)
		// Warning deadline long passed: real escalation restarts at throttle.
		th := mustEv(t, o, proto.VerbThrottled, "young")
		_ = th
		if !hasAct(o.Actions, "young", OpBackground, 0) {
			t.Fatalf("real throttle: %+v", o.Actions)
		}
		eq(t, "virtual", m.job("young").Enf.Virtual, false)
	})
	t.Run("real termination completes in warn-only", func(t *testing.T) {
		m := twoJobs(t, ModeEnforce)
		m.use("young", 9*GiB)
		mustEv(t, m.tick(100), proto.VerbKilled, "young")
		m.strict = false
		m.p.Mode = ModeWarnOnly
		o := m.run(50, 100)
		if !hasAct(o.Actions, "young", OpSignalGroup, sigKILL) {
			t.Fatalf("announced kill must complete: %s", dumpEvents(o))
		}
	})
	t.Run("real cpu throttle released in observe", func(t *testing.T) {
		m := twoJobs(t, ModeEnforce)
		m.mach.Load1Milli = 8 * 2_000
		mustEv(t, m.tick(100), proto.VerbThrottled, "young")
		m.strict = false
		m.p.Mode = ModeObserve
		o := m.tick(100)
		u := mustEv(t, o, proto.VerbUnthrottled, "young")
		eq(t, "policy", u.Policy, proto.PolicyCPU)
		if !hasAct(o.Actions, "young", OpForeground, 0) {
			t.Fatalf("foreground: %+v", o.Actions)
		}
	})
}

func TestCPUThrottleAndRecovery(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.admitStart("low", proto.SubmitRequest{EstimateBytes: GiB, Priority: intp(3)}, 300, 100*MiB)
	m.mach.Load1Milli = 8 * 1_600
	o := m.tick(100)
	t0 := m.now
	th := mustEv(t, o, proto.VerbThrottled, "low")
	eq(t, "policy", th.Policy, proto.PolicyCPU)
	if !hasAct(o.Actions, "low", OpBackground, 0) {
		t.Fatalf("bg: %+v", o.Actions)
	}
	var order []string
	for i := 0; i < 60; i++ {
		o = m.tick(100)
		for _, e := range o.Events {
			order = append(order, fmt.Sprintf("%d:%s:%s", m.now-t0, e.Verb, e.Job))
		}
	}
	eq(t, "one per step", fmt.Sprint(order), "[2000:THROTTLED:young 4000:THROTTLED:old]")
	// Hysteresis band: stays throttled.
	m.mach.Load1Milli = 8 * 1_200
	o = m.run(100, 100)
	eq(t, "band", len(o.Events), 0)
	// Below OK, but not yet for the hold.
	m.mach.Load1Milli = 8 * 900
	o = m.run(50, 100)
	eq(t, "hold", len(o.Events), 0)
	o = m.tick(100)
	eq(t, "all unthrottled at once", len(evs(o, proto.VerbUnthrottled)), 3)
	for _, id := range []string{"low", "young", "old"} {
		if !hasAct(o.Actions, id, OpForeground, 0) {
			t.Fatalf("foreground %s", id)
		}
	}
	t.Run("disabled", func(t *testing.T) {
		p := testParams()
		p.CPUWarnPerCoreMilli = 0
		m := newSim(t, p)
		m.admitStart("a", proto.SubmitRequest{}, 100, GiB)
		m.mach.Load1Milli = 8 * 9_000
		eq(t, "none", len(evs(m.run(10, 1000), proto.VerbThrottled)), 0)
		m.mach.NCPU = 0
		m.p.CPUWarnPerCoreMilli = 1_000
		eq(t, "unknown ncpu", len(evs(m.run(10, 1000), proto.VerbThrottled)), 0)
	})
}

func TestCPUAndMemoryLadderInteract(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.mach.Load1Milli = 8 * 2_000
	m.run(30, 100) // both CPU-throttled
	if !m.job("young").Enf.CPUThrottle || !m.job("old").Enf.CPUThrottle {
		t.Fatal("expected cpu throttles")
	}
	m.setFree(2300 * MiB)
	o := m.run(201, 100) // warn + deadline -> memory throttle of young
	mustEv(t, o, proto.VerbThrottled, "young")
	if hasAct(o.Actions, "young", OpBackground, 0) {
		t.Fatal("already backgrounded for CPU: no second background")
	}
	// Memory recovers while CPU is still high: stays backgrounded, no event.
	m.setFree(10 * GiB)
	o = m.run(60, 100)
	noEv(t, o, proto.VerbUnthrottled, "young")
	eq(t, "cpu throttle kept", m.job("young").Enf.CPUThrottle, true)
	eq(t, "mem stage cleared", m.job("young").Enf.Stage, StageNone)
	// CPU recovers: UNTHROTTLED policy=cpu.
	m.mach.Load1Milli = 0
	o = m.run(60, 100)
	eq(t, "policy", mustEv(t, o, proto.VerbUnthrottled, "young").Policy, proto.PolicyCPU)
}

// Inputs.Stale (a failed sample) starts and escalates nothing: no stage is
// recorded that the daemon would not carry out, and the held transition
// happens, with its signals, on the first fresh step.
func TestStaleInputsHoldEscalation(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(2300 * MiB)
	m.tick(100) // WARN young
	m.tickUntil(100, 300, func(o Output) bool { return hasAct(o.Actions, "young", OpBackground, 0) })
	m.stale = true
	for i := 0; i < 100; i++ {
		o := m.tick(100)
		for _, a := range o.Actions {
			t.Fatalf("action on a stale sample: %+v", a)
		}
		for _, e := range o.Events {
			if e.Verb == proto.VerbFrozen || e.Verb == proto.VerbKilled || e.Verb == proto.VerbThrottled || e.Verb == proto.VerbWarn {
				t.Fatalf("%s on a stale sample", e.Verb)
			}
		}
	}
	eq(t, "stage while stale", m.job("young").Enf.Stage, StageThrottled)
	m.stale = false
	o := m.tick(100)
	mustEv(t, o, proto.VerbFrozen, "young")
	if !hasAct(o.Actions, "young", OpSignalGroup, sigSTOP) {
		t.Fatalf("held freeze not carried out: %+v", o.Actions)
	}
	// The kill: SIGTERM on a fresh step; SIGKILL after grace is held too.
	m.tickUntil(100, 300, func(o Output) bool { return hasAct(o.Actions, "young", OpSignalGroup, sigTERM) })
	m.stale = true
	for i := 0; i < 100; i++ {
		if o := m.tick(100); len(o.Actions) > 0 {
			t.Fatalf("SIGKILL after grace on a stale sample: %+v", o.Actions)
		}
	}
	m.stale = false
	o = m.tick(100)
	if !hasAct(o.Actions, "young", OpSignalGroup, sigKILL) {
		t.Fatalf("held SIGKILL not sent on the fresh step: %+v", o.Actions)
	}
}

// A budget kill decided on a stale sample waits for a fresh one.
func TestStaleInputsHoldBudgetKill(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("j", proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: GiB}, 100, GiB/2)
	m.tick(1000)
	m.use("j", 2*GiB)
	m.stale = true
	if o := m.tick(1000); len(o.Actions) > 0 || len(evs(o, proto.VerbKilled)) > 0 {
		t.Fatalf("budget kill on a stale sample:\n%s", dumpEvents(o))
	}
	m.stale = false
	o := m.tick(1000)
	mustEv(t, o, proto.VerbKilled, "j")
	if !hasAct(o.Actions, "j", OpSignalGroup, sigTERM) {
		t.Fatalf("no SIGTERM: %+v", o.Actions)
	}
}
