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

	"github.com/fireharp/pace/internal/proto"
)

// Regression tests for review findings (ids in the comments).

// #1: the status file holds the final JobStatus (same keys), with the
// daemon's view of class, budget and peak, plus the wrapper's fields.
func TestStatusFileIsJobStatus(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.jobStatus = func(id string) proto.JobStatus {
		return proto.JobStatus{ID: id, Class: "build", State: "done", Stage: "terminating", Priority: 5,
			EstimateBytes: 1 << 30, BudgetBytes: 2 << 30, PeakBytes: 123 << 20, UsedBytes: 0, KilledBy: "mem_budget"}
	}
	f.start()
	status := filepath.Join(e.dir, "st.json")
	res := e.run(Options{StatusFile: status, Argv: sh("echo out; exit 4")})
	if res.code != 4 {
		t.Fatalf("code %d stderr %q", res.code, res.stderr)
	}
	b, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "id", "class", "state", "priority", "argv", "pid", "pgid", "estimate_bytes", "budget_bytes",
		"peak_bytes", "submit_ts", "start_ts", "end_ts", "exit_code", "events", "outcome", "source", "output_bytes"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("status file lacks JobStatus key %q:\n%s", k, b)
		}
	}
	for _, k := range []string{"job", "policy", "submit_ms", "start_ms", "end_ms"} {
		if _, ok := raw[k]; ok {
			t.Errorf("status file still has the old key %q", k)
		}
	}
	var st statusRecord
	json.Unmarshal(b, &st)
	// The daemon's numbers are taken; how this run ended is the wrapper's:
	// it saw no KILLED, so no killed_by and no terminating stage.
	if st.ID != "j1" || st.Class != "build" || st.State != "done" || st.BudgetBytes != 2<<30 || st.PeakBytes != 123<<20 ||
		st.KilledBy != "" || st.Stage != "" || st.ExitCode != 4 || st.OutputBytes != 4 || st.StartTS <= 0 || st.EndTS < st.StartTS {
		t.Fatalf("status %+v", st)
	}
}

// #1: without a daemon the file still has the JobStatus shape.
func TestStatusFileFailOpenShape(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	status := filepath.Join(e.dir, "st.json")
	res := e.run(Options{StatusFile: status, Class: "test", BudgetBytes: 1 << 30, Argv: sh("exit 0")})
	if res.code != 0 {
		t.Fatalf("code %d", res.code)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Class != "test" || st.BudgetBytes != 1<<30 || st.State != "done" || st.Outcome != OutcomeDegraded || st.ID != "" {
		t.Fatalf("status %s", b)
	}
}

// #6: an unwritable status file must not push the EXIT line off the end.
func TestExitLineLastWhenStatusUnwritable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		if s.job.submit.NoWait {
			s.send(proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonHeadroom})
			return
		}
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.hold()
	}
	f.start()
	bad := filepath.Join(e.dir, "no", "such", "dir", "s.json")
	for _, o := range []Options{
		{StatusFile: bad, Argv: sh("exit 200")},             // child's own reserved code
		{StatusFile: bad, NoWait: true, Argv: sh("exit 0")}, // refused (finishPace)
		{StatusFile: bad}, // usage error (paceError)
	} {
		res := e.run(o)
		lines := strings.Split(strings.TrimRight(res.stderr, "\n"), "\n")
		last := lines[len(lines)-1]
		if !strings.HasPrefix(last, "pace: EXIT code=20") || !strings.Contains(res.stderr, "status file") {
			t.Fatalf("%v: last stderr line %q; stderr:\n%s", o.Argv, last, res.stderr)
		}
	}
}

