package attrib

import (
	"reflect"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

// Finding 38: with a job container, a descendant that escaped every rule a
// process listing can follow (own session and group, parent exited so it is
// re-parented to pid 1, PACE_JOB_ID dropped, first seen only after all that)
// is still the job's, and the job's usage is the container's.
func TestContainerMembershipAndUsage(t *testing.T) {
	tr := New(Options{})
	procs := []platform.Proc{
		pr(99, 50, 50, 900, 5, 1),       // wrapper
		pr(100, 99, 100, 1000, 100, 10), // job leader
		pr(300, 1, 300, 1005, 400, 0),   // escaped: setsid, orphaned, no tag
		pr(400, 1, 400, 1006, 200, 0),   // unrelated bypasser
		pr(401, 400, 400, 1007, 100, 0), // its child
		pr(500, 1, 500, 1008, 50, 0),    // in another job's container
		pr(501, 99, 501, 1009, 10, 0),   // second job's leader
		pr(600, 300, 300, 1010, 100, 0), // child of the escaped one
		pr(700, 600, 700, 1011, 0, 0),   // zombie in the container
	}
	procs[8].State = 'Z'
	tr.SetProtectedPIDs([]int{99})
	tr.SetContainers(map[string]Container{
		"j1":   {PIDs: []int{100, 300, 600, 700}, UsedBytes: 480 * M, HaveUsage: true},
		"j2":   {PIDs: []int{501, 500}},
		"gone": {PIDs: []int{400}}, // not a running job: ignored
	})
	jobs := []policy.Job{job("j1", 100), job("j2", 501)}
	r := tr.Attribute(procs, jobs, nil)

	j1 := jobByID(t, r, "j1")
	if !reflect.DeepEqual(r.JobPIDs["j1"], []int{100, 300, 600}) {
		t.Errorf("j1 members %v, want [100 300 600]", r.JobPIDs["j1"])
	}
	if j1.UsedBytes != 480*M {
		t.Errorf("j1 used %d MiB, want the container's 480", j1.UsedBytes/M)
	}
	if !reflect.DeepEqual(j1.ExtraPIDs, []int{300, 600}) {
		t.Errorf("j1 extras %v (signalled directly)", j1.ExtraPIDs)
	}
	j2 := jobByID(t, r, "j2")
	if !reflect.DeepEqual(r.JobPIDs["j2"], []int{500, 501}) {
		t.Errorf("j2 members %v", r.JobPIDs["j2"])
	}
	// No container usage: the per-process sum.
	if j2.UsedBytes != 60*M {
		t.Errorf("j2 used %d MiB, want 60", j2.UsedBytes/M)
	}
	if _, ok := unitByPG(r, 300); ok {
		t.Error("the escaped process is an unmanaged unit")
	}
	if u, ok := unitByPG(r, 400); !ok || u.UsedBytes != 300*M {
		t.Errorf("bypasser unit %+v (a container of no running job must not claim it)", u)
	}
	// Scoped usage counts the container's figure, not the process sum.
	if want := 480*M + 60*M + 300*M; r.ScopedUsedBytes != want {
		t.Errorf("scoped %d MiB, want %d", r.ScopedUsedBytes/M, want/M)
	}

	// Without containers the escaped one is lost (the macOS situation).
	tr2 := New(Options{})
	tr2.SetProtectedPIDs([]int{99})
	r2 := tr2.Attribute(procs, jobs, nil)
	if _, ok := unitByPG(r2, 300); !ok {
		t.Errorf("control: escaped process attributed without a container: %v", r2.JobPIDs)
	}
}

// M3 in Docker: a test shell (or an agent) running as PID 1 without job
// control leaves everything it starts in process group 1. That group is not
// one unit (a pressure kill of it took the caller's own helpers with the
// bypasser): each subtree is, keyed by its root as if it led its own group.
func TestInitGroupSplitBySubtree(t *testing.T) {
	procs := []platform.Proc{
		pr(1, 0, 1, 100, 5, 0),     // PID 1: the shell (scope root)
		pr(20, 1, 1, 200, 4, 0),    // a helper subshell
		pr(21, 20, 1, 210, 1, 0),   // its child (pace status)
		pr(30, 1, 1, 300, 900, 0),  // the bypasser
		pr(31, 30, 1, 310, 300, 0), // its child
		pr(40, 1, 40, 400, 200, 0), // a real group (job control used)
		pr(41, 40, 1, 410, 100, 0), // re-joined group 1 under a real group's leader: own root
		pr(50, 1, 1, 500, 2, 0),    // protected wrapper
		pr(51, 50, 51, 510, 10, 0), // its job
		pr(60, 31, 1, 305, 7, 0),   // started before its parent (pid reuse): out of scope
		pr(70, 1, 1, 700, 0, 0),    // zombie
	}
	procs[10].State = 'Z'
	tr := New(Options{ScopeRootPID: 1})
	tr.SetProtectedPIDs([]int{50})
	r := tr.Attribute(procs, []policy.Job{job("j1", 51)}, nil)
	got := map[string][]int{}
	for _, u := range r.Units {
		got[u.Key] = u.PIDs
		if u.Key == UnitKey(30, 300) && (u.LeaderPID != 30 || u.PGID != 1 || u.UsedBytes != 1200*M || u.Comm != "p30") {
			t.Errorf("bypasser unit %+v", u)
		}
	}
	want := map[string][]int{
		UnitKey(20, 200): {20, 21},
		UnitKey(30, 300): {30, 31},
		UnitKey(40, 400): {40},
		UnitKey(41, 410): {41},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("units %v, want %v", got, want)
	}
	// Without a scope root, pid 1 is never a unit nor anyone's root.
	tr2 := New(Options{})
	r2 := tr2.Attribute([]platform.Proc{pr(1, 0, 1, 100, 50, 0), pr(30, 1, 1, 300, 900, 0)}, nil, nil)
	keys := unitKeys(r2)
	if !reflect.DeepEqual(keys, []string{UnitKey(30, 300)}) {
		t.Errorf("units without scope %v", keys)
	}
}

// The wrapper is never a member, even if it were listed in the container.
func TestContainerNeverTakesProtected(t *testing.T) {
	tr := New(Options{})
	tr.SetProtectedPIDs([]int{99})
	tr.SetContainers(map[string]Container{"j1": {PIDs: []int{99, 100}}})
	r := tr.Attribute([]platform.Proc{pr(99, 50, 50, 900, 5, 1), pr(100, 99, 100, 1000, 10, 0)},
		[]policy.Job{job("j1", 100)}, nil)
	if !reflect.DeepEqual(r.JobPIDs["j1"], []int{100}) {
		t.Errorf("members %v", r.JobPIDs["j1"])
	}
}

// Review: a nested pace run inside a container job that got no container of
// its own (attach failed, PACE_LINUX_CGROUP=0 in the job, another daemon)
// keeps its processes by rule 0, as without containers: they are not
// charged to the outer job, whose usage is then its members' sum (the
// container's figure counts the inner job too). A nested job WITH a
// container of its own is not affected (its processes are in it).
func TestContainerNestedJobWithoutContainerKeepsRule0(t *testing.T) {
	tr := New(Options{})
	procs := []platform.Proc{
		pr(100, 50, 100, 1000, 100, 10), // outer leader
		pr(150, 100, 100, 1001, 5, 0),   // inner wrapper (protected)
		pr(200, 150, 200, 1002, 300, 0), // inner job's leader, still in the outer cgroup
		pr(201, 200, 200, 1003, 50, 0),  // its child
		pr(300, 100, 100, 1004, 20, 0),  // outer's own child
	}
	tr.SetProtectedPIDs([]int{150})
	tr.SetContainers(map[string]Container{
		"outer": {PIDs: []int{100, 150, 200, 201, 300}, UsedBytes: 475 * M, HaveUsage: true},
	})
	r := tr.Attribute(procs, []policy.Job{job("outer", 100), job("inner", 200)}, nil)
	if !reflect.DeepEqual(r.JobPIDs["inner"], []int{200, 201}) {
		t.Fatalf("inner members %v, want [200 201] (rule 0)", r.JobPIDs["inner"])
	}
	if !reflect.DeepEqual(r.JobPIDs["outer"], []int{100, 300}) {
		t.Fatalf("outer members %v", r.JobPIDs["outer"])
	}
	in, out := jobByID(t, r, "inner"), jobByID(t, r, "outer")
	if !in.Alive || in.UsedBytes != 350*M {
		t.Errorf("inner %+v: want alive, 350M", in)
	}
	if out.UsedBytes != 120*M {
		t.Errorf("outer used %d MiB, want its members' 120 (not the container's 475)", out.UsedBytes/M)
	}

	// The inner job has a container of its own: a process left in the outer
	// one with the inner pgid stays the outer's (rule C), usage exact.
	tr2 := New(Options{})
	tr2.SetProtectedPIDs([]int{150})
	tr2.SetContainers(map[string]Container{
		"outer": {PIDs: []int{100, 150, 300, 201}, UsedBytes: 175 * M, HaveUsage: true},
		"inner": {PIDs: []int{200}, UsedBytes: 300 * M, HaveUsage: true},
	})
	r = tr2.Attribute(procs, []policy.Job{job("outer", 100), job("inner", 200)}, nil)
	if !reflect.DeepEqual(r.JobPIDs["inner"], []int{200}) || !reflect.DeepEqual(r.JobPIDs["outer"], []int{100, 201, 300}) {
		t.Fatalf("members inner %v outer %v", r.JobPIDs["inner"], r.JobPIDs["outer"])
	}
	if o := jobByID(t, r, "outer"); o.UsedBytes != 175*M {
		t.Errorf("outer used %d MiB, want the container's 175", o.UsedBytes/M)
	}
}
