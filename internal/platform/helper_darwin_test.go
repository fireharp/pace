package platform

// Helper subprocesses for the darwin tests. The test binary re-executes
// itself with PACE_HELPER=<mode>; init() below dispatches before the testing
// framework starts (no TestMain, so other test files may define one).
//
// Every helper exits on stdin EOF (the test holds the write end; all
// descendants inherit the read end) and after helperLifetime at the latest,
// so nothing outlives a crashed test run.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const helperLifetime = 90 * time.Second

func init() {
	mode := os.Getenv("PACE_HELPER")
	if mode == "" {
		return
	}
	runHelper(mode)
	os.Exit(0)
}

// stdinLines delivers stdin lines; it is closed on EOF.
func stdinLines() <-chan string {
	ch := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			ch <- sc.Text()
		}
		close(ch)
	}()
	return ch
}

func helperWait(lines <-chan string) {
	deadline := time.After(helperLifetime)
	for {
		select {
		case _, ok := <-lines:
			if !ok {
				return
			}
		case <-deadline:
			return
		}
	}
}

func helperWaitFor(lines <-chan string, want string) bool {
	deadline := time.After(helperLifetime)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return false
			}
			if l == want {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func helperSpawnSleep(attr *syscall.SysProcAttr) int {
	c := exec.Command(os.Args[0], "-test.run=^$")
	c.Env = append(cleanEnv(), "PACE_HELPER=sleep")
	c.Stdin = os.Stdin
	c.SysProcAttr = attr
	if err := c.Start(); err != nil {
		fmt.Println("error", err)
		os.Exit(3)
	}
	return c.Process.Pid
}

func cpuSelfMS() int64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return (ru.Utime.Sec+ru.Stime.Sec)*1000 + int64(ru.Utime.Usec+ru.Stime.Usec)/1000
}

var burnSink uint64

func runHelper(mode string) {
	lines := stdinLines()
	name, arg, _ := strings.Cut(mode, ":")
	switch name {
	case "sleep":
		fmt.Println("ready")
		helperWait(lines)
	case "alloc":
		mib, err := strconv.Atoi(strings.TrimSuffix(arg, "M"))
		if err != nil || mib <= 0 || mib > 1024 { // never more than 1 GiB
			fmt.Println("error bad size")
			os.Exit(2)
		}
		// Anonymous mmap rather than the Go heap: under -race the heap is
		// shadowed (x3 footprint, measured) but foreign mappings are not.
		buf, err := unix.Mmap(-1, 0, mib<<20, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
		if err != nil {
			fmt.Println("error mmap", err)
			os.Exit(2)
		}
		for i := 0; i < len(buf); i += 4096 {
			buf[i] = byte(i>>12) | 1
		}
		fmt.Println("ready")
		helperWait(lines)
		burnSink += uint64(buf[len(buf)-4096])
		_ = unix.Munmap(buf)
	case "burn":
		ms, err := strconv.Atoi(arg)
		if err != nil || ms <= 0 || ms > 10000 {
			fmt.Println("error bad duration")
			os.Exit(2)
		}
		fmt.Println("ready")
		if !helperWaitFor(lines, "go") {
			return
		}
		start := cpuSelfMS()
		x := uint64(1)
		for cpuSelfMS()-start < int64(ms) {
			for i := 0; i < 1<<16; i++ {
				x = x*6364136223846793005 + 1442695040888963407
			}
		}
		burnSink += x
		fmt.Println("done")
		helperWait(lines)
	case "group": // one more member of our own process group
		pid := helperSpawnSleep(nil)
		fmt.Println("ready", pid)
		helperWait(lines)
	case "setsid": // a grandchild that escapes into its own session and group
		pid := helperSpawnSleep(&syscall.SysProcAttr{Setsid: true})
		fmt.Println("ready", pid)
		helperWait(lines)
	case "exec":
		fmt.Println("ready")
		if !helperWaitFor(lines, "go") {
			return
		}
		_ = syscall.Exec("/bin/sleep", []string{"sleep", "60"}, cleanEnv())
		os.Exit(4)
	default:
		fmt.Println("error unknown mode", mode)
		os.Exit(2)
	}
}

// cleanEnv is the environment without governor/helper variables.
func cleanEnv() []string {
	var out []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "PACE_HELPER=") || strings.HasPrefix(e, "PACE_JOB_ID=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// parent side

type helper struct {
	t       *testing.T
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan string
	pid     int
	spawned time.Time
	waited  bool
}

type spawnOpts struct {
	mode string
	env  []string
	attr *syscall.SysProcAttr
	path string // non-empty: run this binary instead of a helper
	args []string
}

func spawn(t *testing.T, o spawnOpts) *helper {
	t.Helper()
	var c *exec.Cmd
	if o.path != "" {
		c = exec.Command(o.path, o.args...)
		c.Env = append(cleanEnv(), o.env...)
	} else {
		c = exec.Command(os.Args[0], "-test.run=^$")
		c.Env = append(append(cleanEnv(), "PACE_HELPER="+o.mode), o.env...)
	}
	c.SysProcAttr = o.attr
	c.Stderr = os.Stderr
	in, err := c.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{t: t, cmd: c, stdin: in, lines: make(chan string, 16), spawned: time.Now()}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	h.pid = c.Process.Pid
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			h.lines <- sc.Text()
		}
		close(h.lines)
	}()
	t.Cleanup(h.stop)
	return h
}

// stop closes stdin, kills and reaps the helper (idempotent).
func (h *helper) stop() {
	_ = h.stdin.Close()
	if h.waited {
		return
	}
	_ = h.cmd.Process.Kill()
	_ = h.cmd.Wait()
	h.waited = true
}

// expect waits for a line with the given prefix and returns its fields.
func (h *helper) expect(prefix string) []string {
	h.t.Helper()
	timeout := time.After(20 * time.Second)
	for {
		select {
		case l, ok := <-h.lines:
			if !ok {
				h.t.Fatalf("helper %d exited before %q", h.pid, prefix)
			}
			if strings.HasPrefix(l, "error") {
				h.t.Fatalf("helper %d: %s", h.pid, l)
			}
			if strings.HasPrefix(l, prefix) {
				return strings.Fields(l)
			}
		case <-timeout:
			h.t.Fatalf("timeout waiting for %q from helper %d", prefix, h.pid)
		}
	}
}

func (h *helper) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.stdin, line+"\n"); err != nil {
		h.t.Fatal(err)
	}
}

// killIfSame SIGKILLs pid only if it is still the process we saw (same start
// time and uid), so a recycled pid is never touched.
func killIfSame(pid int, startMS int64) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(k.Proc.P_pid) != pid {
		return
	}
	st := (k.Proc.P_starttime.Sec*1e6 + int64(k.Proc.P_starttime.Usec)) / 1000
	if st != startMS || int(k.Eproc.Ucred.Uid) != os.Getuid() {
		return
	}
	_ = unix.Kill(pid, unix.SIGKILL)
}