// #22: a child's death by a signal that was ignored when pace run started
// (nohup, `cmd &` in a script) must not turn into exit 0.
func TestSignalIgnoredAtStartNotReportedAsSuccess(t *testing.T) {
	t.Parallel()
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("no perl to reset the inherited SIG_IGN in the child")
	}
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			newFake(t, e.sock).start()
			name := map[syscall.Signal]string{syscall.SIGINT: "INT", syscall.SIGHUP: "HUP"}[sig]
			argv := []string{perl, "-e", "$SIG{" + name + "}='DEFAULT'; kill " + name + " => $$; sleep 5"}
			b, _ := json.Marshal(helperSpec{Socket: e.sock, WaitMS: 2000, Argv: argv})
			// The shell's trap '' makes the signal ignored in the exec'd wrapper.
			cmd := exec.Command("/bin/sh", "-c", "trap '' "+name+"; exec \"$0\" -test.run='^$'", os.Args[0])
			cmd.Env = append(testEnv(), helperEnv+"=1", helperSpecEnv+"="+string(b))
			errB := &strings.Builder{}
			cmd.Stderr = errB
			err := cmd.Run()
			ee, _ := err.(*exec.ExitError)
			if ee == nil {
				t.Fatalf("wrapper exited 0 (err %v) for a child killed by %v; stderr %q", err, sig, errB)
			}
			ws := ee.Sys().(syscall.WaitStatus)
			if !ws.Exited() || ws.ExitStatus() != 128+int(sig) {
				t.Fatalf("wrapper status exited=%v code=%d signaled=%v; want exit %d; stderr %q",
					ws.Exited(), ws.ExitStatus(), ws.Signaled(), 128+int(sig), errB)
			}
		})
	}
}

// #24: a daemon that accepts and queues but then goes silent cannot hold
// pace run past its queue timeout.
func TestQueueTimeoutEnforcedOnSilentDaemon(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, Position: proto.Int(1)})
		s.hold()
	}
	f.start()
	o := Options{QueueTimeoutMS: 300, Argv: sh("echo must-not-run")}
	o.t = fastTimings()
	o.t.QueueGrace = 200 * time.Millisecond
	res := e.run(o)
	if res.code != proto.ExitRefused || res.stdout != "" || res.dur > 3*time.Second {
		t.Fatalf("code %d after %v; stderr %q", res.code, res.dur, res.stderr)
	}
	rf, ok := res.find(proto.VerbRefused)
	if !ok || rf.kv["reason"] != "queue_timeout" || rf.kv["job"] != "j1" || rf.kv["retry_after"] != "5s" {
		t.Fatalf("stderr %q", res.stderr)
	}
}

// #24: a daemon that is slow (not stalled) to answer its health check
// while we are queued is not dropped: the job keeps its queue place.
func TestSlowHealthWhileQueuedIsNotLost(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, Position: proto.Int(1)})
		f.healthDelay.Store(int64(1500 * time.Millisecond)) // > PostTimeout (1s)
		time.Sleep(4 * time.Second)
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.hold()
	}
	f.start()
	o := Options{Argv: sh("echo ran")}
	o.t = fastTimings()
	o.t.Liveness = 100 * time.Millisecond
	res := e.run(o)
	if res.code != 0 || res.stdout != "ran\n" {
		t.Fatalf("code %d out %q err %q", res.code, res.stdout, res.stderr)
	}
	if got := strings.Join(res.verbs(), ","); got != "QUEUED,ADMITTED" {
		t.Fatalf("verbs %s\n%s", got, res.stderr)
	}
	if n := len(f.submitList()); n != 1 {
		t.Fatalf("resubmitted: %d submits", n)
	}
}

// #24: a daemon whose health check says its loop is stalled is not used.
func TestStalledDaemonFailsOpen(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.healthStatus.Store(503)
	f.start()
	e.cfg.FailOpenWaitMS = 200
	res := e.run(Options{Argv: sh("echo ran")})
	if res.code != 0 || res.stdout != "ran\n" {
		t.Fatalf("code %d out %q err %q", res.code, res.stdout, res.stderr)
	}
	if d, ok := res.find(proto.VerbDegraded); !ok || d.kv["reason"] != "daemon_unreachable" {
		t.Fatalf("stderr %q", res.stderr)
	}
	if n := len(f.submitList()); n != 0 {
		t.Fatalf("submitted %d jobs to a stalled daemon", n)
	}
}

