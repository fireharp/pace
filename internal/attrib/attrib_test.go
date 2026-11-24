package attrib

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

const M = int64(1 << 20)

// pr builds a process: pid, ppid, pgid, start, footprint (MiB), cpu ms.
func pr(pid, ppid, pgid int, start int64, fpMiB, cpu int64) platform.Proc {
	return platform.Proc{PID: pid, PPID: ppid, PGID: pgid, UID: 501, StartMS: start,
		Comm: fmt.Sprintf("p%d", pid), Footprint: fpMiB * M, CPUMS: cpu, State: 'S'}
}

func job(id string, pgid int) policy.Job {
	return policy.Job{ID: id, State: policy.JobRunning, PID: pgid, PGID: pgid}
}

func jobByID(t *testing.T, r Result, id string) policy.JobSample {
	t.Helper()
	for _, j := range r.Jobs {
		if j.ID == id {
			return j
		}
	}
	t.Fatalf("job %s missing from %+v", id, r.Jobs)
	return policy.JobSample{}
}

func unitKeys(r Result) []string {
	var k []string
	for _, u := range r.Units {
		k = append(k, u.Key)
	}
	return k
}

func unitByPG(r Result, pg int) (policy.UnitSample, bool) {
	for _, u := range r.Units {
		if u.PGID == pg {
			return u, true
		}
	}
	return policy.UnitSample{}, false
}

func TestSimpleTree(t *testing.T) {
	tr := New(Options{})
	procs := []platform.Proc{
		pr(99, 50, 50, 900, 5, 1),        // wrapper (shell's group)
		pr(100, 99, 100, 1000, 100, 10),  // job leader
		pr(101, 100, 100, 1001, 200, 20), // child
		pr(102, 101, 100, 1002, 300, 30), // grandchild
		pr(200, 1, 200, 500, 400, 40),    // unrelated
	}
	r := tr.Attribute(procs, []policy.Job{job("j1", 100)}, nil)
	j := jobByID(t, r, "j1")
	want := policy.JobSample{ID: "j1", UsedBytes: 600 * M, NProcs: 3, CPUMS: 60, Alive: true}
	if !reflect.DeepEqual(j, want) {
		t.Errorf("job %+v, want %+v", j, want)
	}
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:200:500", "pg:50:900"}) {
		t.Errorf("units %v", unitKeys(r))
	}
	u, _ := unitByPG(r, 200)
	if u.UsedBytes != 400*M || u.NProcs != 1 || u.LeaderPID != 200 || u.Comm != "p200" || u.CPUMS != 40 || !reflect.DeepEqual(u.PIDs, []int{200}) {
		t.Errorf("unit %+v", u)
	}
	if r.NProcs != 5 || r.TotalUsedBytes != 1005*M || r.ScopedUsedBytes != 1005*M {
		t.Errorf("totals nprocs=%d total=%d scoped=%d", r.NProcs, r.TotalUsedBytes, r.ScopedUsedBytes)
	}
}

func TestSetsidEscape(t *testing.T) {
	tr := New(Options{})
	procs := []platform.Proc{
		pr(100, 99, 100, 1000, 10, 0),
		pr(101, 100, 100, 1001, 10, 0),
		pr(103, 101, 103, 1003, 50, 0), // setsid'd: own group
		pr(104, 103, 103, 1004, 60, 0), // its child
		pr(105, 104, 105, 1005, 70, 0), // and again
	}
	r := tr.Attribute(procs, []policy.Job{job("j", 100)}, nil)
	j := jobByID(t, r, "j")
	if j.NProcs != 5 || j.UsedBytes != 200*M || !reflect.DeepEqual(j.ExtraPIDs, []int{103, 104, 105}) {
		t.Errorf("job %+v", j)
	}
	if len(r.Units) != 0 {
		t.Errorf("units %v", unitKeys(r))
	}
}

func TestOrphanReparentedStaysSticky(t *testing.T) {
	tr := New(Options{})
	jobs := []policy.Job{job("j", 100)}
	tick1 := []platform.Proc{
		pr(100, 99, 100, 1000, 10, 5),
		pr(101, 100, 100, 1001, 10, 5),  // intermediate
		pr(105, 101, 105, 1005, 20, 5),  // setsid'd group leader
		pr(106, 105, 105, 1006, 300, 5), // grandchild
	}
	tr.Attribute(tick1, jobs, nil)
	// Intermediates exit: 106 is reparented to launchd, keeps pgid 105.
	tick2 := []platform.Proc{
		pr(100, 99, 100, 1000, 10, 5),
		pr(106, 1, 105, 1006, 300, 9),
	}
	r := tr.Attribute(tick2, jobs, nil)
	j := jobByID(t, r, "j")
	if j.NProcs != 2 || j.UsedBytes != 310*M || !reflect.DeepEqual(j.ExtraPIDs, []int{106}) {
		t.Errorf("tick2 job %+v", j)
	}
	if len(r.Units) != 0 {
		t.Errorf("orphan became unit: %v", unitKeys(r))
	}
	// A process first seen now, orphaned, in the group the (gone) member led:
	// rule 3 via the remembered group.
	tick3 := append(tick2, pr(108, 1, 105, 2000, 40, 0))
	r = tr.Attribute(tick3, jobs, nil)
	if j := jobByID(t, r, "j"); j.NProcs != 3 || !reflect.DeepEqual(j.ExtraPIDs, []int{106, 108}) {
		t.Errorf("tick3 job %+v", j)
	}
	// Once every member of group 105 is gone, the group is forgotten: a new
	// unrelated group with that pgid is not attributed.
	tick4 := []platform.Proc{pr(100, 99, 100, 1000, 10, 5)}
	tr.Attribute(tick4, jobs, nil)
	tick5 := []platform.Proc{pr(100, 99, 100, 1000, 10, 5), pr(300, 1, 105, 9000, 80, 0)}
	r = tr.Attribute(tick5, jobs, nil)
	if j := jobByID(t, r, "j"); j.NProcs != 1 {
		t.Errorf("tick5 job %+v", j)
	}
	if _, ok := unitByPG(r, 105); !ok {
		t.Errorf("unrelated group 105 not a unit: %v", unitKeys(r))
	}
}

