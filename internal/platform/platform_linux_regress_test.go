//go:build linux

package platform

// Real-kernel regression tests for the Linux review findings 41, 42, 43 and
// 45 (run in Docker by scripts/docker-linux-test.sh, as root and as uid
// 1000). Every process signalled or reprioritised here is a helper this test
// spawned.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type tidState struct {
	policy uint32
	nice   int32
	ioprio int
}

// tidStates returns policy, nice and ioprio of every thread of pid.
func tidStates(t *testing.T, pid int) map[int]tidState {
	t.Helper()
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]tidState{}
	for _, e := range ents {
		tid, _ := strconv.Atoi(e.Name())
		a, err := unix.SchedGetAttr(tid, 0)
		if err != nil {
			continue
		}
		io, err := ioprioGet(tid)
		if err != nil {
			continue
		}
		out[tid] = tidState{policy: a.Policy, nice: a.Nice, ioprio: io}
	}
	return out
}

func setPolicy(tid int, policy uint32, prio uint32) error {
	a, err := unix.SchedGetAttr(tid, 0)
	if err != nil {
		return err
	}
	a.Policy, a.Priority, a.Flags = policy, prio, 0
	return unix.SchedSetAttr(tid, a, 0)
}

// checkRestored asserts every thread present before is back to its own
// policy, nice and ioprio.
func checkRestored(t *testing.T, pid int, before map[int]tidState) {
	t.Helper()
	after := tidStates(t, pid)
	for tid, w := range before {
		if g, ok := after[tid]; ok && g != w {
			t.Errorf("tid %d: after restore %+v, before %+v", tid, g, w)
		}
	}
}

// Finding 45, case 2: a SCHED_IDLE main thread must not drag the workers to
// (or leave them in) SCHED_IDLE on restore.
func TestLinuxRestorePerThreadIdleMain(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	h := startHelper(t, "threads=4", helperOpt{setpgid: true})
	if err := setPolicy(h.pid, unix.SCHED_IDLE, 0); err != nil {
		t.Fatal(err)
	}
	before := tidStates(t, h.pid)
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	for tid, s := range tidStates(t, h.pid) {
		if !isOurMark(s.ioprio, nil) {
			t.Errorf("tid %d ioprio %#x after background", tid, s.ioprio)
		}
		if tid == h.pid && s.policy != unix.SCHED_IDLE {
			t.Errorf("main thread policy %d, want untouched IDLE", s.policy)
		}
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, h.pid, before)
	if len(b.bg) != 0 {
		t.Errorf("record kept: %v", b.bg)
	}
}

// Finding 45, case 1: an RT main thread must not make restore write
// SCHED_FIFO with priority 0 (EINVAL) onto the fair workers.
func TestLinuxRestorePerThreadFifoMain(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	h := startHelper(t, "threads=4", helperOpt{setpgid: true})
	if err := setPolicy(h.pid, unix.SCHED_FIFO, 1); err != nil {
		t.Skipf("cannot set SCHED_FIFO here: %v", err)
	}
	before := tidStates(t, h.pid)
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	checkRestored(t, h.pid, before)
}

// Finding 45, case 3: a restore without a record (daemon restart) must not
// promote the user's own SCHED_BATCH / SCHED_IDLE threads; a recorded one
// puts a user BATCH thread back to BATCH.
func TestLinuxRestoreKeepsUserPolicies(t *testing.T) {
	h := startHelper(t, "threads=4", helperOpt{setpgid: true})
	var worker []int
	for tid := range tidStates(t, h.pid) {
		if tid != h.pid {
			worker = append(worker, tid)
		}
	}
	if len(worker) < 2 {
		t.Fatalf("workers %v", worker)
	}
	if err := setPolicy(worker[0], unix.SCHED_BATCH, 0); err != nil {
		t.Fatal(err)
	}
	if err := setPolicy(worker[1], unix.SCHED_IDLE, 0); err != nil {
		t.Fatal(err)
	}
	before := tidStates(t, h.pid)
	fresh := newTestBackend(t, defaultOpts())
	if err := fresh.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, h.pid, before)

	b := newTestBackend(t, defaultOpts())
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	checkRestored(t, h.pid, before)
}