// #24: a daemon that stalls while we are queued is dropped (and we fail
// open) instead of waiting on it for the whole queue timeout.
func TestDaemonStallWhileQueued(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, Position: proto.Int(1)})
		f.healthStatus.Store(503)
		s.hold()
	}
	f.start()
	e.cfg.FailOpenWaitMS = 200
	o := Options{Argv: sh("echo ran")}
	o.t = fastTimings()
	o.t.Liveness = 50 * time.Millisecond
	res := e.run(o)
	if res.code != 0 || res.stdout != "ran\n" || res.dur > 5*time.Second {
		t.Fatalf("code %d after %v out %q err %q", res.code, res.dur, res.stdout, res.stderr)
	}
	if got := strings.Join(res.verbs(), ","); got != "QUEUED,DEGRADED,DEGRADED" {
		t.Fatalf("verbs %s\n%s", got, res.stderr)
	}
	g := res.pace()
	if g[1].kv["reason"] != "daemon_lost" || g[2].kv["reason"] != "daemon_unreachable" {
		t.Fatalf("stderr %q", res.stderr)
	}
}

// #25: descendants that outlive the child keep their stdout and stderr:
// no SIGPIPE when pace run finishes, and their output arrives.
func TestDescendantOutputSurvivesWrapper(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	outP, errP := filepath.Join(e.dir, "out"), filepath.Join(e.dir, "err")
	outF, _ := os.Create(outP)
	defer outF.Close()
	errF, _ := os.Create(errP)
	defer errF.Close()
	rcOut, rcErr := filepath.Join(e.dir, "rc-out"), filepath.Join(e.dir, "rc-err")
	script := "(sleep 0.6; echo late-out; echo rc=$? > " + rcOut + ") & " +
		"(sleep 0.6; echo late-err >&2; echo rc=$? > " + rcErr + ") & " +
		"echo early; echo early-err >&2"
	o := Options{Stdout: outF, Stderr: errF, Argv: sh(script)}
	o.t = fastTimings()
	o.t.Drain = 100 * time.Millisecond
	res := e.run(o)
	if res.code != 0 {
		t.Fatalf("code %d", res.code)
	}
	if res.dur > 2*time.Second {
		t.Fatalf("pace run waited %v for descendants", res.dur)
	}
	waitContent(t, rcOut, "rc=0", 5*time.Second)
	waitContent(t, rcErr, "rc=0", 5*time.Second)
	waitContent(t, outP, "early\nlate-out\n", 5*time.Second)
	waitContent(t, errP, "late-err\n", 5*time.Second)
}

// #6/#25: with an EXIT line (code 200-202) a descendant's later stderr is
// cut, so EXIT stays the last stderr line.
func TestExitLineStaysLastWithDescendants(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	errP, done := filepath.Join(e.dir, "err"), filepath.Join(e.dir, "late-done")
	errF, _ := os.Create(errP)
	defer errF.Close()
	o := Options{Stderr: errF, Argv: sh("(sleep 0.6; echo late >&2; touch " + done + ") & echo early >&2; exit 200")}
	o.t = fastTimings()
	o.t.Drain = 100 * time.Millisecond
	res := e.run(o)
	if res.code != 200 {
		t.Fatalf("code %d", res.code)
	}
	time.Sleep(time.Second) // the descendant has written (or died trying)
	b, _ := os.ReadFile(errP)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if last := lines[len(lines)-1]; !strings.HasPrefix(last, "pace: EXIT code=200 source=child") || !strings.Contains(string(b), "early\n") {
		t.Fatalf("stderr %q", b)
	}
}