func TestPIDReuse(t *testing.T) {
	tr := New(Options{})
	jobs := []policy.Job{job("j", 100)}
	tr.Attribute([]platform.Proc{
		pr(100, 99, 100, 1000, 10, 0),
		pr(101, 100, 101, 1001, 10, 0), // escaped member, own group
	}, jobs, nil)
	// 101 exits; pid 101 is reused by an unrelated process (and group).
	r := tr.Attribute([]platform.Proc{
		pr(100, 99, 100, 1000, 10, 0),
		pr(101, 1, 101, 5000, 99, 0),
	}, jobs, nil)
	if j := jobByID(t, r, "j"); j.NProcs != 1 || len(j.ExtraPIDs) != 0 {
		t.Errorf("reused pid attributed: %+v", j)
	}
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:101:5000"}) {
		t.Errorf("units %v", unitKeys(r))
	}

	// The job's own group: leader seen at start 1000; later a new process
	// with pid 100 (start 9000) leads a new group 100 => group reused, not the job.
	r = tr.Attribute([]platform.Proc{
		pr(100, 1, 100, 9000, 77, 0),
		pr(120, 100, 100, 9001, 1, 0),
	}, jobs, nil)
	if j := jobByID(t, r, "j"); j.Alive || j.NProcs != 0 {
		t.Errorf("job group reuse attributed: %+v", j)
	}
	if _, ok := unitByPG(r, 100); !ok {
		t.Errorf("reused group not a unit: %v", unitKeys(r))
	}

	// Parent started after the child: not the child's parent (stale ppid).
	tr2 := New(Options{})
	r = tr2.Attribute([]platform.Proc{
		pr(100, 99, 100, 1000, 10, 0),
		pr(130, 100, 130, 500, 10, 0),
	}, jobs, nil)
	if j := jobByID(t, r, "j"); j.NProcs != 1 {
		t.Errorf("start-order guard: %+v", j)
	}
}

func TestScopeRoot(t *testing.T) {
	tr := New(Options{ScopeRootPID: 50})
	jobs := []policy.Job{job("j", 100)}
	tick1 := []platform.Proc{
		pr(50, 40, 40, 100, 5, 0),     // root (test harness)
		pr(51, 50, 51, 200, 100, 0),   // bypasser
		pr(52, 51, 51, 201, 200, 0),   // its child
		pr(60, 1, 60, 50, 1000, 0),    // unrelated user app: out of scope
		pr(100, 99, 100, 300, 300, 0), // job: always counts
	}
	r := tr.Attribute(tick1, jobs, nil)
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:51:200"}) {
		t.Errorf("units %v", unitKeys(r))
	}
	if r.ScopedUsedBytes != 600*M || r.TotalUsedBytes != 1605*M {
		t.Errorf("scoped %d total %d", r.ScopedUsedBytes/M, r.TotalUsedBytes/M)
	}
	// 51 exits; 52 reparented to 1 stays in scope (sticky); 53 child of 52 too.
	tick2 := []platform.Proc{
		pr(50, 40, 40, 100, 5, 0),
		pr(52, 1, 51, 201, 200, 0),
		pr(53, 52, 53, 400, 50, 0),
		pr(60, 1, 60, 50, 1000, 0),
	}
	r = tr.Attribute(tick2, nil, nil)
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:51:200", "pg:53:400"}) {
		t.Errorf("tick2 units %v", unitKeys(r))
	}
	if r.ScopedUsedBytes != 250*M {
		t.Errorf("tick2 scoped %d", r.ScopedUsedBytes/M)
	}
	// Changing the scope root forgets remembered descendants.
	tr.SetOptions(Options{ScopeRootPID: 60})
	r = tr.Attribute(tick2, nil, nil)
	if len(r.Units) != 0 || r.ScopedUsedBytes != 0 {
		t.Errorf("new root: units %v scoped %d", unitKeys(r), r.ScopedUsedBytes)
	}
}

