package policy

import (
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// Finding 5 (remainder): in observe and warn-only mode a WARN whose next=
// action is not carried out says so (mode=<mode>), and its hint threatens
// nothing that will not happen. next= keeps what enforce mode would do.
func TestNonEnforceWarnLinesAreHonest(t *testing.T) {
	threats := []string{"will be killed", "next to be stopped", "or it will be killed", "will be refused"}
	for _, mode := range []string{ModeObserve, ModeWarnOnly, ModeEnforce} {
		t.Run(mode, func(t *testing.T) {
			// mem_budget, deadline path.
			m := budgetSim(t, mode)
			m.use("j", 3700*MiB)
			w := mustEv(t, m.tick(1000), proto.VerbWarn, "j")
			eq(t, "next", w.Next, "kill")
			// pressure ladder.
			p := twoJobs(t, mode)
			p.setFree(2300 * MiB)
			pw := mustEv(t, p.tick(100), proto.VerbWarn, "young")
			eq(t, "pressure next", pw.Next, "throttle")
			all := p.run(300, 100) // through throttle / freeze / kill (or their INFO)
			// disk.
			d := budgetSim(t, mode)
			d.mach.DiskFreeBytes = 5 * GiB // warn line 10G, critical 3G
			dw := mustEv(t, d.tick(1000), proto.VerbWarn, "")
			eq(t, "disk next", dw.Next, "refuse")

			if mode == ModeEnforce {
				for _, e := range []*proto.Event{w, pw, dw} {
					eq(t, "enforce mode key", e.Mode, "")
				}
				eq(t, "enforce hint", w.Hint, HintMemBudgetWarn)
				eq(t, "enforce disk hint", dw.Hint, HintDiskWarn)
				return
			}
			for _, e := range []*proto.Event{w, pw} {
				eq(t, e.Policy+" mode", e.Mode, mode)
				for _, th := range threats {
					if strings.Contains(e.Hint, th) {
						t.Errorf("%s hint %q threatens %q in %s mode", e.Policy, e.Hint, th, mode)
					}
				}
				if e.Hint == "" {
					t.Errorf("%s WARN lost its hint", e.Policy)
				}
			}
			eq(t, "budget hint", w.Hint, HintMemBudgetWarnWould)
			eq(t, "pressure hint", pw.Hint, HintPressureWarnWould)
			if mode == ModeWarnOnly {
				// Admission is real in warn-only mode: refuse is no bluff.
				eq(t, "warn-only disk mode", dw.Mode, "")
				eq(t, "warn-only disk hint", dw.Hint, HintDiskWarn)
			} else {
				eq(t, "observe disk mode", dw.Mode, ModeObserve)
				eq(t, "observe disk hint", dw.Hint, HintDiskWould)
			}
			// Every line with a next= the mode does not carry out, and every
			// "would" INFO, is marked.
			for _, o := range append(append([]Output{}, m.all...), append(p.all, all)...) {
				for _, e := range o.Events {
					virtualNext := e.Next != "" && (e.Next != "refuse" || mode == ModeObserve)
					would := e.Verb == proto.VerbInfo && strings.HasPrefix(e.Msg, "would ")
					if (virtualNext || would) && e.Mode != mode {
						t.Fatalf("unmarked %s line: %s", mode, e.Line())
					}
					if e.Mode != "" && !strings.Contains(e.Line(), " mode="+mode) {
						t.Fatalf("mode key not printed: %s", e.Line())
					}
				}
			}
		})
	}
}

// The mode key prints right after in=, in the fixed key order.
func TestModeKeyOrder(t *testing.T) {
	e := proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyMemBudget, Next: "kill", InMS: proto.I64(20_000), Mode: ModeWarnOnly, Job: "j1"}
	if got, want := e.Line(), "pace: WARN policy=mem_budget next=kill in=20s mode=warn-only job=j1"; got != want {
		t.Fatalf("line %q, want %q", got, want)
	}
}

// Finding 7 (remainder): a refusal for low disk reports the free disk space
// in free= (as WARN policy=disk does), not the memory headroom, and no
// memory need=.
func TestDiskRefusalReportsDiskFree(t *testing.T) {
	m := newSim(t, testParams())
	m.mach.DiskFreeBytes = 1 * GiB // below the 3G critical line
	m.tick(1000)
	m.submit("a", proto.SubmitRequest{EstimateBytes: GiB})
	r := mustEv(t, m.tick(100), proto.VerbRefused, "a")
	eq(t, "reason", r.Reason, proto.ReasonDisk)
	eq(t, "free is disk free", i64(r.FreeBytes), 1*GiB)
	if r.NeedBytes != nil {
		t.Fatalf("need= on a disk refusal: %s", r.Line())
	}
	eq(t, "hint", r.Hint, HintDisk)

	// A job already queued when disk runs low waits with the same figures.
	q := newSim(t, testParams())
	q.mach.DiskFreeBytes = 5 * GiB
	q.tick(1000)
	for _, id := range []string{"x", "y"} {
		q.submit(id, proto.SubmitRequest{Class: "model", EstimateBytes: 6 * GiB})
	}
	q.tick(100) // x admitted (1 slot), y queued for slots
	q.mach.DiskFreeBytes = 2 * GiB
	qo := q.tick(100)
	qe := mustEv(t, qo, proto.VerbQueued, "y")
	eq(t, "queued reason", qe.Reason, proto.ReasonDisk)
	eq(t, "queued free", i64(qe.FreeBytes), 2*GiB)
	if qe.NeedBytes != nil {
		t.Fatalf("need= on a disk wait: %s", qe.Line())
	}

	// Other reasons keep memory headroom and need.
	h := newSim(t, testParams())
	h.tick(1000)
	h.submit("big", proto.SubmitRequest{EstimateBytes: 13 * GiB, NoWait: true})
	hr := mustEv(t, h.tick(100), proto.VerbRefused, "big")
	if hr.Reason == proto.ReasonDisk || hr.NeedBytes == nil || hr.FreeBytes == nil {
		t.Fatalf("headroom refusal figures: %s", hr.Line())
	}
}