// #25: the child gets the real stdout file (it can fstat and seek it), and
// its bytes still count as output progress.
func TestStdoutRegularFileIsDirect(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock).start()
	res := e.run(Options{Argv: sh(`if [ -f /dev/stdout ]; then echo regular; else echo not-regular; fi; sleep 0.4`)})
	if res.code != 0 || res.stdout != "regular\n" {
		t.Fatalf("code %d stdout %q", res.code, res.stdout)
	}
	pr := f.job("j1").progressList()
	if len(pr) == 0 || pr[len(pr)-1].OutputBytes != int64(len("regular\n")) || pr[len(pr)-1].LastOutputMS == 0 {
		t.Fatalf("progress %+v", pr)
	}
}

// #26: a started report the daemon could not take is retried; it is what
// attributes the job's processes.
func TestStartedRetried(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.startedFail.Store(2)
	f.start()
	o := Options{Argv: sh("sleep 0.6")}
	o.t = fastTimings()
	o.t.StartedRetry = 20 * time.Millisecond
	res := e.run(o)
	if res.code != 0 {
		t.Fatalf("code %d", res.code)
	}
	if n := f.startedCalls.Load(); n < 3 {
		t.Fatalf("started POSTs %d; want retries after the 503s", n)
	}
	if !f.job("j1").hasStarted() {
		t.Fatal("started never delivered")
	}
}

// #28: a slot stays taken while the job's process group lives, even when
// its wrapper (and so its flock) is gone.
func TestFailOpenSlotHeldByLiveGroup(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	dir := filepath.Join(d, "failopen")
	os.MkdirAll(dir, 0o700)
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pg := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pg, syscall.SIGKILL); cmd.Wait() })
	os.WriteFile(filepath.Join(dir, "slot-1"), []byte("pid=1 start_ms=1 pgid="+strconv.Itoa(pg)+" pgid_start_ms=0 argv=x\n"), 0o600)
	f, k, err := acquireSlot([]string{dir}, 1)
	if err != nil || f != nil {
		t.Fatalf("acquired slot %d (%v) of a live job group", k, err)
	}
	syscall.Kill(-pg, syscall.SIGKILL)
	cmd.Wait()
	if f, k, err = acquireFreeSlot(dir); f == nil {
		t.Fatalf("slot not free after the group died: %v %d %v", f, k, err)
	}
	f.Close()
}

