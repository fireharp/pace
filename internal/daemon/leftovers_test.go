package daemon

import (
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// caveatBackend is a Fake whose throttle is weak (as Linux SCHED_BATCH).
type caveatBackend struct{ *platform.Fake }

func (caveatBackend) ThrottleNote(bool) string { return "long note" }
func (caveatBackend) ThrottleCaveat(cpuMax bool) string {
	if cpuMax {
		return ""
	}
	return "throttle on this machine is weak"
}

// Finding 42 (reporting): a THROTTLED line says when the throttle is weak
// or one-way on this platform (msg=), while the decision log keeps the
// policy's own event; a strong throttle (the plain Fake, macOS) adds none.
func TestThrottledCarriesPlatformCaveat(t *testing.T) {
	for _, weak := range []bool{true, false} {
		var be platform.Backend = platform.NewFake()
		if weak {
			be = caveatBackend{platform.NewFake()}
		}
		emitted := false
		fn := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
			ns := freezeState(policy.StageThrottled)
			ns.LastMS = in.NowMS
			var out policy.Output
			if !emitted {
				emitted = true
				out.Events = []proto.Event{
					{Verb: proto.VerbThrottled, Job: "j", Policy: proto.PolicyPressure},
					{Verb: proto.VerbInfo, Job: "j", Msg: "would throttle"},
				}
			}
			return ns, out
		}
		d := newStepDaemon(t, be, fn, nil, nil)
		d.st = freezeState(policy.StageNone)
		d.runStep(true)
		evs := d.feed.last(2)
		if len(evs) != 2 || evs[0].Verb != proto.VerbThrottled {
			t.Fatalf("feed %+v", evs)
		}
		want := ""
		if weak {
			want = "throttle on this machine is weak"
		}
		if evs[0].Msg != want {
			t.Errorf("weak=%v: THROTTLED msg %q, want %q", weak, evs[0].Msg, want)
		}
		if evs[1].Msg != "would throttle" {
			t.Errorf("INFO msg changed: %q", evs[1].Msg)
		}
		recs := d.log.(*memLog).kinds("step")
		if len(recs) == 0 || recs[0].Out.Events[0].Msg != "" {
			t.Errorf("decision log altered: %+v", recs)
		}
	}
}

func frozenUnitState(key string, pgid int, start int64) policy.State {
	return policy.State{Units: []policy.Unit{{Key: key, PGID: pgid, LeaderPID: pgid, StartMS: start,
		Enf: policy.Enforcement{Stage: policy.StageFrozen}}}}
}

func stopped(pid, ppid, pgid int, start int64) platform.Proc {
	p := proc(pid, ppid, pgid, start, 100*miB)
	p.State = 'T'
	return p
}

// Finding 44 (remainder): a frozen unit that leaves the state (attribution
// no longer reports it, or reports its group under a new identity) is
// thawed: SIGCONT to its recorded members that are still the same process,
// never to a pid that now belongs to another process.
func TestFrozenUnitDroppedIsThawed(t *testing.T) {
	for _, reused := range []bool{false, true} {
		name := "same processes"
		if reused {
			name = "member pid reused"
		}
		t.Run(name, func(t *testing.T) {
			f := platform.NewFake()
			const key = "pg:800:5000"
			keep := true
			fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
				if !keep {
					return policy.State{}, nil // dropped (e.g. new identity after a clock step)
				}
				if len(f.Signals()) == 0 {
					return frozenUnitState(key, 800, 5000), []policy.Action{
						{Op: policy.OpSignalGroup, Unit: key, PGID: 800, Signal: int(syscall.SIGSTOP)},
					}
				}
				return frozenUnitState(key, 800, 5000), nil
			})
			d := newStepDaemon(t, f, fn, nil, nil)
			setProcs(f, proc(800, 1, 800, 5000, 1*giB), proc(801, 800, 800, 5100, 1*giB))
			d.runStep(true)
			if !hasSignal(f.Signals(), 0, 800, syscall.SIGSTOP) {
				t.Fatalf("freeze: %s", fmtSent(f.Signals()))
			}
			if reused {
				// 801 exited; its pid now belongs to an unrelated process.
				setProcs(f, stopped(800, 1, 800, 5000), proc(801, 1, 801, 9000, 1*miB))
			}
			keep = false
			n := len(f.Signals())
			d.runStep(true)
			sent := f.Signals()[n:]
			if !hasSignal(sent, 800, 0, syscall.SIGCONT) {
				t.Fatalf("dropped frozen unit not thawed: %s", fmtSent(sent))
			}
			if got := hasSignal(sent, 801, 0, syscall.SIGCONT); got == reused {
				t.Fatalf("SIGCONT to 801 = %v (reused=%v): %s", got, reused, fmtSent(sent))
			}
			for _, s := range sent {
				if s.PGID != 0 || s.Sig != syscall.SIGCONT {
					t.Fatalf("unexpected signal on release: %s", fmtSent(sent))
				}
			}
			if len(d.held) != 0 {
				t.Fatalf("records kept: %v", d.held)
			}
		})
	}
}

