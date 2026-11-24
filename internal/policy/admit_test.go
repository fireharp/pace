package policy

import (
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

func TestSubmitResolution(t *testing.T) {
	hist := func(key string, peaks ...int64) History {
		d := make([]int64, len(peaks))
		for i := range d {
			d[i] = 60_000
		}
		return History{Key: key, Peaks: peaks, DurMS: d}
	}
	learned := []History{
		hist("class:build", 3*GiB, 3*GiB, 3*GiB),
		hist("key:make all", 5*GiB, 1*GiB, 2*GiB), // p90 = 5G
		hist("key:short", 7*GiB, 7*GiB),           // below MinSamples
	}
	cases := []struct {
		name       string
		req        proto.SubmitRequest
		class      string
		prio       int
		est        int64
		budget     int64
		key        string
		qdeadline  int64
		wall       int64
		noProgress int64
	}{
		{name: "class defaults", req: proto.SubmitRequest{Class: "test", Argv: []string{"go", "test", "./..."}},
			class: "test", prio: 4, est: 1 * GiB, budget: 2 * GiB, key: "go test", qdeadline: 30 * 60_000, wall: 1_800_000, noProgress: 600_000},
		{name: "unknown class maps to default", req: proto.SubmitRequest{Class: "lint", Argv: []string{"x"}},
			class: "default", prio: 5, est: 1 * GiB, budget: 2 * GiB, key: "x", qdeadline: 30 * 60_000, wall: 1_800_000, noProgress: 600_000},
		{name: "empty class maps to default", req: proto.SubmitRequest{Argv: []string{"/usr/bin/python3", "-m", "pytest"}},
			class: "default", prio: 5, est: 1 * GiB, budget: 2 * GiB, key: "python3 pytest", qdeadline: 30 * 60_000, wall: 1_800_000, noProgress: 600_000},
		{name: "declared estimate wins and raises class budget", req: proto.SubmitRequest{Class: "build", EstimateBytes: 6 * GiB, Key: "make all"},
			class: "build", prio: 5, est: 6 * GiB, budget: 6 * GiB, key: "make all", qdeadline: 30 * 60_000, wall: 3_600_000, noProgress: 600_000},
		{name: "explicit budget kept even below declared estimate", req: proto.SubmitRequest{Class: "build", EstimateBytes: 6 * GiB, BudgetBytes: 5 * GiB},
			class: "build", prio: 5, est: 6 * GiB, budget: 5 * GiB, key: "cmd-j", qdeadline: 30 * 60_000, wall: 3_600_000, noProgress: 600_000},
		{name: "learned key beats learned class", req: proto.SubmitRequest{Class: "build", Key: "make all"},
			class: "build", prio: 5, est: withMargin(5*GiB, 100), budget: 4 * GiB, key: "make all", qdeadline: 30 * 60_000, wall: 3_600_000, noProgress: 600_000},
		{name: "learned class when key untrusted", req: proto.SubmitRequest{Class: "build", Key: "short"},
			class: "build", prio: 5, est: withMargin(3*GiB, 100), budget: 4 * GiB, key: "short", qdeadline: 30 * 60_000, wall: 3_600_000, noProgress: 600_000},
		{name: "priority clamped high", req: proto.SubmitRequest{Priority: intp(42), QueueTimeoutMS: 5_000, WallLimitMS: 9_000},
			class: "default", prio: 9, est: 1 * GiB, budget: 2 * GiB, key: "cmd-j", qdeadline: 5_000, wall: 9_000, noProgress: 600_000},
		{name: "priority clamped low", req: proto.SubmitRequest{Priority: intp(-3)},
			class: "default", prio: 0, est: 1 * GiB, budget: 2 * GiB, key: "cmd-j", qdeadline: 30 * 60_000, wall: 1_800_000, noProgress: 600_000},
		{name: "explicit priority zero honoured", req: proto.SubmitRequest{Class: "model", Priority: intp(0)},
			class: "model", prio: 0, est: 6 * GiB, budget: 9 * GiB, key: "cmd-j", qdeadline: 30 * 60_000, wall: 4 * 3_600_000, noProgress: 900_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testParams()
			m := newSim(t, p)
			m.s.History = append([]History(nil), learned...)
			m.setFree(1 * GiB) // keep it queued so we can inspect
			m.mach.Load1Milli = 0
			m.submit("j", tc.req)
			o := m.tick(100)
			acc := mustEv(t, o, proto.VerbAccepted, "j")
			eq(t, "accepted need", i64(acc.NeedBytes), tc.est)
			j := m.job("j")
			if j == nil {
				t.Fatalf("job missing:\n%s", dumpEvents(o))
			}
			eq(t, "class", j.Class, tc.class)
			eq(t, "priority", j.Priority, tc.prio)
			eq(t, "estimate", j.EstimateBytes, tc.est)
			eq(t, "budget", j.BudgetBytes, tc.budget)
			eq(t, "key", j.Key, tc.key)
			eq(t, "queue deadline", j.QueueDeadlineMS-j.SubmitMS, tc.qdeadline)
			eq(t, "wall", j.WallLimitMS, tc.wall)
			eq(t, "no progress", j.NoProgressMS, tc.noProgress)
			eq(t, "seq", j.Seq, int64(1))
		})
	}
}

