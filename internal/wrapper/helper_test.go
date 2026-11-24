package wrapper

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/proto"
)

// The test binary doubles as a `pace run` process for tests that need the
// wrapper to be a real, separate process (signal delivery, re-raise, exec).

const (
	helperEnv     = "PACE_WRAPPER_HELPER"
	helperSpecEnv = "PACE_WRAPPER_SPEC"
)

type helperSpec struct {
	Socket         string   `json:"socket"`
	Home           string   `json:"home"`
	WaitMS         int64    `json:"wait_ms"`
	Argv           []string `json:"argv"`
	StatusFile     string   `json:"status_file"`
	QueueTimeoutMS int64    `json:"queue_timeout_ms"`
	// Ctty lets the helper open /dev/tty (its own session's terminal in the
	// pty tests); otherwise it must not touch the terminal of `go test`.
	Ctty bool `json:"ctty"`
}

func noTTY() *os.File { return nil }

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" {
		var sp helperSpec
		if err := json.Unmarshal([]byte(os.Getenv(helperSpecEnv)), &sp); err != nil {
			os.Stderr.WriteString("helper: bad spec: " + err.Error() + "\n")
			os.Exit(250)
		}
		var env []string
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); k == helperEnv || k == helperSpecEnv {
				continue
			}
			env = append(env, kv)
		}
		o := Options{
			Config:         config.Resolved{Socket: sp.Socket, Home: sp.Home, FailOpenWaitMS: sp.WaitMS, FailOpenSlots: 2},
			Argv:           sp.Argv,
			StatusFile:     sp.StatusFile,
			QueueTimeoutMS: sp.QueueTimeoutMS,
			Env:            env,
			t:              fastTimings(),
		}
		if !sp.Ctty {
			o.h.openTTY = noTTY
		}
		os.Exit(Run(o))
	}
	os.Exit(m.Run())
}

type helper struct {
	cmd     *exec.Cmd
	errPath string
	outPath string
	done    chan struct{}
	state   *os.ProcessState
}

// startHelper runs the wrapper as a subprocess. extraEnv entries are added
// to the clean test environment.
func startHelper(t *testing.T, dir string, sp helperSpec, extraEnv ...string) *helper {
	t.Helper()
	return startHelperIO(t, dir, sp, nil, nil, extraEnv...)
}

// startHelperIO is startHelper with explicit stdio (nil = files/devnull);
// setup may adjust the command (e.g. SysProcAttr) before it starts.
func startHelperIO(t *testing.T, dir string, sp helperSpec, stdio []*os.File, setup func(*exec.Cmd), extraEnv ...string) *helper {
	t.Helper()
	b, _ := json.Marshal(sp)
	h := &helper{errPath: filepath.Join(dir, "helper.err"), outPath: filepath.Join(dir, "helper.out"), done: make(chan struct{})}
	errF, _ := os.Create(h.errPath)
	outF, _ := os.Create(h.outPath)
	devnull, _ := os.Open(os.DevNull)
	h.cmd = exec.Command(os.Args[0], "-test.run=^$")
	h.cmd.Env = append(testEnv(extraEnv...), helperEnv+"=1", helperSpecEnv+"="+string(b))
	h.cmd.Stdin, h.cmd.Stdout, h.cmd.Stderr = devnull, outF, errF
	if len(stdio) == 3 {
		for i, f := range stdio {
			if f == nil {
				continue
			}
			switch i {
			case 0:
				h.cmd.Stdin = f
			case 1:
				h.cmd.Stdout = f
			case 2:
				h.cmd.Stderr = f
			}
		}
	}
	if setup != nil {
		setup(h.cmd)
	}
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	errF.Close()
	outF.Close()
	devnull.Close()
	go func() {
		h.cmd.Wait()
		h.state = h.cmd.ProcessState
		close(h.done)
	}()
	t.Cleanup(func() {
		select {
		case <-h.done:
		default:
			h.cmd.Process.Kill()
			<-h.done
		}
	})
	return h
}