func TestExclusions(t *testing.T) {
	tr := New(Options{SelfPIDs: []int{10}})
	tr.SetProtectedPIDs([]int{99, 150})
	sys := pr(300, 1, 300, 10, 800, 0)
	sys.System = true
	sysInJob := pr(110, 100, 110, 1010, 40, 0)
	sysInJob.System = true
	procs := []platform.Proc{
		pr(10, 1, 10, 5, 30, 0),         // daemon itself
		pr(99, 50, 50, 900, 5, 0),       // protected wrapper (group 50)
		pr(51, 50, 50, 901, 70, 0),      // wrapper's sibling in group 50
		sys,                             // system process
		pr(100, 99, 100, 1000, 10, 0),   // job leader
		sysInJob,                        // system binary spawned by the job
		pr(150, 100, 100, 1020, 2, 0),   // nested wrapper inside the job: protected
		pr(151, 150, 151, 1021, 500, 0), // its child (not yet a known job)
	}
	r := tr.Attribute(procs, []policy.Job{job("j", 100)}, nil)
	j := jobByID(t, r, "j")
	// 100 + 110 (system, but a member) ; 150 protected ; 151 is the nested
	// wrapper's own child: a job about to start, counted but never a unit.
	if j.NProcs != 2 || j.UsedBytes != 50*M || !reflect.DeepEqual(j.ExtraPIDs, []int{110}) {
		t.Errorf("job %+v", j)
	}
	// Group 50 is the protected wrapper's (the agent's) group: never a
	// unit, though its other member counts in scoped usage.
	if keys := unitKeys(r); len(keys) != 0 {
		t.Errorf("units %v", keys)
	}
	if r.ScopedUsedBytes != (50+70+500)*M {
		t.Errorf("scoped %d", r.ScopedUsedBytes/M)
	}
	if r.TotalUsedBytes != (30+5+70+800+10+40+2+500)*M {
		t.Errorf("total %d", r.TotalUsedBytes/M)
	}
}

// A wrapper spawns its job and only then reports the pid; until the daemon
// applies that report the job is not running-with-a-PGID. In that window the
// child (and anything in its new group) must not become an unmanaged unit,
// which would make it the first pressure victim and kill it without a
// KILLED line on its wrapper. Its usage still counts.
func TestWrapperChildBeforeStartedIsNotAUnit(t *testing.T) {
	tr := New(Options{ScopeRootPID: 1})
	tr.SetProtectedPIDs([]int{500})
	procs := []platform.Proc{
		pr(400, 1, 400, 100, 5, 0),     // agent shell
		pr(500, 400, 400, 200, 8, 0),   // pace run (protected), in the shell's group
		pr(501, 500, 501, 300, 500, 0), // the job's leader: own group, not reported yet
		pr(502, 501, 501, 310, 200, 0), // its child, same group
		pr(600, 400, 600, 320, 300, 0), // a real bypasser
		pr(700, 1, 700, 50, 64, 0),     // unrelated
	}
	r := tr.Attribute(procs, []policy.Job{{ID: "j", State: policy.JobRunning}}, nil)
	if _, ok := unitByPG(r, 501); ok {
		t.Fatalf("pending job group reported as a unit: %v", unitKeys(r))
	}
	if _, ok := unitByPG(r, 600); !ok {
		t.Fatalf("bypasser missing: %v", unitKeys(r))
	}
	if !r.InScope[600] || !r.InScope[501] || r.InScope[1] || len(r.InScope) != 6 {
		t.Errorf("in scope %v", r.InScope)
	}
	if r0 := New(Options{}).Attribute(procs, nil, nil); r0.InScope != nil {
		t.Errorf("no scope: InScope must be nil, got %v", r0.InScope)
	}
	if r.ScopedUsedBytes != (5+500+200+300+64)*M {
		t.Errorf("scoped %dM", r.ScopedUsedBytes/M)
	}
	// Once the start is known the group is the job's.
	jb := job("j", 501)
	r = tr.Attribute(procs, []policy.Job{jb}, nil)
	if j := jobByID(t, r, "j"); j.UsedBytes != 700*M || j.NProcs != 2 {
		t.Errorf("job %+v", j)
	}
	// A child older than its "wrapper" pid is not the wrapper's (pid reuse).
	tr2 := New(Options{})
	tr2.SetProtectedPIDs([]int{500})
	procs2 := []platform.Proc{
		pr(500, 1, 500, 900, 8, 0),
		pr(510, 500, 510, 800, 300, 0), // started before pid 500 did
	}
	r = tr2.Attribute(procs2, nil, nil)
	if _, ok := unitByPG(r, 510); !ok {
		t.Fatalf("older process wrongly treated as a pending job: %v", unitKeys(r))
	}
}

