//go:build linux

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Finding 38: the JobContainer operations pace run and the daemon now use,
// on a real kernel (scripts/docker-linux-test.sh, root mode).
func TestLinuxCgroupJobAPI(t *testing.T) {
	_, base := containersOrSkip(t)
	c := &cgroupContainer{base: base.Base(), cpuPct: defaultThrottleCPUPct}
	id := fmt.Sprintf("api%d", os.Getpid())
	t.Cleanup(func() { _ = c.Kill(id); _ = c.Remove(id) })
	path, err := c.Create(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p, err := c.Path(id); err != nil || p != path {
		t.Errorf("Path = %q %v, want %q", p, err, path)
	}
	if ids, err := c.Jobs(); err != nil || !slices.Contains(ids, id) {
		t.Errorf("Jobs = %v %v", ids, err)
	}

	// The hard cap can be set and lifted on an existing container.
	if err := c.SetMemMax(id, 200<<20); err != nil {
		t.Fatal(err)
	}
	if v := readTrim(t, path+"/memory.max"); v != strconv.Itoa(200<<20) {
		t.Errorf("memory.max = %s", v)
	}
	if err := c.SetMemMax(id, 0); err != nil {
		t.Fatal(err)
	}
	if v := readTrim(t, path+"/memory.max"); v != "max" {
		t.Errorf("lifted memory.max = %s", v)
	}
	if err := c.SetMemMax("nosuch"+id, 1<<20); err == nil {
		t.Error("SetMemMax on a missing container succeeded")
	}

	// pace run spawns the child straight into the container.
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: fd}
	if err := cmd.Start(); err != nil {
		t.Fatalf("CLONE_INTO_CGROUP spawn: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	pids, err := c.PIDs(id)
	if err != nil || !slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("PIDs = %v %v, want the child %d", pids, err, cmd.Process.Pid)
	}
	if u, err := c.Usage(id); err != nil || u < 0 {
		t.Errorf("Usage = %d %v", u, err)
	}
	if _, err := os.Stat(path + "/memory.peak"); err == nil {
		if p, err := c.Peak(id); err != nil || p <= 0 {
			t.Errorf("Peak = %d %v", p, err)
		}
	}

	// Throttle weights: whatever the delegation gives, set and restored.
	w, err := c.SetThrottle(id, true)
	if err != nil {
		t.Fatal(err)
	}
	_, cpuErr := os.Stat(path + "/cpu.weight")
	if (cpuErr == nil) != (w != "") {
		t.Errorf("SetThrottle reported %q, cpu.weight present: %v", w, cpuErr == nil)
	}
	if cpuErr == nil {
		if v := readTrim(t, path+"/cpu.weight"); v != "1" {
			t.Errorf("throttled cpu.weight = %s", v)
		}
		if c.cpuPct > 0 {
			if v := readTrim(t, path+"/cpu.max"); v != cpuMaxQuota(c.cpuPct) {
				t.Errorf("throttled cpu.max = %s, want %s", v, cpuMaxQuota(c.cpuPct))
			}
		}
		if _, err := c.SetThrottle(id, false); err != nil {
			t.Fatal(err)
		}
		if v := readTrim(t, path+"/cpu.weight"); v != "100" {
			t.Errorf("restored cpu.weight = %s", v)
		}
		if v := readTrim(t, path+"/cpu.max"); v != "max 100000" {
			t.Errorf("restored cpu.max = %s", v)
		}
	}
	t.Logf("MEASURE throttle knobs %q (base weights %q)", w, c.Weights())

	if n, err := c.OOMKills(id); err != nil || n != 0 {
		t.Errorf("OOMKills of a healthy job = %d %v", n, err)
	}
	if err := c.Kill(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child survived cgroup.kill")
	}
	waitEvent(t, path, "populated 0")
	if pids, err := c.PIDs(id); err != nil || len(pids) != 0 {
		t.Errorf("PIDs after kill = %v %v", pids, err)
	}
}

// Review: each daemon gets a scope of its own under the pace root, so a
// second daemon's job containers are not the first's (Jobs never lists
// them); the wrapper opens a daemon's base as given (ContainersAt).
func TestLinuxCgroupScopes(t *testing.T) {
	_, root := containersOrSkip(t)
	sa, sb := fmt.Sprintf("d-ta%d", os.Getpid()), fmt.Sprintf("d-tb%d", os.Getpid())
	a, err := detectCgroupContainer(defaultOpts(), false, sa)
	if err != nil {
		t.Fatal(err)
	}
	b, err := detectCgroupContainer(defaultOpts(), false, sb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	if a.Base() != root.Base()+"/"+sa || b.Base() != root.Base()+"/"+sb {
		t.Fatalf("bases %s %s under %s", a.Base(), b.Base(), root.Base())
	}
	if v := readTrim(t, a.Base()+"/cgroup.subtree_control"); !strings.Contains(v, "memory") {
		t.Fatalf("scope subtree_control %q: no memory controller", v)
	}
	id := fmt.Sprintf("sc%d", os.Getpid())
	t.Cleanup(func() { _ = a.Remove(id) })
	if _, err := a.Create(id, 100<<20); err != nil {
		t.Fatal(err)
	}
	if ids, _ := b.Jobs(); slices.Contains(ids, id) {
		t.Fatalf("daemon B lists daemon A's container: %v", ids)
	}
	if ids, _ := root.Jobs(); slices.Contains(ids, id) {
		t.Fatalf("the root lists a scoped container: %v", ids)
	}
	if ids, _ := a.Jobs(); !slices.Contains(ids, id) {
		t.Fatalf("A does not list its own: %v", ids)
	}
	// The wrapper's view of A's base.
	w, err := openCgroupContainer(defaultOpts(), a.Base())
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := w.Path(id); p != a.Base()+"/job-"+id {
		t.Errorf("wrapper path %s", p)
	}
	for _, bad := range []string{"relative/x", "/tmp", a.Base() + "/nosuch"} {
		if _, err := openCgroupContainer(defaultOpts(), bad); err == nil {
			t.Errorf("opened %q", bad)
		}
	}
	if err := a.Remove(id); err != nil {
		t.Fatal(err)
	}
	b.Close()
	if _, err := os.Stat(b.Base()); !os.IsNotExist(err) {
		t.Errorf("empty scope not removed by Close: %v", err)
	}
	for _, bad := range []string{"", "job-x", "pace-outside", "A", "x/y", strings.Repeat("a", 33)} {
		if _, err := detectCgroupContainer(defaultOpts(), false, bad); bad != "" && err == nil {
			t.Errorf("scope %q accepted", bad)
		}
	}
}

// Review: a pace process inside a job's container is moved out before the
// container is frozen or killed, and survives the job's cgroup.kill.
func TestLinuxCgroupEvict(t *testing.T) {
	_, c := containersOrSkip(t)
	id := fmt.Sprintf("ev%d", os.Getpid())
	t.Cleanup(func() { _ = c.Kill(id); _ = c.Remove(id) })
	path, err := c.Create(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	start := func() *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: fd}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	job, wrapper := start(), start()
	t.Cleanup(func() { _ = wrapper.Process.Kill(); _ = wrapper.Wait() })
	if err := c.Evict(wrapper.Process.Pid); err != nil {
		t.Fatal(err)
	}
	pids, _ := c.PIDs(id)
	if slices.Contains(pids, wrapper.Process.Pid) || !slices.Contains(pids, job.Process.Pid) {
		t.Fatalf("container %v after evicting %d", pids, wrapper.Process.Pid)
	}
	if out, _ := cgPids(c.Base()+"/"+outsideLeaf+"/cgroup.procs", new([]byte)); !slices.Contains(out, wrapper.Process.Pid) {
		t.Fatalf("outside leaf %v", out)
	}
	if err := c.Kill(id); err != nil {
		t.Fatal(err)
	}
	_ = job.Wait()
	if err := wrapper.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("evicted process killed with the job: %v", err)
	}
	if ids, _ := c.Jobs(); slices.Contains(ids, outsideLeaf) || slices.Contains(ids, "outside") {
		t.Fatalf("outside leaf listed as a job: %v", ids)
	}
}

// Round 3: a job that creates sub-cgroups of its container keeps the
// processes it moves there (PIDs lists them), and a sub-cgroup it leaves
// behind does not make the container unremovable: RemoveNow removes it in
// one attempt, without the 1 s wait. Frozen reads cgroup.freeze back,
// whoever wrote it.
func TestLinuxCgroupNestedAndFrozen(t *testing.T) {
	_, c := containersOrSkip(t)
	id := fmt.Sprintf("ns%d", os.Getpid())
	t.Cleanup(func() { _ = c.Kill(id); _ = c.Remove(id) })
	path, err := c.Create(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	inner := path + "/inner/deeper"
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Open(inner, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, UseCgroupFD: true, CgroupFD: fd}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if pids, err := c.PIDs(id); err != nil || !slices.Contains(pids, cmd.Process.Pid) {
		t.Fatalf("PIDs = %v %v, want the process in the sub-cgroup %d", pids, err, cmd.Process.Pid)
	}

	if on, err := c.Frozen(id); err != nil || on {
		t.Fatalf("Frozen = %v %v before a freeze", on, err)
	}
	if err := c.Freeze(id, true); err != nil {
		t.Fatal(err)
	}
	if on, err := c.Frozen(id); err != nil || !on {
		t.Fatalf("Frozen = %v %v after a freeze", on, err)
	}
	if err := os.WriteFile(path+"/cgroup.freeze", []byte("0"), 0); err != nil { // pace run's thawLost
		t.Fatal(err)
	}
	if on, err := c.Frozen(id); err != nil || on {
		t.Fatalf("Frozen = %v %v after a thaw behind its back", on, err)
	}

	// Busy while a process is inside a sub-cgroup: one attempt, no wait.
	t0 := time.Now()
	if err := c.RemoveNow(id); err == nil {
		t.Fatal("removed a container with a process in its sub-cgroup")
	}
	if el := time.Since(t0); el > 200*time.Millisecond {
		t.Errorf("RemoveNow took %v", el)
	}
	if err := c.Kill(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("child in the sub-cgroup survived cgroup.kill")
	}
	waitEvent(t, path, "populated 0")
	if pids, err := c.PIDs(id); err != nil || len(pids) != 0 {
		t.Fatalf("PIDs after kill = %v %v", pids, err)
	}
	// The empty sub-cgroups stay behind: still removable at once.
	t0 = time.Now()
	if err := c.RemoveNow(id); err != nil {
		t.Fatalf("RemoveNow with empty sub-cgroups left: %v", err)
	}
	if el := time.Since(t0); el > 200*time.Millisecond {
		t.Errorf("RemoveNow took %v", el)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("container still there: %v", err)
	}
}
