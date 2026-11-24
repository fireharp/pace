//go:build linux

package platform

// Real-kernel tests for the Linux backend. On macOS they are cross-compiled
// and run in Docker by scripts/docker-linux-test.sh. Every process signalled
// here is a helper this test spawned (the test binary re-executed in helper
// mode, or /bin/sleep); every helper is killed in t.Cleanup.
//
// Environment (set by the script):
//   PACE_LINUX_IN_DOCKER=1       allow the cgroup "leaf dance" (only inside a container)
//   PACE_LINUX_REQUIRE_CGROUP=1  fail (not skip) when containers are unusable
//   PACE_LINUX_EXPECT_MEM_LIMIT  expected container memory.max in bytes
//   PACE_LINUX_MEASURE=1         run the cost/accuracy measurements

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const helperEnv = "PACE_PLAT_HELPER"

var helperSink byte

func init() {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
}

// runHelper implements the helper process. Modes:
//
//	sleep              print ready, block until stdin closes
//	alloc=MB           touch MB of private anonymous memory, ready, block
//	wait-alloc=MB      read a line from stdin first, then like alloc
//	mapfd=MB           mmap fd 3 shared and read-touch MB, ready, block
//	burn=MS            spin MS of wall time, ready, block
//	ignoreterm         ignore SIGTERM, ready, block
//	threads=N          start N extra OS threads, ready, block
func runHelper(mode string) int {
	name, arg, _ := strings.Cut(mode, "=")
	n, _ := strconv.Atoi(arg)
	in := bufio.NewReader(os.Stdin)
	var keep [][]byte
	touch := func(b []byte, write bool) {
		for i := 0; i < len(b); i += 4096 {
			if write {
				b[i] = 1
			} else {
				helperSink += b[i] // global: the read must not be optimised away
			}
		}
	}
	switch name {
	case "sleep":
	case "wait-alloc":
		if _, err := in.ReadString('\n'); err != nil {
			return 3
		}
		fallthrough
	case "alloc":
		b, err := unix.Mmap(-1, 0, n<<20, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mmap:", err)
			return 4
		}
		touch(b, true)
		keep = append(keep, b)
	case "mapfd":
		b, err := unix.Mmap(3, 0, n<<20, unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mmap fd:", err)
			return 4
		}
		touch(b, false)
		keep = append(keep, b)
	case "burn":
		end := time.Now().Add(time.Duration(n) * time.Millisecond)
		x := 0
		for time.Now().Before(end) {
			for i := 0; i < 10000; i++ {
				x += i
			}
		}
		_ = x
	case "ignoreterm":
		signal.Ignore(syscall.SIGTERM)
	case "threads", "threadsplus":
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				runtime.LockOSThread()
				wg.Done()
				select {}
			}()
		}
		wg.Wait()
		if name == "threadsplus" {
			// After ready, every stdin line starts one more OS thread.
			fmt.Println("ready")
			for {
				if _, err := in.ReadString('\n'); err != nil {
					return 0
				}
				go func() {
					runtime.LockOSThread()
					select {}
				}()
			}
		}
	case "spin":
		// Spin forever on one OS thread pinned to CPU n.
		ready := make(chan struct{})
		go func() {
			runtime.LockOSThread()
			var set unix.CPUSet
			set.Set(n)
			if err := unix.SchedSetaffinity(0, &set); err != nil {
				fmt.Fprintln(os.Stderr, "affinity:", err)
				os.Exit(4)
			}
			close(ready)
			x := 0
			for {
				x++
			}
		}()
		<-ready
	case "userns-bg":
		return runUsernsBackground()
	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode", mode)
		return 2
	}
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, in) // until the parent closes stdin or kills us
	runtime.KeepAlive(keep)
	return 0
}

type helper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	pid   int
	ready chan string
	done  chan struct{}
	state *os.ProcessState
	start time.Time
}

type helperOpt struct {
	env     []string
	pgid    int  // join this group (with setpgid)
	setpgid bool // own group
	files   []*os.File
	noReady bool // do not wait for "ready" yet (wait-alloc)
}

func startHelper(t *testing.T, mode string, o helperOpt) *helper {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(append(os.Environ(), helperEnv+"="+mode), o.env...)
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = o.files
	if o.setpgid || o.pgid > 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: o.pgid}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{cmd: cmd, stdin: stdin, ready: make(chan string, 1), done: make(chan struct{}), start: time.Now()}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	h.pid = cmd.Process.Pid
	go func() {
		r := bufio.NewReader(stdout)
		l, _ := r.ReadString('\n')
		h.ready <- l
		_, _ = io.Copy(io.Discard, r)
		h.state, _ = cmd.Process.Wait()
		close(h.done)
	}()
	t.Cleanup(func() { h.kill() })
	if !o.noReady {
		h.waitReady(t)
	}
	return h
}

func (h *helper) waitReady(t *testing.T) {
	t.Helper()
	select {
	case l := <-h.ready:
		if strings.TrimSpace(l) != "ready" {
			t.Fatalf("helper %d: got %q", h.pid, l)
		}
	case <-time.After(60 * time.Second):
		t.Fatalf("helper %d: not ready", h.pid)
	}
}