func TestUnitSelection(t *testing.T) {
	tr := New(Options{MinUnitBytes: 64 * M, MaxUnits: 2})
	procs := []platform.Proc{
		pr(10, 1, 10, 1, 10, 0),
		pr(20, 1, 20, 2, 100, 0),
		pr(30, 1, 30, 3, 200, 0),
		pr(40, 1, 40, 4, 300, 0),
		pr(50, 1, 50, 5, 300, 0),
		pr(51, 50, 50, 6, 0, 0),
	}
	tracked := []policy.Unit{
		{Key: "pg:10:1", PGID: 10, StartMS: 1},     // small but tracked: kept
		{Key: "pg:20:999", PGID: 20, StartMS: 999}, // stale identity: not existing
		{Key: "pg:77:1", PGID: 77, StartMS: 1},     // gone
	}
	r := tr.Attribute(procs, nil, tracked)
	// Largest two (300M tie broken by key: pg:40 < pg:50), plus tracked pg:10.
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:10:1", "pg:40:4", "pg:50:5"}) {
		t.Errorf("units %v", unitKeys(r))
	}
	u, _ := unitByPG(r, 50)
	if u.NProcs != 2 || !reflect.DeepEqual(u.PIDs, []int{50, 51}) {
		t.Errorf("unit 50 %+v", u)
	}
	// Scoped bytes count every unmanaged process, reported or not.
	if r.ScopedUsedBytes != 910*M {
		t.Errorf("scoped %d", r.ScopedUsedBytes/M)
	}
	// No cap, no minimum: all.
	tr2 := New(Options{})
	if r := tr2.Attribute(procs, nil, nil); len(r.Units) != 5 {
		t.Errorf("uncapped units %v", unitKeys(r))
	}
	// MinUnitBytes only.
	tr3 := New(Options{MinUnitBytes: 150 * M})
	if r := tr3.Attribute(procs, nil, nil); !reflect.DeepEqual(unitKeys(r), []string{"pg:30:3", "pg:40:4", "pg:50:5"}) {
		t.Errorf("min only %v", unitKeys(r))
	}
}

func TestUnitIdentityStable(t *testing.T) {
	tr := New(Options{})
	r := tr.Attribute([]platform.Proc{pr(70, 1, 70, 100, 10, 0), pr(71, 70, 70, 150, 10, 0), pr(72, 70, 70, 120, 1, 0)}, nil, nil)
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:70:100"}) {
		t.Fatalf("units %v", unitKeys(r))
	}
	// Leader exits: key unchanged; representative becomes the lowest pid.
	r = tr.Attribute([]platform.Proc{pr(71, 1, 70, 150, 10, 0), pr(72, 1, 70, 120, 1, 0)}, nil, nil)
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:70:100"}) || r.Units[0].LeaderPID != 71 || r.Units[0].StartMS != 100 {
		t.Errorf("after leader exit %+v", r.Units)
	}
	// A fresh tracker (daemon restart) without memory uses the earliest start...
	fresh := New(Options{})
	procs := []platform.Proc{pr(71, 1, 70, 150, 10, 0), pr(72, 1, 70, 120, 1, 0)}
	if r := fresh.Attribute(procs, nil, nil); !reflect.DeepEqual(unitKeys(r), []string{"pg:70:120"}) {
		t.Errorf("fresh %v", unitKeys(r))
	}
	// ...unless the policy tracks the group, whose identity is adopted.
	fresh = New(Options{})
	tracked := []policy.Unit{{Key: "pg:70:100", PGID: 70, StartMS: 100}}
	if r := fresh.Attribute(procs, nil, tracked); !reflect.DeepEqual(unitKeys(r), []string{"pg:70:100"}) {
		t.Errorf("tracked seed %v", unitKeys(r))
	}
	// An inconsistent tracked identity (later than a member) is ignored.
	fresh = New(Options{})
	tracked = []policy.Unit{{Key: "pg:70:130", PGID: 70, StartMS: 130}}
	if r := fresh.Attribute(procs, nil, tracked); !reflect.DeepEqual(unitKeys(r), []string{"pg:70:120"}) {
		t.Errorf("bad tracked seed %v", unitKeys(r))
	}
}

func TestZombies(t *testing.T) {
	tr := New(Options{})
	z := pr(101, 100, 100, 1001, 0, 50)
	z.State = 'Z'
	zu := pr(201, 1, 201, 10, 0, 0)
	zu.State = 'Z'
	ze := pr(102, 100, 102, 1002, 0, 0)
	ze.State = 'Z'
	jobs := []policy.Job{job("j", 100)}
	r := tr.Attribute([]platform.Proc{pr(100, 99, 100, 1000, 10, 10), z, ze, zu}, jobs, nil)
	j := jobByID(t, r, "j")
	if j.NProcs != 1 || j.UsedBytes != 10*M || !j.Alive || len(j.ExtraPIDs) != 0 || j.CPUMS != 60 {
		t.Errorf("job %+v", j)
	}
	if len(r.Units) != 0 {
		t.Errorf("zombie unit %v", unitKeys(r))
	}
	// Only zombies left: not alive, CPU kept (zombie rusage may read 0).
	z0 := z
	z0.CPUMS = 0
	lz := pr(100, 99, 100, 1000, 0, 0)
	lz.State = 'Z'
	r = tr.Attribute([]platform.Proc{lz, z0}, jobs, nil)
	j = jobByID(t, r, "j")
	if j.Alive || j.NProcs != 0 || j.UsedBytes != 0 || j.CPUMS != 60 {
		t.Errorf("zombie-only job %+v", j)
	}
}

