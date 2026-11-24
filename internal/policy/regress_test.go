package policy

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fireharp/pace/internal/proto"
)

// Regression tests for review findings on the policy core.

// A job held behind a larger queued job (no backfill) must not be told
// free >= need, must get a hint that can help, and Assess must not claim
// that a new job would be admitted.
func TestBlockedQueueFreeHintAndAdmitting(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(3*GiB + warnB) // headroom 3G
	m.submit("huge", proto.SubmitRequest{EstimateBytes: 4 * GiB, Priority: intp(9)})
	q := mustEv(t, m.tick(100), proto.VerbQueued, "huge")
	eq(t, "blocker free", i64(q.FreeBytes), 3*GiB)

	a := Assess(m.p, m.s, m.lastIn)
	eq(t, "headroom", a.HeadroomBytes, 3*GiB)
	eq(t, "admitting", a.Admitting, false)
	eq(t, "reasons", a.Reasons, []string{proto.ReasonHeadroom})

	m.submit("nw", proto.SubmitRequest{EstimateBytes: 100 * MiB, NoWait: true})
	m.submit("w", proto.SubmitRequest{EstimateBytes: 100 * MiB})
	o := m.tick(100)
	r := mustEv(t, o, proto.VerbRefused, "nw")
	eq(t, "reason", r.Reason, proto.ReasonHeadroom)
	if i64(r.FreeBytes) >= i64(r.NeedBytes) {
		t.Fatalf("REFUSED reason=headroom with free >= need: %s", r.Line())
	}
	eq(t, "hint", r.Hint, HintQueuedAhead)
	w := mustEv(t, o, proto.VerbQueued, "w")
	if i64(w.FreeBytes) >= i64(w.NeedBytes) {
		t.Fatalf("QUEUED reason=headroom with free >= need: %s", w.Line())
	}

	t.Run("lower-priority blocker does not stop a new default job", func(t *testing.T) {
		m := newSim(t, testParams())
		m.setFree(3*GiB + warnB)
		m.submit("huge", proto.SubmitRequest{EstimateBytes: 4 * GiB, Priority: intp(1)})
		m.tick(100)
		a := Assess(m.p, m.s, m.lastIn)
		eq(t, "admitting", a.Admitting, true)
		eq(t, "reasons", len(a.Reasons), 0)
		m.submit("small", proto.SubmitRequest{EstimateBytes: GiB})
		mustEv(t, m.tick(100), proto.VerbAdmitted, "small")
	})
	t.Run("default class slots full", func(t *testing.T) {
		p := testParams()
		for i := range p.Classes {
			if p.Classes[i].Name == "default" {
				p.Classes[i].Slots = 1
			}
		}
		m := newSim(t, p)
		m.admitStart("a", proto.SubmitRequest{}, 100, 100*MiB)
		a := Assess(m.p, m.s, m.lastIn)
		eq(t, "admitting", a.Admitting, false)
		eq(t, "reasons", a.Reasons, []string{proto.ReasonSlots})
	})
	t.Run("no headroom", func(t *testing.T) {
		m := newSim(t, testParams())
		m.setFree(warnB) // normal level (not below the line), headroom 0
		m.tick(100)
		a := Assess(m.p, m.s, m.lastIn)
		eq(t, "admitting", a.Admitting, false)
		eq(t, "reasons", a.Reasons, []string{proto.ReasonHeadroom})
	})
}

// QUEUED is re-emitted when the reason changes at an unchanged position.
func TestQueuedReemittedOnReasonChange(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("m1", proto.SubmitRequest{Class: "model"}, 100, GiB) // the only model slot
	m.submit("m2", proto.SubmitRequest{Class: "model", EstimateBytes: 3 * GiB})
	q := mustEv(t, m.tick(100), proto.VerbQueued, "m2")
	eq(t, "first reason", q.Reason, proto.ReasonSlots)
	eq(t, "first position", *q.Position, 1)

	m.exit("m1", 0)
	m.setFree(GiB + warnB) // slot free, but only 1G of headroom for 3G
	o := m.run(5, 1000)
	q = mustEv(t, o, proto.VerbQueued, "m2")
	eq(t, "new reason", q.Reason, proto.ReasonHeadroom)
	eq(t, "same position", *q.Position, 1)
	eq(t, "re-emitted once", len(evs(o, proto.VerbQueued)), 1)
	eq(t, "state", m.job("m2").State, JobQueued)
}

