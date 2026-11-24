package policy

import (
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// budgetSim: a job "j" with a 4 GiB budget, started at pid 100.
func budgetSim(t *testing.T, mode string) *sim {
	p := testParams()
	p.Mode = mode
	m := newSim(t, p)
	m.admitStart("j", proto.SubmitRequest{Class: "build", EstimateBytes: GiB}, 100, GiB)
	return m
}

func TestMemBudgetWarnOKAndHysteresis(t *testing.T) {
	m := budgetSim(t, ModeEnforce)
	m.use("j", 3686*MiB) // just below 90% of 4G (3686.4M)
	o := m.tick(1000)
	noEv(t, o, proto.VerbWarn, "j")
	m.use("j", 3700*MiB)
	o = m.tick(1000)
	w := mustEv(t, o, proto.VerbWarn, "j")
	eq(t, "policy", w.Policy, proto.PolicyMemBudget)
	eq(t, "next", w.Next, "kill")
	eq(t, "in", i64(w.InMS), int64(20_000))
	eq(t, "budget", i64(w.BudgetBytes), 4*GiB)
	eq(t, "used", i64(w.UsedBytes), 3700*MiB)
	eq(t, "hint", w.Hint, HintMemBudgetWarn)
	// No repeat while it stays high.
	o = m.tick(1000)
	noEv(t, o, proto.VerbWarn, "j")
	// Falls below warn but inside the hysteresis band: still warned.
	m.use("j", 3600*MiB) // okBelow = 3686.4M - 204.8M = 3481.6M
	o = m.tick(1000)
	noEv(t, o, proto.VerbOK, "j")
	m.use("j", 3400*MiB)
	o = m.tick(1000)
	ok := mustEv(t, o, proto.VerbOK, "j")
	eq(t, "ok policy", ok.Policy, proto.PolicyMemBudget)
	if m.job("j").Enf.Warnings != nil {
		t.Fatal("warning not cleared")
	}
}

func TestMemBudgetDeadline(t *testing.T) {
	t.Run("kill at deadline when above budget", func(t *testing.T) {
		m := budgetSim(t, ModeEnforce)
		m.use("j", 4*GiB+100*MiB) // over budget, under tolerance (4.4G)
		o := m.tick(1000)
		mustEv(t, o, proto.VerbWarn, "j")
		noEv(t, o, proto.VerbKilled, "j")
		o = m.tick(19_999)
		noEv(t, o, proto.VerbKilled, "j")
		o = m.tick(1)
		k := mustEv(t, o, proto.VerbKilled, "j")
		eq(t, "policy", k.Policy, proto.PolicyMemBudget)
		eq(t, "hint", k.Hint, HintMemBudgetKill)
		eq(t, "msg", k.Msg, "") // the kill the WARN announced
		eq(t, "used", i64(k.UsedBytes), 4*GiB+100*MiB)
		eq(t, "budget", i64(k.BudgetBytes), 4*GiB)
		eq(t, "level", k.Level, proto.LevelNormal)
		if !hasAct(o.Actions, "j", OpSignalGroup, sigTERM) {
			t.Fatalf("no SIGTERM:\n%s", dumpEvents(o))
		}
	})
	t.Run("no kill at deadline when back within budget", func(t *testing.T) {
		m := budgetSim(t, ModeEnforce)
		m.use("j", 3900*MiB)
		m.tick(1000)
		o := m.run(25, 1000)
		noEv(t, o, proto.VerbKilled, "j")
		// Exceeds budget after the deadline: killed at once.
		m.use("j", 4*GiB+1)
		o = m.tick(1000)
		mustEv(t, o, proto.VerbKilled, "j")
	})
	t.Run("kill immediately above tolerance", func(t *testing.T) {
		m := budgetSim(t, ModeEnforce)
		m.use("j", 4*GiB+410*MiB) // > 4.4G
		o := m.tick(1000)
		k := mustEv(t, o, proto.VerbKilled, "j")
		noEv(t, o, proto.VerbWarn, "j")
		eq(t, "hint", k.Hint, HintMemBudgetKill)
		eq(t, "msg", k.Msg, "used is over budget + tolerance (4.4G): killed at once")
	})
	t.Run("above tolerance before the WARN's deadline says why", func(t *testing.T) {
		// The WARN said next=kill in=20s; a jump past the tolerance line
		// kills at once, and the KILLED line says that is why it is early.
		m := budgetSim(t, ModeEnforce)
		m.use("j", 4*GiB+100*MiB)
		w := mustEv(t, m.tick(1000), proto.VerbWarn, "j")
		eq(t, "in", i64(w.InMS), int64(20_000))
		m.use("j", 4*GiB+500*MiB)
		o := m.tick(500)
		k := mustEv(t, o, proto.VerbKilled, "j")
		eq(t, "hint", k.Hint, HintMemBudgetKill)
		eq(t, "msg", k.Msg, "used is over budget + tolerance (4.4G): killed at once, before the WARN's deadline")
		if !strings.Contains(k.Line(), `msg="used is over budget + tolerance (4.4G): killed at once, before the WARN's deadline"`) {
			t.Fatalf("line %q", k.Line())
		}
	})
	for _, mode := range []string{ModeObserve, ModeWarnOnly} {
		t.Run("above tolerance in "+mode+" warns, then logs the kill", func(t *testing.T) {
			m := budgetSim(t, mode)
			m.use("j", 4*GiB+410*MiB) // > 4.4G
			o := m.tick(1000)
			w := mustEv(t, o, proto.VerbWarn, "j")
			eq(t, "policy", w.Policy, proto.PolicyMemBudget)
			eq(t, "next", w.Next, "kill")
			eq(t, "in", i64(w.InMS), int64(0))
			eq(t, "hint", w.Hint, HintMemBudgetWarnWould)
			eq(t, "mode", w.Mode, mode)
			noEv(t, o, proto.VerbKilled, "j")
			info := mustEv(t, o, proto.VerbInfo, "j")
			eq(t, "info mode", info.Mode, mode)
			eq(t, "info policy", info.Policy, proto.PolicyMemBudget)
			if len(o.Actions) != 0 || len(o.Would) == 0 {
				t.Fatalf("want only would-actions:\n%s", dumpEvents(o))
			}
			// The WARN precedes the would-kill INFO.
			for _, ev := range o.Events {
				if ev.Verb == proto.VerbInfo {
					t.Fatal("INFO before WARN")
				}
				if ev.Verb == proto.VerbWarn {
					break
				}
			}
		})
	}
	t.Run("zero budget disables the policy", func(t *testing.T) {
		p := testParams()
		p.Classes[1].BudgetBytes = 0
		m := newSim(t, p)
		m.admitStart("j", proto.SubmitRequest{}, 100, 9*GiB)
		o := m.run(30, 1000)
		noEv(t, o, proto.VerbWarn, "j")
		noEv(t, o, proto.VerbKilled, "j")
	})
}

func TestTerminationGraceThenSIGKILL(t *testing.T) {
	m := budgetSim(t, ModeEnforce)
	m.samples["j"].ExtraPIDs = []int{777, 555, 555, 1, 0}
	m.use("j", 5*GiB)
	o := m.tick(1000)
	mustEv(t, o, proto.VerbKilled, "j")
	term := acts(o.Actions, "j", OpSignalGroup, sigTERM)
	eq(t, "term pgid", term[0].PGID, 100)
	eq(t, "term policy", term[0].Policy, proto.PolicyMemBudget)
	extras := acts(o.Actions, "j", OpSignal, sigTERM)
	eq(t, "extras", extras[0].PIDs, []int{555, 777})
	if hasAct(o.Actions, "j", OpSignalGroup, sigCONT) {
		t.Fatal("SIGCONT sent to a job that was not frozen")
	}
	eq(t, "stage", m.job("j").Enf.Stage, StageTerminating)
	// No more policy events while dying, no SIGKILL before grace.
	o = m.tick(4_999)
	eq(t, "quiet", len(o.Events)+len(o.Actions), 0)
	o = m.tick(1)
	if !hasAct(o.Actions, "j", OpSignalGroup, sigSTOP) || !hasAct(o.Actions, "j", OpSignalGroup, sigKILL) ||
		!hasAct(o.Actions, "j", OpSignal, sigKILL) {
		t.Fatalf("SIGSTOP+SIGKILL expected:\n%s", dumpEvents(o))
	}
	eq(t, "no new events", len(o.Events), 0)
	eq(t, "stage", m.job("j").Enf.Stage, StageKilled)
	// Still alive after another grace: SIGKILL re-sent.
	o = m.tick(5_000)
	if !hasAct(o.Actions, "j", OpSignalGroup, sigKILL) {
		t.Fatalf("SIGKILL not repeated:\n%s", dumpEvents(o))
	}
	// Group empties: nothing more is sent.
	m.dead("j")
	o = m.tick(5_000)
	eq(t, "dead: no actions", len(o.Actions), 0)
	// Exit: removed, no history (policy kill truncates the peak).
	m.exit("j", 137)
	m.tick(100)
	if m.job("j") != nil || len(m.s.History) != 0 {
		t.Fatalf("job kept or learned from a kill: %+v", m.s.History)
	}
}

func TestStuck(t *testing.T) {
	newStuck := func(t *testing.T) *sim {
		p := testParams()
		p.Classes[1].NoProgressMS = 60_000
		m := newSim(t, p)
		m.admitStart("j", proto.SubmitRequest{}, 100, 100*MiB)
		return m
	}
	t.Run("warn then kill", func(t *testing.T) {
		m := newStuck(t)
		o := m.run(59, 1000) // 59.1s idle since start
		noEv(t, o, proto.VerbWarn, "j")
		o = m.tick(1000)
		w := mustEv(t, o, proto.VerbWarn, "j")
		eq(t, "policy", w.Policy, proto.PolicyStuck)
		eq(t, "limit", i64(w.LimitMS), int64(60_000))
		eq(t, "idle", i64(w.IdleMS), int64(60_000))
		eq(t, "hint", w.Hint, HintStuckWarn)
		o = m.run(19, 1000)
		noEv(t, o, proto.VerbKilled, "j")
		o = m.tick(1000)
		k := mustEv(t, o, proto.VerbKilled, "j")
		eq(t, "policy", k.Policy, proto.PolicyStuck)
		eq(t, "hint", k.Hint, HintStuckKill)
		eq(t, "idle", i64(k.IdleMS), int64(80_000))
	})
	for _, kind := range []string{"output", "cpu"} {
		t.Run(kind+" progress clears", func(t *testing.T) {
			m := newStuck(t)
			m.run(60, 1000)
			if len(m.job("j").Enf.Warnings) != 1 {
				t.Fatal("expected stuck warning")
			}
			if kind == "output" {
				m.output("j", 10)
			} else {
				m.cpu("j", 5)
			}
			o := m.tick(1000)
			eq(t, "ok", mustEv(t, o, proto.VerbOK, "j").Policy, proto.PolicyStuck)
			o = m.run(30, 1000)
			noEv(t, o, proto.VerbKilled, "j")
		})
	}
	t.Run("output that does not grow is not progress", func(t *testing.T) {
		m := newStuck(t)
		m.output("j", 10)
		m.tick(1000)
		m.output("j", 10)
		m.output("j", 5)
		o := m.run(60, 1000)
		mustEv(t, o, proto.VerbWarn, "j")
	})
	t.Run("zero warn deadline kills at once", func(t *testing.T) {
		m := newStuck(t)
		m.p.WarnDeadlineMS = 0
		o := m.run(60, 1000)
		mustEv(t, o, proto.VerbWarn, "j")
		mustEv(t, o, proto.VerbKilled, "j")
	})
}

func TestWallClock(t *testing.T) {
	p := testParams()
	m := newSim(t, p)
	m.admitStart("j", proto.SubmitRequest{WallLimitMS: 60_000}, 100, 100*MiB)
	start := m.job("j").StartMS
	m.cpu("j", 1)
	o := m.run(38, 1000) // elapsed 38.1s < 40s
	noEv(t, o, proto.VerbWarn, "j")
	for m.now-start < 40_000 {
		m.cpu("j", 1)
		o = m.tick(100)
	}
	w := mustEv(t, o, proto.VerbWarn, "j")
	eq(t, "policy", w.Policy, proto.PolicyWallClock)
	eq(t, "limit", i64(w.LimitMS), int64(60_000))
	eq(t, "in", i64(w.InMS), 60_000-(m.now-start))
	eq(t, "hint", w.Hint, HintWallClock)
	for m.now-start < 60_000 {
		m.cpu("j", 1)
		o = m.tick(100)
		if m.now-start < 60_000 {
			noEv(t, o, proto.VerbKilled, "j")
		}
	}
	k := mustEv(t, o, proto.VerbKilled, "j")
	eq(t, "policy", k.Policy, proto.PolicyWallClock)
	eq(t, "elapsed", i64(k.ElapsedMS), int64(60_000))
	eq(t, "hint", k.Hint, HintWallClock)
}

func TestOrphanAndAttach(t *testing.T) {
	t.Run("lost wrapper -> killed after grace, no warning, no hint", func(t *testing.T) {
		m := newSim(t, testParams())
		m.admitStart("j", proto.SubmitRequest{}, 100, 100*MiB)
		m.lost("j")
		o := m.tick(100)
		eq(t, "lost at", m.job("j").WrapperLostMS, m.now)
		eq(t, "silent", len(o.Events), 0)
		m.lost("j") // duplicate keeps the first time
		o = m.run(98, 100)
		noEv(t, o, proto.VerbKilled, "j")
		o = m.tick(100)
		noEv(t, o, proto.VerbKilled, "j")
		o = m.tick(100)
		k := mustEv(t, o, proto.VerbKilled, "j")
		eq(t, "policy", k.Policy, proto.PolicyOrphaned)
		eq(t, "hint", k.Hint, "")
		noEv(t, m.lastOut, proto.VerbWarn, "j")
		// Group empties: with no wrapper to report exit, the job is dropped.
		m.dead("j")
		m.tick(100)
		if m.job("j") != nil {
			t.Fatal("orphan not dropped when its group emptied")
		}
	})
	t.Run("attach within grace re-adopts", func(t *testing.T) {
		m := newSim(t, testParams())
		m.admitStart("j", proto.SubmitRequest{}, 100, 100*MiB)
		m.lost("j")
		m.tick(5_000)
		m.events = append(m.events, InEvent{Kind: EvAttach, Job: "j", PID: 100, PGID: 100, OutputBytes: 99})
		m.tick(100)
		j := m.job("j")
		eq(t, "lost cleared", j.WrapperLostMS, int64(0))
		eq(t, "output", j.OutputBytes, int64(99))
		o := m.run(200, 100)
		noEv(t, o, proto.VerbKilled, "j")
	})
	t.Run("attach of unknown job creates it running", func(t *testing.T) {
		m := newSim(t, testParams())
		m.samples["x"] = &JobSample{ID: "x", Alive: true, UsedBytes: 300 * MiB}
		m.events = append(m.events, InEvent{Kind: EvAttach, Job: "x", PID: 4242, PGID: 4242, StartMS: T0 - 60_000,
			Submit: &proto.SubmitRequest{Class: "build", EstimateBytes: 3 * GiB}, OutputBytes: 7})
		o := m.tick(100)
		eq(t, "no events", len(o.Events), 0)
		j := m.job("x")
		eq(t, "state", j.State, JobRunning)
		eq(t, "class", j.Class, "build")
		eq(t, "pid", j.PID, 4242)
		eq(t, "start", j.StartMS, T0-60_000)
		eq(t, "reserved", j.ReservedBytes, 3*GiB)
		eq(t, "used", j.UsedBytes, 300*MiB)
		eq(t, "budget", j.BudgetBytes, 4*GiB)
	})
	t.Run("attach without submit uses defaults; attach of queued job runs it", func(t *testing.T) {
		m := newSim(t, testParams())
		m.events = append(m.events, InEvent{Kind: EvAttach, Job: "y", PID: 600})
		m.tick(100)
		j := m.job("y")
		eq(t, "class", j.Class, "default")
		eq(t, "start defaults to now", j.StartMS, m.now)
		m.setFree(2*GiB + 1)
		m.submit("q", proto.SubmitRequest{})
		m.tick(100)
		eq(t, "queued", m.job("q").State, JobQueued)
		m.events = append(m.events, InEvent{Kind: EvAttach, Job: "q", PID: 601, PGID: 601})
		m.tick(100)
		eq(t, "running", m.job("q").State, JobRunning)
	})
	t.Run("lost while queued or before start: removed silently", func(t *testing.T) {
		m := newSim(t, testParams())
		m.submit("r", proto.SubmitRequest{}) // admitted, never started
		m.setFree(2*GiB + 1)
		m.submit("q", proto.SubmitRequest{}) // queued
		m.tick(100)
		m.lost("q")
		m.lost("r")
		o := m.tick(100)
		eq(t, "silent", len(o.Events)+len(o.Actions), 0)
		eq(t, "removed", len(m.s.Jobs), 0)
	})
}

func TestStaleDeadSampleOnStartIgnored(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("j", proto.SubmitRequest{})
	m.tick(100)
	m.started("j", 100)
	m.lost("j")
	m.dead("j") // the sample predates the spawn
	m.tick(100)
	if m.job("j") == nil {
		t.Fatal("job dropped on a stale not-alive sample")
	}
}

func TestDeadJobGetsNoPolicies(t *testing.T) {
	m := budgetSim(t, ModeEnforce)
	m.use("j", 9*GiB)
	m.dead("j")
	o := m.tick(1000)
	noEv(t, o, proto.VerbKilled, "j")
	j := m.job("j")
	eq(t, "used reset", j.UsedBytes, int64(0))
	eq(t, "reservation kept until exit", j.ReservedBytes > 0, true)
}

func TestExitLearning(t *testing.T) {
	p := testParams()
	p.HistorySize = 4
	m := newSim(t, p)
	peaks := []int64{1 * GiB, 2 * GiB, 3 * GiB, 4 * GiB, 5 * GiB}
	for i, pk := range peaks {
		id := string(rune('a' + i))
		m.admitStart(id, proto.SubmitRequest{Class: "test", BudgetBytes: 10 * GiB, Argv: []string{"go", "test"}}, 100+i, pk)
		m.use(id, pk/2) // peak sticks
		m.tick(10_000)
		m.exit(id, 0)
		m.tick(100)
		if i == 1 {
			// 2 samples < MinSamples: still the class default.
			est, src := ClassEstimate(m.p, m.s, "test", "go test")
			eq(t, "untrusted", src, "default")
			eq(t, "default est", est, GiB)
		}
	}
	h := m.s.History
	eq(t, "keys", []string{h[0].Key, h[1].Key}, []string{"class:test", "key:go test"})
	eq(t, "ring", h[1].Peaks, []int64{2 * GiB, 3 * GiB, 4 * GiB, 5 * GiB})
	eq(t, "durations", h[1].DurMS, []int64{10_200, 10_200, 10_200, 10_200})
	// The class learned only from the command's first run.
	eq(t, "class ring", h[0].Peaks, []int64{1 * GiB})
	est, src := ClassEstimate(m.p, m.s, "test", "go test")
	eq(t, "source", src, "key")
	eq(t, "p90 with margin", est, withMargin(5*GiB, 100))
	// A new submission uses it.
	m.submit("n", proto.SubmitRequest{Class: "test", Argv: []string{"go", "test", "-run", "X"}})
	m.tick(100)
	eq(t, "learned estimate", m.job("n").EstimateBytes, withMargin(5*GiB, 100))
	// A job that never started or never used memory teaches nothing.
	m.exit("n", 0)
	m.tick(100)
	eq(t, "unchanged", len(m.s.History[1].Peaks), 4)
}

func TestReservationGrowsNeverShrinks(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("j", proto.SubmitRequest{EstimateBytes: GiB}, 100, 500*MiB)
	eq(t, "R=E", m.job("j").ReservedBytes, GiB)
	m.use("j", 2*GiB)
	m.tick(1000)
	eq(t, "R=peak*1.1", m.job("j").ReservedBytes, withMargin(2*GiB, 100))
	m.use("j", 100*MiB)
	m.tick(1000)
	eq(t, "R kept", m.job("j").ReservedBytes, withMargin(2*GiB, 100))
	eq(t, "peak", m.job("j").PeakBytes, 2*GiB)
	a := Assess(m.p, m.s, m.lastIn)
	eq(t, "unconsumed", a.ReservedBytes, withMargin(2*GiB, 100)-100*MiB)
}