// Finding 45, case 3 after a daemon restart: background by one backend,
// restore by a fresh one (no record). The user's own SCHED_BATCH and
// SCHED_IDLE threads keep their policy (BATCH is not promoted to NORMAL, and
// an IDLE thread we never touched is not "restored", which as uid 1000 was
// EPERM); the threads we changed go back to NORMAL, and nice raised from 0
// back to 0 where lowering is permitted.
func TestLinuxRestoreAfterRestartKeepsUserPolicies(t *testing.T) {
	privileged := newTestBackend(t, defaultOpts()).capSysNice
	for _, forceBatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch+nice=%v", forceBatch), func(t *testing.T) {
			h := startHelper(t, "threads=4", helperOpt{setpgid: true})
			var worker []int
			for tid := range tidStates(t, h.pid) {
				if tid != h.pid {
					worker = append(worker, tid)
				}
			}
			if len(worker) < 3 {
				t.Fatalf("workers %v", worker)
			}
			if err := setPolicy(worker[0], unix.SCHED_BATCH, 0); err != nil {
				t.Fatal(err)
			}
			if err := setPolicy(worker[1], unix.SCHED_IDLE, 0); err != nil {
				t.Fatal(err)
			}
			before := tidStates(t, h.pid)
			o := defaultOpts()
			o.ThrottleNice = defaultThrottleNice
			b := newTestBackend(t, o)
			if forceBatch {
				b.capSysNice = false // the BATCH + nice throttle, as unprivileged
			}
			if err := b.SetBackground(h.pid, true); err != nil {
				t.Fatal(err)
			}
			mid := tidStates(t, h.pid)
			t.Logf("background: user BATCH %+v, user IDLE %+v, worker %+v", mid[worker[0]], mid[worker[1]], mid[worker[2]])
			fresh := newTestBackend(t, o)
			if err := fresh.SetBackground(h.pid, false); err != nil {
				t.Fatalf("restore after restart: %v", err)
			}
			after := tidStates(t, h.pid)
			for tid, w := range before {
				g, ok := after[tid]
				if !ok {
					continue
				}
				if g.policy != w.policy || g.ioprio != w.ioprio {
					t.Errorf("tid %d: after restart restore %+v, before %+v", tid, g, w)
				}
				if g.nice != w.nice && privileged {
					t.Errorf("tid %d: nice %d, before %d (lowering is permitted here)", tid, g.nice, w.nice)
				}
			}
		})
	}
}

// Finding 45: threads created after background inherit our settings and go
// back to the defaults; restore with a record and without one both work.
func TestLinuxRestoreNewThreads(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh=%v", fresh), func(t *testing.T) {
			b := newTestBackend(t, defaultOpts())
			h := startHelper(t, "threadsplus=2", helperOpt{setpgid: true})
			before := tidStates(t, h.pid)
			if err := b.SetBackground(h.pid, true); err != nil {
				t.Fatal(err)
			}
			var added []int
			for i := 0; i < 50 && len(added) == 0; i++ {
				if _, err := h.stdin.Write([]byte("x\n")); err != nil {
					t.Fatal(err)
				}
				time.Sleep(20 * time.Millisecond)
				for tid := range tidStates(t, h.pid) {
					if _, ok := before[tid]; !ok {
						added = append(added, tid)
					}
				}
			}
			if len(added) == 0 {
				t.Fatal("helper started no new OS thread")
			}
			now := tidStates(t, h.pid)
			for _, tid := range added {
				if !isOurMark(now[tid].ioprio, nil) || now[tid].policy == unix.SCHED_NORMAL {
					t.Errorf("new tid %d did not inherit: %+v", tid, now[tid])
				}
			}
			r := b
			if fresh {
				r = newTestBackend(t, defaultOpts())
			}
			if err := r.SetBackground(h.pid, false); err != nil {
				t.Fatal(err)
			}
			checkRestored(t, h.pid, before)
			after := tidStates(t, h.pid)
			for _, tid := range added {
				if s := after[tid]; s.policy != unix.SCHED_NORMAL || ioprioClass(s.ioprio) == ioprioClassIdle {
					t.Errorf("new tid %d after restore %+v", tid, s)
				}
			}
		})
	}
}

