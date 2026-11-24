package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// Regression tests for the review findings fixed in the daemon, attrib and
// dlog packages (one or more per finding, named after what they guard).

const (
	miB = int64(1) << 20
	giB = int64(1) << 30
)

// realParams are complete policy parameters for tests that run policy.Step.
func realParams() policy.Params {
	return policy.Params{
		Mode: policy.ModeEnforce, CapacityBytes: 16 * giB, ReserveBytes: 2 * giB,
		WarnAvailPermille: 150, CritAvailPermille: 70, PSIWarn10: 100, PSICrit10: 50,
		BudgetWarnPermille: 900, TolerancePermille: 100, WarnDeadlineMS: 20_000,
		GraceMS: 5_000, CriticalGraceMS: 1_000, StepMS: 2_000, CriticalStepMS: 500,
		RecoverHoldMS: 5_000, OrphanGraceMS: 10_000, MinVictimBytes: 64 * miB,
		UnmanagedPriority: 2, DefaultQueueTimeoutMS: 30 * 60_000,
		CPUWarnPerCoreMilli: 1_500, CPUOKPerCoreMilli: 1_000,
		DiskWarnFreeBytes: 10 * giB, DiskCritFreeBytes: 3 * giB, MarginPermille: 100,
		HistorySize: 20, HistoryMinSamples: 3, DefaultClass: "default",
		Classes: []policy.ClassParams{{Name: "default", EstimateBytes: 1 * giB, BudgetBytes: 2 * giB, Priority: 5}},
	}
}

// newStepDaemon is a daemon that is driven by calling runStep directly (no
// server, no loop goroutine), with a fake backend and an injected clock.
func newStepDaemon(t *testing.T, be platform.Backend, fn StepFunc, now *int64, cfg func(*config.Resolved)) *daemon {
	t.Helper()
	c := baseConfig(shortDir(t))
	if cfg != nil {
		cfg(&c)
	}
	o := Options{Config: c, Backend: be, UID: os.Getuid(), Log: &memLog{}, NoSignals: true, NoSelfTune: true, Step: fn}
	if now != nil {
		o.Now = func() int64 { return *now }
	}
	d, err := newDaemon(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	d.alerts = newAlertWriter(d.alertsPath, alertMaxBytes, d.fault)
	d.persist = newPersister(d.statePath, d.fault)
	t.Cleanup(func() { d.persist.close(); d.alerts.close() })
	return d
}

func proc(pid, ppid, pgid int, start int64, fp int64) platform.Proc {
	return platform.Proc{PID: pid, PPID: ppid, PGID: pgid, UID: os.Getuid(), StartMS: start, Comm: "p", Footprint: fp, State: 'S'}
}

func setProcs(f *platform.Fake, ps ...platform.Proc) {
	f.Lock()
	f.Procs = ps
	f.Unlock()
}

// fixedState is a step function that installs a state and emits actions
// produced by act (called with the step's inputs).
func fixedState(st func(in policy.Inputs) (policy.State, []policy.Action)) StepFunc {
	return func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		ns, acts := st(in)
		ns.LastMS = in.NowMS
		return ns, policy.Output{Actions: acts}
	}
}

func hasSignal(sent []platform.SentSignal, pid, pgid int, sig syscall.Signal) bool {
	for _, s := range sent {
		if s.PID == pid && s.PGID == pgid && s.Sig == sig {
			return true
		}
	}
	return false
}

// ---- finding 2: virtual (observe / warn-only) kills are not real ----------

