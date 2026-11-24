package wrapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// Round 3: a restarted daemon killed the job before pace run re-attached
// (its KILLED never reached the lost stream). pace run asks the daemon about
// the job when the child dies, prints the KILLED line, exits 201 with an
// EXIT line, and reports the exit to the daemon, whose record then has it.
func TestLostDaemonKillBeforeReattachIs201(t *testing.T) {
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
	errPath := filepath.Join(e.dir, "stderr")
	errF, _ := os.Create(errPath)
	defer errF.Close()
	statusPath := filepath.Join(e.dir, "status.json")
	done := make(chan result, 1)
	go func() {
		done <- e.run(Options{Stderr: errF, StatusFile: statusPath, Argv: sh("exec sleep 30")})
	}()
	<-lostNow
	st := <-f1.job("j1").started
	f1.stop()
	waitContent(t, errPath, "DEGRADED", 5*time.Second)

	f2 := newFake(t, e.sock)
	f2.attachStatus = 503 // never re-attached before the kill
	killed := proto.Event{V: 1, TS: time.Now().UnixMilli(), Verb: proto.VerbKilled, Job: "j1", Policy: proto.PolicyPressure,
		Level: proto.LevelCritical, UsedBytes: proto.I64(300 << 20), Hint: "run less at once"}
	f2.jobStatus = func(id string) proto.JobStatus {
		return proto.JobStatus{ID: id, State: "done", Stage: "killed", KilledBy: proto.PolicyPressure, Events: []proto.Event{killed}}
	}
	f2.start()
	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil { // the restarted daemon's kill
		t.Fatal(err)
	}
	res := <-done
	b, _ := os.ReadFile(errPath)
	if res.code != proto.ExitKilled || len(res.raised) != 0 {
		t.Fatalf("code %d raised %v, want 201:\n%s", res.code, res.raised, b)
	}
	g := parsePace(string(b))
	var verbs []string
	for _, l := range g {
		verbs = append(verbs, l.verb)
	}
	if got := strings.Join(verbs, ","); got != "DEGRADED,KILLED,EXIT" {
		t.Fatalf("verbs %s:\n%s", got, b)
	}
	if k := g[1]; k.kv["policy"] != "pressure" || k.kv["job"] != "j1" {
		t.Fatalf("KILLED %+v", k)
	}
	ex := f2.job("j1").exitReq(t)
	if ex.Code != proto.ExitKilled {
		t.Fatalf("exit report to the restarted daemon %+v, want code 201", ex)
	}
	var sf map[string]any
	raw, _ := os.ReadFile(statusPath)
	if err := json.Unmarshal(raw, &sf); err != nil || sf["outcome"] != OutcomeKilled || sf["killed_by"] != "pressure" {
		t.Fatalf("status file %s (%v)", raw, err)
	}
}

// Round 3: on re-attach pace run says which events it already has, so the
// daemon can replay the rest.
func TestReattachSendsLastEventTS(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f1 := newFake(t, e.sock)
	lostNow := make(chan struct{})
	const warnTS = 1_763_800_000_123
	f1.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.waitStarted(t)
		s.send(proto.Event{Verb: proto.VerbInfo, TS: warnTS, Msg: "x"})
		time.Sleep(100 * time.Millisecond)
		close(lostNow)
		s.hold()
	}
	f1.start()
	goFile := filepath.Join(e.dir, "go")
	done := make(chan result, 1)
	go func() {
		done <- e.run(Options{Argv: sh("while [ ! -e " + goFile + " ]; do sleep 0.01; done")})
	}()
	<-lostNow
	f1.stop()
	f2 := newFake(t, e.sock)
	attached := make(chan proto.AttachRequest, 1)
	f2.onAttach = func(s *jobStream, req proto.AttachRequest) {
		attached <- req
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.hold()
	}
	f2.start()
	var req proto.AttachRequest
	select {
	case req = <-attached:
	case <-time.After(10 * time.Second):
		t.Fatal("no re-attach")
	}
	os.WriteFile(goFile, nil, 0o600)
	<-done
	if req.LastEventTS < warnTS {
		t.Fatalf("attach last_event_ts %d, want >= %d", req.LastEventTS, warnTS)
	}
}

// Round 3: a command that is not found (127) or not executable (126) still
// gets its status file, with outcome error and the error.
func TestExecFailureWritesStatusFile(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	noexec := filepath.Join(e.dir, "noexec")
	os.WriteFile(noexec, []byte("#!/bin/sh\n"), 0o644)
	for _, c := range []struct {
		argv []string
		code int
	}{{[]string{"nosuchcmd-xyz-round3"}, 127}, {[]string{noexec}, 126}} {
		sf := filepath.Join(e.dir, "st.json")
		os.Remove(sf)
		res := e.run(Options{StatusFile: sf, Argv: c.argv})
		if res.code != c.code {
			t.Fatalf("%v: code %d, want %d", c.argv, res.code, c.code)
		}
		raw, err := os.ReadFile(sf)
		if err != nil {
			t.Fatalf("%v: no status file: %v", c.argv, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil || m["outcome"] != OutcomeError || m["exit_code"] != float64(c.code) ||
			m["source"] != "child" || m["error"] == "" {
			t.Fatalf("%v: status file %s (%v)", c.argv, raw, err)
		}
	}
}

// Round 3: a lost daemon's throttle and hard cap do not stay on the job's
// container: pace run lifts them with the freeze (fail-open is ungoverned).
func TestLostDaemonReleasesContainerThrottleAndCap(t *testing.T) {
	fc := &platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{"j1": {Throttled: true, MemMax: 440 << 20}}}
	r := &runner{o: Options{Quiet: true}, id: "j1", cg: &jobCgroup{c: fc, id: "j1", fd: -1}}
	r.thawLost(&child{pid: os.Getpid(), pgid: -1})
	g, _ := fc.Get("j1")
	if g.Throttled || g.MemMax != 0 {
		t.Fatalf("after the daemon was lost: %+v, want unthrottled and uncapped", g)
	}
}
