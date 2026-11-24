package platform

import "testing"

// Finding 38: a kernel kill at the job container's own memory.max is a
// governor kill (KILLED policy=mem_budget); a machine-wide OOM kill or one
// for an ancestor's limit (a docker --memory) is not, and must be passed
// through like any other signal death.
func TestOwnOOMKills(t *testing.T) {
	for _, tc := range []struct {
		events string
		want   int64
	}{
		{"low 0\nhigh 0\nmax 12\noom 1\noom_kill 1\noom_group_kill 1\n", 1},
		{"low 0\nhigh 0\nmax 3\noom 2\noom_kill 3\noom_group_kill 0\n", 3},
		{"low 0\nhigh 0\nmax 0\noom 0\noom_kill 2\noom_group_kill 0\n", 0}, // ancestor/global OOM
		{"low 0\nhigh 0\nmax 0\noom 0\noom_kill 0\n", 0},
		{"oom 1\noom_kill 0\noom_group_kill 2\n", 2},
		{"", 0},
	} {
		if got := ownOOMKills([]byte(tc.events)); got != tc.want {
			t.Errorf("%q: %d, want %d", tc.events, got, tc.want)
		}
	}
}

// Container usage is the job's own memory (anon + shmem), not the kubelet
// working set: active page cache (a kill does not free it) is left out.
func TestCgroupOwnMemory(t *testing.T) {
	stat := "anon 299892736\nfile 157286400\nkernel 4194304\nshmem 1048576\nactive_file 52428800\ninactive_file 104857600\nanon_thp 0\n"
	if got, ok := cgroupOwnMemory([]byte(stat)); !ok || got != 299892736+1048576 {
		t.Errorf("got %d %v", got, ok)
	}
	// "anon_thp" must not be taken for "anon".
	if _, ok := cgroupOwnMemory([]byte("anon_thp 5\nfile 1\n")); ok {
		t.Error("anon_thp read as anon")
	}
}

// Finding 42 remainder: the container throttle's CPU quota.
func TestCPUMaxQuota(t *testing.T) {
	for pct, want := range map[int]string{25: "25000 100000", 100: "100000 100000", 250: "250000 100000", 0: "max 100000", -3: "max 100000"} {
		if got := cpuMaxQuota(pct); got != want {
			t.Errorf("cpuMaxQuota(%d) = %q, want %q", pct, got, want)
		}
	}
}

// The fake container implements the whole JobContainer contract the daemon
// relies on (Kill reaches the fake's processes, Remove refuses a populated
// container).
func TestFakeContainers(t *testing.T) {
	f := NewFake()
	f.Procs = []Proc{{PID: 10, PGID: 10, UID: 1}, {PID: 11, PGID: 11, UID: 1}}
	if _, ok := f.Containers(""); ok {
		t.Fatal("containers before EnableContainers")
	}
	c := f.EnableContainers("/cg/pace")
	jc, ok := f.Containers("")
	if !ok || jc != JobContainer(c) {
		t.Fatal("no containers")
	}
	p, _ := jc.Create("j1", 0)
	if want, _ := jc.Path("j1"); p != want || p != "/cg/pace/job-j1" {
		t.Errorf("path %q / %q", p, want)
	}
	_ = jc.Attach("j1", 10)
	if err := jc.Remove("j1"); err == nil {
		t.Error("removed a populated container")
	}
	_ = jc.Kill("j1")
	if procs, _ := f.ListProcs(1); len(procs) != 1 || procs[0].PID != 11 {
		t.Errorf("after kill: %+v", procs)
	}
	if err := jc.Remove("j1"); err != nil {
		t.Error(err)
	}
	if _, err := jc.PIDs("j1"); err == nil {
		t.Error("removed container still listed")
	}
}
