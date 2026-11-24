package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// Third review of the Linux job containers.

// scopedBackend gives each scope its own containers, as the Linux backend
// does (<root>/<scope>/job-<id>), over one shared fake process table.
type scopedBackend struct {
	*platform.Fake
	byScope map[string]*platform.FakeContainers
}

func (b scopedBackend) Containers(scope string) (platform.JobContainer, bool) {
	c := b.byScope[scope]
	if c == nil {
		c = &platform.FakeContainers{Base: "/cg/pace/" + scope, CG: map[string]*platform.FakeCgroup{}, Weights: "cpu.weight=1", Scope: scope}
		b.byScope[scope] = c
	}
	return c, true
}

// A second daemon of the same user (a test daemon with its own PACE_HOME and
// PACE_SOCK beside the installed one) has a container scope of its own: its
// orphan sweep at start never lists, releases or removes the first
// daemon's containers, whose cap and throttle stay in place.
func TestContainerSecondDaemonLeavesFirstAlone(t *testing.T) {
	be := scopedBackend{platform.NewFake(), map[string]*platform.FakeContainers{}}
	stage := policy.StageThrottled
	fnA := fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return cgJobState(stage, 400*miB), nil })
	a := newStepDaemon(t, be, fnA, nil, cgConfig)
	b := newStepDaemon(t, be, fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return policy.State{}, nil }), nil, cgConfig)
	if a.sockPath == b.sockPath {
		t.Fatal("test daemons share a socket")
	}
	ca, cb := a.cont.(*platform.FakeContainers), b.cont.(*platform.FakeContainers)
	if ca == cb || ca.Scope == cb.Scope || ca.Scope != platform.ContainerScope(a.sockPath) {
		t.Fatalf("scopes %q / %q: want one per socket", ca.Scope, cb.Scope)
	}
	if platform.ContainerScope(a.sockPath) != platform.ContainerScope(a.sockPath+"/") {
		t.Error("scope not stable for the same socket")
	}

	ca.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
	a.st = cgJobState(stage, 400*miB)
	setProcs(be.Fake, proc(5200, 4242, 5200, 1000, 10*miB))
	a.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/" + ca.Scope + "/job-j"})
	a.runStep(true)
	a.runStep(true)
	if g, _ := ca.Get("j"); g.MemMax != 440*miB || !g.Throttled {
		t.Fatalf("A governs %+v: want capped at 440M and throttled", g)
	}

	b.restoreContainers()
	b.runStep(true)
	b.runStep(true)
	b.stopRelease()
	b.closeContainers()
	a.runStep(true)
	if g, ok := ca.Get("j"); !ok || g.MemMax != 440*miB || !g.Throttled {
		t.Fatalf("after a second daemon ran: %+v (ok=%v): A's cap and throttle must stay", g, ok)
	}
	if len(b.cg) != 0 {
		t.Fatalf("B adopted %v", b.cg)
	}
}

// pace's own processes are never reached by a job's container: a wrapper of
// another governed job (a nested pace run, born in the outer job's
// container) and the daemon itself are moved out before cgroup.freeze and
// cgroup.kill, and every tick (memory.oom.group would take them too).
func TestContainerEvictsPaceProcesses(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	stage := policy.StageNone
	var acts []policy.Action
	fn := fixedState(func(policy.Inputs) (policy.State, []policy.Action) {
		a := acts
		acts = nil
		return cgJobState(stage, 0), a
	})
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState("", 0)
	const innerWrapper = 5250
	d.reg.create("inner", proto.SubmitRequest{Argv: []string{"x"}, WrapperPID: innerWrapper}, proto.Event{Verb: proto.VerbAccepted, Job: "inner"})
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200, innerWrapper, d.selfPID, 5300}, Usage: 50 * miB})
	setProcs(f,
		proc(5200, 4242, 5200, 1000, 10*miB),
		proc(innerWrapper, 5200, 5200, 1050, 5*miB), // the nested pace run, in the job's group
		proc(5300, 5200, 5200, 1100, 10*miB),
	)
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	d.runStep(true)
	g, _ := cont.Get("j")
	if len(g.PIDs) != 2 || g.PIDs[0] != 5200 || g.PIDs[1] != 5300 {
		t.Fatalf("container holds %v after a tick: pace processes not moved out (evicted %v)", g.PIDs, cont.Evicted)
	}

	// A wrapper that started inside since the tick's listing is moved out
	// right before the freeze and the kill.
	late := 5260
	d.reg.create("late", proto.SubmitRequest{Argv: []string{"x"}, WrapperPID: late}, proto.Event{Verb: proto.VerbAccepted, Job: "late"})
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200, 5300, late}})
	stage = policy.StageFrozen
	acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
	d.runStep(true)
	if g, _ := cont.Get("j"); !g.Frozen || len(g.PIDs) != 2 {
		t.Fatalf("freeze: %+v (evicted %v)", g, cont.Evicted)
	}
	for _, s := range f.Signals() {
		if s.PID == innerWrapper || s.PID == late || s.PGID == 5200 && s.Sig == syscall.SIGSTOP {
			t.Fatalf("signal reached a wrapper or the frozen group: %s", fmtSent(f.Signals()))
		}
	}
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200, 5300, innerWrapper}, Frozen: true})
	stage = policy.StageTerminating
	acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGKILL)}}
	d.runStep(true)
	g, _ = cont.Get("j")
	if g.Kills != 1 {
		t.Fatalf("SIGKILL did not use cgroup.kill: %+v", g)
	}
	if procs, _ := f.ListProcs(os.Getuid()); len(procs) != 1 || procs[0].PID != innerWrapper {
		t.Fatalf("after the kill %+v: only the nested wrapper may survive", procs)
	}
}