func TestSubmitEdgeCases(t *testing.T) {
	p := testParams()
	p.DefaultQueueTimeoutMS = 0
	m := newSim(t, p)
	m.events = append(m.events,
		InEvent{Kind: EvSubmit, Job: "a"},                                 // nil submit
		InEvent{Kind: EvSubmit, Submit: &proto.SubmitRequest{}},           // no id
		InEvent{Kind: EvSubmit, Job: "b", Submit: &proto.SubmitRequest{}}, // ok
		InEvent{Kind: EvSubmit, Job: "b", Submit: &proto.SubmitRequest{}}, // duplicate
		InEvent{Kind: "bogus", Job: "b"},                                  // unknown kind
		InEvent{Kind: EvStarted, Job: "zzz", PID: 50},                     // unknown job
		InEvent{Kind: EvProgress, Job: "zzz", OutputBytes: 5},             // unknown job
		InEvent{Kind: EvExit, Job: "zzz"},                                 // unknown job
		InEvent{Kind: EvLost, Job: "zzz"},                                 // unknown job
		InEvent{Kind: EvAttach, Job: ""},                                  // no id
		InEvent{Kind: EvSubmit, Job: "c", Submit: &proto.SubmitRequest{Argv: []string{}}},
	)
	o := m.tick(100)
	eq(t, "accepted", len(evs(o, proto.VerbAccepted)), 2)
	eq(t, "jobs", len(m.s.Jobs), 2)
	eq(t, "no queue deadline", m.job("b").QueueDeadlineMS, int64(0))
	eq(t, "empty argv key", m.job("c").Key, "")
	eq(t, "seq b", m.job("b").Seq, int64(1))
	eq(t, "seq c", m.job("c").Seq, int64(2))
	eq(t, "next seq", m.s.NextSeq, int64(2))
}

func TestAdmitImmediately(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("a", proto.SubmitRequest{Class: "build"})
	o := m.tick(100)
	ad := mustEv(t, o, proto.VerbAdmitted, "a")
	eq(t, "waited", i64(ad.WaitedMS), int64(0))
	eq(t, "need", i64(ad.NeedBytes), 2*GiB)
	eq(t, "budget", i64(ad.BudgetBytes), 4*GiB)
	eq(t, "free", i64(ad.FreeBytes), 14*GiB-warnB) // 16 - 2 used - effective reserve (the warn line)
	noEv(t, o, proto.VerbQueued, "a")
	j := m.job("a")
	eq(t, "state", j.State, JobRunning)
	eq(t, "reserved", j.ReservedBytes, 2*GiB)
	eq(t, "was queued", j.WasQueued, false)
	// ACCEPTED precedes ADMITTED.
	if o.Events[0].Verb != proto.VerbAccepted || o.Events[len(o.Events)-1].Verb != proto.VerbAdmitted {
		t.Fatalf("order:\n%s", dumpEvents(o))
	}
}