func TestVirtualEnforcementNotReportedAsReal(t *testing.T) {
	var js proto.JobStatus
	j := policy.Job{ID: "j", State: policy.JobRunning, Enf: policy.Enforcement{Stage: policy.StageKilled, KillPolicy: proto.PolicyMemBudget, Virtual: true}}
	fillJobStatus(&js, &j, nil)
	if js.Stage != "" || js.KilledBy != "" {
		t.Fatalf("virtual kill shows stage=%q killed_by=%q", js.Stage, js.KilledBy)
	}
	j.Enf.Virtual = false
	fillJobStatus(&js, &j, nil)
	if js.Stage != policy.StageKilled || js.KilledBy != proto.PolicyMemBudget {
		t.Fatalf("real kill shows stage=%q killed_by=%q", js.Stage, js.KilledBy)
	}
	s := policy.State{Units: []policy.Unit{{Key: "pg:9:1", Enf: policy.Enforcement{Stage: policy.StageThrottled, Virtual: true}}}}
	in := policy.Inputs{Units: []policy.UnitSample{{Key: "pg:9:1", PGID: 9}}}
	if us := topUnits(policy.Params{}, s, in); len(us) != 1 || us[0].Stage != "" {
		t.Fatalf("virtual unit stage: %+v", us)
	}
}

// ---- finding 4: public unit id is pg:<pgid> -----------------------------

func TestUnitRefInStatusAndWarnings(t *testing.T) {
	s := policy.State{Units: []policy.Unit{{Key: "pg:42:1700000000000", PGID: 42,
		Enf: policy.Enforcement{Warnings: []policy.Warning{{Policy: proto.PolicyMemBudget, IssuedMS: 1}}}}}}
	in := policy.Inputs{Units: []policy.UnitSample{{Key: "pg:42:1700000000000", PGID: 42}}}
	if us := topUnits(policy.Params{}, s, in); len(us) != 1 || us[0].Key != "pg:42" {
		t.Fatalf("unmanaged key: %+v", us)
	}
	d, _ := newTestDaemon(t)
	if ws := d.activeWarnings(5, s); len(ws) != 1 || ws[0].Unit != "pg:42" {
		t.Fatalf("warning unit: %+v", ws)
	}
}

// ---- finding 7: disk WARN liveness ------------------------------------------

func TestDiskWarnListedWhileDiskLow(t *testing.T) {
	d := newStepDaemon(t, platform.NewFake(), nil, nil, nil)
	disk := proto.Event{V: 1, Verb: proto.VerbWarn, Policy: proto.PolicyDisk, Level: proto.LevelCritical, Next: "refuse"}
	d.fanout([]proto.Event{disk}, proto.LevelNormal)
	// Disk low, memory normal: listed.
	s := policy.State{Pressure: policy.Pressure{Level: proto.LevelNormal, DiskLow: proto.LevelCritical}}
	if ws := d.activeWarnings(1, s); len(ws) != 1 || ws[0].Policy != proto.PolicyDisk {
		t.Fatalf("disk warning while disk low: %+v", ws)
	}
	// Disk recovered (no OK seen), memory pressure up: not listed.
	s = policy.State{Pressure: policy.Pressure{Level: proto.LevelWarn}}
	if ws := d.activeWarnings(2, s); len(ws) != 0 {
		t.Fatalf("stale disk warning during memory pressure: %+v", ws)
	}
	// The matching OK drops it at once.
	d.fanout([]proto.Event{disk}, proto.LevelNormal)
	d.fanout([]proto.Event{{V: 1, Verb: proto.VerbOK, Policy: proto.PolicyDisk}}, proto.LevelNormal)
	s = policy.State{Pressure: policy.Pressure{Level: proto.LevelNormal, DiskLow: proto.LevelCritical}}
	if ws := d.activeWarnings(3, s); len(ws) != 0 {
		t.Fatalf("warning after OK: %+v", ws)
	}
}

// ---- finding 11: restart must not adopt a reused leader pid -----------------