// runUsernsBackground is the "userns-bg" helper: inside a child user
// namespace (CapEff full, uid_map not the identity) it deprioritises a
// grandchild and must be able to restore it.
func runUsernsBackground() int {
	b, err := newLinux(LinuxOptions{Footprint: FootprintAnon, ThrottleNice: 10})
	if err != nil {
		fmt.Println("FAIL newLinux:", err)
		return 1
	}
	raw, _ := os.ReadFile("/proc/self/uid_map")
	fmt.Printf("uid_map=%q capeff_sys_nice=%v capSysNice=%v\n", strings.TrimSpace(string(raw)), selfHasCap(&b.buf, 23), b.capSysNice)
	self, _ := os.Executable()
	c := exec.Command(self)
	c.Env = append(os.Environ(), helperEnv+"=sleep")
	stdin, _ := c.StdinPipe()
	stdout, _ := c.StdoutPipe()
	if err := c.Start(); err != nil {
		fmt.Println("FAIL start:", err)
		return 1
	}
	defer func() { _ = c.Process.Kill(); _ = stdin.Close(); _ = c.Wait() }()
	if l, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(l) != "ready" {
		fmt.Println("FAIL helper:", l)
		return 1
	}
	pid := c.Process.Pid
	if err := b.SetBackground(pid, true); err != nil {
		fmt.Println("FAIL background:", err)
		return 1
	}
	a, _ := unix.SchedGetAttr(pid, 0)
	fmt.Printf("background policy=%d nice=%d\n", a.Policy, a.Nice)
	if err := b.SetBackground(pid, false); err != nil {
		fmt.Println("FAIL restore:", err)
		return 1
	}
	a, _ = unix.SchedGetAttr(pid, 0)
	fmt.Printf("restored policy=%d nice=%d records=%d\n", a.Policy, a.Nice, len(b.bg))
	if a.Policy != unix.SCHED_NORMAL || len(b.bg) != 0 {
		fmt.Println("FAIL not reversible")
		return 1
	}
	fmt.Println("ok")
	return 0
}

// Finding 41: in a user namespace CapEff includes CAP_SYS_NICE, but the
// kernel checks it against the initial namespace, so SCHED_IDLE would be
// one-way. The backend must not trust CapEff there.
func TestLinuxSetBackgroundInUserNamespace(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), helperEnv+"=userns-bg")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		t.Skipf("cannot create a user namespace here: %v", err)
	}
	t.Logf("MEASURE userns: %s", strings.ReplaceAll(strings.TrimSpace(string(out)), "\n", "; "))
	if err != nil || !strings.Contains(string(out), "\nok") {
		t.Errorf("SetBackground not reversible inside a user namespace: %v\n%s", err, out)
	}
}

// cpuTicksOf returns utime+stime of pid in ticks.
func cpuTicksOf(t *testing.T, pid int) int64 {
	t.Helper()
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseStat(b)
	if err != nil {
		t.Fatal(err)
	}
	return int64(s.UTime + s.STime)
}

// Finding 42: unprivileged, SCHED_BATCH alone keeps the full CFS weight, so
// a throttled job's CPU share did not change (measured 50 % -> 50 %). With
// the nice fallback it must drop well below an equal share.
func TestLinuxThrottleCutsCPUShare(t *testing.T) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		t.Fatal(err)
	}
	cpu := 0
	for !set.IsSet(cpu) {
		cpu++
	}
	o := defaultOpts()
	o.ThrottleNice = defaultThrottleNice
	b := newTestBackend(t, o)
	slow := startHelper(t, fmt.Sprintf("spin=%d", cpu), helperOpt{setpgid: true})
	other := startHelper(t, fmt.Sprintf("spin=%d", cpu), helperOpt{setpgid: true})
	share := func() float64 {
		a0, b0 := cpuTicksOf(t, slow.pid), cpuTicksOf(t, other.pid)
		time.Sleep(2 * time.Second)
		a1, b1 := cpuTicksOf(t, slow.pid), cpuTicksOf(t, other.pid)
		return 100 * float64(a1-a0) / float64(max(a1-a0+b1-b0, 1))
	}
	base := share()
	if err := b.SetBackground(slow.pid, true); err != nil {
		t.Fatal(err)
	}
	a, _ := unix.SchedGetAttr(slow.pid, 0)
	throttled := share()
	t.Logf("MEASURE throttle uid=%d capSysNice=%v policy=%d nice=%d: share %.1f%% -> %.1f%%",
		os.Getuid(), b.capSysNice, a.Policy, a.Nice, base, throttled)
	if throttled > 25 {
		t.Errorf("throttled share %.1f%% (was %.1f%%): the throttle does not deprioritise", throttled, base)
	}
	if err := b.SetBackground(slow.pid, false); err != nil {
		t.Fatalf("restore: %v", err)
	}
	a, _ = unix.SchedGetAttr(slow.pid, 0)
	if a.Policy != unix.SCHED_NORMAL {
		t.Errorf("restored policy %d", a.Policy)
	}
	if b.capSysNice && a.Nice != 0 {
		t.Errorf("privileged restore left nice %d", a.Nice)
	}
	t.Logf("MEASURE after restore: policy=%d nice=%d (nice is one-way without CAP_SYS_NICE/RLIMIT_NICE)", a.Policy, a.Nice)
}

