package daemon

import (
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// Round 3: a container-frozen job that its pace run thawed (thawLost on a
// lost daemon, a forwarded signal) is frozen again, with the "frozen again"
// FROZEN line, both after a restart (restoreContainers, re-attach) and while
// the same daemon keeps running.
func TestContainerThawedBehindDaemonIsRefrozen(t *testing.T) {
	t.Run("restart", func(t *testing.T) {
		f := platform.NewFake()
		cont := f.EnableContainers("/cg/pace")
		// thawLost left the container thawed; state.json still says frozen.
		cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}, Frozen: false})
		fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
			return cgJobState(policy.StageFrozen, 0), nil
		})
		d := newStepDaemon(t, f, fn, nil, cgConfig)
		d.st = cgJobState(policy.StageFrozen, 0)
		setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
		d.restoreContainers()
		d.enqueue(policy.InEvent{Kind: policy.EvAttach, Job: "j", PID: 5200, PGID: 5200, Cgroup: "/cg/pace/job-j"})
		for range 3 {
			d.runStep(true)
		}
		if g, _ := cont.Get("j"); !g.Frozen {
			t.Fatal("a container the state calls frozen was left thawed after a restart")
		}
		if n := len(refrozeLines(d)); n != 1 {
			t.Fatalf("FROZEN frozen-again lines: %d, want 1", n)
		}
		for _, s := range f.Signals() {
			if s.Sig == syscall.SIGSTOP {
				t.Fatalf("container member SIGSTOPped: %s", fmtSent(f.Signals()))
			}
		}
	})
	t.Run("live", func(t *testing.T) {
		f := platform.NewFake()
		cont := f.EnableContainers("/cg/pace")
		cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
		stage := policy.StageNone
		var acts []policy.Action
		fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
			a := acts
			acts = nil
			return cgJobState(stage, 0), a
		})
		d := newStepDaemon(t, f, fn, nil, cgConfig)
		d.st = cgJobState("", 0)
		setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
		d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
		d.runStep(true)
		d.runStep(true)
		stage = policy.StageFrozen
		acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
		d.runStep(true)
		if g, _ := cont.Get("j"); !g.Frozen {
			t.Fatal("not frozen")
		}
		// pace run forwarded a signal (or lost its stream): it thawed the job.
		if err := cont.Freeze("j", false); err != nil {
			t.Fatal(err)
		}
		d.enqueue(policy.InEvent{Kind: policy.EvAttach, Job: "j", PID: 5200, PGID: 5200, Cgroup: "/cg/pace/job-j"})
		d.runStep(true)
		d.runStep(true)
		if g, _ := cont.Get("j"); !g.Frozen {
			t.Fatal("a container thawed by its wrapper was never frozen again")
		}
		if n := len(refrozeLines(d)); n != 1 {
			t.Fatalf("FROZEN frozen-again lines: %d, want 1", n)
		}
	})
}

// Round 3: a re-attaching wrapper says which events it already has; the
// daemon replays the later ones (a restarted daemon's KILLED or FROZEN sent
// before the re-attach), oldest first, before the new ACCEPTED. A wrapper
// that says nothing gets the new stream only.
func TestAttachReplaysUndeliveredEvents(t *testing.T) {
	g := newRegistry()
	ev := func(ts int64, verb string) proto.Event { return proto.Event{V: 1, TS: ts, Verb: verb, Job: "j"} }
	g.create("j", submitReq("make"), ev(100, proto.VerbAccepted))
	g.apply(stepResult{nowMS: 300, state: &policy.State{}, events: []proto.Event{
		ev(200, proto.VerbWarn), ev(300, proto.VerbFrozen), ev(400, proto.VerbKilled),
	}})
	req := proto.AttachRequest{Submit: submitReq("make"), LastEventTS: 200}
	s, _, _ := g.attach("j", req, ev(500, proto.VerbAccepted))
	got, dropped := g.read(s, nil)
	var verbs []string
	for _, e := range got {
		verbs = append(verbs, e.Verb)
	}
	if dropped != 0 || len(verbs) != 3 || verbs[0] != proto.VerbFrozen || verbs[1] != proto.VerbKilled || verbs[2] != proto.VerbAccepted {
		t.Fatalf("attach stream %v (dropped %d), want FROZEN KILLED ACCEPTED", verbs, dropped)
	}
	s, _, _ = g.attach("j", proto.AttachRequest{Submit: submitReq("make")}, ev(600, proto.VerbAccepted))
	if got, _ := g.read(s, nil); len(got) != 1 || got[0].Verb != proto.VerbAccepted {
		t.Fatalf("attach without last_event_ts replayed %+v", got)
	}
}