func TestRestartDoesNotAdoptReusedLeaderGroup(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	d := newStepDaemon(t, f, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.OrphanGraceMS = 200
	})
	// state.json from the previous daemon: job j1 led by pid 500 started at 1000.
	sf := stateFile{V: stateFileV, State: policy.State{NextSeq: 2, Jobs: []policy.Job{{
		ID: "j1", Seq: 1, Class: "default", State: policy.JobRunning, PID: 500, PGID: 500,
		StartMS: 1000, SubmitMS: 900, AdmitMS: 900, LastProgressMS: 900,
	}}}}
	b, _ := json.Marshal(sf)
	if err := os.WriteFile(d.statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d.loadState()
	// pid 500 now leads an unrelated group (a login item started later).
	a := proc(500, 1, 500, now-60_000, 50*miB)
	a.System, a.Comm = true, "loginitem"
	c := proc(501, 500, 500, now-59_000, 50*miB)
	c.System = true
	setProcs(f, a, c)
	for i := 0; i < 10; i++ {
		now += 100
		d.runStep(true)
	}
	if sent := f.Signals(); len(sent) != 0 {
		t.Fatalf("signalled an unrelated group after restart: %s", fmtSent(sent))
	}
	if len(d.st.Jobs) != 0 {
		t.Fatalf("job with no members left and no wrapper not dropped: %+v", d.st.Jobs)
	}
}

// ---- finding 16: a job's reused pgid is never signalled ----------------------

func TestReusedJobGroupNotSignalled(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	d := newStepDaemon(t, f, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.OrphanGraceMS = 1000
	})
	d.st = policy.State{NextSeq: 2, Jobs: []policy.Job{{
		ID: "j1", Seq: 1, Class: "default", State: policy.JobRunning, PID: 500, PGID: 500,
		StartMS: now - 5000, SubmitMS: now - 6000, AdmitMS: now - 6000, LastProgressMS: now, WrapperLostMS: now,
	}}}
	// Tick 1: the leader and an escaped member (own group).
	setProcs(f, proc(500, 1, 500, now-5000, 100*miB), proc(600, 500, 600, now-4000, 100*miB))
	d.runStep(true)
	// Tick 2: the leader exited; pid 500 was reused by an unrelated group
	// leader; the escaped member is still there. The orphan kill is due.
	innocent := proc(500, 1, 500, now+1000, 10*miB)
	innocent.Comm = "innocent"
	setProcs(f, innocent, proc(600, 1, 600, now-4000, 100*miB))
	now += 2000
	d.runStep(true)
	sent := f.Signals()
	if !hasSignal(sent, 600, 0, syscall.SIGTERM) {
		t.Fatalf("escaped member not signalled: %s", fmtSent(sent))
	}
	for _, s := range sent {
		if s.PGID == 500 || s.PID == 500 {
			t.Fatalf("signal reached the reused group 500: %s", fmtSent(sent))
		}
	}
}

// ---- findings 12 / 30: unit group signals stay within the unit ---------------

func TestUnitGroupSignalSparesOutOfScopeAndJobMembers(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	d := newStepDaemon(t, f, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.UnmanagedBudgetBytes = 1 * giB
		c.ScopeRootPID = 100
	})
	// The agent (90, out of scope) leads pgid 90; the scope root shell 100
	// inherited it. The hog 101 leads group 101, which an out-of-scope
	// helper of the agent (91) joined. (A hog that shares the root's group
	// is no unit at all: TestUnitNeverRootDaemonWrapperGroup.)
	setProcs(f, proc(90, 1, 90, 1000, 50*miB), proc(100, 90, 90, 2000, 5*miB),
		proc(91, 90, 101, 2500, 5*miB), proc(101, 100, 101, 3000, 2*giB))
	d.runStep(true)
	sent := f.Signals()
	if len(sent) != 1 || sent[0] != (platform.SentSignal{PID: 101, Sig: syscall.SIGTERM}) {
		t.Fatalf("sent %s, want only pid 101", fmtSent(sent))
	}

	// A unit's group that also holds a governed job's member (it joined the
	// group with setpgid): the unit's kill must not reach the job.
	now = 20_000_000
	f2 := platform.NewFake()
	d2 := newStepDaemon(t, f2, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.UnmanagedBudgetBytes = 1 * giB
	})
	d2.st = policy.State{NextSeq: 2, Jobs: []policy.Job{{
		ID: "j", Seq: 1, Class: "default", State: policy.JobRunning, PID: 300, PGID: 300,
		StartMS: 5000, SubmitMS: 4000, AdmitMS: 4000, LastProgressMS: now,
	}}}
	setProcs(f2, proc(300, 1, 300, 5000, 10*miB), proc(302, 300, 400, 6000, 10*miB), // job member in group 400
		proc(400, 1, 400, 5500, 2*giB)) // unmanaged hog leading group 400
	d2.runStep(true)
	sent = f2.Signals()
	if !hasSignal(sent, 400, 0, syscall.SIGTERM) {
		t.Fatalf("hog not signalled: %s", fmtSent(sent))
	}
	for _, s := range sent {
		if s.PGID == 400 || s.PID == 302 {
			t.Fatalf("unit kill reached the job member: %s", fmtSent(sent))
		}
	}
}