// An unknown --class is governed as the default class, and the job is told.
func TestUnknownClassInfo(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("typo", proto.SubmitRequest{Class: "bulid"})
	m.submit("known", proto.SubmitRequest{Class: "build"})
	m.submit("none", proto.SubmitRequest{})
	o := m.tick(100)
	info := mustEv(t, o, proto.VerbInfo, "typo")
	eq(t, "class", info.Class, "default")
	if !strings.Contains(info.Msg, "unknown class bulid") || !strings.Contains(info.Msg, "build default model test") {
		t.Fatalf("msg = %q", info.Msg)
	}
	// It follows ACCEPTED (the wrapper learns its job id first).
	for _, e := range o.Events {
		if e.Job == "typo" {
			eq(t, "first event", e.Verb, proto.VerbAccepted)
			break
		}
	}
	noEv(t, o, proto.VerbInfo, "known")
	noEv(t, o, proto.VerbInfo, "none")
}

// Admission keeps available memory above the warn line: a job admitted
// into the headroom that then uses its whole estimate is never put on the
// pressure ladder.
func TestAdmissionKeepsAvailAboveWarn(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(5 * GiB)
	m.tick(100)
	hr := Assess(m.p, m.s, m.lastIn).HeadroomBytes
	eq(t, "headroom", hr, 5*GiB-warnB)

	// The finding's job: need 2.9G when 2.6G is left. It must wait.
	m.submit("j0", proto.SubmitRequest{EstimateBytes: 2900 * MiB, BudgetBytes: 4 * GiB, NoWait: true})
	mustEv(t, m.tick(100), proto.VerbRefused, "j0")

	m.admitStart("j1", proto.SubmitRequest{EstimateBytes: hr, BudgetBytes: 4 * GiB}, 4242, hr)
	o := m.run(40, 1000)
	for _, v := range []string{proto.VerbWarn, proto.VerbThrottled, proto.VerbFrozen, proto.VerbKilled} {
		noEv(t, o, v, "j1")
	}
	eq(t, "level", m.s.Pressure.Level, proto.LevelNormal)

	t.Run("admit_below_warn restores the plain reserve", func(t *testing.T) {
		p := testParams()
		p.AdmitBelowWarn = true
		m := newSim(t, p)
		m.setFree(5 * GiB)
		m.tick(100)
		eq(t, "headroom", Assess(m.p, m.s, m.lastIn).HeadroomBytes, 3*GiB)
		eq(t, "reserve", EffectiveReserve(p, p.CapacityBytes), 2*GiB)
		m.submit("j0", proto.SubmitRequest{EstimateBytes: 2900 * MiB})
		mustEv(t, m.tick(100), proto.VerbAdmitted, "j0")
	})
}

// trainHeavy runs `python train.py` n times on an otherwise idle machine:
// each declares 1G and peaks at 13G (it fits), then exits normally.
func trainHeavy(m *sim, n int) {
	m.base = 0
	train := []string{"python", "train.py"}
	for i := 0; i < n; i++ {
		id := "t" + strconv.Itoa(i)
		m.admitStart(id, proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: 14 * GiB, Argv: train}, 100+i, 13*GiB)
		m.exit(id, 0)
		m.tick(100)
	}
}

// A heavy command must not poison its class: the class learns one sample
// per command, so an unrelated undeclared command keeps the class default,
// is admitted on a loaded machine and blocks nobody behind it.
func TestLearnedEstimateNeverTooBig(t *testing.T) {
	m := newSim(t, testParams())
	trainHeavy(m, 4)
	ci := findHistory(&m.s, "class:default")
	eq(t, "class samples", len(m.s.History[ci].Peaks), 1)

	m.base = 2 * GiB // other apps: avail < capacity, as on any real machine
	m.submit("ls", proto.SubmitRequest{Argv: []string{"ls"}})
	m.submit("ls-nw", proto.SubmitRequest{Argv: []string{"ls"}, NoWait: true})
	m.submit("small", proto.SubmitRequest{Argv: []string{"make"}, EstimateBytes: 100 * MiB})
	o := m.tick(100)
	for _, id := range []string{"ls", "ls-nw", "small"} {
		noEv(t, o, proto.VerbRefused, id)
		noEv(t, o, proto.VerbQueued, id)
		mustEv(t, o, proto.VerbAdmitted, id)
	}
	eq(t, "ls estimate", m.job("ls").EstimateBytes, int64(GiB))

	t.Run("declared estimate still too_big", func(t *testing.T) {
		m := newSim(t, testParams())
		limit := maxAdmissible(&m.p, m.p.CapacityBytes)
		m.submit("x", proto.SubmitRequest{EstimateBytes: limit + 1})
		r := mustEv(t, m.tick(100), proto.VerbRefused, "x")
		eq(t, "reason", r.Reason, proto.ReasonTooBig)
		eq(t, "hint", r.Hint, HintTooBig)
	})
}