func TestCPUMonotonic(t *testing.T) {
	tr := New(Options{})
	jobs := []policy.Job{job("j", 100)}
	steps := []struct {
		procs []platform.Proc
		want  int64
	}{
		{[]platform.Proc{pr(100, 99, 100, 1000, 1, 100), pr(101, 100, 100, 1001, 1, 5000)}, 5100},
		// long-lived member 101 exits; its CPU is retired, not lost
		{[]platform.Proc{pr(100, 99, 100, 1000, 1, 100)}, 5100},
		// a new child burns CPU: progress is visible
		{[]platform.Proc{pr(100, 99, 100, 1000, 1, 100), pr(102, 100, 100, 2000, 1, 300)}, 5400},
		// a reading that goes down (sampling glitch) does not lower the value
		{[]platform.Proc{pr(100, 99, 100, 1000, 1, 50), pr(102, 100, 100, 2000, 1, 300)}, 5400},
		{[]platform.Proc{pr(100, 99, 100, 1000, 1, 150), pr(102, 100, 100, 2000, 1, 400)}, 5550},
	}
	prev := int64(0)
	for i, s := range steps {
		r := tr.Attribute(s.procs, jobs, nil)
		got := jobByID(t, r, "j").CPUMS
		if got != s.want {
			t.Errorf("step %d: cpu %d, want %d", i, got, s.want)
		}
		if got < prev {
			t.Errorf("step %d: cpu went down %d -> %d", i, prev, got)
		}
		prev = got
	}
}

type fakeTags struct {
	tags  map[int]string
	calls []int
}

func (f *fakeTags) JobTag(pid int) string {
	f.calls = append(f.calls, pid)
	return f.tags[pid]
}

func TestJobTag(t *testing.T) {
	ft := &fakeTags{tags: map[int]string{500: "j", 501: "later"}}
	tr := New(Options{})
	tr.SetTagReader(ft)
	jobs := []policy.Job{job("j", 100)}
	procs := []platform.Proc{pr(100, 99, 100, 1000, 10, 0)}
	for i := 0; i < 40; i++ {
		procs = append(procs, pr(600+i, 1, 600+i, int64(2000+i), 1, 0))
	}
	procs = append(procs,
		pr(500, 1, 500, 1, 30, 0),  // orphan carrying PACE_JOB_ID=j (oldest: read first)
		pr(502, 500, 500, 2, 5, 0), // its child follows via rule 2
		pr(501, 1, 501, 3, 20, 0),  // tagged for a job not running yet
		pr(503, 40, 503, 4, 20, 0), // not ppid 1 (a subreaper's orphan): read too, untagged
	)
	sys := pr(504, 1, 504, 5, 20, 0)
	sys.System = true
	procs = append(procs, sys) // system: never read
	r := tr.Attribute(procs, jobs, nil)
	j := jobByID(t, r, "j")
	if j.NProcs != 3 || !reflect.DeepEqual(j.ExtraPIDs, []int{500, 502}) {
		t.Errorf("tagged job %+v", j)
	}
	if len(ft.calls) != MaxTagReads {
		t.Fatalf("tick1 reads %d, want %d", len(ft.calls), MaxTagReads)
	}
	for _, pid := range ft.calls {
		if pid == 504 || pid == 502 || pid == 100 {
			t.Errorf("read tag of pid %d", pid)
		}
	}
	// Tick 2: the remaining candidates are read; tick 3: nothing (cached).
	ft.calls = nil
	tr.Attribute(procs, jobs, nil)
	if len(ft.calls) != 43-MaxTagReads {
		t.Errorf("tick2 reads %d (%v)", len(ft.calls), ft.calls)
	}
	ft.calls = nil
	tr.Attribute(procs, jobs, nil)
	if len(ft.calls) != 0 {
		t.Errorf("tick3 reads %v", ft.calls)
	}
	// Job "later" starts: 501 is attributed from the cache without a read.
	jobs = append(jobs, job("later", 900))
	r = tr.Attribute(procs, jobs, nil)
	if l := jobByID(t, r, "later"); l.NProcs != 1 || !reflect.DeepEqual(l.ExtraPIDs, []int{501}) {
		t.Errorf("later job %+v", l)
	}
	if len(ft.calls) != 0 {
		t.Errorf("cached tag re-read: %v", ft.calls)
	}
	// Backend-filled JobTag is used for any process, without a reader.
	tr2 := New(Options{})
	tagged := pr(700, 40, 700, 10, 5, 0)
	tagged.JobTag = "j"
	r = tr2.Attribute([]platform.Proc{pr(100, 99, 100, 1000, 10, 0), tagged}, []policy.Job{job("j", 100)}, nil)
	if j := jobByID(t, r, "j"); j.NProcs != 2 {
		t.Errorf("backend tag %+v", j)
	}
}

func TestNestedJobRule0Overrides(t *testing.T) {
	tr := New(Options{})
	outer := job("outer", 100)
	tick := []platform.Proc{
		pr(100, 99, 100, 1000, 10, 100),
		pr(150, 100, 100, 1100, 2, 1),    // inner `pace run` wrapper, in outer's group
		pr(151, 150, 151, 1101, 500, 50), // inner job leader (own group)
	}
	// The daemon does not know the inner wrapper/job yet: all of it is outer's.
	r := tr.Attribute(tick, []policy.Job{outer}, nil)
	o := jobByID(t, r, "outer")
	if o.NProcs != 3 || o.CPUMS != 151 {
		t.Fatalf("tick1 outer %+v", o)
	}
	// Inner job registered and its wrapper protected.
	tr.SetProtectedPIDs([]int{150})
	r = tr.Attribute(tick, []policy.Job{outer, job("inner", 151)}, nil)
	o, in := jobByID(t, r, "outer"), jobByID(t, r, "inner")
	if o.NProcs != 1 || o.UsedBytes != 10*M || in.NProcs != 1 || in.UsedBytes != 500*M {
		t.Errorf("tick2 outer %+v inner %+v", o, in)
	}
	if o.CPUMS < 151 {
		t.Errorf("outer CPU went down: %d", o.CPUMS)
	}
	if in.CPUMS != 50 {
		t.Errorf("inner CPU %d", in.CPUMS)
	}
}