// ---- findings 13 / 31: no destructive action on a failed sample ----------

func TestStaleSampleHoldsDestructiveActions(t *testing.T) {
	for _, which := range []string{"procs", "machine"} {
		t.Run(which, func(t *testing.T) {
			be := &splitBackend{Fake: platform.NewFake()}
			setProcs(be.Fake, proc(800, 1, 800, 5000, 3*giB))
			fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
				st := policy.State{Units: []policy.Unit{{Key: "pg:800:5000", PGID: 800, LeaderPID: 800, StartMS: 5000, Enf: policy.Enforcement{Stage: policy.StageKilled}}}}
				return st, []policy.Action{
					{Op: policy.OpSignalGroup, Unit: "pg:800:5000", PGID: 800, Signal: int(syscall.SIGKILL)},
					{Op: policy.OpSignal, Unit: "pg:800:5000", PIDs: []int{800}, Signal: int(syscall.SIGCONT)},
				}
			})
			d := newStepDaemon(t, be, fn, nil, nil)
			d.runStep(true) // fresh: executes
			if n := len(be.Fake.Signals()); n != 2 {
				t.Fatalf("fresh sample: sent %s", fmtSent(be.Fake.Signals()))
			}
			// The original process is gone; pid 800 now belongs to an
			// innocent process the (failing) sampler cannot see.
			innocent := proc(800, 1, 800, 99_999, 1*miB)
			setProcs(be.Fake, innocent)
			if which == "procs" {
				be.set(nil, errors.New("process table kept growing"))
			} else {
				be.set(errors.New("sysctl broke"), nil)
			}
			before := len(be.Fake.Signals())
			for i := 0; i < 4; i++ {
				d.runStep(true)
			}
			for _, s := range be.Fake.Signals()[before:] {
				if s.Sig != syscall.SIGCONT {
					t.Fatalf("destructive signal on a stale sample: %s", fmtSent(be.Fake.Signals()[before:]))
				}
			}
			if d.faults.count("actuate_held") == 0 {
				t.Fatal("held actions not reported as a fault")
			}
		})
	}
}

func TestHealthzReportsFailingSampler(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.curIvMS.Store(1000)
	now := time.Now().UnixMilli()
	d.lastStep.Store(now)
	d.lastMachOK.Store(now)
	d.lastProcsOK.Store(now - 11_000)
	w := httptest.NewRecorder()
	d.hHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "process listing failing") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	d.lastProcsOK.Store(now)
	d.lastMachOK.Store(now - 11_000)
	w = httptest.NewRecorder()
	d.hHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "machine sampling failing") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

// ---- findings 15 / 20: throttle reaches every member and is restored -------

func throttleState(stage string, cpu bool) policy.State {
	return policy.State{NextSeq: 2, Jobs: []policy.Job{{
		ID: "j", Seq: 1, Class: "default", State: policy.JobRunning, PID: 600, PGID: 600, StartMS: 1000,
		Enf: policy.Enforcement{Stage: stage, CPUThrottle: cpu},
	}}}
}