// Finding 44 (remainder): a daemon restarted on a state.json that says a
// unit (or job) is frozen but has no records of what was stopped (written
// before the records existed) thaws that group when the target leaves the
// state, if its leader is still the same process; a reused leader pid is
// never signalled.
func TestRestoredFrozenWithoutRecordsThawed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		st     policy.State
		procs  []platform.Proc
		thawed []int
	}{
		{"unit", frozenUnitState("pg:900:7000", 900, 7000),
			[]platform.Proc{stopped(900, 1, 900, 7000), stopped(901, 900, 900, 7100), proc(950, 1, 950, 7200, miB)},
			[]int{900, 901}},
		{"unit leader reused", frozenUnitState("pg:900:7000", 900, 7000),
			[]platform.Proc{stopped(900, 1, 900, 8000), stopped(901, 900, 900, 8100)},
			nil},
		{"job, wrapper clock 1s off", freezeAt(5200, 1000),
			[]platform.Proc{stopped(5200, 1, 5200, 2000), stopped(5201, 5200, 5200, 2100)},
			[]int{5200, 5201}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := platform.NewFake()
			d1 := newStepDaemon(t, f, nil, nil, nil)
			d1.st = tc.st
			d1.persistNow() // no held records: an older daemon's state.json

			setProcs(f, tc.procs...)
			home := d1.home
			d2 := newStepDaemon(t, f, fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return policy.State{}, nil }), nil,
				func(c *config.Resolved) { c.Home, c.LogDir = home, home+"/logs" })
			d2.loadState()
			d2.runStep(true)
			sent := f.Signals()
			for _, pid := range tc.thawed {
				if !hasSignal(sent, pid, 0, syscall.SIGCONT) {
					t.Errorf("pid %d not thawed: %s", pid, fmtSent(sent))
				}
			}
			if len(tc.thawed) == 0 && len(sent) != 0 {
				t.Errorf("reused leader's group signalled: %s", fmtSent(sent))
			}
			if hasSignal(sent, 950, 0, syscall.SIGCONT) {
				t.Errorf("process outside the group thawed: %s", fmtSent(sent))
			}
			if len(d2.held) != 0 {
				t.Errorf("records kept: %v", d2.held)
			}
		})
	}
}

func freezeAt(pid int, startMS int64) policy.State {
	s := freezeState(policy.StageFrozen)
	s.Jobs[0].PID, s.Jobs[0].PGID, s.Jobs[0].StartMS = pid, pid, startMS
	return s
}

// Safety (e) through the daemon and the real policy: with scope_root_pid
// set, a hog that shares the scope root's group is no unit, so the
// unmanaged budget never reaches it (or the root); a hog in a group of its
// own is killed. Its memory still counts in scoped usage.
func TestScopeRootGroupNeverAUnit(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	d := newStepDaemon(t, f, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.UnmanagedBudgetBytes = 1 * giB
		c.ScopeRootPID = 100
	})
	setProcs(f, proc(90, 1, 90, 1000, 50*miB), proc(100, 90, 90, 2000, 5*miB),
		proc(101, 100, 90, 3000, 2*giB),  // shares the root's (and its caller's) group
		proc(102, 100, 102, 3100, 2*giB)) // its own group
	d.runStep(true)
	sent := f.Signals()
	if len(sent) != 1 || !hasSignal(sent, 0, 102, syscall.SIGTERM) {
		t.Fatalf("sent %s, want only group 102 terminated", fmtSent(sent))
	}
	if got := d.lastAttr.ScopedUsedBytes; got != 4*giB {
		t.Errorf("scoped usage %d MiB, want 4096", got/miB)
	}
}

// Safety (e), production (no scope): the user's terminal shell is never
// signalled for a unit; a heavy command running in the shell's own group
// (no job control) is still governed, member by member.
func TestTerminalShellSpared(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	d := newStepDaemon(t, f, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.UnmanagedBudgetBytes = 1 * giB
	})
	shell := proc(500, 1, 500, 1000, 8*miB)
	shell.TermLeader = true
	setProcs(f, shell, proc(501, 500, 500, 2000, 2*giB))
	d.runStep(true)
	sent := f.Signals()
	if !hasSignal(sent, 501, 0, syscall.SIGTERM) {
		t.Fatalf("heavy member not signalled: %s", fmtSent(sent))
	}
	for _, s := range sent {
		if s.PID == 500 || s.PGID == 500 {
			t.Fatalf("signal reached the terminal shell or its group: %s", fmtSent(sent))
		}
	}
}