// exited waits up to d for the helper to exit.
func (h *helper) exited(d time.Duration) bool {
	select {
	case <-h.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (h *helper) kill() {
	_ = h.cmd.Process.Signal(syscall.SIGCONT)
	_ = h.cmd.Process.Kill()
	_ = h.stdin.Close()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		_, _ = h.cmd.Process.Wait()
	}
}

func newTestBackend(t *testing.T, o LinuxOptions) *linuxBackend {
	t.Helper()
	b, err := newLinux(o)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func defaultOpts() LinuxOptions {
	return LinuxOptions{Footprint: FootprintAnon, CgroupMemLimit: true}
}

func procOf(t *testing.T, b *linuxBackend, pid int) Proc {
	t.Helper()
	ps, err := b.ListProcs(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.PID == pid {
			return p
		}
	}
	t.Fatalf("pid %d not listed", pid)
	return Proc{}
}

// waitState polls until pid reaches state.
func waitState(t *testing.T, b *linuxBackend, pid int, want func(byte) bool) Proc {
	t.Helper()
	var p Proc
	for i := 0; i < 200; i++ {
		p = procOf(t, b, pid)
		if want(p.State) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d stuck in state %c", pid, p.State)
	return p
}

// ---------------------------------------------------------------------------

func TestLinuxNewAndName(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "linux" {
		t.Errorf("Name = %q", b.Name())
	}
	if _, ok := b.(TagReader); !ok {
		t.Errorf("linux backend is not a TagReader")
	}
	if _, ok := b.(ContainerProvider); !ok {
		t.Errorf("linux backend is not a ContainerProvider")
	}
	t.Setenv("PACE_LINUX_FOOTPRINT", "bogus")
	if _, err := New(); err == nil {
		t.Errorf("bad PACE_LINUX_FOOTPRINT accepted")
	}
}

func TestLinuxSampleMachine(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	dir := t.TempDir()
	m, err := b.SampleMachine([]string{dir, "/nonexistent-pace-path"})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("machine: %+v", m)
	if m.TotalBytes <= 0 || m.AvailBytes <= 0 || m.AvailBytes > m.TotalBytes {
		t.Errorf("total/avail = %d/%d", m.TotalBytes, m.AvailBytes)
	}
	if m.NCPU != runtime.NumCPU() || m.NCPU <= 0 {
		t.Errorf("ncpu = %d", m.NCPU)
	}
	if m.KernelLevel != 0 {
		t.Errorf("kernel level = %d, want 0 (policy folds PSI)", m.KernelLevel)
	}
	for _, v := range []int64{m.PSIMemSome10, m.PSIMemFull10} {
		if v < -1 || v > 1000 {
			t.Errorf("psi = %d", v)
		}
	}
	if _, err := os.Stat("/proc/pressure/memory"); err == nil && m.PSIMemSome10 < 0 {
		t.Errorf("PSI present but some10 = -1")
	}
	if m.DiskFreeBytes <= 0 {
		t.Errorf("disk free = %d", m.DiskFreeBytes)
	}
	if m.SwapUsed < 0 || m.SwapUsed > m.SwapTotal || m.Load1Milli < 0 {
		t.Errorf("swap/load: %+v", m)
	}
	if d := time.Now().UnixMilli() - m.NowMS; d < 0 || d > 5000 {
		t.Errorf("NowMS off by %d", d)
	}
	if m2, _ := b.SampleMachine(nil); m2.DiskFreeBytes != -1 {
		t.Errorf("no disk paths: %d", m2.DiskFreeBytes)
	}
	if m3, _ := b.SampleMachine([]string{"/nonexistent-pace-path"}); m3.DiskFreeBytes != -1 {
		t.Errorf("bad disk path: %d", m3.DiskFreeBytes)
	}

	raw, _ := os.ReadFile("/proc/meminfo")
	mi, err := parseMeminfo(raw)
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getenv("PACE_LINUX_EXPECT_MEM_LIMIT"); want != "" {
		lim, _ := strconv.ParseInt(want, 10, 64)
		if m.TotalBytes != min(lim, mi.Total) {
			t.Errorf("container limit: total = %d want %d", m.TotalBytes, lim)
		}
		nolim := newTestBackend(t, LinuxOptions{Footprint: FootprintAnon})
		m4, _ := nolim.SampleMachine(nil)
		if m4.TotalBytes != mi.Total {
			t.Errorf("limit disabled: total = %d want %d", m4.TotalBytes, mi.Total)
		}
	}
	if q := QuickPressure(); q != 1 && q != 2 && q != 4 {
		t.Errorf("QuickPressure = %d", q)
	}
}

func TestQuickLevel(t *testing.T) {
	cases := []struct {
		ms   memSample
		want int
	}{
		{memSample{total: 1000, avail: 500, some10: -1, full10: -1}, 1},
		{memSample{total: 1000, avail: 140, some10: -1, full10: -1}, 2},
		{memSample{total: 1000, avail: 60, some10: -1, full10: -1}, 4},
		{memSample{total: 1000, avail: 500, some10: 100, full10: 0}, 2},
		{memSample{total: 1000, avail: 500, some10: 99, full10: 49}, 1},
		{memSample{total: 1000, avail: 500, some10: 0, full10: 50}, 4},
		{memSample{total: 1000, avail: 60, some10: 100, full10: 0}, 4},
		{memSample{total: 0, avail: 0, some10: -1, full10: -1}, 1},
	}
	for _, c := range cases {
		if got := quickLevel(c.ms); got != c.want {
			t.Errorf("quickLevel(%+v) = %d want %d", c.ms, got, c.want)
		}
	}
}

func TestLinuxListProcsSelf(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	// Burn some CPU so the accounting check is meaningful.
	end := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(end) {
	}
	p := procOf(t, b, os.Getpid())
	var ru unix.Rusage
	_ = unix.Getrusage(unix.RUSAGE_SELF, &ru)
	ruMS := ru.Utime.Sec*1000 + int64(ru.Utime.Usec)/1000 + ru.Stime.Sec*1000 + int64(ru.Stime.Usec)/1000
	t.Logf("self: %+v rusage_ms=%d", p, ruMS)
	exe, _ := os.Executable()
	if p.PPID != os.Getppid() || p.PGID != unix.Getpgrp() || p.UID != os.Getuid() {
		t.Errorf("ids: %+v", p)
	}
	if p.Exe != exe || p.System {
		t.Errorf("exe/system: %q %v (want %q)", p.Exe, p.System, exe)
	}
	if !strings.HasPrefix(filepath.Base(exe), p.Comm) || p.Comm == "" {
		t.Errorf("comm %q vs exe %q", p.Comm, exe)
	}
	if p.CPUMS < 200 || abs64(p.CPUMS-ruMS) > 150 {
		t.Errorf("cpu_ms = %d, rusage %d", p.CPUMS, ruMS)
	}
	if p.StartMS <= 0 || p.StartMS > time.Now().UnixMilli() {
		t.Errorf("start = %d", p.StartMS)
	}
	if p.Footprint <= 0 {
		t.Errorf("footprint = %d", p.Footprint)
	}
	if p.State != 'R' && p.State != 'S' {
		t.Errorf("state %c", p.State)
	}
	// Uid filter: nobody's processes are not ours.
	if os.Getuid() != 65534 {
		ps, err := b.ListProcs(65534)
		if err != nil {
			t.Fatal(err)
		}
		for _, q := range ps {
			if q.PID == os.Getpid() {
				t.Errorf("self listed for uid 65534")
			}
		}
	}
	if _, err := b.ListProcs(-1); err == nil {
		t.Errorf("uid -1 accepted")
	}
	// Sorted, unique pids; init is protected when visible.
	ps, _ := b.ListProcs(os.Getuid())
	for i := range ps {
		if i > 0 && ps[i].PID <= ps[i-1].PID {
			t.Fatalf("not sorted at %d", i)
		}
		if ps[i].PID == 1 && !ps[i].System {
			t.Errorf("pid 1 not system")
		}
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestLinuxHelperIdentity(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	h := startHelper(t, "sleep", helperOpt{setpgid: true})
	p := procOf(t, b, h.pid)
	if p.PPID != os.Getpid() || p.PGID != h.pid {
		t.Errorf("ppid/pgid: %+v", p)
	}
	// StartMS is boot time (integer seconds) + start ticks: within ~1 s.
	if d := p.StartMS - h.start.UnixMilli(); d < -1500 || d > 1500 {
		t.Errorf("start off by %d ms", d)
	}
	// Same group member.
	h2 := startHelper(t, "sleep", helperOpt{pgid: h.pid})
	if q := procOf(t, b, h2.pid); q.PGID != h.pid {
		t.Errorf("member pgid = %d want %d", q.PGID, h.pid)
	}
	// Without setpgid the child is in our group.
	h3 := startHelper(t, "sleep", helperOpt{})
	if q := procOf(t, b, h3.pid); q.PGID != unix.Getpgrp() {
		t.Errorf("inherited pgid = %d", q.PGID)
	}
	// StartMS is stable across scans and across backends.
	b2 := newTestBackend(t, defaultOpts())
	if q := procOf(t, b2, h.pid); q.StartMS != p.StartMS {
		t.Errorf("start unstable: %d vs %d", q.StartMS, p.StartMS)
	}
}

func TestLinuxCPUAccounting(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	t0 := time.Now()
	h := startHelper(t, "burn=800", helperOpt{setpgid: true})
	wall := time.Since(t0).Milliseconds()
	p := procOf(t, b, h.pid)
	t.Logf("burn 800ms: cpu_ms=%d wall=%d", p.CPUMS, wall)
	if p.CPUMS < 500 || p.CPUMS > wall+100 {
		t.Errorf("cpu_ms = %d (wall %d)", p.CPUMS, wall)
	}
	time.Sleep(200 * time.Millisecond)
	if q := procOf(t, b, h.pid); q.CPUMS < p.CPUMS || q.CPUMS > p.CPUMS+150 {
		t.Errorf("idle helper cpu went %d -> %d", p.CPUMS, q.CPUMS)
	}
}

func TestLinuxFootprintAlloc200M(t *testing.T) {
	const mb = 200
	h := startHelper(t, fmt.Sprintf("alloc=%d", mb), helperOpt{setpgid: true})
	idle := startHelper(t, "sleep", helperOpt{setpgid: true})
	want := int64(mb << 20)
	for _, m := range FootprintMethods {
		b := newTestBackend(t, LinuxOptions{Footprint: m})
		p := procOf(t, b, h.pid)
		base := procOf(t, b, idle.pid).Footprint
		dev := float64(p.Footprint-want) / float64(want) * 100
		t.Logf("MEASURE footprint method=%-8s alloc200M=%7.1fMiB (%+.1f%%) idle_helper=%5.1fMiB", m, mib(p.Footprint), dev, mib(base))
		if p.Footprint < want*9/10 || p.Footprint > want*11/10 {
			t.Errorf("%s: footprint %d not within 10%% of %d", m, p.Footprint, want)
		}
	}
}

func mib(v int64) float64 { return float64(v) / (1 << 20) }

// Two processes mapping the same 100 MiB of shared memory: rss/anon count it
// in both, pss splits it. Documents why pss is the only non-double-counting
// method.
func TestLinuxFootprintSharedMemory(t *testing.T) {
	const mb = 100
	fd, err := unix.MemfdCreate("pace-test", unix.MFD_CLOEXEC)
	if err != nil {
		t.Skipf("memfd_create: %v", err)
	}
	f := os.NewFile(uintptr(fd), "memfd")
	defer f.Close()
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	for i := 0; i < mb; i++ {
		if _, err := f.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	a := startHelper(t, fmt.Sprintf("mapfd=%d", mb), helperOpt{setpgid: true, files: []*os.File{f}})
	c := startHelper(t, fmt.Sprintf("mapfd=%d", mb), helperOpt{pgid: a.pid, files: []*os.File{f}})
	for _, m := range FootprintMethods {
		b := newTestBackend(t, LinuxOptions{Footprint: m})
		pa, pc := procOf(t, b, a.pid), procOf(t, b, c.pid)
		sum := pa.Footprint + pc.Footprint
		t.Logf("MEASURE shared100M method=%-8s each=%6.1f/%6.1fMiB sum=%6.1fMiB", m, mib(pa.Footprint), mib(pc.Footprint), mib(sum))
		switch m {
		case FootprintPSS, FootprintPSSAnon:
			if sum < mb<<20*85/100 || sum > mb<<20*125/100 {
				t.Errorf("%s: shared sum %d, want ~%d MiB", m, sum, mb)
			}
		default:
			if pa.Footprint < mb<<20*9/10 || pc.Footprint < mb<<20*9/10 {
				t.Errorf("%s: each should count the full shared mapping: %d %d", m, pa.Footprint, pc.Footprint)
			}
		}
	}
}

func TestLinuxListProcsVanishing(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = exec.Command("/bin/true").Run()
		}
	}()
	for i := 0; i < 100; i++ {
		if _, err := b.ListProcs(os.Getuid()); err != nil {
			t.Errorf("scan %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
}

func TestLinuxSignals(t *testing.T) {
	b := newTestBackend(t, defaultOpts())

	t.Run("stop-cont", func(t *testing.T) {
		h := startHelper(t, "sleep", helperOpt{setpgid: true})
		if err := b.Signal(h.pid, syscall.SIGSTOP); err != nil {
			t.Fatal(err)
		}
		waitState(t, b, h.pid, func(s byte) bool { return s == 'T' })
		if err := b.SignalGroup(h.pid, syscall.SIGCONT); err != nil {
			t.Fatal(err)
		}
		waitState(t, b, h.pid, func(s byte) bool { return s != 'T' })
	})
	t.Run("group-kill", func(t *testing.T) {
		h := startHelper(t, "sleep", helperOpt{setpgid: true})
		h2 := startHelper(t, "sleep", helperOpt{pgid: h.pid})
		if err := b.SignalGroup(h.pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if !h.exited(5*time.Second) || !h2.exited(5*time.Second) {
			t.Fatal("group not killed")
		}
		if ws := h2.state.Sys().(syscall.WaitStatus); ws.Signal() != syscall.SIGKILL {
			t.Errorf("member status %v", h2.state)
		}
		// Gone: ESRCH is not an error.
		if err := b.Signal(h.pid, syscall.SIGTERM); err != nil {
			t.Errorf("signal dead pid: %v", err)
		}
		if err := b.SignalGroup(h.pid, syscall.SIGTERM); err != nil {
			t.Errorf("signal dead group: %v", err)
		}
	})
	t.Run("ignore-term", func(t *testing.T) {
		h := startHelper(t, "ignoreterm", helperOpt{setpgid: true})
		if err := b.SignalGroup(h.pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if h.exited(300 * time.Millisecond) {
			t.Fatal("SIGTERM-ignoring helper exited")
		}
		if err := b.Signal(h.pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if !h.exited(5 * time.Second) {
			t.Fatal("SIGKILL did not kill")
		}
	})
	t.Run("refusals", func(t *testing.T) {
		pidMax := 4194304
		if raw, err := os.ReadFile("/proc/sys/kernel/pid_max"); err == nil {
			pidMax, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		}
		if err := b.Signal(pidMax+1, syscall.SIGTERM); err != nil {
			t.Errorf("nonexistent pid: %v", err)
		}
		for _, pid := range []int{-1, 0, 1, os.Getpid()} {
			if err := b.Signal(pid, 0); err == nil {
				t.Errorf("Signal(%d) accepted", pid)
			}
		}
		for _, pg := range []int{-1, 0, 1, unix.Getpgrp()} {
			if err := b.SignalGroup(pg, 0); err == nil {
				t.Errorf("SignalGroup(%d) accepted", pg)
			}
		}
		if err := b.SetBackground(1, true); err == nil {
			t.Errorf("SetBackground(1) accepted")
		}
	})
}

// schedOf returns policy and ioprio class of every thread of pid.
func schedOf(t *testing.T, pid int) (policies map[uint32]int, ioclasses map[int]int, n int) {
	t.Helper()
	ents, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
	if err != nil {
		t.Fatal(err)
	}
	policies, ioclasses = map[uint32]int{}, map[int]int{}
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
		policies[a.Policy]++
		ioclasses[ioprioClass(io)]++
		n++
	}
	return
}

func TestLinuxSetBackground(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	h := startHelper(t, "threads=4", helperOpt{setpgid: true})
	pol0, io0, n := schedOf(t, h.pid)
	if n < 5 || pol0[unix.SCHED_NORMAL] != n {
		t.Fatalf("baseline: %d threads, policies %v", n, pol0)
	}
	idle := b.canRevertIdle(h.pid, 0)
	wantCPU := uint32(unix.SCHED_BATCH)
	if idle {
		wantCPU = unix.SCHED_IDLE
	}
	t.Logf("MEASURE setbackground uid=%d cap_sys_nice=%v revertible_idle=%v -> cpu policy %d", os.Getuid(), b.capSysNice, idle, wantCPU)

	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	pol, io, n2 := schedOf(t, h.pid)
	if pol[wantCPU] != n2 || io[ioprioClassIdle] != n2 {
		t.Errorf("background: policies %v ioclasses %v of %d threads", pol, io, n2)
	}
	// Idempotent.
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	pol, io, _ = schedOf(t, h.pid)
	if !mapsEqual(pol, pol0) || !mapsEqual(io, io0) {
		t.Errorf("restore: policies %v (was %v) ioclasses %v (was %v)", pol, pol0, io, io0)
	}
	if len(b.bg) != 0 {
		t.Errorf("bg records left: %v", b.bg)
	}

	// Restore without a record (daemon restart / inherited): kernel defaults.
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	fresh := newTestBackend(t, defaultOpts())
	if err := fresh.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	if pol, io, _ = schedOf(t, h.pid); !mapsEqual(pol, pol0) || !mapsEqual(io, io0) {
		t.Errorf("fresh restore: %v %v", pol, io)
	}
	// The first backend's record is pruned once the pid is gone.
	h.kill()
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Errorf("vanished pid: %v", err)
	}
	if _, err := b.ListProcs(os.Getuid()); err != nil {
		t.Fatal(err)
	}
	if len(b.bg) != 0 {
		t.Errorf("bg records not pruned: %v", b.bg)
	}
}

func mapsEqual[K comparable](a, b map[K]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Measures what an unprivileged (or privileged) process may do reversibly to
// its child. Logged, and asserted against the kernel rules in sched(7).
func TestLinuxMeasureReversibility(t *testing.T) {
	h := startHelper(t, "sleep", helperOpt{setpgid: true})
	res := func(err error) string {
		if err == nil {
			return "ok"
		}
		return err.Error()
	}
	b := newTestBackend(t, defaultOpts())
	a, err := unix.SchedGetAttr(h.pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	a.Policy = unix.SCHED_IDLE
	toIdle := unix.SchedSetAttr(h.pid, a, 0)
	a.Policy = unix.SCHED_NORMAL
	back := unix.SchedSetAttr(h.pid, a, 0)
	t.Logf("MEASURE uid=%d SCHED_IDLE set: %s; back to SCHED_OTHER: %s", os.Getuid(), res(toIdle), res(back))
	if toIdle != nil {
		t.Errorf("SCHED_IDLE should always be allowed: %v", toIdle)
	}
	if !b.capSysNice && back == nil && !b.canRevertIdle(h.pid, 0) {
		t.Errorf("revert from SCHED_IDLE succeeded but canRevertIdle said no")
	}
	if b.capSysNice && back != nil {
		t.Errorf("CAP_SYS_NICE but revert failed: %v", back)
	}
	if back != nil { // leave the helper in a sane state for the rest
		h.kill()
		h = startHelper(t, "sleep", helperOpt{setpgid: true})
	}

	a, _ = unix.SchedGetAttr(h.pid, 0)
	a.Policy = unix.SCHED_BATCH
	toBatch := unix.SchedSetAttr(h.pid, a, 0)
	a.Policy = unix.SCHED_NORMAL
	fromBatch := unix.SchedSetAttr(h.pid, a, 0)
	t.Logf("MEASURE SCHED_BATCH set: %s; back: %s", res(toBatch), res(fromBatch))
	if toBatch != nil || fromBatch != nil {
		t.Errorf("SCHED_BATCH must be reversible: %v %v", toBatch, fromBatch)
	}

	io0, _ := ioprioGet(h.pid)
	ioIdle := ioprioSet(h.pid, ioprioClassIdle<<ioprioClassShift)
	ioBack := ioprioSet(h.pid, io0)
	io1, _ := ioprioGet(h.pid)
	t.Logf("MEASURE ioprio IDLE set: %s; back to %#x: %s (now %#x)", res(ioIdle), io0, res(ioBack), io1)
	if ioIdle != nil || ioBack != nil || io1 != io0 {
		t.Errorf("ioprio must be reversible: %v %v %#x", ioIdle, ioBack, io1)
	}

	up := unix.Setpriority(unix.PRIO_PROCESS, h.pid, 10)
	down := unix.Setpriority(unix.PRIO_PROCESS, h.pid, 0)
	t.Logf("MEASURE nice 0->10: %s; 10->0: %s", res(up), res(down))
	if up != nil {
		t.Errorf("raising nice must be allowed: %v", up)
	}
	if !b.capSysNice && down == nil {
		t.Logf("note: nice lowered without CAP_SYS_NICE (RLIMIT_NICE permits it)")
	}
}

func TestLinuxJobTag(t *testing.T) {
	b := newTestBackend(t, defaultOpts())
	tagged := startHelper(t, "sleep", helperOpt{setpgid: true, env: []string{"PACE_JOB_ID=job-xyz"}})
	plain := startHelper(t, "sleep", helperOpt{setpgid: true, env: []string{"NOT_PACE_JOB_ID=job-no"}})
	if got := b.JobTag(tagged.pid); got != "job-xyz" {
		t.Errorf("tagged = %q", got)
	}
	if got := b.JobTag(plain.pid); got != "" && got != os.Getenv("PACE_JOB_ID") {
		t.Errorf("plain = %q", got)
	}
	plain.kill()
	if got := b.JobTag(plain.pid); got != "" {
		t.Errorf("dead = %q", got)
	}
	if got := b.JobTag(0); got != "" {
		t.Errorf("pid 0 = %q", got)
	}
}

// ---------------------------------------------------------------------------
// cgroup v2 containers.

var cgDanceOnce sync.Once

// cgroupDance prepares a privileged container with a private cgroup
// namespace: everything in the namespace root moves into a leaf so the root
// can enable the memory controller (no-internal-processes rule). Guarded to
// run only inside a container we were told about.
func cgroupDance(t *testing.T) {
	cgDanceOnce.Do(func() {
		if os.Getenv("PACE_LINUX_IN_DOCKER") != "1" || os.Geteuid() != 0 {
			return
		}
		if _, err := os.Stat("/.dockerenv"); err != nil {
			return
		}
		self, _ := os.ReadFile("/proc/self/cgroup")
		if strings.TrimSpace(string(self)) != "0::/" {
			return
		}
		root := "/sys/fs/cgroup"
		sc, _ := os.ReadFile(root + "/cgroup.subtree_control")
		if hasWord(sc, "memory") {
			return
		}
		leaf := root + "/pace-test-init"
		if err := os.MkdirAll(leaf, 0o755); err != nil {
			t.Logf("dance: %v", err)
			return
		}
		var buf []byte
		pids, _ := cgPids(root+"/cgroup.procs", &buf)
		for _, p := range pids {
			if err := cgWrite(leaf+"/cgroup.procs", strconv.Itoa(p)); err != nil {
				t.Logf("dance: move %d: %v", p, err)
			}
		}
		if err := cgWrite(root+"/cgroup.subtree_control", "+memory"); err != nil {
			t.Logf("dance: enable memory: %v", err)
		}
		// As a systemd Delegate=yes unit would: the container throttle's
		// cpu.weight/cpu.max/io.weight need them (best effort).
		_ = cgWrite(root+"/cgroup.subtree_control", "+cpu")
		_ = cgWrite(root+"/cgroup.subtree_control", "+io")
	})
}

func containersOrSkip(t *testing.T) (*linuxBackend, *cgroupContainer) {
	t.Helper()
	cgroupDance(t)
	b := newTestBackend(t, defaultOpts())
	jc, ok := b.Containers("")
	if !ok {
		if os.Getenv("PACE_LINUX_REQUIRE_CGROUP") == "1" {
			t.Fatalf("containers unusable: %s", b.ContainersReason())
		}
		t.Skipf("containers unusable: %s", b.ContainersReason())
	}
	c := jc.(*cgroupContainer)
	t.Logf("cgroup base %s", c.Base())
	return b, c
}

func readTrim(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func waitEvent(t *testing.T, dir, kv string) {
	t.Helper()
	for i := 0; i < 300; i++ {
		raw, _ := os.ReadFile(dir + "/cgroup.events")
		for _, l := range strings.Split(string(raw), "\n") {
			if l == kv {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(dir + "/cgroup.events")
	t.Fatalf("%s: never %q (events %q)", dir, kv, raw)
}

func TestLinuxCgroupContainer(t *testing.T) {
	b, c := containersOrSkip(t)
	id := fmt.Sprintf("t%d", os.Getpid())
	t.Cleanup(func() { _ = c.Kill(id); _ = c.Remove(id) })

	path, err := c.Create(id, 300<<20)
	if err != nil {
		t.Fatal(err)
	}
	if path != c.Base()+"/job-"+id {
		t.Errorf("path = %s", path)
	}
	if v := readTrim(t, path+"/memory.max"); v != strconv.Itoa(300<<20) {
		t.Errorf("memory.max = %s", v)
	}
	if v := readTrim(t, path+"/memory.high"); v != "max" {
		t.Errorf("memory.high = %s, want max (default)", v)
	}
	// With a high percentage the kernel stores it rounded down to a page.
	c90 := &cgroupContainer{base: c.Base(), highPct: 90}
	if _, err := c90.Create(id, 300<<20); err != nil {
		t.Fatal(err)
	}
	hv, _ := strconv.ParseInt(readTrim(t, path+"/memory.high"), 10, 64)
	if want := int64(300<<20) / 100 * 90; hv > want || hv <= want-int64(os.Getpagesize()) {
		t.Errorf("memory.high = %d, want ~%d", hv, want)
	}
	if v := readTrim(t, path+"/memory.swap.max"); v != "0" {
		t.Errorf("memory.swap.max = %s", v)
	}
	if v := readTrim(t, path+"/memory.oom.group"); v != "1" {
		t.Errorf("oom.group = %s", v)
	}
	// Re-create is idempotent and can lift the limit.
	if _, err := c.Create(id, 0); err != nil {
		t.Fatal(err)
	}
	if v := readTrim(t, path+"/memory.max"); v != "max" {
		t.Errorf("unlimited memory.max = %s", v)
	}
	if _, err := c.Create(id, 300<<20); err != nil {
		t.Fatal(err)
	}

	// Attach BEFORE the helper allocates: cgroup v2 charges pages to the
	// cgroup that faulted them, and charges do not move with the process.
	h := startHelper(t, "wait-alloc=100", helperOpt{setpgid: true, noReady: true})
	if err := c.Attach(id, h.pid); err != nil {
		t.Fatal(err)
	}
	if procs := readTrim(t, path+"/cgroup.procs"); procs != strconv.Itoa(h.pid) {
		t.Errorf("cgroup.procs = %q", procs)
	}
	if _, err := io.WriteString(h.stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	h.waitReady(t)
	u, err := c.Usage(id)
	if err != nil {
		t.Fatal(err)
	}
	fp := procOf(t, b, h.pid).Footprint
	t.Logf("MEASURE cgroup usage=%.1fMiB footprint(anon)=%.1fMiB memory.current=%s", mib(u), mib(fp), readTrim(t, path+"/memory.current"))
	if u < 95<<20 || u > 115<<20 {
		t.Errorf("usage %d not ~100 MiB", u)
	}

	if err := c.Freeze(id, true); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, path, "frozen 1")
	if err := c.Freeze(id, false); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, path, "frozen 0")

	// Remove refuses while a live process is inside (after its 1 s wait).
	if err := c.Remove(id); err == nil {
		t.Fatal("Remove with a live member succeeded")
	}
	if err := c.Kill(id); err != nil {
		t.Fatal(err)
	}
	if !h.exited(5 * time.Second) {
		t.Fatal("Kill did not kill")
	}
	if ws := h.state.Sys().(syscall.WaitStatus); ws.Signal() != syscall.SIGKILL {
		t.Errorf("status %v", h.state)
	}
	waitEvent(t, path, "populated 0")
	if err := c.Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dir still there: %v", err)
	}
	// Idempotent / missing job behaviour.
	if err := c.Remove(id); err != nil {
		t.Errorf("Remove missing: %v", err)
	}
	if err := c.Kill(id); err != nil {
		t.Errorf("Kill missing: %v", err)
	}
	if _, err := c.Usage(id); err == nil {
		t.Errorf("Usage missing: no error")
	}
	if err := c.Freeze(id, true); err == nil {
		t.Errorf("Freeze missing: no error")
	}
	if err := c.Attach(id, os.Getpid()); err == nil {
		t.Errorf("Attach to missing job succeeded")
	}
}

func TestLinuxCgroupKernelEnforcesMax(t *testing.T) {
	_, c := containersOrSkip(t)
	// Default (memory.high = max): over-cap allocation is OOM-killed fast.
	d, killed := oomRun(t, c, 0, 60*time.Second)
	t.Logf("MEASURE oom high=max: killed=%v after %v", killed, d)
	if !killed {
		t.Fatal("helper over memory.max was not OOM-killed")
	}
	// memory.high = 90 %: measured only (the reason the default is max).
	d, killed = oomRun(t, c, 90, 15*time.Second)
	t.Logf("MEASURE oom high=90%%: killed=%v after %v (no swap: reclaim throttling instead of a kill)", killed, d)
}

// oomRun caps a job at 64 MiB, makes it allocate 200 MiB and reports whether
// (and when) the kernel killed it within limit. The helper is always killed.
func oomRun(t *testing.T, base *cgroupContainer, highPct int, limit time.Duration) (time.Duration, bool) {
	t.Helper()
	c := &cgroupContainer{base: base.Base(), highPct: highPct}
	id := fmt.Sprintf("oom%d-%d", os.Getpid(), highPct)
	defer func() { _ = c.Kill(id); _ = c.Remove(id) }()
	path, err := c.Create(id, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	h := startHelper(t, "wait-alloc=200", helperOpt{setpgid: true, noReady: true})
	if err := c.Attach(id, h.pid); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	_, _ = io.WriteString(h.stdin, "go\n")
	if !h.exited(limit) {
		_ = c.Kill(id)
		h.exited(5 * time.Second)
		return time.Since(t0), false
	}
	d := time.Since(t0)
	if ws := h.state.Sys().(syscall.WaitStatus); ws.Signal() != syscall.SIGKILL {
		t.Errorf("status %v", h.state)
	}
	n, err := c.OOMKills(id)
	if err != nil || n < 1 {
		t.Errorf("oom_kill = %d, %v (events %q)", n, err, readTrim(t, path+"/memory.events"))
	}
	waitEvent(t, path, "populated 0")
	return d, true
}

func TestLinuxCgroupKillTree(t *testing.T) {
	_, c := containersOrSkip(t)
	id := fmt.Sprintf("tree%d", os.Getpid())
	t.Cleanup(func() { _ = c.Kill(id); _ = c.Remove(id) })
	path, err := c.Create(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Members in different process groups (an escaped setsid child): the
	// cgroup still kills all of them.
	var hs []*helper
	for i := 0; i < 3; i++ {
		h := startHelper(t, "sleep", helperOpt{setpgid: true})
		if err := c.Attach(id, h.pid); err != nil {
			t.Fatal(err)
		}
		hs = append(hs, h)
	}
	if err := c.Kill(id); err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if !h.exited(5 * time.Second) {
			t.Fatalf("member %d survived", h.pid)
		}
	}
	waitEvent(t, path, "populated 0")
	if err := c.Remove(id); err != nil {
		t.Fatal(err)
	}
}

func TestLinuxCgroupValidation(t *testing.T) {
	_, c := containersOrSkip(t)
	for _, bad := range []string{"", ".hidden", "a/b", "../x", "x y", strings.Repeat("a", 65), "ü"} {
		if _, err := c.Create(bad, 0); err == nil {
			t.Errorf("Create(%q) accepted", bad)
		}
	}
	if _, err := c.Create("neg", -1); err == nil {
		t.Errorf("negative limit accepted")
	}
	id := fmt.Sprintf("v%d", os.Getpid())
	if _, err := c.Create(id, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Remove(id) })
	if err := c.Attach(id, 0); err == nil {
		t.Errorf("Attach pid 0 accepted")
	}
}

// ---------------------------------------------------------------------------
// Measurements (PACE_LINUX_MEASURE=1).

func TestLinuxMeasureListProcsCost(t *testing.T) {
	if os.Getenv("PACE_LINUX_MEASURE") != "1" {
		t.Skip("set PACE_LINUX_MEASURE=1")
	}
	const n = 300
	var cmds []*exec.Cmd
	t.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	})
	for i := 0; i < n; i++ {
		c := exec.Command("/bin/sleep", "600")
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, c)
	}
	big := startHelper(t, "alloc=200", helperOpt{setpgid: true})
	time.Sleep(200 * time.Millisecond)

	for _, m := range FootprintMethods {
		b := newTestBackend(t, LinuxOptions{Footprint: m})
		ps, _ := b.ListProcs(os.Getuid()) // warm the exe cache
		const iters = 30
		durs := make([]time.Duration, iters)
		for i := range durs {
			t0 := time.Now()
			if _, err := b.ListProcs(os.Getuid()); err != nil {
				t.Fatal(err)
			}
			durs[i] = time.Since(t0)
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		med := durs[iters/2]
		t.Logf("MEASURE ListProcs method=%-8s procs=%d median=%v p90=%v per_proc=%.1fus", m, len(ps), med, durs[iters*9/10], float64(med.Microseconds())/float64(len(ps)))
	}
	// Cold scan (no exe cache) for the default method.
	cold := newTestBackend(t, defaultOpts())
	t0 := time.Now()
	ps, _ := cold.ListProcs(os.Getuid())
	t.Logf("MEASURE ListProcs cold (exe readlink for all) procs=%d %v", len(ps), time.Since(t0))

	// Per-file read cost: a small process vs the 200 MiB helper.
	var buf []byte
	for _, pid := range []int{cmds[0].Process.Pid, big.pid} {
		for _, f := range []string{"stat", "statm", "status", "smaps_rollup", "environ"} {
			p := fmt.Sprintf("/proc/%d/%s", pid, f)
			const iters = 200
			t0 := time.Now()
			for i := 0; i < iters; i++ {
				if _, err := readPath(p, &buf, maxFileBuf); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("MEASURE read pid=%d %-13s %.1fus", pid, f, float64(time.Since(t0).Microseconds())/iters)
		}
	}
	// JobTag and SampleMachine cost.
	b := newTestBackend(t, defaultOpts())
	t0 = time.Now()
	for i := 0; i < 200; i++ {
		_ = b.JobTag(big.pid)
	}
	t.Logf("MEASURE JobTag %.1fus", float64(time.Since(t0).Microseconds())/200)
	t0 = time.Now()
	for i := 0; i < 200; i++ {
		_, _ = b.SampleMachine([]string{"/tmp"})
	}
	t.Logf("MEASURE SampleMachine %.1fus", float64(time.Since(t0).Microseconds())/200)
	t0 = time.Now()
	for i := 0; i < 200; i++ {
		_ = QuickPressure()
	}
	t.Logf("MEASURE QuickPressure %.1fus", float64(time.Since(t0).Microseconds())/200)
}

func BenchmarkListProcs(b *testing.B) {
	for _, m := range FootprintMethods {
		b.Run(string(m), func(b *testing.B) {
			be, err := newLinux(LinuxOptions{Footprint: m})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := be.ListProcs(os.Getuid()); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