func TestQueueThenAdmitAndPositions(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(3*GiB + warnB) // headroom 3G
	m.submit("big", proto.SubmitRequest{EstimateBytes: 2560 * MiB})
	m.submit("q1", proto.SubmitRequest{EstimateBytes: 2 * GiB})
	m.submit("q2", proto.SubmitRequest{EstimateBytes: 1 * GiB})
	m.submit("q3", proto.SubmitRequest{EstimateBytes: 1 * GiB})
	o := m.tick(100)
	mustEv(t, o, proto.VerbAdmitted, "big") // 2.5G of 3G
	for i, id := range []string{"q1", "q2", "q3"} {
		q := mustEv(t, o, proto.VerbQueued, id)
		eq(t, id+" position", *q.Position, i+1)
		eq(t, id+" reason", q.Reason, proto.ReasonHeadroom)
		// q1 does not fit the 512M left; q2 and q3 see what is left after
		// q1's claim (nothing), so free < need holds for every headroom line.
		wantFree := int64(0)
		if id == "q1" {
			wantFree = 512 * MiB
		}
		eq(t, id+" free", i64(q.FreeBytes), wantFree)
		eq(t, id+" eta", i64(q.ETAMS), int64(i+1)*DefaultETAPerPositionMS)
	}
	// Nothing changes -> no re-emission.
	o = m.tick(1000)
	if len(evs(o, proto.VerbQueued)) != 0 {
		t.Fatalf("QUEUED re-emitted without position change:\n%s", dumpEvents(o))
	}
	// big finishes; q1 (2G) and q2 (1G) fit into 3G, q3 moves to position 1.
	m.started("big", 100)
	m.tick(100)
	m.exit("big", 0)
	o = m.tick(100)
	a1 := mustEv(t, o, proto.VerbAdmitted, "q1")
	eq(t, "waited", i64(a1.WaitedMS), int64(1200))
	mustEv(t, o, proto.VerbAdmitted, "q2")
	q3 := mustEv(t, o, proto.VerbQueued, "q3")
	eq(t, "q3 new position", *q3.Position, 1)
	if !m.job("q1").WasQueued {
		t.Fatal("q1 should be marked WasQueued")
	}
}

func TestPriorityOrder(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(2*GiB + 512*MiB) // headroom 512M: nothing fits
	m.submit("low", proto.SubmitRequest{Priority: intp(1)})
	m.submit("hi", proto.SubmitRequest{Priority: intp(8)})
	m.submit("mid1", proto.SubmitRequest{Priority: intp(5)})
	m.submit("mid2", proto.SubmitRequest{Priority: intp(5)})
	o := m.tick(100)
	want := map[string]int{"hi": 1, "mid1": 2, "mid2": 3, "low": 4}
	for id, pos := range want {
		eq(t, id, *mustEv(t, o, proto.VerbQueued, id).Position, pos)
	}
	// A new high-priority job jumps the queue and pushes the rest back.
	m.submit("urgent", proto.SubmitRequest{Priority: intp(9)})
	o = m.tick(100)
	eq(t, "urgent", *mustEv(t, o, proto.VerbQueued, "urgent").Position, 1)
	eq(t, "low", *mustEv(t, o, proto.VerbQueued, "low").Position, 5)
}

func TestSlotsSkipVersusHeadroomHeadOfLine(t *testing.T) {
	t.Run("slots-blocked job is skipped", func(t *testing.T) {
		m := newSim(t, testParams())
		// model has 1 slot; occupy it.
		m.admitStart("m1", proto.SubmitRequest{Class: "model", EstimateBytes: 1 * GiB}, 100, 0)
		m.submit("m2", proto.SubmitRequest{Class: "model", EstimateBytes: 1 * GiB, Priority: intp(9)})
		m.submit("small", proto.SubmitRequest{EstimateBytes: 1 * GiB, Priority: intp(1)})
		o := m.tick(100)
		q := mustEv(t, o, proto.VerbQueued, "m2")
		eq(t, "reason", q.Reason, proto.ReasonSlots)
		mustEv(t, o, proto.VerbAdmitted, "small")
		// When the slot frees, m2 is admitted.
		m.exit("m1", 0)
		o = m.tick(100)
		mustEv(t, o, proto.VerbAdmitted, "m2")
	})
	t.Run("headroom-blocked job blocks everything behind it", func(t *testing.T) {
		m := newSim(t, testParams())
		m.setFree(5 * GiB) // headroom 3G
		m.submit("huge", proto.SubmitRequest{EstimateBytes: 4 * GiB, Priority: intp(9)})
		m.submit("tiny", proto.SubmitRequest{EstimateBytes: 100 * MiB, Priority: intp(1)})
		o := m.tick(100)
		eq(t, "huge reason", mustEv(t, o, proto.VerbQueued, "huge").Reason, proto.ReasonHeadroom)
		tiny := mustEv(t, o, proto.VerbQueued, "tiny")
		eq(t, "tiny reason", tiny.Reason, proto.ReasonHeadroom)
		eq(t, "tiny position", *tiny.Position, 2)
		noEv(t, o, proto.VerbAdmitted, "tiny")
	})
	t.Run("slot-blocked job behind a headroom block reports slots", func(t *testing.T) {
		m := newSim(t, testParams())
		m.admitStart("m1", proto.SubmitRequest{Class: "model", EstimateBytes: 1 * GiB}, 100, 0)
		m.setFree(4 * GiB) // headroom = 4 - 1 (m1 unconsumed) - 2 = 1G
		m.submit("huge", proto.SubmitRequest{EstimateBytes: 4 * GiB, Priority: intp(9)})
		m.submit("m2", proto.SubmitRequest{Class: "model", EstimateBytes: 1 * GiB})
		o := m.tick(100)
		eq(t, "m2", mustEv(t, o, proto.VerbQueued, "m2").Reason, proto.ReasonSlots)
	})
}

