package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fireharp/pace/internal/units"
)

func newEnv() (*env, *bytes.Buffer, *bytes.Buffer) {
	var o, e bytes.Buffer
	return &env{stdout: &o, stderr: &e, self: "/nonexistent/pacetest"}, &o, &e
}

func TestParseAlloc(t *testing.T) {
	cases := []struct {
		args []string
		ok   bool
		mem  int64
		step int64
	}{
		{[]string{"--mem", "50M"}, true, 50 << 20, 50 << 20},
		{[]string{"--mem", "1G", "--step", "256M", "--every", "100ms"}, true, 1 << 30, 256 << 20},
		{[]string{"--mem", "100M", "--step", "1G"}, true, 100 << 20, 100 << 20}, // step capped at mem
		{[]string{"--mem", "8G"}, true, 8 << 30, 8 << 30},
		{[]string{"--mem", "9G", "--i-know"}, true, 9 << 30, 9 << 30},
		{[]string{"--mem", "9G"}, false, 0, 0}, // cap
		{[]string{"--mem", "8193M"}, false, 0, 0},
		{[]string{}, false, 0, 0},
		{[]string{"--mem", "x"}, false, 0, 0},
		{[]string{"--mem", "1G", "--step", "0"}, false, 0, 0},
		{[]string{"--mem", "1G", "--fill", "zero"}, false, 0, 0},
		{[]string{"--mem", "1G", "--hold", "soon"}, false, 0, 0},
		{[]string{"--mem", "1G", "extra"}, false, 0, 0},
	}
	for _, c := range cases {
		e, _, _ := newEnv()
		o, code, done := e.parseAlloc(c.args)
		if c.ok {
			if done {
				t.Errorf("%v: unexpected failure code %d", c.args, code)
				continue
			}
			if o.mem.v != c.mem || o.step.v != c.step {
				t.Errorf("%v: mem=%d step=%d", c.args, o.mem.v, o.step.v)
			}
		} else if !done || code != exitUsage {
			t.Errorf("%v: want usage error, got done=%v code=%d", c.args, done, code)
		}
	}
	// Defaults.
	e, _, _ := newEnv()
	o, _, _ := e.parseAlloc([]string{"--mem", "1M"})
	if !o.hold.forever || o.printEvery.d != time.Second || o.fill != "random" || o.maxLife.d != time.Hour || o.maxLife.forever {
		t.Fatalf("defaults %+v", o)
	}
}

