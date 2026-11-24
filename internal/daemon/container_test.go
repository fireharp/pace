package daemon

import (
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// Finding 38: the job container is wired in. These drive the daemon with the
// fake backend's containers.

func cgJobState(stage string, budget int64) policy.State {
	return policy.State{NextSeq: 2, Jobs: []policy.Job{{
		ID: "j", Seq: 1, Class: "default", State: policy.JobRunning, PID: 5200, PGID: 5200, StartMS: 1000,
		BudgetBytes: budget, Enf: policy.Enforcement{Stage: stage},
	}}}
}

func cgConfig(c *config.Resolved) {
	c.HardCap = true
	c.Params.TolerancePermille = 100
}

// capture is a fixedState step that also keeps every step's job samples.
type capture struct {
	mu   sync.Mutex
	jobs [][]policy.JobSample
}

func (c *capture) last() []policy.JobSample {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.jobs) == 0 {
		return nil
	}
	return c.jobs[len(c.jobs)-1]
}

func (c *capture) step(st func(in policy.Inputs) (policy.State, []policy.Action)) StepFunc {
	return fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		c.mu.Lock()
		c.jobs = append(c.jobs, append([]policy.JobSample(nil), in.Jobs...))
		c.mu.Unlock()
		return st(in)
	})
}

func TestContainerAdoptAccountCapRemove(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200, 5300}, Usage: 300 * miB})
	ended := false
	var cap capture
	fn := cap.step(func(in policy.Inputs) (policy.State, []policy.Action) {
		if ended {
			return policy.State{}, nil
		}
		return cgJobState("", 400*miB), nil
	})
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState("", 400*miB)
	setProcs(f,
		proc(5200, 4242, 5200, 1000, 100*miB), // leader
		proc(5300, 1, 5300, 1100, 50*miB),     // escaped: own group, orphaned, no tag
		proc(5400, 1, 5400, 1200, 70*miB),     // a bypasser, not in the container
	)
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	if d.cg["j"] == nil {
		t.Fatalf("container not adopted (faults %v)", d.faults.active(time.Now().UnixMilli()))
	}
	d.runStep(true) // the first sample may predate the adoption
	js := cap.last()
	if len(js) != 1 || js[0].UsedBytes != 300*miB || len(js[0].ExtraPIDs) != 1 || js[0].ExtraPIDs[0] != 5300 {
		t.Fatalf("job sample %+v: want the container's 300M and the escaped pid 5300 as an extra", js)
	}
	if g, _ := cont.Get("j"); g.MemMax != 440*miB {
		t.Errorf("memory.max %d MiB, want budget+10%% = 440", g.MemMax/miB)
	}
	// No hard cap outside enforce mode: observe must never kill.
	d.cfg.Params.Mode = policy.ModeObserve
	d.runStep(true)
	if g, _ := cont.Get("j"); g.MemMax != 0 {
		t.Errorf("observe mode keeps memory.max %d", g.MemMax)
	}
	d.cfg.Params.Mode = policy.ModeEnforce
	d.runStep(true)
	// The job ends with a descendant still inside: released, kept.
	ended = true
	d.runStep(true)
	g, ok := cont.Get("j")
	if !ok || g.MemMax != 0 {
		t.Fatalf("ended job's container: ok=%v %+v (cap must be lifted, container kept while populated)", ok, g)
	}
	cont.Set("j", platform.FakeCgroup{MemMax: 0})
	d.runStep(true)
	if _, ok := cont.Get("j"); ok {
		t.Fatal("empty container of an ended job not removed")
	}
	if len(d.cg) != 0 {
		t.Fatalf("records left: %v", d.cg)
	}
}

