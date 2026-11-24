package policy

import (
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// Finding 38: the kernel kills a job at its container's hard limit (budget *
// (1 + tolerance)) before any sample sees it there. The wrapper reports it
// with the exit; the agent must get the same KILLED policy=mem_budget line
// (and hint) as for a sampled kill, and the truncated run is not learned.
func TestKernelOOMExitIsMemBudgetKill(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("j1", proto.SubmitRequest{BudgetBytes: 300 * MiB, Argv: []string{"alloc"}}, 500, 200*MiB)
	m.events = append(m.events, InEvent{Kind: EvExit, Job: "j1", Code: 137, Signaled: true, Signal: 9, OOMKill: true, PeakBytes: 330 * MiB})
	delete(m.samples, "j1")
	o := m.tick(100)
	k := mustEv(t, o, proto.VerbKilled, "j1")
	if k.Policy != proto.PolicyMemBudget || i64(k.UsedBytes) != 330*MiB || i64(k.BudgetBytes) != 300*MiB {
		t.Fatalf("KILLED %+v", k)
	}
	if k.Hint != HintMemBudgetKill || k.Msg != proto.MsgKernelMemMax {
		t.Fatalf("KILLED hint %q msg %q", k.Hint, k.Msg)
	}
	if m.job("j1") != nil {
		t.Fatal("job not removed")
	}
	for _, h := range m.s.History {
		if h.Key == histKeyPrefix+"alloc" {
			t.Fatalf("a kernel-killed run was learned: %+v", h)
		}
	}

	// A plain signal death (no OOM at the job's own limit) is not a kill.
	m.admitStart("j2", proto.SubmitRequest{BudgetBytes: 300 * MiB}, 600, 100*MiB)
	m.events = append(m.events, InEvent{Kind: EvExit, Job: "j2", Code: 137, Signaled: true, Signal: 9})
	delete(m.samples, "j2")
	noEv(t, m.tick(100), proto.VerbKilled, "j2")
}

// A job warned under mem_budget (next=kill in=20s) that the kernel kills at
// the hard limit before that deadline: the KILLED line says the kill came
// before the deadline the WARN gave (the hard limit is the tolerance line,
// past which mem_budget kills at once). Once the deadline has passed, the
// kernel's msg alone.
func TestKernelOOMBeforeWarnDeadline(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("j1", proto.SubmitRequest{BudgetBytes: 300 * MiB}, 500, 200*MiB)
	m.use("j1", 285*MiB) // 95%: WARN, kill in 20s if still over budget
	w := mustEv(t, m.tick(100), proto.VerbWarn, "j1")
	if w.Next != "kill" || i64(w.InMS) != 20_000 {
		t.Fatalf("WARN %+v", w)
	}
	m.tick(2000)
	m.events = append(m.events, InEvent{Kind: EvExit, Job: "j1", Code: 137, Signaled: true, Signal: 9, OOMKill: true, PeakBytes: 330 * MiB})
	delete(m.samples, "j1")
	k := mustEv(t, m.tick(100), proto.VerbKilled, "j1")
	if want := proto.MsgKernelMemMax + proto.MsgBeforeWarnDeadline; k.Msg != want || k.Hint != HintMemBudgetKill {
		t.Fatalf("KILLED msg %q hint %q, want msg %q", k.Msg, k.Hint, want)
	}

	// Warned, and the deadline passed (still within budget, so not killed):
	// the kernel's kill is not before it.
	m.admitStart("j2", proto.SubmitRequest{BudgetBytes: 300 * MiB}, 600, 200*MiB)
	m.use("j2", 285*MiB)
	mustEv(t, m.tick(100), proto.VerbWarn, "j2")
	m.run(21, 1000)
	m.events = append(m.events, InEvent{Kind: EvExit, Job: "j2", Code: 137, Signaled: true, Signal: 9, OOMKill: true, PeakBytes: 330 * MiB})
	delete(m.samples, "j2")
	k = mustEv(t, m.tick(100), proto.VerbKilled, "j2")
	if k.Msg != proto.MsgKernelMemMax {
		t.Fatalf("KILLED msg %q, want %q", k.Msg, proto.MsgKernelMemMax)
	}
}

// A job the policy is already killing gets no second KILLED.
func TestKernelOOMAfterPolicyKillNoDuplicate(t *testing.T) {
	m := newSim(t, testParams())
	m.admitStart("j1", proto.SubmitRequest{BudgetBytes: 300 * MiB}, 500, 200*MiB)
	m.use("j1", 400*MiB) // far above budget+tolerance: killed at once
	mustEv(t, m.tick(100), proto.VerbKilled, "j1")
	m.events = append(m.events, InEvent{Kind: EvExit, Job: "j1", Code: 137, Signaled: true, Signal: 9, OOMKill: true})
	delete(m.samples, "j1")
	noEv(t, m.tick(100), proto.VerbKilled, "j1")
}