func TestJobStatesAndPIDOnly(t *testing.T) {
	tr := New(Options{})
	procs := []platform.Proc{pr(100, 99, 100, 1000, 10, 0), pr(101, 100, 100, 1001, 10, 0), pr(200, 99, 200, 1000, 10, 0)}
	jobs := []policy.Job{
		{ID: "q", State: policy.JobQueued},
		{ID: "d", State: policy.JobDone, PID: 200, PGID: 200},
		{ID: "pidonly", State: policy.JobRunning, PID: 100},
		{ID: "nopid", State: policy.JobRunning},
	}
	r := tr.Attribute(procs, jobs, nil)
	if len(r.Jobs) != 1 || r.Jobs[0].ID != "pidonly" || r.Jobs[0].NProcs != 2 {
		t.Errorf("jobs %+v", r.Jobs)
	}
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:200:1000"}) {
		t.Errorf("done job's group should be unmanaged: %v", unitKeys(r))
	}
	// Job ends: its members become unmanaged.
	r = tr.Attribute(procs, nil, nil)
	if len(r.Jobs) != 0 || !reflect.DeepEqual(unitKeys(r), []string{"pg:100:1000", "pg:200:1000"}) {
		t.Errorf("after end: jobs %+v units %v", r.Jobs, unitKeys(r))
	}
	// A running job with no processes yet: present, not alive.
	r = tr.Attribute(nil, []policy.Job{job("empty", 4242)}, nil)
	if len(r.Jobs) != 1 || r.Jobs[0].Alive || r.Jobs[0].NProcs != 0 {
		t.Errorf("empty job %+v", r.Jobs)
	}
}

func TestDuplicatePIDsInInput(t *testing.T) {
	tr := New(Options{})
	r := tr.Attribute([]platform.Proc{
		pr(100, 99, 100, 1000, 10, 0),
		pr(300, 1, 300, 50, 10, 0),  // stale duplicate
		pr(300, 1, 300, 900, 20, 0), // current
	}, []policy.Job{job("j", 100)}, nil)
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:300:900"}) || r.Units[0].UsedBytes != 20*M {
		t.Errorf("units %+v", r.Units)
	}
}

// scenario is a multi-tick process history used for determinism checks.
func scenario() [][]platform.Proc {
	sys := pr(30, 1, 30, 1, 400, 0)
	sys.System = true
	z := pr(109, 100, 100, 1009, 0, 0)
	z.State = 'Z'
	return [][]platform.Proc{
		{
			pr(10, 1, 10, 1, 30, 0), sys,
			pr(99, 50, 50, 900, 5, 0), pr(100, 99, 100, 1000, 100, 10), pr(101, 100, 100, 1001, 50, 20),
			pr(103, 101, 103, 1003, 70, 5), pr(104, 103, 103, 1004, 80, 5), z,
			pr(200, 1, 200, 500, 300, 40), pr(201, 200, 200, 501, 100, 1), pr(202, 1, 202, 502, 1, 1),
			pr(300, 1, 300, 600, 90, 0), pr(301, 300, 301, 601, 90, 0),
		},
		{
			pr(10, 1, 10, 1, 30, 0), sys,
			pr(99, 50, 50, 900, 5, 0), pr(100, 99, 100, 1000, 110, 30),
			pr(104, 1, 103, 1004, 80, 9), pr(105, 1, 103, 1500, 20, 1),
			pr(200, 1, 200, 500, 310, 50), pr(201, 200, 200, 501, 100, 1), pr(202, 1, 202, 502, 1, 1),
			pr(301, 1, 301, 601, 90, 0), pr(302, 301, 301, 1600, 1, 0),
		},
		{
			pr(10, 1, 10, 1, 30, 0), sys,
			pr(100, 99, 100, 1000, 110, 40), pr(104, 1, 103, 1004, 80, 9),
			pr(200, 1, 200, 500, 310, 50), pr(202, 1, 202, 502, 1, 1), pr(104+1000, 1, 1104, 2000, 3, 0),
		},
	}
}

func runScenario(ticks [][]platform.Proc, perm func([]platform.Proc)) []Result {
	tr := New(Options{SelfPIDs: []int{10}, MinUnitBytes: 50 * M, MaxUnits: 3})
	tr.SetProtectedPIDs([]int{99})
	tr.SetTagReader(&fakeTags{tags: map[int]string{1104: "j"}})
	jobs := []policy.Job{job("j", 100), job("k", 777)}
	var out []Result
	var tracked []policy.Unit
	for _, tick := range ticks {
		in := append([]platform.Proc(nil), tick...)
		perm(in)
		r := tr.Attribute(in, jobs, tracked)
		out = append(out, r)
		tracked = nil
		for _, u := range r.Units {
			tracked = append(tracked, policy.Unit{Key: u.Key, PGID: u.PGID, StartMS: u.StartMS})
		}
	}
	return out
}

