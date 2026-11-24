package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// Regression tests for the second review of the daemon fixes.

// Finding 13: a sampling outage holds the pressure ladder in the policy
// itself, so no freeze or kill is recorded that never happened, and the
// kill after the outage starts with SIGTERM (then grace, SIGSTOP, SIGKILL).
func TestStaleOutageHoldsStageTransitions(t *testing.T) {
	be := &splitBackend{Fake: platform.NewFake()}
	setProcs(be.Fake, proc(800, 1, 800, 5000, 3*giB))
	now := int64(100_000)
	d := newStepDaemon(t, be, nil, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.CapacityBytes = 0 // the fake machine's AvailBytes decides
	})
	d.runStep(true) // fresh, normal pressure
	if lvl := d.st.Pressure.Level; lvl != proto.LevelNormal {
		t.Fatalf("level %q", lvl)
	}
	be.set(nil, errors.New("process table kept growing"))
	be.Fake.Lock()
	be.Fake.Mach.AvailBytes = 1500 * miB // warn level
	be.Fake.Unlock()
	for i := 0; i < 40; i++ {
		now += 1000
		d.runStep(true)
		for _, u := range d.st.Units {
			if u.Enf.Stage != policy.StageNone || len(u.Enf.Warnings) > 0 {
				t.Fatalf("tick %d of the outage: unit %s escalated to %q %+v", i, u.Key, u.Enf.Stage, u.Enf.Warnings)
			}
		}
	}
	if sent := be.Fake.Signals(); len(sent) != 0 {
		t.Fatalf("signal during the outage: %s", fmtSent(sent))
	}
	if n := d.faults.count("actuate_held"); n != 0 {
		t.Fatalf("policy still decided %d destructive action(s) on stale samples", n)
	}

	be.set(nil, nil) // sampling recovers; pressure persists
	for i := 0; i < 60 && !hasSignal(be.Fake.Signals(), 0, 800, syscall.SIGTERM); i++ {
		now += 1000
		d.runStep(true)
	}
	var order []syscall.Signal
	for _, s := range be.Fake.Signals() {
		if s.PGID == 800 || s.PID == 800 {
			order = append(order, s.Sig)
		}
	}
	idx := func(sig syscall.Signal) int {
		for i, s := range order {
			if s == sig {
				return i
			}
		}
		return -1
	}
	// The fake ends a process on SIGTERM, so SIGKILL may never be needed.
	stop, term, kill := idx(syscall.SIGSTOP), idx(syscall.SIGTERM), idx(syscall.SIGKILL)
	if stop < 0 || term < 0 || stop > term || (kill >= 0 && kill < term) {
		t.Fatalf("after recovery want freeze, then SIGTERM (then SIGKILL): %s", fmtSent(be.Fake.Signals()))
	}
}

// Finding 13 (second review): a target the state calls frozen is frozen,
// including members found after the freeze and a target restored from an
// older state.json without records.
func TestFrozenTargetMembersStopped(t *testing.T) {
	f := platform.NewFake()
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		return freezeState(policy.StageFrozen), nil
	})
	d := newStepDaemon(t, f, fn, nil, nil)
	d.st = freezeState(policy.StageFrozen) // restored, no held records
	setProcs(f, proc(5200, 1, 5200, 1000, 10*miB), proc(5201, 5200, 5200, 1100, 10*miB))
	d.runStep(true)
	if !hasSignal(f.Signals(), 5200, 0, syscall.SIGSTOP) || !hasSignal(f.Signals(), 5201, 0, syscall.SIGSTOP) {
		t.Fatalf("frozen target left running: %s", fmtSent(f.Signals()))
	}
	n := len(f.Signals())
	d.runStep(true)
	if len(f.Signals()) != n {
		t.Fatalf("already stopped members signalled again: %s", fmtSent(f.Signals()[n:]))
	}
	f.Lock()
	f.Procs = append(f.Procs, proc(5202, 5200, 5200, 1200, 10*miB))
	f.Unlock()
	d.runStep(true)
	if !hasSignal(f.Signals(), 5202, 0, syscall.SIGSTOP) {
		t.Fatalf("member found after the freeze not stopped: %s", fmtSent(f.Signals()))
	}
}