// Round 3: after a kernel kill at the hard limit, the job's record has the
// kernel's peak (the KILLED line's used=), not the last sample's (or 0).
func TestKernelKillPeakInJobRecord(t *testing.T) {
	g := newRegistry()
	g.create("j", submitReq("alloc"), proto.Event{V: 1, TS: 1, Verb: proto.VerbAccepted, Job: "j"})
	st := policy.State{Jobs: []policy.Job{{ID: "j", State: policy.JobRunning, PID: 5200, PGID: 5200, PeakBytes: 20 * miB}}}
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "j"}})
	g.apply(stepResult{nowMS: 10, state: &st})
	g.markExit("j", proto.ExitRequest{Code: 137, Signaled: true, Signal: 9, OOMKill: true, PeakBytes: 330 * miB})
	g.apply(stepResult{nowMS: 20, state: &st}) // a step that still has the job
	if js, _ := g.jobStatus("j"); js.PeakBytes != 330*miB {
		t.Fatalf("peak_bytes %d MiB while the exit is pending, want 330", js.PeakBytes/miB)
	}
	g.apply(stepResult{nowMS: 30, state: &policy.State{}, processed: []policy.InEvent{{Kind: policy.EvExit, Job: "j"}}})
	if js, _ := g.jobStatus("j"); js.PeakBytes != 330*miB || js.State != policy.JobDone {
		t.Fatalf("done job: peak_bytes %d MiB state %s, want 330", js.PeakBytes/miB, js.State)
	}
}

// Round 3: pace status warnings say what follows now: after the throttle,
// the freeze at the next step time (not the WARN's stale next=throttle
// in=0ms); after the freeze, the kill.
func TestActiveWarningFollowsLadder(t *testing.T) {
	d := newStepDaemon(t, platform.NewFake(), fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return policy.State{}, nil }), nil, nil)
	warn := proto.Event{V: 1, TS: 1000, Verb: proto.VerbWarn, Job: "j", Policy: proto.PolicyPressure, Next: "throttle", InMS: proto.I64(4000)}
	d.lastWarn[warnKey("j", "", proto.PolicyPressure)] = warn
	st := cgJobState(policy.StageThrottled, 0)
	st.Jobs[0].Enf.Warnings = []policy.Warning{{Policy: proto.PolicyPressure, IssuedMS: 1000, DeadlineMS: 5000}}
	st.Jobs[0].Enf.NextStepMS = 9000
	ws := d.activeWarnings(6000, st)
	if len(ws) != 1 || ws[0].Next != "freeze" || ws[0].InMS == nil || *ws[0].InMS != 3000 {
		t.Fatalf("warnings %+v, want next=freeze in=3s", ws)
	}
	st.Jobs[0].Enf.Stage = policy.StageFrozen
	st.Jobs[0].Enf.NextStepMS = 11000
	ws = d.activeWarnings(10000, st)
	if len(ws) != 1 || ws[0].Next != "kill" || *ws[0].InMS != 1000 {
		t.Fatalf("warnings %+v, want next=kill in=1s", ws)
	}
	// Before the deadline the WARN's own next and in hold.
	st.Jobs[0].Enf.Stage = policy.StageNone
	ws = d.activeWarnings(2000, st)
	if len(ws) != 1 || ws[0].Next != "throttle" || *ws[0].InMS != 3000 {
		t.Fatalf("warnings %+v, want next=throttle in=3s", ws)
	}
}