func TestDeterminismShuffled(t *testing.T) {
	want := runScenario(scenario(), func([]platform.Proc) {})
	// Sanity on the scenario itself.
	last := want[len(want)-1]
	if j := jobByID(t, last, "j"); j.NProcs != 3 || !reflect.DeepEqual(j.ExtraPIDs, []int{104, 1104}) {
		t.Errorf("scenario job %+v", j)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 200; i++ {
		got := runScenario(scenario(), func(p []platform.Proc) {
			rng.Shuffle(len(p), func(a, b int) { p[a], p[b] = p[b], p[a] })
		})
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("permutation %d differs:\n%+v\nvs\n%+v", i, got, want)
		}
	}
	// Outputs are sorted.
	for _, r := range want {
		if !sort.SliceIsSorted(r.Jobs, func(a, b int) bool { return r.Jobs[a].ID < r.Jobs[b].ID }) {
			t.Error("jobs not sorted")
		}
		if !sort.SliceIsSorted(r.Units, func(a, b int) bool { return r.Units[a].Key < r.Units[b].Key }) {
			t.Error("units not sorted")
		}
	}
}

// synth builds n processes: 10 jobs of 20 processes (with setsid'd
// subtrees) and the rest unmanaged in groups of ~3.
func synth(n int) ([]platform.Proc, []policy.Job) {
	var procs []platform.Proc
	var jobs []policy.Job
	pid := 1000
	start := int64(1_000_000)
	next := func() (int, int64) { pid++; start++; return pid, start }
	for j := 0; j < 10; j++ {
		lp, ls := next()
		procs = append(procs, pr(lp, 1, lp, ls, 50, 100))
		jobs = append(jobs, job(fmt.Sprintf("job%02d", j), lp))
		parent, pg := lp, lp
		for k := 1; k < 20; k++ {
			p, s := next()
			if k%7 == 0 {
				pg = p // setsid
			}
			procs = append(procs, pr(p, parent, pg, s, 20, 10))
			if k%3 == 0 {
				parent = p
			}
		}
	}
	for len(procs) < n {
		lp, ls := next()
		procs = append(procs, pr(lp, 1, lp, ls, 80, 1))
		for k := 0; k < 2 && len(procs) < n; k++ {
			p, s := next()
			procs = append(procs, pr(p, lp, lp, s, 10, 1))
		}
	}
	return procs, jobs
}

func TestSynthAttribution(t *testing.T) {
	procs, jobs := synth(800)
	tr := New(Options{MinUnitBytes: 64 * M, MaxUnits: 50})
	r := tr.Attribute(procs, jobs, nil)
	if len(r.Jobs) != 10 {
		t.Fatalf("jobs %d", len(r.Jobs))
	}
	for _, j := range r.Jobs {
		if j.NProcs != 20 {
			t.Errorf("%s nprocs %d", j.ID, j.NProcs)
		}
	}
	if len(r.Units) != 50 || r.NProcs != 800 {
		t.Errorf("units %d nprocs %d", len(r.Units), r.NProcs)
	}
}

func BenchmarkAttribute800(b *testing.B) {
	procs, jobs := synth(800)
	tr := New(Options{MinUnitBytes: 64 * M, MaxUnits: 64})
	tr.Attribute(procs, jobs, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tr.Attribute(procs, jobs, nil)
	}
}

// Finding 11: a fresh tracker (daemon restart) must not adopt whatever
// process now holds the old leader pid; the job's StartMS is the
// expectation until the leader is seen.
func TestRestartLeaderCheckedAgainstJobStart(t *testing.T) {
	j := job("j1", 500)
	j.StartMS = 1000
	tr := New(Options{})
	r := tr.Attribute([]platform.Proc{pr(500, 1, 500, 900_000, 50, 0), pr(501, 500, 500, 900_001, 50, 0)}, []policy.Job{j}, nil)
	if js := jobByID(t, r, "j1"); js.Alive || len(r.JobPIDs["j1"]) != 0 {
		t.Fatalf("reused leader adopted: %+v %v", js, r.JobPIDs)
	}
	if len(r.Units) != 1 || r.Units[0].PGID != 500 {
		t.Fatalf("unrelated group not a unit: %v", unitKeys(r))
	}
	// The real leader, within the tolerance of the reported start (Linux
	// wrappers report their clock): adopted, and then matched exactly.
	tr = New(Options{})
	r = tr.Attribute([]platform.Proc{pr(500, 1, 500, 1000-LeaderStartToleranceMS/2, 50, 0)}, []policy.Job{j}, nil)
	if !reflect.DeepEqual(r.JobPIDs["j1"], []int{500}) {
		t.Fatalf("leader not adopted: %v", r.JobPIDs)
	}
	r = tr.Attribute([]platform.Proc{pr(500, 1, 500, 1000+LeaderStartToleranceMS/2, 50, 0)}, []policy.Job{j}, nil)
	if len(r.JobPIDs["j1"]) != 0 {
		t.Fatalf("a different process at the leader pid adopted: %v", r.JobPIDs)
	}
}