func TestNoWaitRefusals(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(m *sim)
		req    proto.SubmitRequest
		reason string
		hint   string
	}{
		{"headroom", func(m *sim) { m.setFree(3 * GiB) }, proto.SubmitRequest{EstimateBytes: 2 * GiB, NoWait: true}, proto.ReasonHeadroom, HintHeadroom},
		{"slots", func(m *sim) {
			m.admitStart("m1", proto.SubmitRequest{Class: "model", EstimateBytes: GiB}, 100, 0)
		}, proto.SubmitRequest{Class: "model", EstimateBytes: GiB, NoWait: true}, proto.ReasonSlots, HintSlots},
		{"pressure", func(m *sim) { m.setFree(2 * GiB); m.tick(100) }, proto.SubmitRequest{EstimateBytes: 1, NoWait: true}, proto.ReasonPressure, HintPressureWait},
		{"disk", func(m *sim) { m.mach.DiskFreeBytes = 1 * GiB }, proto.SubmitRequest{NoWait: true}, proto.ReasonDisk, HintDisk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newSim(t, testParams())
			tc.setup(m)
			m.submit("nw", tc.req)
			o := m.tick(100)
			r := mustEv(t, o, proto.VerbRefused, "nw")
			eq(t, "reason", r.Reason, tc.reason)
			eq(t, "hint", r.Hint, tc.hint)
			if i64(r.RetryAfterMS) < MinETAMS {
				t.Fatalf("retry_after missing: %s", r.Line())
			}
			noEv(t, o, proto.VerbQueued, "nw")
			if m.job("nw") != nil {
				t.Fatal("refused job kept in state")
			}
		})
	}
	t.Run("no_wait admitted when it fits", func(t *testing.T) {
		m := newSim(t, testParams())
		m.submit("nw", proto.SubmitRequest{NoWait: true})
		mustEv(t, m.tick(100), proto.VerbAdmitted, "nw")
	})
}

func TestTooBig(t *testing.T) {
	m := newSim(t, testParams())
	m.submit("x", proto.SubmitRequest{EstimateBytes: 16*GiB - warnB + 1})
	m.submit("ok", proto.SubmitRequest{EstimateBytes: 14*GiB - warnB})
	o := m.tick(100)
	r := mustEv(t, o, proto.VerbRefused, "x")
	eq(t, "reason", r.Reason, proto.ReasonTooBig)
	eq(t, "hint", r.Hint, HintTooBig)
	if r.RetryAfterMS != nil {
		t.Fatal("too_big must not suggest retrying")
	}
	mustEv(t, o, proto.VerbAdmitted, "ok") // exactly C-reserve-used fits (reserve = the warn line)
	t.Run("observe admits with INFO", func(t *testing.T) {
		p := testParams()
		p.Mode = ModeObserve
		m := newSim(t, p)
		m.submit("x", proto.SubmitRequest{EstimateBytes: 15 * GiB})
		o := m.tick(100)
		info := mustEv(t, o, proto.VerbInfo, "x")
		eq(t, "reason", info.Reason, proto.ReasonTooBig)
		mustEv(t, o, proto.VerbAdmitted, "x")
	})
	t.Run("unknown capacity skips too_big", func(t *testing.T) {
		p := testParams()
		p.CapacityBytes = 0
		m := newSim(t, p)
		m.mach.TotalBytes = 0
		m.submit("x", proto.SubmitRequest{EstimateBytes: 15 * GiB})
		o := m.tick(100)
		noEv(t, o, proto.VerbRefused, "x")
	})
}