// Round 3: pace run lifts its container's throttle and cap when it loses its
// stream; a re-attach to the same (live) daemon makes the daemon write the
// throttle and cap its state wants again, not trust what it last wrote.
func TestReattachReappliesContainerThrottleAndCap(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		return cgJobState(policy.StageThrottled, 400*miB), nil
	})
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState(policy.StageThrottled, 400*miB)
	setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	d.runStep(true)
	if g, _ := cont.Get("j"); !g.Throttled || g.MemMax != 440*miB {
		t.Fatalf("before: %+v", g)
	}
	// thawLost
	_, _ = cont.SetThrottle("j", false)
	_ = cont.SetMemMax("j", 0)
	d.enqueue(policy.InEvent{Kind: policy.EvAttach, Job: "j", PID: 5200, PGID: 5200, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	d.runStep(true)
	if g, _ := cont.Get("j"); !g.Throttled || g.MemMax != 440*miB {
		t.Fatalf("after the re-attach: %+v, want throttled and capped at 440M again", g)
	}
}

// Round 3: an ended job's container that cannot be removed (a sub-cgroup it
// left behind) never blocks the loop: the daemon makes single attempts
// (RemoveNow, never the waiting Remove), backed off, not one every step.
func TestEndedContainerRemovalBacksOff(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{Busy: true})
	now := int64(10_000)
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) { return policy.State{}, nil })
	d := newStepDaemon(t, f, fn, &now, cgConfig)
	d.restoreContainers() // a container of a job this daemon does not know
	if d.cg["j"] == nil || !d.cg["j"].ended {
		t.Fatalf("container not found: %+v", d.cg)
	}
	for range 5 {
		d.runStep(true)
		now += 100
	}
	if cont.Removes != 0 {
		t.Fatalf("the loop called the waiting Remove %d times", cont.Removes)
	}
	if cont.RemoveNows != 1 {
		t.Fatalf("%d removal attempts in 500ms, want 1 (backed off)", cont.RemoveNows)
	}
	now += 1000
	d.runStep(true)
	if cont.RemoveNows != 2 {
		t.Fatalf("%d removal attempts after the delay, want 2", cont.RemoveNows)
	}
	g, _ := cont.Get("j")
	g.Busy = false
	cont.Set("j", g)
	now += 60_000
	d.runStep(true)
	if _, ok := cont.Get("j"); ok || d.cg["j"] != nil {
		t.Fatal("container not removed once it could be")
	}
}

// Round 3: a THROTTLED line of a job whose container caps it with cpu.max
// does not call the throttle weak; one of a unit (no container) still does.
func TestThrottledCaveatHonoursContainerCPUMax(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
	steps := 0
	fn := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		ns := cgJobState(policy.StageThrottled, 0)
		ns.LastMS = in.NowMS
		var out policy.Output
		if steps++; steps == 2 { // the container is adopted in step 1
			out.Events = []proto.Event{
				{Verb: proto.VerbThrottled, Job: "j", Policy: proto.PolicyPressure},
				{Verb: proto.VerbThrottled, Unit: "pg:77", Policy: proto.PolicyPressure},
			}
		}
		return ns, out
	}
	d := newStepDaemon(t, caveatBackend{f}, fn, nil, cgConfig)
	d.contWeights = "cpu.weight=1, cpu.max=25% of one CPU, io.weight=1"
	d.st = cgJobState("", 0)
	setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	d.runStep(true)
	got := map[string]string{}
	for _, e := range d.feed.last(100) {
		if e.Verb == proto.VerbThrottled {
			got[e.Job+e.Unit] = e.Msg
		}
	}
	if m, ok := got["j"]; !ok || m != "" {
		t.Errorf("container job capped by cpu.max: THROTTLED msg %q (present %v), want none", m, ok)
	}
	if m := got["pg:77"]; m != "throttle on this machine is weak" {
		t.Errorf("unit THROTTLED msg %q", m)
	}
}
