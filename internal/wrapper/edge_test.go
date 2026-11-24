package wrapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

func TestSubmitServerErrorFailsOpen(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.submitStatus = 500
	f.start()
	res := e.run(Options{Argv: sh("echo ran")})
	if res.code != 0 || res.stdout != "ran\n" {
		t.Fatalf("code %d out %q err %q", res.code, res.stdout, res.stderr)
	}
	d, ok := res.find(proto.VerbDegraded)
	if !ok || d.kv["reason"] != "daemon_unreachable" || d.kv["failopen_slot"] == "" {
		t.Fatalf("stderr %q", res.stderr)
	}
	if n := len(f.submitList()); n < 2 {
		t.Fatalf("5xx not retried (%d submits)", n)
	}
}

func TestSpawnFailureReported(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock).start()
	script := filepath.Join(e.dir, "bad-interp")
	os.WriteFile(script, []byte("#!/nonexistent/interpreter\n"), 0o755)
	status := filepath.Join(e.dir, "st.json")
	res := e.run(Options{StatusFile: status, Argv: []string{script}})
	if res.code != 127 {
		t.Fatalf("code %d stderr %q", res.code, res.stderr)
	}
	if !strings.HasPrefix(res.stderr, "pace run: "+script+": ") || !strings.Contains(res.stderr, "no such file or directory") {
		t.Fatalf("stderr %q", res.stderr)
	}
	ex := f.job("j1").exitReq(t)
	if ex.Code != 127 {
		t.Fatalf("exit POST %+v", ex)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Outcome != OutcomeError || st.ExitCode != 127 || st.ID != "j1" {
		t.Fatalf("status %+v", st)
	}
}

func TestReattachRefusedByEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f1 := newFake(t, e.sock)
	lostNow := make(chan struct{})
	f1.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.waitStarted(t)
		close(lostNow)
		s.hold()
	}
	f1.start()
	goFile := filepath.Join(e.dir, "go")
	errPath := filepath.Join(e.dir, "stderr")
	errF, _ := os.Create(errPath)
	defer errF.Close()
	done := make(chan result, 1)
	go func() {
		done <- e.run(Options{Stderr: errF, Argv: sh("while [ ! -e " + goFile + " ]; do sleep 0.01; done")})
	}()
	<-lostNow
	f1.stop()
	f2 := newFake(t, e.sock)
	f2.onAttach = func(s *jobStream, req proto.AttachRequest) {
		s.send(proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonHeadroom})
	}
	f2.start()
	waitContent(t, errPath, "refused to re-attach", 5*time.Second)
	time.Sleep(300 * time.Millisecond) // would re-attach ~3 times if it kept trying
	os.WriteFile(goFile, nil, 0o600)
	res := <-done
	b, _ := os.ReadFile(errPath)
	if res.code != 0 || strings.Contains(string(b), "reattached") || strings.Contains(string(b), "REFUSED") {
		t.Fatalf("code %d stderr %q", res.code, b)
	}
	if got := strings.Join(verbsOf(string(b)), ","); got != "DEGRADED,INFO" {
		t.Fatalf("verbs %s", got)
	}
	if n := len(f2.attachList()); n != 1 {
		t.Fatalf("attach attempts %d", n)
	}
}

func TestSubprocessInterruptDuringDial(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	h := startHelper(t, d, helperSpec{Socket: filepath.Join(d, "none.sock"), Home: d, WaitMS: 20000, Argv: sh("echo ran")})
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	h.cmd.Process.Signal(syscall.SIGINT)
	ws := h.wait(t, 10*time.Second)
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("status %v stderr %q", ws, h.stderr())
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("Ctrl-C during the daemon wait took %v", el)
	}
	if h.stdout() != "" {
		t.Fatal("command ran")
	}
}

func TestSubprocessSIGTERMDuringFailOpenWait(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	// hold both slots so the helper waits in the fail-open queue
	dirs := []string{filepath.Join(d, "failopen")}
	for i := 0; i < 2; i++ {
		f, _, err := acquireSlot(dirs, 2)
		if err != nil || f == nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	h := startHelper(t, d, helperSpec{Socket: filepath.Join(d, "none.sock"), Home: d, WaitMS: 50, QueueTimeoutMS: 30000, Argv: sh("echo ran")})
	waitContent(t, h.errPath, "QUEUED reason=failopen_full", 10*time.Second)
	h.cmd.Process.Signal(syscall.SIGTERM)
	ws := h.wait(t, 10*time.Second)
	if !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("status %v stderr %q", ws, h.stderr())
	}
}

func TestSlowHeartbeatsDoNotDelayExit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.progressDelay = 10 * time.Second // every heartbeat times out
	f.start()
	o := Options{t: fastTimings(), Argv: sh("sleep 0.6; exit 6")}
	o.t.Progress = 10 * time.Millisecond
	o.t.PostTimeout = 150 * time.Millisecond
	res := e.run(o)
	if res.code != 6 {
		t.Fatalf("code %d", res.code)
	}
	// 60 heartbeats were due; without coalescing the exit POST would wait
	// behind up to 16 timed-out heartbeats (2.4s).
	if res.dur > 1500*time.Millisecond {
		t.Fatalf("wrapper took %v", res.dur)
	}
	if ex := f.job("j1").exitReq(t); ex.Code != 6 {
		t.Fatalf("exit %+v", ex)
	}
}