func (h *helper) wait(t *testing.T, d time.Duration) syscall.WaitStatus {
	t.Helper()
	select {
	case <-h.done:
	case <-time.After(d):
		b, _ := os.ReadFile(h.errPath)
		t.Fatalf("helper did not exit; stderr %q", b)
	}
	return h.state.Sys().(syscall.WaitStatus)
}

func (h *helper) stderr() string { b, _ := os.ReadFile(h.errPath); return string(b) }
func (h *helper) stdout() string { b, _ := os.ReadFile(h.outPath); return string(b) }

func TestSubprocessUnrelatedSIGKILL(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	pidf := filepath.Join(e.dir, "pid")
	status := filepath.Join(e.dir, "st.json")
	h := startHelper(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000, StatusFile: status,
		Argv: sh("echo $$ > " + pidf + "; exec sleep 30")})
	pid := readPID(t, pidf)
	time.Sleep(50 * time.Millisecond)
	syscall.Kill(pid, syscall.SIGKILL) // jetsam-like kill the governor did not send
	ws := h.wait(t, 10*time.Second)
	if !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("wrapper status %v (exited=%v code=%d); want death by SIGKILL; stderr %q", ws, ws.Exited(), ws.ExitStatus(), h.stderr())
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("status file: %v", err)
	}
	if st.Signal != 9 || st.Source != "child" {
		t.Fatalf("status %+v", st)
	}
}

func TestSubprocessSignalForwarding(t *testing.T) {
	tests := []struct {
		name       string
		sig        syscall.Signal
		script     string
		wantSignal syscall.Signal // wrapper dies of this
		wantCode   int            // else exits with this
	}{
		{name: "TERM kills child, re-raised", sig: syscall.SIGTERM, script: "exec sleep 30", wantSignal: syscall.SIGTERM},
		{name: "INT kills child, re-raised", sig: syscall.SIGINT, script: "exec sleep 30", wantSignal: syscall.SIGINT},
		{name: "HUP kills child, re-raised", sig: syscall.SIGHUP, script: "exec sleep 30", wantSignal: syscall.SIGHUP},
		{name: "QUIT re-raised via exec fallback", sig: syscall.SIGQUIT, script: "exec sleep 30", wantSignal: syscall.SIGQUIT},
		{name: "child traps TERM and exits 7", sig: syscall.SIGTERM, script: `trap 'exit 7' TERM; while :; do sleep 0.01; done`, wantCode: 7},
		{name: "child traps TERM and exits 0", sig: syscall.SIGTERM, script: `trap 'echo bye; exit 0' TERM; while :; do sleep 0.01; done`, wantCode: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			newFake(t, e.sock).start()
			pidf := filepath.Join(e.dir, "pid")
			readyf := filepath.Join(e.dir, "ready")
			h := startHelper(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000,
				Argv: sh("echo $$ > " + pidf + "; touch " + readyf + "; " + tc.script)})
			pid := readPID(t, pidf)
			waitFile(t, readyf, 5*time.Second)
			time.Sleep(100 * time.Millisecond) // trap installed / exec done
			if err := h.cmd.Process.Signal(tc.sig); err != nil {
				t.Fatal(err)
			}
			ws := h.wait(t, 10*time.Second)
			if tc.wantSignal != 0 {
				if !ws.Signaled() || ws.Signal() != tc.wantSignal {
					t.Fatalf("wrapper status exited=%v code=%d signaled=%v sig=%v; want %v; stderr %q",
						ws.Exited(), ws.ExitStatus(), ws.Signaled(), ws.Signal(), tc.wantSignal, h.stderr())
				}
			} else if !ws.Exited() || ws.ExitStatus() != tc.wantCode {
				t.Fatalf("wrapper status %v code=%d; want exit %d; stderr %q", ws, ws.ExitStatus(), tc.wantCode, h.stderr())
			}
			waitGone(t, pid, 5*time.Second) // the child got the signal and is gone
		})
	}
}