func TestParseSpike(t *testing.T) {
	e, _, _ := newEnv()
	o, _, done := e.parseSpike([]string{"--base", "10M", "--peak", "30M", "--at", "100ms"})
	if done || o.base.v != 10<<20 || o.peak.v != 30<<20 || o.at.d != 100*time.Millisecond {
		t.Fatalf("spike %+v", o)
	}
	for _, bad := range [][]string{
		{"--base", "10M", "--peak", "5M", "--at", "1s"},
		{"--peak", "5M", "--at", "1s"},
		{"--base", "1M", "--peak", "9G", "--at", "1s"},
		{"--base", "1M", "--peak", "2M", "--at", "forever"},
	} {
		e, _, _ := newEnv()
		if _, code, done := e.parseSpike(bad); !done || code != exitUsage {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestDurVal(t *testing.T) {
	for in, want := range map[string]durVal{
		"forever": {forever: true}, "inf": {forever: true}, "-1": {forever: true},
		"0": {}, "40": {d: 40 * time.Second}, "250ms": {d: 250 * time.Millisecond}, "1h0m0s": {d: time.Hour},
	} {
		var d durVal
		if err := d.Set(in); err != nil || d != want {
			t.Errorf("Set(%q) = %+v %v", in, d, err)
		}
		// String round-trips through Set (used to pass flags to children).
		var d2 durVal
		if err := d2.Set(d.String()); err != nil || d2 != d {
			t.Errorf("round trip %q -> %q -> %+v", in, d.String(), d2)
		}
	}
	var d durVal
	if d.Set("nope") == nil || d.Set("-5s") == nil {
		t.Fatal("bad durations accepted")
	}
}

func TestParseSignal(t *testing.T) {
	for in, want := range map[string]syscall.Signal{"TERM": syscall.SIGTERM, "sigkill": syscall.SIGKILL, "9": syscall.SIGKILL, "SIGINT": syscall.SIGINT} {
		if got, err := parseSignal(in); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "FOO", "0", "99"} {
		if _, err := parseSignal(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestUsageErrorsInProcess(t *testing.T) {
	for _, args := range [][]string{
		{}, {"bogus"}, {"exit"}, {"exit", "256"}, {"exit", "-1"}, {"exit", "x"}, {"exit", "1", "2"},
		{"exit", "1", "--signal", "TERM"}, {"exit", "--signal", "FOO"}, {"burn", "--cpus", "0"},
		{"print", "--every", "0"}, {"tree", "--n", "0", "--mem", "1M"}, {"tree", "--mem", "5G", "--n", "2"},
		{"tree", "--n", "2"}, {"escape"}, {"escape", "--mem", "9G"}, {"hang", "--for", "x"}, {"alloc", "--mem", "9G"},
	} {
		e, _, _ := newEnv()
		if code := e.main(args); code != exitUsage {
			t.Errorf("%v: exit %d, want %d", args, code, exitUsage)
		}
	}
	e, out, _ := newEnv()
	if code := e.main([]string{"help"}); code != 0 || !strings.Contains(out.String(), "Commands:") {
		t.Fatal("help")
	}
	for _, code := range []int{0, 1, 7, 200, 255} {
		e, _, _ := newEnv()
		if got := e.main([]string{"exit", strconv.Itoa(code)}); got != code {
			t.Errorf("exit %d returned %d", code, got)
		}
	}
}

func TestArenaGrow(t *testing.T) {
	for _, fill := range []string{"random", "page"} {
		a := newArena(fill)
		a.grow(3*chunk + 123)
		if a.allocated() != 3*chunk+123 || len(a.chunks) != 4 {
			t.Fatalf("%s: allocated %d in %d chunks", fill, a.allocated(), len(a.chunks))
		}
		// Every page was touched.
		for _, c := range a.chunks {
			for i := 0; i < len(c); i += 4096 {
				if fill == "page" && c[i] != 1 {
					t.Fatalf("%s: page %d untouched", fill, i)
				}
			}
		}
	}
	// Random fill is not trivially compressible: few zero bytes.
	a := newArena("random")
	a.grow(1 << 20)
	zeros := bytes.Count(a.chunks[0], []byte{0})
	if zeros > (1<<20)/128 {
		t.Fatalf("random fill has %d zero bytes per MiB", zeros)
	}
}

// ---- process-level tests (spawn the real binary) ----

var (
	buildOnce sync.Once
	bin       string
	buildErr  error
)

func pacetest(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("spawns processes")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pacetest-bin-")
		if err != nil {
			buildErr = err
			return
		}
		bin = filepath.Join(dir, "pacetest")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return bin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if bin != "" {
		os.RemoveAll(filepath.Dir(bin))
	}
	os.Exit(code)
}

// start runs pacetest with args; the process is SIGKILLed at cleanup.
func start(t *testing.T, args ...string) (*exec.Cmd, *bufio.Scanner, *bytes.Buffer) {
	t.Helper()
	cmd := exec.Command(pacetest(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd, bufio.NewScanner(out), &stderr
}

func run(t *testing.T, args ...string) (string, *os.ProcessState) {
	t.Helper()
	cmd := exec.Command(pacetest(t), args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	_ = cmd.Run()
	return stdout.String(), cmd.ProcessState
}

func maxRSS(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "linux" {
		return ru.Maxrss * 1024 // KiB on Linux
	}
	return ru.Maxrss // bytes on darwin
}

func TestAllocTouchesMemory(t *testing.T) {
	out, ps := run(t, "alloc", "--mem", "50M", "--step", "20M", "--every", "10ms", "--hold", "0", "--print-every", "0")
	if ps.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", ps.ExitCode(), out)
	}
	if !strings.Contains(out, "pacetest: alloc start pid=") || !strings.Contains(out, "pacetest: alloc done allocated=50M") {
		t.Fatalf("output:\n%s", out)
	}
	rss := maxRSS(ps)
	if rss < 50<<20 {
		t.Fatalf("max RSS %s < 50M: pages not touched", units.FormatBytes(rss))
	}
	if rss > 150<<20 {
		t.Fatalf("max RSS %s far above the request", units.FormatBytes(rss))
	}
}

func TestAllocHoldPrintsAndIsKillable(t *testing.T) {
	cmd, sc, _ := start(t, "alloc", "--mem", "20M", "--print-every", "50ms", "--fill", "page")
	holds := 0
	deadline := time.Now().Add(5 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		if strings.HasPrefix(sc.Text(), "pacetest: hold allocated=20M") {
			holds++
			if holds == 2 {
				break
			}
		}
	}
	if holds < 2 {
		t.Fatal("no hold progress lines")
	}
	cmd.Process.Signal(syscall.SIGTERM)
	err := cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("want death by SIGTERM, got %v", err)
	}
}

func TestExitCodesAndSignals(t *testing.T) {
	for _, code := range []int{0, 3, 201} {
		_, ps := run(t, "exit", strconv.Itoa(code))
		if ps.ExitCode() != code {
			t.Errorf("exit %d -> %d", code, ps.ExitCode())
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		_, ps := run(t, "exit", "--signal", strconv.Itoa(int(sig)))
		ws := ps.Sys().(syscall.WaitStatus)
		if !ws.Signaled() || ws.Signal() != sig {
			t.Errorf("exit --signal %v: %v", sig, ws)
		}
	}
}

func TestIgnoreTerm(t *testing.T) {
	cmd, sc, _ := start(t, "ignore-term", "--for", "20s")
	if !sc.Scan() || !strings.HasPrefix(sc.Text(), "pacetest: ignoring SIGTERM pid=") {
		t.Fatalf("no ready line")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("process died of SIGTERM: %v", err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("want SIGKILL death, got %v", ws)
	}
}

func TestEscapeChildLeavesSessionAndGroup(t *testing.T) {
	out, ps := run(t, "escape", "--mem", "10M", "--hold", "30s", "--max-life", "60s")
	if ps.ExitCode() != 0 {
		t.Fatalf("parent exit %d: %s", ps.ExitCode(), out)
	}
	m := regexp.MustCompile(`pacetest: escape child=(\d+) mem=10M`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no child line: %q", out)
	}
	child, _ := strconv.Atoi(m[1])
	// Kill the child we spawned, whatever happens below.
	t.Cleanup(func() {
		syscall.Kill(child, syscall.SIGKILL)
	})
	if err := syscall.Kill(child, 0); err != nil {
		t.Fatalf("escaped child %d not alive: %v", child, err)
	}
	sid, err := unix.Getsid(child)
	if err != nil {
		t.Fatal(err)
	}
	if sid != child {
		t.Fatalf("child sid %d, want its own session %d", sid, child)
	}
	pgid, _ := syscall.Getpgid(child)
	if pgid == syscall.Getpgrp() {
		t.Fatal("child still in the test's process group")
	}
	syscall.Kill(child, syscall.SIGKILL)
	// Wait until it is gone (it is reparented to init, which reaps it).
	for i := 0; i < 100; i++ {
		if syscall.Kill(child, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("escaped child %d survived SIGKILL", child)
}

func TestEscapeLinger(t *testing.T) {
	start := time.Now()
	out, ps := run(t, "escape", "--mem", "1M", "--hold", "30s", "--linger", "400ms", "--clear-env", "--max-life", "60s")
	if el := time.Since(start); el < 400*time.Millisecond {
		t.Fatalf("parent exited after %v, before --linger", el)
	}
	if ps.ExitCode() != 0 {
		t.Fatalf("parent exit %d: %s", ps.ExitCode(), out)
	}
	m := regexp.MustCompile(`pacetest: escape child=(\d+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no child line: %q", out)
	}
	child, _ := strconv.Atoi(m[1])
	syscall.Kill(child, syscall.SIGKILL)
}

func TestTree(t *testing.T) {
	out, ps := run(t, "tree", "--n", "3", "--mem", "8M", "--hold", "0")
	if ps.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", ps.ExitCode(), out)
	}
	if strings.Count(out, "pacetest: tree child=") != 3 || strings.Count(out, "pacetest: alloc done allocated=8M") != 3 {
		t.Fatalf("output:\n%s", out)
	}
}

func TestTreeDiesWithGroupSignal(t *testing.T) {
	cmd := exec.Command(pacetest(t), "tree", "--n", "2", "--mem", "4M", "--max-life", "30s")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pgid, syscall.SIGKILL) })
	sc := bufio.NewScanner(out)
	done := 0
	for sc.Scan() {
		if strings.Contains(sc.Text(), "alloc done") {
			done++
			if done == 2 {
				break
			}
		}
	}
	syscall.Kill(-pgid, syscall.SIGTERM)
	cmd.Wait()
	ws := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("tree parent: %v", ws)
	}
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatal("children survived the group SIGTERM")
	}
}

func TestBurnHangPrint(t *testing.T) {
	_, ps := run(t, "burn", "--cpus", "1", "--for", "300ms")
	// 300ms of burning: at least 50ms of CPU even on a loaded machine (a
	// stricter bound failed under load 9 on 12 CPUs with 134ms)
	if ps.ExitCode() != 0 || ps.UserTime() < 50*time.Millisecond {
		t.Fatalf("burn: exit %d user %v", ps.ExitCode(), ps.UserTime())
	}
	t0 := time.Now()
	out, ps := run(t, "hang", "--for", "200ms")
	if ps.ExitCode() != 0 || out != "" || time.Since(t0) < 200*time.Millisecond || ps.UserTime()+ps.SystemTime() > 100*time.Millisecond {
		t.Fatalf("hang: exit %d out %q cpu %v", ps.ExitCode(), out, ps.UserTime()+ps.SystemTime())
	}
	out, ps = run(t, "print", "--every", "40ms", "--for", "300ms")
	if n := strings.Count(out, "pacetest: tick n="); ps.ExitCode() != 0 || n < 4 || n > 8 {
		t.Fatalf("print: %d ticks:\n%s", n, out)
	}
}

func TestMaxLife(t *testing.T) {
	t0 := time.Now()
	out, ps := run(t, "hang", "--max-life", "150ms")
	if ps.ExitCode() != exitMaxLife || !strings.Contains(out, "max-life") {
		t.Fatalf("exit %d: %q", ps.ExitCode(), out)
	}
	if el := time.Since(t0); el > 3*time.Second {
		t.Fatalf("took %v", el)
	}
}

func TestSpike(t *testing.T) {
	t0 := time.Now()
	out, ps := run(t, "spike", "--base", "10M", "--peak", "40M", "--at", "200ms", "--hold", "0")
	if ps.ExitCode() != 0 {
		t.Fatalf("exit %d: %s", ps.ExitCode(), out)
	}
	if time.Since(t0) < 200*time.Millisecond {
		t.Fatal("spike happened before --at")
	}
	bi, si := strings.Index(out, "base done allocated=10M"), strings.Index(out, "spike done allocated=40M")
	if bi < 0 || si < 0 || si < bi {
		t.Fatalf("output:\n%s", out)
	}
	if rss := maxRSS(ps); rss < 40<<20 {
		t.Fatalf("peak RSS %s", units.FormatBytes(rss))
	}
}