func bgMap(f *platform.Fake) map[int]bool {
	f.Lock()
	defer f.Unlock()
	out := map[int]bool{}
	for k, v := range f.Background {
		out[k] = v
	}
	return out
}

func TestThrottleCoversWholeJobAndIsRestored(t *testing.T) {
	for _, end := range []string{"unthrottle", "job end", "shutdown"} {
		t.Run(end, func(t *testing.T) {
			f := platform.NewFake()
			phase := "throttle"
			fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
				switch phase {
				case "throttle":
					phase = "hold"
					return throttleState(policy.StageThrottled, false), []policy.Action{{Op: policy.OpBackground, Job: "j", PGID: 600, PIDs: []int{600}, Policy: proto.PolicyPressure}}
				case "hold":
					return throttleState(policy.StageThrottled, false), nil
				case "unthrottle":
					return throttleState(policy.StageNone, false), []policy.Action{{Op: policy.OpForeground, Job: "j", PGID: 600, PIDs: []int{600}, Policy: proto.PolicyPressure}}
				}
				return policy.State{}, nil // job end: gone from the state
			})
			d := newStepDaemon(t, f, fn, nil, nil)
			d.st = throttleState(policy.StageNone, false)
			// make (leader) and two compiler children in its group.
			setProcs(f, proc(600, 1, 600, 1000, 10*miB), proc(601, 600, 600, 1100, 10*miB), proc(602, 600, 600, 1200, 10*miB))
			d.runStep(true)
			if bg := bgMap(f); !bg[600] || !bg[601] || !bg[602] {
				t.Fatalf("throttle did not reach the group: %v", bg)
			}
			// Forked while throttled: a child in the group and one that left it.
			setProcs(f, proc(600, 1, 600, 1000, 10*miB), proc(601, 600, 600, 1100, 10*miB), proc(602, 600, 600, 1200, 10*miB),
				proc(603, 601, 600, 2000, 10*miB), proc(604, 602, 704, 2100, 10*miB))
			d.runStep(true)
			if bg := bgMap(f); !bg[603] || !bg[604] {
				t.Fatalf("children forked while throttled not throttled: %v", bg)
			}
			switch end {
			case "shutdown":
				d.stopRelease()
				if d.st.Jobs[0].Enf.Stage != policy.StageNone {
					t.Fatalf("stage kept after shutdown release: %q", d.st.Jobs[0].Enf.Stage)
				}
			default:
				phase = end
				d.runStep(true)
			}
			for pid, on := range bgMap(f) {
				if on {
					t.Fatalf("pid %d still backgrounded after %s: %v", pid, end, bgMap(f))
				}
			}
		})
	}
}

// ---- finding 14: freezes are released --------------------------------------

func freezeState(stage string) policy.State {
	return policy.State{NextSeq: 2, Jobs: []policy.Job{{
		ID: "j", Seq: 1, Class: "default", State: policy.JobRunning, PID: 5200, PGID: 5200, StartMS: 1000,
		Enf: policy.Enforcement{Stage: stage},
	}}}
}