func TestContainerFreezeThawKill(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200, 5300}, Usage: 100 * miB})
	stage := policy.StageNone
	var acts []policy.Action
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		a := acts
		acts = nil
		return cgJobState(stage, 0), a
	})
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState("", 0)
	setProcs(f,
		proc(5200, 4242, 5200, 1000, 10*miB),
		proc(5300, 1, 5300, 1100, 10*miB),    // escaped, inside the container
		proc(5500, 5200, 5500, 1200, 10*miB), // a member that left the container
	)
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	d.runStep(true)

	stage = policy.StageFrozen
	acts = []policy.Action{
		{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)},
		{Op: policy.OpSignal, Job: "j", PIDs: []int{5300, 5500}, Signal: int(syscall.SIGSTOP)},
	}
	d.runStep(true)
	d.runStep(true) // reconcile: nothing inside is ever SIGSTOPped
	if g, _ := cont.Get("j"); !g.Frozen {
		t.Fatal("freeze did not use cgroup.freeze")
	}
	for _, s := range f.Signals() {
		if s.Sig == syscall.SIGSTOP && (s.PGID == 5200 || s.PID == 5200 || s.PID == 5300) {
			t.Fatalf("SIGSTOP reached the container: %s", fmtSent(f.Signals()))
		}
	}
	if !hasSignal(f.Signals(), 5500, 0, syscall.SIGSTOP) {
		t.Fatalf("member outside the container not stopped: %s", fmtSent(f.Signals()))
	}

	stage = policy.StageNone
	d.runStep(true)
	if g, _ := cont.Get("j"); g.Frozen {
		t.Fatal("not thawed when the state released the freeze")
	}
	if !hasSignal(f.Signals(), 5500, 0, syscall.SIGCONT) {
		t.Fatalf("outside member left stopped: %s", fmtSent(f.Signals()))
	}

	stage = policy.StageTerminating
	acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGKILL)}}
	d.runStep(true)
	if g, _ := cont.Get("j"); g.Kills != 1 {
		t.Fatalf("SIGKILL did not use cgroup.kill: %+v", g)
	}
}

func TestContainerThrottleRestartShutdown(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	// A previous daemon left the container frozen and throttled.
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}, Frozen: true, Throttled: true, MemMax: 1})
	stage := policy.StageNone
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) { return cgJobState(stage, 400*miB), nil })
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState("", 400*miB)
	setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
	d.restoreContainers() // found by job id
	if d.cg["j"] == nil {
		t.Fatal("restart did not find the container by job id")
	}
	d.runStep(true)
	g, _ := cont.Get("j")
	if g.Frozen || g.Throttled || g.MemMax != 440*miB {
		t.Fatalf("after restart %+v: want thawed, unthrottled, capped at 440M", g)
	}
	stage = policy.StageThrottled
	d.runStep(true)
	if g, _ := cont.Get("j"); !g.Throttled {
		t.Fatal("throttle did not lower the container weights")
	}
	if !f.Background[5200] {
		t.Fatal("per-thread throttle not applied as well")
	}
	d.stopRelease()
	if g, _ := cont.Get("j"); g.Throttled || g.MemMax != 0 {
		t.Fatalf("shutdown left %+v", g)
	}
}

// Containers of jobs a restarted daemon does not know are released and
// removed once empty; one whose wrapper re-attaches is governed again.
func TestContainerOrphansAtRestart(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("old", platform.FakeCgroup{})
	cont.Set("lost", platform.FakeCgroup{PIDs: []int{7000}, Frozen: true, MemMax: 5})
	attached := false
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		if !attached {
			return policy.State{}, nil
		}
		return policy.State{NextSeq: 2, Jobs: []policy.Job{{ID: "lost", Seq: 1, Class: "default", State: policy.JobRunning,
			PID: 7000, PGID: 7000, StartMS: 1000, BudgetBytes: 100 * miB}}}, nil
	})
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	setProcs(f, proc(7000, 1, 7000, 1000, 10*miB))
	d.restoreContainers()
	d.runStep(true)
	if _, ok := cont.Get("old"); ok {
		t.Error("empty orphan container not removed")
	}
	g, ok := cont.Get("lost")
	if !ok || g.Frozen || g.MemMax != 0 {
		t.Fatalf("populated orphan %+v (ok=%v): want kept, thawed, uncapped", g, ok)
	}
	attached = true
	d.enqueue(policy.InEvent{Kind: policy.EvAttach, Job: "lost", PID: 7000, PGID: 7000, Cgroup: "/cg/pace/job-lost"})
	d.runStep(true)
	if g, _ := cont.Get("lost"); g.MemMax != 110*miB {
		t.Fatalf("re-attached job's cap %d, want 110M", g.MemMax)
	}
}