// evictFails is a container facility that cannot move processes.
type evictFails struct{ *platform.FakeContainers }

func (evictFails) Evict(int) error { return errors.New("permission denied") }

// A pace process that cannot be moved out of the container keeps the
// container's freeze and kill off it: the job is stopped and killed by
// signal, member by member, which never reaches a wrapper.
func TestContainerUnmovablePaceProcessFallsBackToSignals(t *testing.T) {
	be := scopedBackend{platform.NewFake(), map[string]*platform.FakeContainers{}}
	stage := policy.StageNone
	var acts []policy.Action
	fn := fixedState(func(policy.Inputs) (policy.State, []policy.Action) {
		a := acts
		acts = nil
		return cgJobState(stage, 0), a
	})
	d := newStepDaemon(t, be, fn, nil, cgConfig)
	fc := d.cont.(*platform.FakeContainers)
	fc.Set("j", platform.FakeCgroup{PIDs: []int{5200, 5250}})
	d.cont = evictFails{fc}
	d.st = cgJobState("", 0)
	d.reg.create("inner", proto.SubmitRequest{Argv: []string{"x"}, WrapperPID: 5250}, proto.Event{Verb: proto.VerbAccepted, Job: "inner"})
	setProcs(be.Fake, proc(5200, 4242, 5200, 1000, 10*miB), proc(5250, 5200, 5200, 1050, 5*miB))
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: fc.Base + "/job-j"})
	d.runStep(true)
	d.runStep(true)
	stage = policy.StageFrozen
	acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
	d.runStep(true)
	if g, _ := fc.Get("j"); g.Frozen {
		t.Fatal("container frozen with a wrapper inside")
	}
	if !hasSignal(be.Signals(), 5200, 0, syscall.SIGSTOP) {
		t.Fatalf("job not stopped by signal: %s", fmtSent(be.Signals()))
	}
	stage = policy.StageTerminating
	acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGKILL)}}
	d.runStep(true)
	if g, _ := fc.Get("j"); g.Kills != 0 {
		t.Fatal("cgroup.kill with a wrapper inside")
	}
	if !hasSignal(be.Signals(), 5200, 0, syscall.SIGKILL) {
		t.Fatalf("job not killed by signal: %s", fmtSent(be.Signals()))
	}
	for _, s := range be.Signals() {
		if s.PID == 5250 || s.PGID == 5200 {
			t.Fatalf("signal reached the wrapper: %s", fmtSent(be.Signals()))
		}
	}
	if !strings.Contains(strings.Join(d.faults.active(time.Now().UnixMilli()), "\n"), "pace process") {
		t.Errorf("no fault: %v", d.faults.active(time.Now().UnixMilli()))
	}
}

// peekBackend runs onSig before every signal it sends.
type peekBackend struct {
	*platform.Fake
	onSig func()
}

func (b peekBackend) Signal(pid int, sig syscall.Signal) error {
	b.onSig()
	return b.Fake.Signal(pid, sig)
}

func (b peekBackend) SignalGroup(pgid int, sig syscall.Signal) error {
	b.onSig()
	return b.Fake.SignalGroup(pgid, sig)
}