func TestFreezeReleasedOnJobEndAndShutdown(t *testing.T) {
	for _, end := range []string{"job end", "shutdown"} {
		t.Run(end, func(t *testing.T) {
			f := platform.NewFake()
			frozen := true
			fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
				if !frozen {
					return policy.State{}, nil
				}
				if in.NowMS > 0 && len(f.Signals()) == 0 {
					return freezeState(policy.StageFrozen), []policy.Action{
						{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)},
						{Op: policy.OpSignal, Job: "j", PIDs: []int{5300}, Signal: int(syscall.SIGSTOP)},
					}
				}
				return freezeState(policy.StageFrozen), nil
			})
			d := newStepDaemon(t, f, fn, nil, nil)
			d.st = freezeState(policy.StageNone)
			// Leader and an escaped member in its own group.
			setProcs(f, proc(5200, 1, 5200, 1000, 10*miB), proc(5300, 5200, 5300, 1100, 10*miB))
			d.lastPersistAt = time.Now() // debounce: only a synchronous save can write now
			d.runStep(true)
			b, err := os.ReadFile(d.statePath)
			if err != nil || !strings.Contains(string(b), `"stage":"frozen"`) {
				t.Fatalf("state.json not saved before the freeze took effect: %v %.200s", err, b)
			}
			if !hasSignal(f.Signals(), 0, 5200, syscall.SIGSTOP) || !hasSignal(f.Signals(), 5300, 0, syscall.SIGSTOP) {
				t.Fatalf("freeze: %s", fmtSent(f.Signals()))
			}
			if end == "job end" {
				// The leader is killed from outside; the wrapper reports the
				// exit and the job leaves the state. The escaped member stays.
				setProcs(f, platform.Proc{PID: 5300, PPID: 1, PGID: 5300, UID: os.Getuid(), StartMS: 1100, State: 'T'})
				frozen = false
				d.runStep(true)
			} else {
				d.stopRelease()
				if d.st.Jobs[0].Enf.Stage != policy.StageNone {
					t.Fatalf("stage kept after shutdown release: %q", d.st.Jobs[0].Enf.Stage)
				}
				ml := d.log.(*memLog)
				if len(ml.kinds(dlog.KindStop)) != 1 {
					t.Fatal("release not logged")
				}
			}
			if !hasSignal(f.Signals(), 5300, 0, syscall.SIGCONT) {
				t.Fatalf("escaped member left stopped: %s", fmtSent(f.Signals()))
			}
			f.Lock()
			defer f.Unlock()
			for _, p := range f.Procs {
				if p.State == 'T' {
					t.Fatalf("pid %d still stopped", p.PID)
				}
			}
		})
	}
}

// The loop releases on a graceful stop (pace uninstall, logout).
func TestShutdownThawsThroughLoop(t *testing.T) {
	var frozen bool
	h := newHarness(t, hopt{fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		s = freezeState(policy.StageFrozen)
		s.LastMS = in.NowMS
		var out policy.Output
		if !frozen {
			frozen = true
			out.Actions = []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
		}
		return s, out
	}, noStart: true})
	setProcs(h.fake, proc(5200, 1, 5200, 1000, 10*miB))
	h.d.st = freezeState(policy.StageNone) // the job is known before the freeze
	h.start()
	eventually(t, "frozen", func() bool { return hasSignal(h.fake.Signals(), 0, 5200, syscall.SIGSTOP) })
	h.stop()
	if !hasSignal(h.fake.Signals(), 5200, 0, syscall.SIGCONT) && !hasSignal(h.fake.Signals(), 0, 5200, syscall.SIGCONT) {
		t.Fatalf("not thawed on shutdown: %s", fmtSent(h.fake.Signals()))
	}
	b, _ := os.ReadFile(filepath.Join(h.dir, "state.json"))
	if strings.Contains(string(b), `"stage":"frozen"`) {
		t.Fatal("state.json still says frozen after the release")
	}
}

// ---- finding 33: argv is bounded ------------------------------------------