// A path that is not this daemon's container for the job is not adopted
// (a different delegated tree, or a lie): the job stays governed by group.
func TestContainerForeignPathIgnored(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
	d := newStepDaemon(t, f, fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return cgJobState("", 0), nil }), nil, cgConfig)
	d.st = cgJobState("", 0)
	for _, p := range []string{"/other/pace/job-j", "/cg/pace/job-j/../job-j2", "/cg/pace/notajob", "/cg/pace/job-missing"} {
		d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, Cgroup: p})
	}
	if len(d.cg) != 0 {
		t.Fatalf("adopted %v", d.cg)
	}
	if !strings.Contains(strings.Join(d.faults.active(time.Now().UnixMilli()), "\n"), "container") {
		t.Errorf("no fault recorded: %v", d.faults.active(time.Now().UnixMilli()))
	}
}

// Status says how jobs are governed on Linux, both ways; nothing on a
// backend without containers.
func TestContainerNotes(t *testing.T) {
	f := platform.NewFake()
	f.EnableContainers("/cg/pace")
	d := newStepDaemon(t, f, nil, nil, cgConfig)
	if n := strings.Join(d.platformNotes(), "\n"); !strings.Contains(n, "cgroup v2 container") || !strings.Contains(n, "memory.max") {
		t.Errorf("notes %q", n)
	}
	d2 := newStepDaemon(t, reasonBackend{platform.NewFake()}, nil, nil, nil)
	if n := strings.Join(d2.platformNotes(), "\n"); !strings.Contains(n, "no cgroup v2 job containers (cgroupfs read-only)") {
		t.Errorf("fallback notes %q", n)
	}
	d3 := newStepDaemon(t, platform.NewFake(), nil, nil, nil)
	if n := d3.platformNotes(); len(n) != 0 {
		t.Errorf("container-less backend notes %v", n)
	}
}

// ADMITTED carries the hard cap so the wrapper creates the container with it
// (a fast allocator is past the line before the daemon adopts the
// container); none outside enforce mode, without containers or hard_cap.
func TestAdmittedCarriesHardCap(t *testing.T) {
	f := platform.NewFake()
	f.EnableContainers("/cg/pace")
	d := newStepDaemon(t, f, nil, nil, cgConfig)
	d.st = cgJobState("", 400*miB)
	get := func() *int64 {
		evs := []proto.Event{{Verb: proto.VerbAdmitted, Job: "j"}, {Verb: proto.VerbWarn, Job: "j"}}
		d.decorateAdmitted(evs)
		if evs[1].MemMaxBytes != nil {
			t.Fatal("decorated a non-ADMITTED event")
		}
		return evs[0].MemMaxBytes
	}
	if c := get(); c == nil || *c != 440*miB {
		t.Fatalf("cap %v, want 440M", c)
	}
	d.cfg.Params.Mode = policy.ModeWarnOnly
	if c := get(); c != nil {
		t.Fatalf("warn-only cap %d", *c)
	}
	d.cfg.Params.Mode = policy.ModeEnforce
	d.cfg.HardCap = false
	if c := get(); c != nil {
		t.Fatalf("hard_cap=false cap %d", *c)
	}
	d2 := newStepDaemon(t, platform.NewFake(), nil, nil, cgConfig)
	d2.st = cgJobState("", 400*miB)
	evs := []proto.Event{{Verb: proto.VerbAdmitted, Job: "j"}}
	d2.decorateAdmitted(evs)
	if evs[0].MemMaxBytes != nil {
		t.Fatal("cap without containers")
	}
	// Never printed: the stderr grammar is unchanged.
	if l := (&proto.Event{Verb: proto.VerbAdmitted, Job: "j", MemMaxBytes: proto.I64(1)}).Line(); strings.Contains(l, "max") {
		t.Fatalf("line %q", l)
	}
}

type reasonBackend struct{ *platform.Fake }

func (reasonBackend) ContainersReason() string { return "cgroupfs read-only" }