func TestSubprocessInterruptWhileQueued(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	streamClosed := make(chan struct{})
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, Position: proto.Int(3)})
		<-s.ctx.Done()
		close(streamClosed)
	}
	f.start()
	marker := filepath.Join(e.dir, "ran")
	h := startHelper(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000, Argv: sh("touch " + marker)})
	waitContent(t, h.errPath, "QUEUED", 5*time.Second)
	h.cmd.Process.Signal(syscall.SIGINT)
	ws := h.wait(t, 10*time.Second)
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("status %v; stderr %q", ws, h.stderr())
	}
	select {
	case <-streamClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not see the stream close (job not dequeued)")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("queued command ran")
	}
}

func TestSubprocessNestedPassthrough(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	start := time.Now()
	// No daemon at all, and a long fail-open wait: a nested call must not
	// even try to connect.
	h := startHelper(t, d, helperSpec{Socket: filepath.Join(d, "none.sock"), WaitMS: 10000,
		Argv: sh(`echo "$PACE_JOB_ID $$"; echo nested-err >&2; exit 7`)}, EnvJobID+"=outer-7")
	ws := h.wait(t, 10*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 7 {
		t.Fatalf("status %v", ws)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("nested passthrough took %v", el)
	}
	want := "outer-7 " + strconv.Itoa(h.cmd.Process.Pid) + "\n" // same pid: exec'd in place
	if h.stdout() != want {
		t.Fatalf("stdout %q want %q", h.stdout(), want)
	}
	if h.stderr() != "nested-err\n" {
		t.Fatalf("stderr %q", h.stderr())
	}
}

func TestNestedExecFailure(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	var gotPath string
	var gotArgv, gotEnv []string
	o := Options{Env: testEnv(EnvJobID + "=outer"), Argv: []string{"sh", "-c", "true"}}
	o.h.exec = func(path string, argv, env []string) error {
		gotPath, gotArgv, gotEnv = path, argv, env
		return syscall.ENOEXEC
	}
	res := e.run(o)
	if res.code != 126 || !strings.Contains(res.stderr, "pace run: sh:") {
		t.Fatalf("code %d stderr %q", res.code, res.stderr)
	}
	if !strings.HasSuffix(gotPath, "/sh") || strings.Join(gotArgv, " ") != "sh -c true" || getenv(gotEnv, EnvJobID) != "outer" {
		t.Fatalf("exec(%q, %v, ...)", gotPath, gotArgv)
	}
}

func TestSubprocessChildSignalDeathMirrored(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGPIPE, syscall.SIGUSR1, syscall.SIGALRM, syscall.SIGQUIT} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			newFake(t, e.sock).start()
			// The child kills itself (no governor involvement): the wrapper must
			// die of the same signal.
			h := startHelper(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000,
				Argv: []string{"/bin/sh", "-c", "exec /bin/kill -" + strconv.Itoa(int(sig)) + " $$"}})
			ws := h.wait(t, 10*time.Second)
			if !ws.Signaled() || ws.Signal() != sig {
				t.Fatalf("wrapper: exited=%v code=%d signaled=%v sig=%v; want %v; stderr %q",
					ws.Exited(), ws.ExitStatus(), ws.Signaled(), ws.Signal(), sig, h.stderr())
			}
		})
	}
}

func TestSubprocessClosedStdoutPipe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// like `pace run yes | head -1`
	h := startHelperIO(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000, Argv: []string{"yes"}}, []*os.File{nil, pw, nil}, nil)
	pw.Close()
	buf := make([]byte, 4)
	if _, err := pr.Read(buf); err != nil || string(buf[:2]) != "y\n" {
		t.Fatalf("read %q %v", buf, err)
	}
	pr.Close()
	ws := h.wait(t, 10*time.Second)
	if !ws.Signaled() || ws.Signal() != syscall.SIGPIPE {
		t.Fatalf("wrapper: exited=%v code=%d sig=%v; want SIGPIPE; stderr %q", ws.Exited(), ws.ExitStatus(), ws.Signal(), h.stderr())
	}
}