// When cgroup.freeze fails the freeze falls back to SIGSTOP: those stops are
// recorded in state.json before the signals, like every other stop, so a
// daemon that crashes right after still has them to continue.
func TestContainerFreezeFallbackStopsPersisted(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
	stage := policy.StageNone
	var acts []policy.Action
	fn := fixedState(func(policy.Inputs) (policy.State, []policy.Action) {
		a := acts
		acts = nil
		return cgJobState(stage, 0), a
	})
	var d *daemon
	persisted := map[int]bool{}
	be := peekBackend{f, func() {
		var sf stateFile
		if b, err := os.ReadFile(d.statePath); err == nil && json.Unmarshal(b, &sf) == nil {
			for _, p := range sf.Held["j:j"].Stopped {
				persisted[p.PID] = true
			}
		}
	}}
	d = newStepDaemon(t, be, fn, nil, cgConfig)
	d.st = cgJobState("", 0)
	setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	d.runStep(true)
	cont.FreezeErr = errors.New("cgroup.freeze: device busy")
	stage = policy.StageFrozen
	acts = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
	d.runStep(true)
	if !hasSignal(f.Signals(), 0, 5200, syscall.SIGSTOP) && !hasSignal(f.Signals(), 5200, 0, syscall.SIGSTOP) {
		t.Fatalf("no SIGSTOP fallback: %s", fmtSent(f.Signals()))
	}
	if !persisted[5200] {
		t.Fatal("fallback SIGSTOP sent before the stop was in state.json")
	}
	// Shutdown continues it.
	d.stopRelease()
	if !hasSignal(f.Signals(), 5200, 0, syscall.SIGCONT) && !hasSignal(f.Signals(), 0, 5200, syscall.SIGCONT) {
		t.Fatalf("not continued at shutdown: %s", fmtSent(f.Signals()))
	}
}

// ADMITTED's container fields are the job stream's alone: the stream
// carries them; the global feed, alerts, GET /v1/jobs/{id} events and
// state.json do not.
func TestAdmittedContainerFieldsStreamOnly(t *testing.T) {
	f := platform.NewFake()
	f.EnableContainers("/cg/pace")
	fn := fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return cgJobState("", 400*miB), nil })
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState("", 400*miB)
	s := d.reg.create("j", submitReq("make"), proto.Event{Verb: proto.VerbAccepted, Job: "j"})
	evs := []proto.Event{{V: 1, Verb: proto.VerbAdmitted, Job: "j"}}
	d.decorateAdmitted(evs)
	if evs[0].Cgroup != "/cg/pace/job-j" || evs[0].MemMaxBytes == nil {
		t.Fatalf("ADMITTED %+v: want the job's container path and cap", evs[0])
	}
	d.reg.apply(stepResult{nowMS: 1, state: &d.st, events: evs})
	d.fanout(evs, proto.LevelNormal)

	got, _ := d.reg.read(s, nil)
	if len(got) < 2 || got[1].Cgroup == "" || got[1].MemMaxBytes == nil {
		t.Fatalf("stream %+v: ADMITTED must carry the container fields", got)
	}
	for _, e := range d.feed.last(10) {
		if e.Cgroup != "" || e.MemMaxBytes != nil {
			t.Fatalf("feed event %+v carries stream-only fields", e)
		}
	}
	js, _ := d.reg.jobStatus("j")
	for _, e := range js.Events {
		if e.Cgroup != "" || e.MemMaxBytes != nil {
			t.Fatalf("job record event %+v carries stream-only fields", e)
		}
	}
	for _, pj := range d.reg.export() {
		for _, e := range pj.Events {
			if e.Cgroup != "" || e.MemMaxBytes != nil {
				t.Fatalf("persisted event %+v carries stream-only fields", e)
			}
		}
	}
	if s, _ := d.reg.read(s, nil); len(s) != 0 {
		t.Fatalf("unexpected events %+v", s)
	}
}

// pace status notes say which running jobs got no container (their pace run
// could not place the child: a root-owned session scope), instead of
// claiming every job runs in one.
func TestContainerNotesListJobsWithoutContainer(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	fn := fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return cgJobState("", 0), nil })
	d := newStepDaemon(t, f, fn, nil, cgConfig)
	d.st = cgJobState("", 0)
	setProcs(f, proc(5200, 4242, 5200, 1000, 10*miB))
	d.enqueue(policy.InEvent{Kind: policy.EvStarted, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000}) // no container
	d.runStep(true)
	n := strings.Join(d.platformNotes(), "\n")
	if !strings.Contains(n, "1 running job(s) without a container") || !strings.Contains(n, ": j") {
		t.Fatalf("notes %q", n)
	}
	cont.Set("j", platform.FakeCgroup{PIDs: []int{5200}})
	d.enqueue(policy.InEvent{Kind: policy.EvAttach, Job: "j", PID: 5200, PGID: 5200, StartMS: 1000, Cgroup: "/cg/pace/job-j"})
	d.runStep(true)
	if n := strings.Join(d.platformNotes(), "\n"); strings.Contains(n, "without a container") {
		t.Fatalf("notes %q after adoption", n)
	}
}

// The daemon removes its (empty) scope on shutdown.
func TestContainerScopeClosedAtShutdown(t *testing.T) {
	f := platform.NewFake()
	cont := f.EnableContainers("/cg/pace")
	h := newHarness(t, hopt{fn: miniPolicy, backend: f, cfg: func(c *config.Resolved) { cgConfig(c) }})
	h.stop()
	if cont.Closed != 1 {
		t.Fatalf("Close called %d times", cont.Closed)
	}
}