// The command's own history: when the margin lifts its learned estimate
// above the limit, the observed peak (which this machine gave it) is used;
// that is a real need, so it waits like a declared --mem of that size.
func TestLearnedKeyEstimateDropsMarginToFit(t *testing.T) {
	m := newSim(t, testParams())
	trainHeavy(m, 3)
	limit := maxAdmissible(&m.p, m.p.CapacityBytes)
	if withMargin(13*GiB, m.p.MarginPermille) <= limit || 13*GiB > limit {
		t.Fatalf("test setup: limit %d must sit between the peak and the peak with margin", limit)
	}
	m.submit("tr", proto.SubmitRequest{Argv: []string{"python", "train.py"}})
	o := m.tick(100)
	noEv(t, o, proto.VerbRefused, "tr")
	mustEv(t, o, proto.VerbAdmitted, "tr") // idle machine: it fits
	eq(t, "estimate", m.job("tr").EstimateBytes, int64(13*GiB))
}

// A command whose own observed peak is above the limit is too_big, and
// told how to get past it; a class value above the limit never is.
func TestLearnedOverLimit(t *testing.T) {
	p := testParams()
	limit := maxAdmissible(&p, p.CapacityBytes)
	over := limit + GiB

	t.Run("key", func(t *testing.T) {
		m := newSim(t, p)
		for i := 0; i < 3; i++ {
			recordHistory(&m.p, &m.s, histKeyPrefix+"big run", over, 1000, 1)
		}
		m.submit("b", proto.SubmitRequest{Argv: []string{"big", "run"}})
		r := mustEv(t, m.tick(100), proto.VerbRefused, "b")
		eq(t, "reason", r.Reason, proto.ReasonTooBig)
		eq(t, "hint", r.Hint, HintTooBigLearned)
	})
	t.Run("class", func(t *testing.T) {
		m := newSim(t, p)
		for i := 0; i < 3; i++ { // three different heavy commands
			recordHistory(&m.p, &m.s, histClassPrefix+"default", over, 1000, 1)
		}
		m.submit("ls", proto.SubmitRequest{Argv: []string{"ls"}})
		m.submit("small", proto.SubmitRequest{Argv: []string{"make"}, EstimateBytes: 100 * MiB})
		o := m.tick(100)
		mustEv(t, o, proto.VerbAdmitted, "ls")
		mustEv(t, o, proto.VerbAdmitted, "small")
		eq(t, "ls estimate", m.job("ls").EstimateBytes, int64(GiB)) // class default
		a := Assess(m.p, m.s, m.inputs())
		if !a.Admitting {
			t.Fatalf("assess not admitting: %+v", a)
		}
	})
}

// The class learns from the first run of each command only, and is
// trusted once HistoryMinSamples different commands have run.
func TestClassLearnsOneSamplePerCommand(t *testing.T) {
	m := newSim(t, testParams())
	for i, cmd := range []string{"ls", "ls", "git", "ls", "make"} {
		id := "c" + strconv.Itoa(i)
		m.admitStart(id, proto.SubmitRequest{Argv: []string{cmd}}, 200+i, 300*MiB)
		m.exit(id, 0)
		m.tick(100)
	}
	ci := findHistory(&m.s, "class:default")
	eq(t, "class samples", len(m.s.History[ci].Peaks), 3)
	est, src := ClassEstimate(m.p, m.s, "default", "cargo")
	eq(t, "source", src, "class")
	eq(t, "estimate", est, withMargin(300*MiB, m.p.MarginPermille))
}