// Finding 14: what the actuator stopped is persisted, so a daemon that
// replaces a crashed one thaws those processes even when the frozen job
// leaves the state in its first step and the escapee is not re-attributed,
// and even when its first process listing fails.
func TestRestartThawsPersistedStops(t *testing.T) {
	for _, firstListFails := range []bool{false, true} {
		name := "fresh"
		if firstListFails {
			name = "first list fails"
		}
		t.Run(name, func(t *testing.T) {
			f := platform.NewFake()
			fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
				if len(f.Signals()) == 0 {
					return freezeState(policy.StageFrozen), []policy.Action{
						{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)},
						{Op: policy.OpSignal, Job: "j", PIDs: []int{5300}, Signal: int(syscall.SIGSTOP)},
					}
				}
				return freezeState(policy.StageFrozen), nil
			})
			d1 := newStepDaemon(t, f, fn, nil, nil)
			d1.st = freezeState(policy.StageNone)
			// Leader, and a child that left its group and dropped PACE_JOB_ID
			// (attributed through its parent).
			setProcs(f, proc(5200, 1, 5200, 1000, 10*miB), proc(5300, 5200, 5300, 1100, 10*miB))
			d1.runStep(true)
			if !hasSignal(f.Signals(), 5300, 0, syscall.SIGSTOP) {
				t.Fatalf("freeze: %s", fmtSent(f.Signals()))
			}
			// d1 crashes: no release. state.json holds the records.
			b, err := os.ReadFile(d1.statePath)
			if err != nil {
				t.Fatal(err)
			}
			var sf stateFile
			if err := json.Unmarshal(b, &sf); err != nil {
				t.Fatal(err)
			}
			if h := sf.Held["j:j"]; len(h.Stopped) == 0 {
				t.Fatalf("stopped processes not persisted: %.300s", b)
			}

			// The leader died and the wrapper reported the exit while the
			// daemon was down: the job leaves the state in the new daemon's
			// first step. The escapee, re-parented, is no member for a fresh
			// tracker.
			setProcs(f, platform.Proc{PID: 5300, PPID: 1, PGID: 5300, UID: os.Getuid(), StartMS: 1100, State: 'T'})
			be := &splitBackend{Fake: f}
			if firstListFails {
				be.set(nil, errors.New("process table kept growing"))
			}
			home := d1.home
			d2 := newStepDaemon(t, be, fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return policy.State{}, nil }), nil, func(c *config.Resolved) {
				c.Home, c.LogDir = home, home+"/logs"
			})
			d2.loadState()
			n := len(f.Signals())
			d2.runStep(true)
			if firstListFails {
				if len(f.Signals()) != n {
					t.Fatalf("signalled without a process list: %s", fmtSent(f.Signals()[n:]))
				}
				if h := d2.held["j:j"]; h == nil || len(h.stopped) == 0 {
					t.Fatal("records dropped while the process list was failing")
				}
				be.set(nil, nil)
				d2.runStep(true)
			}
			if !hasSignal(f.Signals()[n:], 5300, 0, syscall.SIGCONT) {
				t.Fatalf("escapee left stopped after the restart: %s", fmtSent(f.Signals()[n:]))
			}
			if len(d2.held) != 0 {
				t.Fatalf("records kept after the release: %v", d2.held)
			}
		})
	}
}

// Finding 33: a state.json of an older version holds the full argv in
// status.argv; it is bounded on restore.
func TestRestoreBoundsOldStatusArgv(t *testing.T) {
	big := strings.Repeat("x", 100<<10)
	argv := []string{"cc", "-c", big}
	g := newRegistry()
	g.restore([]persistedJob{{
		ID: "a", Submit: proto.SubmitRequest{Argv: argv},
		Status: proto.JobStatus{ID: "a", State: policy.JobDone, Argv: argv}, Terminal: true, EndMS: 1000,
	}}, nil)
	js, ok := g.jobStatus("a")
	if !ok {
		t.Fatal("job missing")
	}
	n := 0
	for _, a := range js.Argv {
		n += len(a)
	}
	if n > maxStoredArgvBytes+64 {
		t.Fatalf("restored status argv keeps %d bytes", n)
	}
}

// Finding 33 (attach): the policy gets the full argv, so a key derived from
// it (an unknown re-attached job) is not the truncation marker.
func TestAttachGivesPolicyFullArgv(t *testing.T) {
	argv := []string{"cc"}
	for n := 0; n < 2*maxStoredArgvBytes; n += 48 {
		argv = append(argv, "-I/very/long/include/path/that/fills/the/argv/x")
	}
	argv = append(argv, "file.c")
	if k := policy.DeriveKey(boundSubmit(proto.SubmitRequest{Argv: argv}).Argv); !strings.Contains(k, "truncated") {
		t.Fatalf("test argv not long enough: bounded key %q", k)
	}
	h := newHarness(t, hopt{fn: miniPolicy})
	a := h.openStream("/v1/jobs/jabc-8/attach", proto.AttachRequest{Submit: proto.SubmitRequest{Argv: argv, WrapperPID: 31}, PID: 900, PGID: 900})
	defer a.close()
	a.expect(proto.VerbAccepted)
	var att *policy.InEvent
	eventually(t, "attach event", func() bool {
		for _, e := range h.rec.events() {
			if e.Kind == policy.EvAttach {
				att = &e
				return true
			}
		}
		return false
	})
	if att.Submit == nil || len(att.Submit.Argv) != len(argv) {
		t.Fatalf("policy got a bounded argv (%d args)", len(att.Submit.Argv))
	}
	if k := policy.DeriveKey(att.Submit.Argv); k != "cc file.c" {
		t.Fatalf("key %q", k)
	}
}