// Finding 16: the leader exits before it is ever seen by this tracker and
// its pid is reused while an escaped member keeps the job alive.
func TestLeaderGoneThenPIDReused(t *testing.T) {
	j := job("j1", 500)
	j.StartMS = 1000
	tr := New(Options{})
	tr.Attribute([]platform.Proc{pr(600, 500, 600, 1100, 10, 0)}, []policy.Job{j}, nil) // no leader listed
	r := tr.Attribute([]platform.Proc{pr(500, 1, 500, 50_000, 10, 0), pr(600, 1, 600, 1100, 10, 0)}, []policy.Job{j}, nil)
	for _, p := range r.JobPIDs["j1"] {
		if p == 500 {
			t.Fatalf("reused pid 500 is a job member: %v", r.JobPIDs)
		}
	}
}

// Finding 40: rule 4 reads tags of processes re-parented to a subreaper
// (not pid 1), but never of processes older than every running job.
func TestJobTagSubreaperAndAge(t *testing.T) {
	const t0 = int64(1_700_000_000_000)
	ft := &fakeTags{tags: map[int]string{500: "j", 400: "j"}}
	tr := New(Options{})
	tr.SetTagReader(ft)
	j := job("j", 100)
	j.StartMS = t0
	procs := []platform.Proc{
		pr(1234, 1, 1234, t0-3_600_000, 5, 0),  // systemd --user, since login
		pr(100, 1, 100, t0, 10, 0),             // the job
		pr(500, 1234, 500, t0+2000, 900, 0),    // escapee, re-parented to the subreaper
		pr(400, 1234, 400, t0-600_000, 100, 0), // older than the job: never read
	}
	r := tr.Attribute(procs, []policy.Job{j}, nil)
	if !reflect.DeepEqual(r.JobPIDs["j"], []int{100, 500}) {
		t.Fatalf("members %v", r.JobPIDs)
	}
	for _, pid := range ft.calls {
		if pid == 400 || pid == 1234 || pid == 100 {
			t.Fatalf("read tag of pid %d (%v)", pid, ft.calls)
		}
	}
}

// Review finding 11: a job first seen by a running tracker records its
// leader exactly, even when the wrapper's StartMS (Linux: wall clock) is
// seconds away from the backend's start time after a clock step.
func TestLiveJobLeaderAdoptedDespiteClockStep(t *testing.T) {
	ft := &fakeTags{tags: map[int]string{700: "j1"}}
	tr := New(Options{})
	tr.SetTagReader(ft)
	tr.SetProtectedPIDs([]int{400})
	wrapper := pr(400, 1, 400, 999_000, 10, 0)
	tr.Attribute([]platform.Proc{wrapper}, nil, nil) // daemon running before the job
	j := job("j1", 500)
	j.StartMS = 1_005_000 // wall clock stepped 5 s ahead of the backend's frame
	procs := []platform.Proc{
		wrapper,
		pr(500, 400, 500, 1_000_000, 100, 0),
		pr(501, 500, 500, 1_000_010, 100, 0),
		pr(700, 1, 700, 1_000_500, 100, 0), // escapee (setsid, re-parented), started before the wrapper's StartMS
	}
	for tick := 0; tick < 3; tick++ {
		r := tr.Attribute(procs, []policy.Job{j}, nil)
		js := jobByID(t, r, "j1")
		if !js.Alive || js.UsedBytes != 300*M || !reflect.DeepEqual(r.JobPIDs["j1"], []int{500, 501, 700}) || len(r.Units) != 0 {
			t.Fatalf("tick %d: alive=%v used=%d jobpids=%v units=%v", tick, js.Alive, js.UsedBytes, r.JobPIDs, unitKeys(r))
		}
	}
	// A different process at the leader pid later is still caught.
	procs[1] = pr(500, 1, 500, 1_200_000, 100, 0)
	procs[2] = pr(501, 500, 500, 1_200_010, 100, 0)
	r := tr.Attribute(procs, []policy.Job{j}, nil)
	if !reflect.DeepEqual(r.JobPIDs["j1"], []int{700}) {
		t.Fatalf("reused group adopted: %v", r.JobPIDs)
	}
}

// A live job whose leader exited before this tracker first saw the job: a
// process that later holds the leader's pid is never the leader.
func TestLiveJobLeaderGoneBeforeSeen(t *testing.T) {
	tr := New(Options{})
	tr.Attribute(nil, nil, nil)
	j := job("j1", 500)
	j.StartMS = 1000
	tr.Attribute([]platform.Proc{pr(600, 1, 600, 1100, 10, 0)}, []policy.Job{j}, nil)
	r := tr.Attribute([]platform.Proc{pr(500, 1, 500, 1200, 10, 0), pr(600, 1, 600, 1100, 10, 0)}, []policy.Job{j}, nil)
	if len(r.JobPIDs["j1"]) != 0 {
		t.Fatalf("reused pid 500 adopted as leader: %v", r.JobPIDs)
	}
}
