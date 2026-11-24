package daemon

import (
	"reflect"
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

// With scope_root_pid set, a unit's group signal must not reach group
// members outside the scope (e.g. the test shell that started a bypasser in
// its own group): the group is split and only in-scope members are hit.
func TestGroupSignalSkipsOutOfScopeMembers(t *testing.T) {
	d, f := newTestDaemon(t)
	procs := []platform.Proc{{PID: 900, PGID: 900}, {PID: 901, PGID: 900}, {PID: 902, PGID: 900}}
	unit := policy.UnitSample{Key: "pg:900:1", PGID: 900, PIDs: []int{901, 902}}
	d.setAttribution(attrib.Result{InScope: map[int]bool{901: true, 902: true}, Units: []policy.UnitSample{unit}}, procs)
	d.execute([]policy.Action{{Op: policy.OpSignalGroup, Unit: "pg:900:1", PGID: 900, Signal: int(syscall.SIGTERM)}}, 1)
	want := []platform.SentSignal{{PID: 901, Sig: syscall.SIGTERM}, {PID: 902, Sig: syscall.SIGTERM}}
	if got := f.Signals(); !reflect.DeepEqual(got, want) {
		t.Fatalf("sent %s, want %s", fmtSent(got), fmtSent(want))
	}
	// A job whose members are the whole group: killpg.
	d2, f2 := newTestDaemon(t)
	d2.setAttribution(attrib.Result{InScope: map[int]bool{901: true, 902: true}, JobPIDs: map[string][]int{"j": {900, 901, 902}}}, procs)
	d2.execute([]policy.Action{{Op: policy.OpSignalGroup, Job: "j", PGID: 900, Signal: int(syscall.SIGTERM)}}, 1)
	if got := f2.Signals(); len(got) != 1 || got[0].PGID != 900 {
		t.Fatalf("job group signal %s", fmtSent(got))
	}
	// No scope, the unit is the whole group: killpg.
	d3, f3 := newTestDaemon(t)
	d3.setAttribution(attrib.Result{Units: []policy.UnitSample{{Key: "pg:900:1", PGID: 900, PIDs: []int{900, 901, 902}}}}, procs)
	d3.execute([]policy.Action{{Op: policy.OpSignalGroup, Unit: "pg:900:1", PGID: 900, Signal: int(syscall.SIGTERM)}}, 1)
	if got := f3.Signals(); len(got) != 1 || got[0].PGID != 900 {
		t.Fatalf("unscoped group signal %s", fmtSent(got))
	}
}

// Found by scripts/accept/pressure.sh: a job's first sample after its
// start must already be charged to the job.
func TestJobsWithPendingStarts(t *testing.T) {
	jobs := []policy.Job{
		{ID: "a", State: policy.JobRunning},
		{ID: "b", State: policy.JobRunning, PID: 7, PGID: 7},
		{ID: "c", State: policy.JobQueued},
	}
	pending := []policy.InEvent{
		{Kind: policy.EvProgress, Job: "a"},
		{Kind: policy.EvStarted, Job: "a", PID: 100, PGID: 100},
		{Kind: policy.EvStarted, Job: "b", PID: 200, PGID: 200}, // already started: kept
		{Kind: policy.EvStarted, Job: "c", PID: 300, PGID: 300}, // not admitted: ignored
	}
	got := jobsWithPendingStarts(jobs, pending)
	if got[0].PID != 100 || got[0].PGID != 100 || got[1].PID != 7 || got[2].PID != 0 {
		t.Fatalf("got %+v", got)
	}
	if jobs[0].PID != 0 {
		t.Fatal("input mutated")
	}
	if g := jobsWithPendingStarts(jobs, nil); &g[0] != &jobs[0] {
		t.Fatal("no pending starts: want the same slice")
	}
}