func TestQueueTimeout(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(3 * GiB)
	m.submit("q", proto.SubmitRequest{EstimateBytes: 2 * GiB, QueueTimeoutMS: 10_000})
	o := m.tick(100)
	mustEv(t, o, proto.VerbQueued, "q")
	o = m.tick(9_999)
	noEv(t, o, proto.VerbRefused, "q")
	o = m.tick(1)
	r := mustEv(t, o, proto.VerbRefused, "q")
	eq(t, "reason", r.Reason, proto.ReasonQueueTimeout)
	eq(t, "hint", r.Hint, HintQueueTimeout)
	eq(t, "retry", i64(r.RetryAfterMS), DefaultETAPerPositionMS)
	if m.job("q") != nil {
		t.Fatal("timed-out job kept")
	}
}

func TestQueueTimeoutBeatsAdmission(t *testing.T) {
	// The deadline is strict: a job whose deadline has passed is refused
	// even if it would fit on that very tick.
	m := newSim(t, testParams())
	m.setFree(3 * GiB)
	m.submit("q", proto.SubmitRequest{EstimateBytes: 2 * GiB, QueueTimeoutMS: 1_000})
	m.tick(100)
	m.setFree(10 * GiB)
	o := m.tick(1_000)
	eq(t, "reason", mustEv(t, o, proto.VerbRefused, "q").Reason, proto.ReasonQueueTimeout)
}

func TestETAFromHistory(t *testing.T) {
	m := newSim(t, testParams())
	m.s.History = []History{{Key: "class:build", Peaks: []int64{GiB, GiB, GiB}, DurMS: []int64{50_000, 60_000, 70_000}}}
	m.admitStart("b1", proto.SubmitRequest{Class: "build", EstimateBytes: 5 * GiB}, 100, 0)
	m.tick(10_000)     // b1 has run ~10.1s, p50 60s -> ~49.9s remaining
	m.setFree(6 * GiB) // headroom = 6 - 5 - 2 < 0
	m.submit("q", proto.SubmitRequest{EstimateBytes: GiB})
	m.submit("q2", proto.SubmitRequest{EstimateBytes: GiB})
	o := m.tick(100)
	q := mustEv(t, o, proto.VerbQueued, "q")
	b1 := m.job("b1")
	want := int64(60_000) - (m.now - b1.StartMS)
	eq(t, "eta pos1", i64(q.ETAMS), want)
	q2 := mustEv(t, o, proto.VerbQueued, "q2")
	eq(t, "eta pos2", i64(q2.ETAMS), want+DefaultETAPerPositionMS)

	// etaFor table.
	for _, tc := range []struct {
		rem  []int64
		pos  int
		want int64
	}{
		{nil, 0, DefaultETAPerPositionMS},
		{nil, 3, 3 * DefaultETAPerPositionMS},
		{[]int64{0}, 1, MinETAMS},
		{[]int64{5_000, 9_000}, 2, 9_000},
		{[]int64{5_000, 9_000}, 4, 9_000 + 2*DefaultETAPerPositionMS},
	} {
		eq(t, "etaFor", etaFor(tc.rem, tc.pos), tc.want)
	}
}

func TestAdmissionPausedByPressureUntilRecovered(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("a", proto.SubmitRequest{EstimateBytes: GiB}, 100, 1*GiB)
	m.setFree(2 * GiB) // < 2.4G warn
	m.submit("q", proto.SubmitRequest{EstimateBytes: 1})
	o := m.tick(100)
	eq(t, "reason", mustEv(t, o, proto.VerbQueued, "q").Reason, proto.ReasonPressure)
	mustEv(t, o, proto.VerbWarn, "a")
	// Back to normal: still paused while the pressure warning is outstanding.
	m.setFree(10 * GiB)
	o = m.tick(100)
	noEv(t, o, proto.VerbAdmitted, "q")
	a := Assess(m.p, m.s, m.lastIn)
	if a.Admitting || len(a.Reasons) != 1 || a.Reasons[0] != proto.ReasonPressure {
		t.Fatalf("assessment while recovering: %+v", a)
	}
	o = m.run(50, 100) // 5s hold
	mustEv(t, o, proto.VerbOK, "a")
	mustEv(t, o, proto.VerbAdmitted, "q")
}