// acquireFreeSlot acquires slot 1, retrying for a moment: a parallel
// test's fork can briefly share the slot file (and its flock, which
// belongs to the open file) until its exec closes it.
func acquireFreeSlot(dir string) (*os.File, int, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, k, err := acquireSlot([]string{dir}, 1)
		if (err == nil && f != nil && k == 1) || time.Now().After(deadline) {
			return f, k, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// #28: a recorded group id now used by an unrelated group (another start
// time) does not hold the slot, on Linux too.
func TestFailOpenSlotReusedPgid(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	dir := filepath.Join(d, "failopen")
	os.MkdirAll(dir, 0o700)
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pg := cmd.Process.Pid
	t.Cleanup(func() { syscall.Kill(-pg, syscall.SIGKILL); cmd.Wait() })
	st := procStartMS(pg)
	if now := time.Now().UnixMilli(); st <= 0 || absDiff(st, now) > 2000 {
		t.Fatalf("procStartMS(%d) = %d, now %d", pg, st, now)
	}
	write := func(start int64) {
		os.WriteFile(filepath.Join(dir, "slot-1"), []byte("pid=1 start_ms=1 pgid="+strconv.Itoa(pg)+
			" pgid_start_ms="+strconv.FormatInt(start, 10)+" argv=x\n"), 0o600)
	}
	write(st)
	if f, _, _ := acquireSlot([]string{dir}, 1); f != nil {
		f.Close()
		t.Fatal("slot of the live recorded group was acquired")
	}
	write(st - 60_000)
	f, k, err := acquireFreeSlot(dir)
	if f == nil {
		t.Fatalf("slot held by an unrelated group reusing the id: %v %d %v", f, k, err)
	}
	f.Close()
}

// #28: a finished fail-open job leaves no group id in its slot file.
func TestFailOpenSlotForgetsFinishedJob(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	e.cfg.FailOpenSlots = 1
	res := e.run(Options{Argv: sh("true")})
	if res.code != 0 {
		t.Fatalf("code %d %q", res.code, res.stderr)
	}
	files, _ := filepath.Glob(filepath.Join(e.dir, "*", "failopen", "slot-1"))
	more, _ := filepath.Glob(filepath.Join(e.dir, "failopen", "slot-1"))
	files = append(files, more...)
	if len(files) == 0 {
		t.Fatal("no slot file")
	}
	for _, p := range files {
		if b, _ := os.ReadFile(p); strings.Contains(string(b), "pgid=") {
			t.Fatalf("%s still records the job: %q", p, b)
		}
	}
}

// #28 end to end: a fail-open job whose background descendant outlives it
// keeps its slot.
func TestFailOpenSlotOutlivesWrapper(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	e.cfg.FailOpenSlots = 1
	pidf := filepath.Join(e.dir, "bg")
	res := e.run(Options{Argv: sh("sleep 30 >/dev/null 2>&1 & echo $! > " + pidf)})
	pid := readPID(t, pidf)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	if res.code != 0 {
		t.Fatalf("first: code %d %q", res.code, res.stderr)
	}
	res = e.run(Options{NoWait: true, Argv: sh("echo must-not-run")})
	if rf, ok := res.find(proto.VerbRefused); res.code != 200 || !ok || rf.kv["reason"] != "failopen_full" {
		t.Fatalf("second: code %d stderr %q", res.code, res.stderr)
	}
	syscall.Kill(pid, syscall.SIGKILL)
	waitGone(t, pid, 5*time.Second)
	res = e.run(Options{NoWait: true, Argv: sh("echo ran")})
	if res.code != 0 || res.stdout != "ran\n" {
		t.Fatalf("third: code %d stderr %q", res.code, res.stderr)
	}
}

// #29: Ctrl-C/SIGTERM to pace run while the daemon has the job frozen
// takes effect now, not when the daemon thaws or kills it.
func TestSignalReachesFrozenJob(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		st := s.waitStarted(t)
		if st.PGID <= 1 {
			return
		}
		time.Sleep(100 * time.Millisecond)
		syscall.Kill(-st.PGID, syscall.SIGSTOP) // the daemon's freeze
		s.send(proto.Event{Verb: proto.VerbFrozen, Policy: proto.PolicyPressure})
		s.hold()
	}
	f.start()
	// The job handles SIGTERM (as Go, node or python programs do): a
	// stopped process keeps a caught signal pending until it is continued.
	h := startHelper(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000,
		Argv: sh(`trap 'exit 7' TERM; while :; do sleep 0.05; done`)})
	waitContent(t, h.errPath, "FROZEN", 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	h.cmd.Process.Signal(syscall.SIGTERM)
	ws := h.wait(t, 3*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 7 {
		t.Fatalf("wrapper: exited=%v code=%d sig=%v; want exit 7 from the job's TERM handler; stderr %q",
			ws.Exited(), ws.ExitStatus(), ws.Signal(), h.stderr())
	}
}

// Every stop is now reported by wait4: a daemon freeze (SIGSTOP) must read
// as a stop, not as the child's end (Go's BSD WaitStatus calls it
// "continued"), and the thaw lets the job finish normally.
func TestFreezeThenThawIsNotAnExit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		st := s.waitStarted(t)
		if st.PGID <= 1 {
			return
		}
		syscall.Kill(-st.PGID, syscall.SIGSTOP)
		s.send(proto.Event{Verb: proto.VerbFrozen, Policy: proto.PolicyPressure})
		time.Sleep(400 * time.Millisecond)
		s.send(proto.Event{Verb: proto.VerbThawed, Policy: proto.PolicyPressure})
		syscall.Kill(-st.PGID, syscall.SIGCONT)
		s.hold()
	}
	f.start()
	res := e.run(Options{Argv: sh("sleep 0.3; echo done; exit 3")})
	if res.code != 3 || res.stdout != "done\n" {
		t.Fatalf("code %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	if got := strings.Join(res.verbs(), ","); got != "FROZEN,THAWED" {
		t.Fatalf("verbs %s", got)
	}
}

func TestStopSignalDecoding(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGSTOP, syscall.SIGTSTP, syscall.SIGTTIN} {
		ws := syscall.WaitStatus(int(sig)<<8 | 0x7f)
		if got, ok := stopSignal(ws); !ok || got != sig {
			t.Errorf("stopSignal(%#x) = %v, %v; want %v", uint32(ws), got, ok, sig)
		}
	}
	for _, ws := range []syscall.WaitStatus{0, 3 << 8, syscall.WaitStatus(syscall.SIGKILL)} {
		if _, ok := stopSignal(ws); ok {
			t.Errorf("stopSignal(%#x) reports a stop", uint32(ws))
		}
	}
}

