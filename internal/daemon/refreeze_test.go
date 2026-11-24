package daemon

import (
	"strings"
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

func refrozeLines(d *daemon) []proto.Event {
	var out []proto.Event
	for _, e := range d.feed.last(1000) {
		if e.Verb == proto.VerbFrozen && e.Msg == MsgRefrozen {
			out = append(out, e)
		}
	}
	return out
}

func withState(p platform.Proc, st byte) platform.Proc {
	p.State = st
	return p
}

// A frozen job that someone else continued (pace run thawing it when its
// daemon was lost; a kill -CONT) is stopped again by the daemon, which says
// so with a FROZEN line: a target the state calls frozen is frozen. A stop
// the daemon sent that has not taken effect yet (uninterruptible sleep) is
// not mistaken for one.
func TestContinuedFrozenJobStoppedAgain(t *testing.T) {
	f := platform.NewFake()
	fn := fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		if len(f.Signals()) == 0 {
			return freezeState(policy.StageFrozen), []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
		}
		return freezeState(policy.StageFrozen), nil
	})
	d := newStepDaemon(t, f, fn, nil, nil)
	d.st = freezeState(policy.StageNone)
	leader, child := proc(5200, 1, 5200, 1000, 10*miB), proc(5201, 5200, 5200, 1100, 10*miB)
	setProcs(f, leader, child)
	d.runStep(true)
	if !hasSignal(f.Signals(), 0, 5200, syscall.SIGSTOP) {
		t.Fatalf("freeze: %s", fmtSent(f.Signals()))
	}

	// The stop is still pending (uninterruptible sleep): not continued.
	setProcs(f, withState(leader, 'D'), withState(child, 'T'))
	n := len(f.Signals())
	d.runStep(true)
	if sent := f.Signals()[n:]; len(sent) != 0 || len(refrozeLines(d)) != 0 {
		t.Fatalf("a pending stop was taken for a continue: %s", fmtSent(sent))
	}
	setProcs(f, withState(leader, 'T'), withState(child, 'T'))
	d.runStep(true)

	// pace run lost its daemon stream and continued the job.
	setProcs(f, leader, child)
	n = len(f.Signals())
	d.runStep(true)
	sent := f.Signals()[n:]
	if !hasSignal(sent, 5200, 0, syscall.SIGSTOP) || !hasSignal(sent, 5201, 0, syscall.SIGSTOP) {
		t.Fatalf("continued frozen job not stopped again: %s", fmtSent(sent))
	}
	lines := refrozeLines(d)
	if len(lines) != 1 || lines[0].Job != "j" || lines[0].Policy != proto.PolicyPressure || lines[0].PGID != 5200 {
		t.Fatalf("FROZEN line: %+v", lines)
	}
	// Once is enough: it is stopped again and stays so.
	d.runStep(true)
	if len(refrozeLines(d)) != 1 {
		t.Fatalf("FROZEN repeated: %+v", refrozeLines(d))
	}
}

// The crash-restart case: a daemon restarted (launchd KeepAlive) on records
// whose processes pace run continued meanwhile stops them again, instead of
// keeping stage=frozen for a running job.
func TestRestartedDaemonStopsContinuedFrozenJob(t *testing.T) {
	f := platform.NewFake()
	d1 := newStepDaemon(t, f, fixedState(func(in policy.Inputs) (policy.State, []policy.Action) {
		if len(f.Signals()) == 0 {
			return freezeState(policy.StageFrozen), []policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}}
		}
		return freezeState(policy.StageFrozen), nil
	}), nil, nil)
	d1.st = freezeState(policy.StageNone)
	leader := proc(5200, 1, 5200, 1000, 10*miB)
	setProcs(f, leader)
	d1.runStep(true)
	d1.persistNow()

	setProcs(f, leader) // continued by pace run while no daemon ran
	home := d1.home
	d2 := newStepDaemon(t, f, fixedState(func(policy.Inputs) (policy.State, []policy.Action) { return freezeState(policy.StageFrozen), nil }), nil,
		func(c *config.Resolved) { c.Home, c.LogDir = home, home+"/logs" })
	d2.loadState()
	n := len(f.Signals())
	d2.runStep(true)
	if sent := f.Signals()[n:]; !hasSignal(sent, 5200, 0, syscall.SIGSTOP) {
		t.Fatalf("restarted daemon left the continued job running: %s", fmtSent(sent))
	}
	if l := refrozeLines(d2); len(l) != 1 || !strings.Contains(l[0].Msg, "continued") {
		t.Fatalf("FROZEN line: %+v", l)
	}
}