// Finding 43: Load1Milli/NCPU must be the host load per online CPU (or our
// CPUs' runqueue), not host loadavg divided by our affinity mask.
func TestLinuxSampleMachineLoadNotInflated(t *testing.T) {
	root := fixtureRoot(t)
	busy, idle := make([]uint64, 64), make([]uint64, 64)
	if err := os.WriteFile(filepath.Join(root, "stat"), procStatFixture(13, 0, busy, idle), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loadavg"), []byte("16.00 8.00 4.00 13/300 999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procRoot
	procRoot = root
	t.Cleanup(func() { procRoot = old })
	o := defaultOpts()
	o.CgroupMemLimit = false
	b := newTestBackend(t, o)
	m, err := b.SampleMachine(nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.NCPU <= 0 || m.Load1Milli/int64(m.NCPU) != 250 {
		t.Errorf("load per CPU = %d/%d = %d, want 250 (16 / 64 online)", m.Load1Milli, m.NCPU, m.Load1Milli/int64(max(m.NCPU, 1)))
	}
	// 13 running (12 + us) on 64 CPUs: the average falls toward 0.1875.
	b.load.last = b.load.last.Add(-10 * time.Minute)
	m, _ = b.SampleMachine(nil)
	if got := m.Load1Milli / int64(m.NCPU); got < 180 || got > 200 {
		t.Errorf("after 10 min: load per CPU %d, want ~187 (%s)", got, b.load.source)
	}
}

// Review of finding 42: the one-way nice of the unprivileged throttle is
// reported (status notes), and only when it applies.
func TestLinuxThrottleNote(t *testing.T) {
	old := sysBlockRoot
	t.Cleanup(func() { sysBlockRoot = old })
	sysBlockRoot = blockFixture(t, map[string]string{"sda": "mq-deadline kyber [bfq] none\n"})
	o := defaultOpts()
	o.ThrottleNice = defaultThrottleNice
	b := newTestBackend(t, o)
	b.capSysNice = false
	b.quota = cpuQuota{} // (TestLinuxThrottleNoteUnderQuota covers a container CPU quota)
	if n := b.ThrottleNote(false); !strings.Contains(n, "nice 10") {
		t.Errorf("unprivileged note %q", n)
	}
	if c := b.ThrottleCaveat(false); !strings.Contains(c, "leaves nice 10") {
		t.Errorf("unprivileged caveat %q", c)
	}
	b.capSysNice = true
	if n, c := b.ThrottleNote(false), b.ThrottleCaveat(false); n != "" || c != "" {
		t.Errorf("privileged (SCHED_IDLE, reversible) note %q caveat %q", n, c)
	}
	// Finding 42 (reporting): BATCH alone is reversible but weak.
	b.capSysNice = false
	b.opts.ThrottleNice = 0
	if n := b.ThrottleNote(false); !strings.Contains(n, "barely slows") {
		t.Errorf("ThrottleNice 0 (BATCH only, weak) note %q", n)
	}
	if c := b.ThrottleCaveat(false); !strings.Contains(c, "is weak") {
		t.Errorf("ThrottleNice 0 caveat %q", c)
	}
	// Round 3: a job whose container caps it with cpu.max is not weakly
	// throttled, and the note says which jobs the weakness concerns.
	if c := b.ThrottleCaveat(true); c != "" {
		t.Errorf("ThrottleNice 0 caveat with cpu.max %q, want none", c)
	}
	if n := b.ThrottleNote(true); !strings.Contains(n, "cpu.max") || !strings.Contains(n, "without a job container") {
		t.Errorf("ThrottleNice 0 note with cpu.max %q", n)
	}

	// No disk under BFQ: the I/O half does nothing, even with SCHED_IDLE.
	sysBlockRoot = blockFixture(t, map[string]string{"nvme0n1": "[none] mq-deadline kyber bfq\n", "loop0": "[bfq]\n"})
	b2 := newTestBackend(t, o)
	b2.capSysNice = true
	b2.quota = cpuQuota{}
	if n := b2.ThrottleNote(false); !strings.Contains(n, "BFQ") {
		t.Errorf("no-BFQ note %q", n)
	}
	if c := b2.ThrottleCaveat(false); c != "throttle on this machine does not slow I/O (no BFQ disk); see pace status notes" {
		t.Errorf("no-BFQ caveat %q", c)
	}
	var _ ThrottleNoter = b
	var _ ThrottleCaveater = b
}

// threadMasks is the affinity mask of each thread of pid.
func threadMasks(t *testing.T, pid int) map[int]unix.CPUSet {
	t.Helper()
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]unix.CPUSet{}
	for _, e := range ents {
		tid, _ := strconv.Atoi(e.Name())
		var m unix.CPUSet
		if unix.SchedGetaffinity(tid, &m) == nil {
			out[tid] = m
		}
	}
	return out
}

// F5 (stress s10): the cgroup-namespace root's cpu.max is read as the
// container's CPU quota; under one that binds, pace status notes and the
// THROTTLED caveat say the throttle is limited (not for a job its own
// container's cpu.max holds), and the throttle itself is unchanged: it
// does not touch the job's CPU affinity.
func TestLinuxThrottleNoteUnderQuota(t *testing.T) {
	dir := t.TempDir()
	var buf []byte
	if q := readCPUQuota(dir, &buf, 8); q != (cpuQuota{}) {
		t.Errorf("no cpu.max: %+v", q)
	}
	if err := os.WriteFile(dir+"/cpu.max", []byte("200000 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if q := readCPUQuota(dir, &buf, 8); q != (cpuQuota{200000, 100000, 8}) || !q.binds() {
		t.Errorf("docker --cpus 2 on 8 CPUs: %+v binds=%v", q, q.binds())
	}
	if err := os.WriteFile(dir+"/cpu.max", []byte("max 100000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if q := readCPUQuota(dir, &buf, 8); q.binds() {
		t.Errorf("max binds: %+v", q)
	}
	if q := readCPUQuota("", &buf, 8); q != (cpuQuota{}) {
		t.Errorf("no cgroup2 mount: %+v", q)
	}

	b := newTestBackend(t, defaultOpts())
	b.quota = cpuQuota{quota: 100000, period: 100000, ncpu: 8}
	if n := b.ThrottleNote(false); !strings.Contains(n, "CPU quota (cpu.max 1 of 8 CPUs) is limited") {
		t.Errorf("quota note %q", n)
	}
	if c := b.ThrottleCaveat(false); !strings.Contains(c, "is limited by the container's CPU quota") || !strings.HasPrefix(c, "throttle on this machine ") {
		t.Errorf("quota caveat %q", c)
	}
	if c := b.ThrottleCaveat(true); strings.Contains(c, "quota") {
		t.Errorf("quota caveat for a job its container's cpu.max holds %q", c)
	}
	h := startHelper(t, "threads=4", helperOpt{setpgid: true})
	m0 := threadMasks(t, h.pid)
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	for tid, m := range threadMasks(t, h.pid) {
		if was, ok := m0[tid]; ok && was != m {
			t.Errorf("throttled: thread %d CPU mask changed (%d CPUs, was %d)", tid, m.Count(), was.Count())
		}
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
}