// #27: SIGTSTP to pace run (Ctrl-Z when the child does not own the
// terminal) stops the job too, and SIGCONT resumes both.
func TestTSTPStopsJobWithWrapper(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	ticks := filepath.Join(e.dir, "ticks")
	h := startHelperIO(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000,
		Argv: sh("i=0; while [ $i -lt 30 ]; do echo t >> " + ticks + "; i=$((i+1)); sleep 0.05; done")}, nil,
		func(c *exec.Cmd) {
			// Its own process group, as a shell job: not orphaned, so it can stop.
			c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		})
	waitContent(t, ticks, "t\nt\n", 10*time.Second)
	pid := h.cmd.Process.Pid
	syscall.Kill(pid, syscall.SIGTSTP)
	deadline := time.Now().Add(5 * time.Second)
	for !procStopped(pid) {
		if time.Now().After(deadline) {
			if emulatedChildOf(pid) {
				// Measured in an amd64 image on arm64 (Rosetta): the emulated
				// sh sits in D state with every host signal blocked, so the
				// forwarded SIGTSTP (and SIGCHLD) stay pending and the job
				// never stops, with or without pace run. Natively: 0 of 90.
				t.Skip("job shell runs under CPU emulation, which defers its signals")
			}
			t.Fatalf("wrapper did not stop; stderr %q", h.stderr())
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(150 * time.Millisecond)
	n1 := countLines(ticks)
	time.Sleep(400 * time.Millisecond)
	if n2 := countLines(ticks); n2 != n1 {
		syscall.Kill(pid, syscall.SIGCONT)
		t.Fatalf("job kept running while pace run was stopped (%d -> %d ticks)", n1, n2)
	}
	syscall.Kill(pid, syscall.SIGCONT)
	ws := h.wait(t, 10*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("status %v; stderr %q", ws, h.stderr())
	}
	if n := countLines(ticks); n != 30 {
		t.Fatalf("ticks %d after resume", n)
	}
}

// emulatedChildOf reports whether a child of pid runs under a CPU
// emulator (Linux /proc only).
func emulatedChildOf(pid int) bool {
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	for _, d := range dirs {
		b, err := os.ReadFile(d + "/stat")
		if err != nil {
			continue
		}
		i := strings.LastIndexByte(string(b), ')')
		if f := strings.Fields(string(b[i+1:])); i < 0 || len(f) < 2 || f[1] != strconv.Itoa(pid) {
			continue
		}
		cmd, _ := os.ReadFile(d + "/cmdline")
		if c := string(cmd); strings.Contains(c, "rosetta") || strings.Contains(c, "qemu-") {
			return true
		}
	}
	return false
}

func countLines(path string) int {
	b, _ := os.ReadFile(path)
	return strings.Count(string(b), "\n")
}

func TestExitRequestForGovernorKill(t *testing.T) {
	var term syscall.WaitStatus = syscall.WaitStatus(syscall.SIGTERM)
	x := exitRequest(term, true)
	if x.Code != proto.ExitKilled || x.Signaled || x.Signal != int(syscall.SIGTERM) {
		t.Fatalf("exitRequest(TERM, killed) = %+v", x)
	}
}