func TestStoredArgvBounded(t *testing.T) {
	big := strings.Repeat("x", 100<<10)
	g := newRegistry()
	g.create("a", proto.SubmitRequest{Argv: []string{"cc", "-c", big, "tail"}}, proto.Event{Verb: proto.VerbAccepted})
	js, _ := g.jobStatus("a")
	n := 0
	for _, a := range js.Argv {
		n += len(a)
	}
	if n > maxStoredArgvBytes+64 || js.Argv[0] != "cc" || !strings.Contains(js.Argv[len(js.Argv)-1], "truncated") {
		t.Fatalf("argv kept %d bytes: %.80q", n, js.Argv)
	}
	for _, pj := range g.export() {
		if pj.Status.Argv != nil {
			t.Fatal("argv persisted twice (submit and status)")
		}
	}
	g2 := newRegistry()
	g2.restore(g.export(), nil)
	if js2, _ := g2.jobStatus("a"); len(js2.Argv) == 0 || js2.Argv[0] != "cc" {
		t.Fatalf("restored argv %q", js2.Argv)
	}

	// Retention is bounded in bytes too.
	g3 := newRegistry()
	arg := strings.Repeat("y", 3000)
	for i := 0; i < maxFinishedJobs; i++ {
		id := fmt.Sprintf("j%06d", i)
		g3.create(id, proto.SubmitRequest{Argv: []string{"x", arg}}, proto.Event{Verb: proto.VerbAccepted})
		g3.mu.Lock()
		rec := g3.jobs[id]
		for k := 0; k < jobRingSize; k++ {
			rec.events.add(proto.Event{Verb: proto.VerbInfo, Msg: "m"})
		}
		g3.terminateLocked(rec, int64(1000+i))
		delete(g3.live, id)
		g3.mu.Unlock()
	}
	g3.prune(5000)
	var total int64
	for _, rec := range g3.jobs {
		total += recBytes(rec)
	}
	if len(g3.jobs) >= maxFinishedJobs || total > maxFinishedBytes {
		t.Fatalf("kept %d finished jobs, ~%d bytes", len(g3.jobs), total)
	}
	if g3.jobs["j000999"] == nil {
		t.Fatal("newest finished job pruned")
	}
}

// ---- finding 34: log seq continues after the log, not only state.json ------