func TestAdmitPauseOnJobWarnOption(t *testing.T) {
	for _, on := range []bool{false, true} {
		p := testParams()
		p.AdmitPauseOnJobWarn = on
		m := newSim(t, p)
		m.admitStart("a", proto.SubmitRequest{BudgetBytes: GiB}, 100, 950*MiB) // mem_budget WARN
		if m.job("a").Enf.Warnings == nil {
			t.Fatal("expected mem_budget warning")
		}
		m.submit("q", proto.SubmitRequest{EstimateBytes: GiB, NoWait: true})
		o := m.tick(100)
		if on {
			eq(t, "reason", mustEv(t, o, proto.VerbRefused, "q").Reason, ReasonJobWarning)
			eq(t, "hint", mustEv(t, o, proto.VerbRefused, "q").Hint, HintJobWarning)
		} else {
			mustEv(t, o, proto.VerbAdmitted, "q")
		}
	}
}

func TestDiskLevels(t *testing.T) {
	m := newSim(t, testParams())
	m.mach.DiskFreeBytes = 8 * GiB
	o := m.tick(100)
	w := evs(o, proto.VerbWarn)
	if len(w) != 1 || w[0].Policy != proto.PolicyDisk || w[0].Level != proto.LevelWarn || w[0].Job != "" {
		t.Fatalf("disk warn:\n%s", dumpEvents(o))
	}
	eq(t, "disk state", m.s.Pressure.DiskLow, proto.LevelWarn)
	// warn does not stop admission
	m.submit("a", proto.SubmitRequest{})
	mustEv(t, m.tick(100), proto.VerbAdmitted, "a")
	// critical: machine-level WARN, new submissions refused, queued wait.
	m.setFree(3 * GiB)
	m.submit("q", proto.SubmitRequest{EstimateBytes: 2 * GiB})
	m.tick(100) // queued for headroom
	m.mach.DiskFreeBytes = 2 * GiB
	m.submit("n", proto.SubmitRequest{})
	o = m.tick(100)
	w = evs(o, proto.VerbWarn)
	if len(w) != 1 || w[0].Level != proto.LevelCritical {
		t.Fatalf("disk critical:\n%s", dumpEvents(o))
	}
	r := mustEv(t, o, proto.VerbRefused, "n")
	eq(t, "reason", r.Reason, proto.ReasonDisk)
	eq(t, "q still queued", m.job("q").State, JobQueued)
	// Steady: no repeated WARN.
	o = m.tick(100)
	eq(t, "no repeat", len(evs(o, proto.VerbWarn)), 0)
	noEv(t, o, proto.VerbQueued, "q") // position unchanged -> not re-emitted
}

func TestDiskRecoveryAndUnknown(t *testing.T) {
	m := newSim(t, testParams())
	m.mach.DiskFreeBytes = 1 * GiB
	m.tick(100)
	m.mach.DiskFreeBytes = 50 * GiB
	o := m.tick(100)
	ok := evs(o, proto.VerbOK)
	if len(ok) != 1 || ok[0].Policy != proto.PolicyDisk {
		t.Fatalf("disk OK:\n%s", dumpEvents(o))
	}
	eq(t, "cleared", m.s.Pressure.DiskLow, "")
	m.mach.DiskFreeBytes = -1
	o = m.tick(100)
	eq(t, "unknown disk is normal", len(o.Events), 0)
}

func TestObserveAdmitsEverything(t *testing.T) {
	p := testParams()
	p.Mode = ModeObserve
	m := newSim(t, p)
	m.setFree(1 * GiB) // critical
	for _, id := range []string{"a", "b", "c"} {
		m.submit(id, proto.SubmitRequest{EstimateBytes: 4 * GiB, NoWait: true})
	}
	o := m.tick(100)
	for _, id := range []string{"a", "b", "c"} {
		mustEv(t, o, proto.VerbAdmitted, id)
	}
	eq(t, "no queue", len(evs(o, proto.VerbQueued)), 0)
	eq(t, "no refusal", len(evs(o, proto.VerbRefused)), 0)
	if a := Assess(m.p, m.s, m.lastIn); !a.Admitting || a.Reasons != nil {
		t.Fatalf("observe assessment: %+v", a)
	}
}

func TestStartedWhileQueuedIsGoverned(t *testing.T) {
	m := newSim(t, testParams())
	m.setFree(2*GiB + 100*MiB)
	m.submit("q", proto.SubmitRequest{EstimateBytes: GiB})
	m.tick(100)
	eq(t, "queued", m.job("q").State, JobQueued)
	m.started("q", 300)
	m.tick(100)
	j := m.job("q")
	eq(t, "running", j.State, JobRunning)
	eq(t, "reserved", j.ReservedBytes, GiB)
}