// After recovery has sent OK, a target that is still throttled or frozen
// gets a fresh WARN (and its full deadline) before it is escalated again.
func TestRecoverThenPressureReturnsRewarns(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.setFree(1200 * MiB) // warn: both are victims
	m.run(230, 100)       // both frozen
	eq(t, "old frozen", m.job("old").Enf.Stage, StageFrozen)
	eq(t, "young frozen", m.job("young").Enf.Stage, StageFrozen)

	m.setFree(10 * GiB)
	o := m.tickUntil(100, 100, func(o Output) bool { return evFor(o, proto.VerbOK, "young") != nil })
	mustEv(t, o, proto.VerbOK, "old")
	if !memStage(&m.job("young").Enf) {
		t.Fatal("young should still be stepped down later")
	}

	m.setFree(1200 * MiB)
	o = m.tick(100)
	for _, id := range []string{"old", "young"} {
		w := mustEv(t, o, proto.VerbWarn, id)
		eq(t, id+" in", i64(w.InMS), m.p.WarnDeadlineMS)
		noEv(t, o, proto.VerbKilled, id)
		noEv(t, o, proto.VerbFrozen, id)
	}
	// Nothing escalates before the new deadline.
	o = m.run(int(m.p.WarnDeadlineMS/100)-2, 100)
	for _, id := range []string{"old", "young"} {
		noEv(t, o, proto.VerbKilled, id)
	}
	// Then the ladder resumes.
	o = m.run(int(3*m.p.StepMS/100)+5, 100)
	mustEv(t, o, proto.VerbKilled, "young")
}

// A refused no_wait job leaves the queue and blocks nobody.
func TestRefusedNoWaitDoesNotBlock(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(4*GiB + warnB) // headroom 4G
	m.submit("big", proto.SubmitRequest{EstimateBytes: 10 * GiB, NoWait: true, Priority: intp(9)})
	m.submit("small", proto.SubmitRequest{EstimateBytes: GiB, NoWait: true})
	m.submit("waiter", proto.SubmitRequest{EstimateBytes: GiB})
	o := m.tick(100)
	r := mustEv(t, o, proto.VerbRefused, "big")
	eq(t, "big reason", r.Reason, proto.ReasonHeadroom)
	eq(t, "big hint", r.Hint, HintHeadroom)
	mustEv(t, o, proto.VerbAdmitted, "small")
	mustEv(t, o, proto.VerbAdmitted, "waiter")
}

// History holds at most HistoryMaxKeys command keys (least recently used
// evicted, class entries kept) and keys are bounded in length.
func TestHistoryKeysBounded(t *testing.T) {
	p := testParams()
	p.HistoryMaxKeys = 5
	m := newSim(t, p)
	for i := 0; i < 12; i++ {
		id := "j" + strconv.Itoa(i)
		m.admitStart(id, proto.SubmitRequest{Argv: []string{"pytest", "tests/test_mod" + strconv.Itoa(i) + ".py"}}, 100+i, 100*MiB)
		m.exit(id, 0)
		m.tick(100)
		if i == 3 {
			// Refresh an old key: it becomes the most recently used.
			m.admitStart("again", proto.SubmitRequest{Argv: []string{"pytest", "tests/test_mod0.py"}}, 99, 100*MiB)
			m.exit("again", 0)
			m.tick(100)
		}
	}
	var keys []string
	for _, h := range m.s.History {
		if strings.HasPrefix(h.Key, histKeyPrefix) {
			keys = append(keys, h.Key)
		}
	}
	eq(t, "key entries", len(keys), 5)
	if findHistory(&m.s, "class:default") < 0 {
		t.Fatal("class entry evicted")
	}
	if findHistory(&m.s, "key:pytest tests/test_mod11.py") < 0 {
		t.Fatal("newest key evicted")
	}
	if findHistory(&m.s, "key:pytest tests/test_mod1.py") >= 0 {
		t.Fatal("least recently used key kept")
	}
	eq(t, "default cap", historyMaxKeys(&Params{}), DefaultHistoryMaxKeys)

	long := DeriveKey([]string{"bash", "-c", "cd /repo && " + strings.Repeat("é", 200)})
	if len(long) > MaxKeyLen || !utf8.ValidString(long) || !strings.HasPrefix(long, "bash cd /repo") {
		t.Fatalf("long key not capped cleanly: %d bytes %q", len(long), long)
	}
	m.submit("k", proto.SubmitRequest{Key: strings.Repeat("k", 500)})
	m.tick(100)
	eq(t, "submitted key capped", len(m.job("k").Key), MaxKeyLen)
}

// cloneState keeps every field, including the ones added for these fixes
// (a lossy clone makes `pace replay` report mismatches).
func TestCloneStateKeepsNewFields(t *testing.T) {
	s := State{
		Jobs:    []Job{{ID: "a", LastQueueReason: proto.ReasonSlots, WasQueued: true, LastPosition: 2}},
		History: []History{{Key: "key:x", Peaks: []int64{1}, DurMS: []int64{2}, LastMS: 3}},
	}
	eq(t, "clone", mustJSON(t, cloneState(s)), mustJSON(t, s))
}