func TestLogSeqContinuesAfterCrash(t *testing.T) {
	dir := shortDir(t)
	logDir := filepath.Join(dir, "logs")
	w, err := dlog.Open(logDir, dlog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		w.Write(dlog.Record{Kind: dlog.KindHeartbeat, Seq: int64(i)})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// state.json lagged behind (debounced) when the daemon was SIGKILLed.
	b, _ := json.Marshal(stateFile{V: stateFileV, LogSeq: 6})
	if err := os.WriteFile(filepath.Join(dir, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, hopt{dir: dir, fn: miniPolicy, opts: func(o *Options) { o.Log = nil }})
	h.stop()
	var seqs []int64
	if err := dlog.ReadRecords(filepath.Join(logDir, dlog.FileName), func(r dlog.Record) error {
		seqs = append(seqs, r.Seq)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] <= seqs[i-1] {
			t.Fatalf("seq not strictly increasing: %v", seqs)
		}
	}
	if len(seqs) < 11 {
		t.Fatalf("no start record appended: %v", seqs)
	}
}

// ---- finding 35: a failed attach push leaves no live record ----------------

func TestAttachPushFailureUndone(t *testing.T) {
	be := &splitBackend{Fake: platform.NewFake()}
	be.set(errors.New("down"), nil) // the loop holds events: the queue stays full
	h := newHarness(t, hopt{fn: miniPolicy, backend: be, noStart: true})
	h.d.pushTimeout = 50 * time.Millisecond
	h.d.evCh = make(chan policy.InEvent, 1)
	h.d.evCh <- policy.InEvent{Kind: policy.EvLost, Job: "dummy"}
	h.start()
	sub := submitReq("make")
	sub.WrapperPID = 31337
	resp, body := h.post("/v1/jobs/jv1-1/attach", proto.AttachRequest{Submit: sub, PID: 40000, PGID: 40000})
	mustCode(t, resp, body, http.StatusServiceUnavailable)
	if p := h.d.reg.protectedPIDs(nil); len(p) != 0 {
		t.Fatalf("dead wrapper still protected: %v", p)
	}
	if h.d.reg.known("jv1-1") {
		t.Fatal("unknown job registered by a failed attach")
	}
	be.set(nil, nil)
}

func TestAttachUndoRestoresKnownJob(t *testing.T) {
	g := newRegistry()
	g.restore([]persistedJob{{ID: "k", Submit: proto.SubmitRequest{Argv: []string{"x"}, WrapperPID: 31337}, Status: proto.JobStatus{ID: "k", State: policy.JobRunning}}}, map[string]bool{"k": true})
	st, _, undo := g.attach("k", proto.AttachRequest{Submit: proto.SubmitRequest{Argv: []string{"x"}, WrapperPID: 31337}}, proto.Event{Verb: proto.VerbAccepted})
	g.undoAttach(st, undo)
	// The policy reaped the orphan: the job leaves the state, the record
	// becomes terminal and its wrapper unprotected.
	empty := policy.State{}
	g.apply(stepResult{nowMS: 10, state: &empty})
	if p := g.protectedPIDs(nil); len(p) != 0 {
		t.Fatalf("protected %v", p)
	}
	if js, _ := g.jobStatus("k"); js.State != policy.JobDone {
		t.Fatalf("state %q", js.State)
	}
}

// ---- finding 36: one daemon per home --------------------------------------

func TestSecondDaemonSameHomeRefused(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	cfg := baseConfig(h.dir)
	other := shortDir(t)
	cfg.Socket = filepath.Join(other, "s.sock")
	d2, err := newDaemon(Options{Config: cfg, Backend: platform.NewFake(), Log: &memLog{}, NoSignals: true, NoSelfTune: true})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := d2.listen()
	if err == nil {
		ln.Close()
		d2.unlock()
		t.Fatal("second daemon bound with the same home")
	}
	if !strings.Contains(err.Error(), "another pace daemon is running") {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Stat(cfg.Socket); err == nil {
		t.Fatal("second daemon left a socket")
	}
}

// ---- finding 37: an existing socket dir is not chmodded ---------------------

func TestExistingSocketDirNotChmodded(t *testing.T) {
	base := shortDir(t)
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	d := &daemon{sockPath: filepath.Join(shared, "g.sock"), home: base, allowUID: os.Geteuid(), faults: newFaultSet()}
	ln, err := d.listen()
	if err != nil {
		t.Fatalf("listen in an existing 0755 dir: %v", err)
	}
	ln.Close()
	d.unlock()
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o755 {
		t.Fatalf("existing dir mode changed to %v", fi.Mode().Perm())
	}
	// Group-writable: refused, not changed.
	if err := os.Chmod(shared, 0o775); err != nil {
		t.Fatal(err)
	}
	d2 := &daemon{sockPath: filepath.Join(shared, "g2.sock"), home: base, allowUID: os.Geteuid(), faults: newFaultSet()}
	if ln, err := d2.listen(); err == nil {
		ln.Close()
		d2.unlock()
		t.Fatal("bound in a group-writable dir")
	} else if !strings.Contains(err.Error(), "writable by group or others") {
		t.Fatalf("err %v", err)
	}
	if fi, _ := os.Stat(shared); fi.Mode().Perm() != 0o775 {
		t.Fatalf("dir mode changed to %v", fi.Mode().Perm())
	}
}

// ---- finding 40: PACE_JOB_ID tags are read --------------------------------

type tagBackend struct {
	*platform.Fake
	tags map[int]string
}

func (b *tagBackend) JobTag(pid int) string { return b.tags[pid] }

func TestDaemonWiresTagReader(t *testing.T) {
	be := &tagBackend{Fake: platform.NewFake(), tags: map[int]string{500: "j"}}
	d := newStepDaemon(t, be, nil, nil, nil)
	const t0 = int64(1_700_000_000_000)
	d.st = policy.State{Jobs: []policy.Job{{ID: "j", State: policy.JobRunning, PID: 100, PGID: 100, StartMS: t0}}}
	// The escapee was re-parented to a subreaper (1234, systemd --user,
	// running since login).
	setProcs(be.Fake, proc(1234, 1, 1234, t0-3_600_000, 5*miB), proc(100, 1, 100, t0, 10*miB), proc(500, 1234, 500, t0+2000, 900*miB))
	d.sample()
	if js := d.lastAttr.Jobs; len(js) != 1 || js[0].NProcs != 2 || len(js[0].ExtraPIDs) != 1 || js[0].ExtraPIDs[0] != 500 {
		t.Fatalf("job sample %+v, want the tagged escapee 500 counted", js)
	}
}
