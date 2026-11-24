package wrapper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

func TestAdmittedPassThrough(t *testing.T) {
	tests := []struct {
		name     string
		script   string
		code     int
		stdout   string
		stderr   string // exact stderr; "" = none
		exitLine bool   // EXIT code=<code> source=child expected
	}{
		{name: "exit 0", script: "echo hello", code: 0, stdout: "hello\n"},
		{name: "exit 3", script: "echo oops >&2; exit 3", code: 3, stderr: "oops\n"},
		{name: "exit 1 no output", script: "exit 1", code: 1},
		{name: "exit 199 not reserved", script: "exit 199", code: 199},
		{name: "exit 200 from child", script: "exit 200", code: 200, exitLine: true},
		{name: "exit 201 from child", script: "exit 201", code: 201, exitLine: true},
		{name: "exit 202 from child", script: "exit 202", code: 202, exitLine: true},
		{name: "exit 203 not reserved", script: "exit 203", code: 203},
		{name: "job id exported", script: `printf '%s' "$PACE_JOB_ID"`, code: 0, stdout: "j1"},
		{name: "partial last line", script: "printf 'no newline' >&2", code: 0, stderr: "no newline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			f := newFake(t, e.sock).start()
			res := e.run(Options{Argv: sh(tc.script), Class: "build", EstimateBytes: 1 << 30})
			if res.code != tc.code {
				t.Fatalf("code = %d, want %d; stderr=%q", res.code, tc.code, res.stderr)
			}
			if res.stdout != tc.stdout {
				t.Errorf("stdout = %q, want %q", res.stdout, tc.stdout)
			}
			wantErr := tc.stderr
			if tc.exitLine {
				wantErr += "pace: EXIT code=" + strconv.Itoa(tc.code) + " source=child job=j1\n"
			}
			if res.stderr != wantErr {
				t.Errorf("stderr = %q, want %q", res.stderr, wantErr)
			}
			if len(res.raised) != 0 {
				t.Errorf("raised %v", res.raised)
			}
			subs := f.submitList()
			if len(subs) != 1 {
				t.Fatalf("submits = %d", len(subs))
			}
			s := subs[0]
			if s.Class != "build" || s.EstimateBytes != 1<<30 || s.WrapperPID != os.Getpid() || s.Cwd == "" ||
				strings.Join(s.Argv, " ") != strings.Join(sh(tc.script), " ") {
				t.Errorf("submit = %+v", s)
			}
			j := f.job("j1")
			ex := j.exitReq(t)
			if ex.Code != tc.code || ex.Signaled {
				t.Errorf("exit POST = %+v", ex)
			}
			st := <-j.started
			if st.PID <= 0 || st.PGID != st.PID || st.StartMS <= 0 {
				t.Errorf("started = %+v", st)
			}
			if d := time.Now().UnixMilli() - st.StartMS; d < 0 || d > 60_000 {
				t.Errorf("start_ms %d is %dms from now", st.StartMS, d)
			}
		})
	}
}

func TestChildOwnProcessGroup(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	// ps prints the child's pgid; it must equal its pid (own group) and differ from ours.
	res := e.run(Options{Argv: sh(`echo $$ $(ps -o pgid= -p $$)`)})
	f := strings.Fields(res.stdout)
	if res.code != 0 || len(f) != 2 || f[0] != f[1] {
		t.Fatalf("pid/pgid = %q (code %d, stderr %q)", res.stdout, res.code, res.stderr)
	}
	if f[1] == strconv.Itoa(syscall.Getpgrp()) {
		t.Fatalf("child shares our process group")
	}
}

func TestQueuedThenAdmitted(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, FreeBytes: proto.I64(1 << 30), NeedBytes: proto.I64(4 << 30), Position: proto.Int(2), ETAMS: proto.I64(40000)})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, FreeBytes: proto.I64(1 << 30), NeedBytes: proto.I64(4 << 30), Position: proto.Int(2), ETAMS: proto.I64(30000)})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, Position: proto.Int(1), ETAMS: proto.I64(10000)})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonPressure, Level: proto.LevelWarn, Position: proto.Int(1)})
		s.send(proto.Event{Verb: proto.VerbAdmitted, WaitedMS: proto.I64(2000)})
		s.hold()
	}
	f.start()
	res := e.run(Options{Argv: sh("echo ran >&2")})
	if res.code != 0 {
		t.Fatalf("code %d: %s", res.code, res.stderr)
	}
	// position=1 came within QueuedReprint of the first line: held, then
	// superseded by the new reason (printed at once) and ADMITTED.
	want := "pace: QUEUED reason=headroom free=1.0G need=4.0G position=2 eta=40s job=j1\n" +
		"pace: QUEUED reason=pressure level=warn position=1 job=j1\n" +
		"pace: ADMITTED waited=2s job=j1\n" +
		"ran\n"
	if res.stderr != want {
		t.Fatalf("stderr:\n%s\nwant:\n%s", res.stderr, want)
	}
}

// A long queue whose position moves every step prints at most one QUEUED
// line per QueuedReprint, with the latest position; a position that moves
// back to the one last printed prints nothing; a new reason prints at once.
// The fake daemon paces itself on the wrapper's stderr (not on sleeps), and
// every interval is checked from a time taken before the line it starts
// from, so a slow machine can delay the lines but not fail the test.
func TestQueuedReprintLimited(t *testing.T) {
	t.Parallel()
	const interval = time.Second
	e := newEnv(t)
	f := newFake(t, e.sock)
	errPath := filepath.Join(e.dir, "queued.err")
	var mu sync.Mutex
	var problems []string
	bad := func(format string, a ...any) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	q := func(s *jobStream, reason string, pos int) {
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: reason, Position: proto.Int(pos)})
	}
	f.onSubmit = func(s *jobStream) {
		defer s.hold()
		s.send(proto.Event{Verb: proto.VerbAccepted})
		sent := time.Now()
		for _, p := range []int{19, 18, 17, 16, 15} {
			q(s, proto.ReasonHeadroom, p) // 19 at once, 15 held
		}
		at, ok := pollContent(errPath, "position=15 ", 10*time.Second)
		if !ok {
			bad("position=15 never printed")
			return
		}
		if at.Sub(sent) < interval {
			bad("position=15 printed %v after position=19 was sent, want >= %v", at.Sub(sent), interval)
		}
		// Well inside the interval after the position=15 line:
		q(s, proto.ReasonHeadroom, 14) // held ...
		q(s, proto.ReasonHeadroom, 15) // ... and back: nothing to print
		// Past the timer the position=14 armed (it must print nothing).
		time.Sleep(time.Until(at.Add(interval + 300*time.Millisecond)))
		q(s, proto.ReasonSlots, 15) // a new reason: at once
		q(s, proto.ReasonSlots, 3)  // held, superseded by ADMITTED
		s.send(proto.Event{Verb: proto.VerbAdmitted, WaitedMS: proto.I64(2000)})
	}
	f.start()
	errF, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer errF.Close()
	tm := fastTimings()
	tm.QueuedReprint = interval
	res := e.run(Options{t: tm, Argv: sh("true"), Stderr: errF})
	b, _ := os.ReadFile(errPath)
	if res.code != 0 {
		t.Fatalf("code %d: %s", res.code, b)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range problems {
		t.Error(p)
	}
	want := "pace: QUEUED reason=headroom position=19 job=j1\n" +
		"pace: QUEUED reason=headroom position=15 job=j1\n" +
		"pace: QUEUED reason=slots position=15 job=j1\n" +
		"pace: ADMITTED waited=2s job=j1\n"
	if string(b) != want {
		t.Fatalf("stderr:\n%s\nwant:\n%s", b, want)
	}
}

// A new reason prints at once, and the interval of the next position change
// counts from that line: a timer held for a position of the old reason must
// not print the new reason's position early (it used to fire at the old
// line's deadline, a fraction of the interval after the new line).
func TestQueuedReasonChangeRestartsInterval(t *testing.T) {
	t.Parallel()
	const interval = 2 * time.Second
	e := newEnv(t)
	f := newFake(t, e.sock)
	errPath := filepath.Join(e.dir, "queued.err")
	var mu sync.Mutex
	var problems []string
	bad := func(format string, a ...any) {
		mu.Lock()
		problems = append(problems, fmt.Sprintf(format, a...))
		mu.Unlock()
	}
	q := func(s *jobStream, reason string, pos int) {
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: reason, Position: proto.Int(pos)})
	}
	f.onSubmit = func(s *jobStream) {
		defer s.hold()
		s.send(proto.Event{Verb: proto.VerbAccepted})
		first := time.Now()
		q(s, proto.ReasonHeadroom, 5) // at once
		q(s, proto.ReasonHeadroom, 4) // held: arms the timer for first+interval
		if _, ok := pollContent(errPath, "reason=headroom position=5 ", 10*time.Second); !ok {
			bad("headroom position=5 never printed")
			return
		}
		// Well inside the interval, and far enough from its end that the old
		// timer would fire long before the new line's interval is over.
		time.Sleep(time.Until(first.Add(interval * 6 / 10)))
		changed := time.Now()
		q(s, proto.ReasonSlots, 4) // a new reason: at once (drops held 4)
		q(s, proto.ReasonSlots, 3) // held for the whole interval from here
		at, ok := pollContent(errPath, "reason=slots position=3 ", 10*time.Second)
		if !ok {
			bad("slots position=3 never printed")
		} else if at.Sub(changed) < interval {
			bad("slots position=3 printed %v after the reason change, want >= %v", at.Sub(changed), interval)
		}
		s.send(proto.Event{Verb: proto.VerbAdmitted, WaitedMS: proto.I64(5000)})
	}
	f.start()
	errF, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer errF.Close()
	tm := fastTimings()
	tm.QueuedReprint = interval
	res := e.run(Options{t: tm, Argv: sh("true"), Stderr: errF})
	b, _ := os.ReadFile(errPath)
	if res.code != 0 {
		t.Fatalf("code %d: %s", res.code, b)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range problems {
		t.Error(p)
	}
	want := "pace: QUEUED reason=headroom position=5 job=j1\n" +
		"pace: QUEUED reason=slots position=4 job=j1\n" +
		"pace: QUEUED reason=slots position=3 job=j1\n" +
		"pace: ADMITTED waited=5s job=j1\n"
	if string(b) != want {
		t.Fatalf("stderr:\n%s\nwant:\n%s", b, want)
	}
}

// pollContent polls path until it contains sub and returns when it saw it
// (false after d). Unlike waitContent it never fails the test, so a fake
// daemon's goroutine may call it.
func pollContent(path, sub string, d time.Duration) (time.Time, bool) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), sub) {
			return time.Now(), true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Time{}, false
}

func TestQuietSuppressesInfo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	goFile := filepath.Join(e.dir, "go")
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonSlots, Position: proto.Int(1)})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.waitStarted(t)
		s.send(proto.Event{Verb: proto.VerbInfo, Msg: "chatty"})
		s.send(proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyMemBudget, Next: "kill"})
		time.Sleep(50 * time.Millisecond)
		os.WriteFile(goFile, nil, 0o600)
		s.hold()
	}
	f.start()
	res := e.run(Options{Quiet: true, Argv: sh("while [ ! -e " + goFile + " ]; do sleep 0.01; done")})
	if got := strings.Join(res.verbs(), ","); res.code != 0 || got != "QUEUED,ADMITTED,WARN" {
		t.Fatalf("code %d verbs %s", res.code, got)
	}
}

func TestRefused(t *testing.T) {
	tests := []struct {
		name     string
		ev       proto.Event
		wantLine string
		wantHint string
	}{
		{
			name:     "daemon hint",
			ev:       proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonHeadroom, FreeBytes: proto.I64(1288490188), NeedBytes: proto.I64(4 << 30), RetryAfterMS: proto.I64(40000), Hint: "lower parallelism or retry in 40s"},
			wantLine: "pace: REFUSED reason=headroom free=1.2G need=4.0G retry_after=40s job=j1",
			wantHint: "lower parallelism or retry in 40s",
		},
		{
			name:     "default hint queue timeout",
			ev:       proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonQueueTimeout, WaitedMS: proto.I64(1800000)},
			wantLine: "pace: REFUSED reason=queue_timeout waited=30m job=j1",
			wantHint: "the machine stayed busy for the whole queue timeout: retry later or raise the queue timeout",
		},
		{
			name:     "default hint too big",
			ev:       proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonTooBig},
			wantLine: "pace: REFUSED reason=too_big job=j1",
			wantHint: "the estimate exceeds what this machine can ever give: lower parallelism or the declared estimate",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			f := newFake(t, e.sock)
			f.onSubmit = func(s *jobStream) {
				s.send(proto.Event{Verb: proto.VerbAccepted})
				s.send(tc.ev)
			}
			f.start()
			marker := filepath.Join(e.dir, "ran")
			status := filepath.Join(e.dir, "status.json")
			res := e.run(Options{NoWait: true, StatusFile: status, Argv: sh("touch " + marker)})
			if res.code != proto.ExitRefused {
				t.Fatalf("code = %d", res.code)
			}
			want := tc.wantLine + "\npace: hint: " + tc.wantHint + "\npace: EXIT code=200 source=pace job=j1\n"
			if res.stderr != want {
				t.Fatalf("stderr:\n%q\nwant:\n%q", res.stderr, want)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("refused command ran")
			}
			if f.job("j1").hasStarted() {
				t.Fatal("started posted for a refused job")
			}
			if !f.submitList()[0].NoWait {
				t.Fatal("no_wait not sent")
			}
			var st statusRecord
			b, _ := os.ReadFile(status)
			if err := json.Unmarshal(b, &st); err != nil {
				t.Fatal(err)
			}
			if st.Outcome != OutcomeRefused || st.ExitCode != 200 || st.Source != "pace" || st.ID != "j1" || st.Reason != tc.ev.Reason {
				t.Fatalf("status = %+v", st)
			}
			if n := len(st.Events); n != 2 || st.Events[0].Verb != proto.VerbRefused || st.Events[1].Verb != proto.VerbExit {
				t.Fatalf("status events = %+v", st.Events)
			}
		})
	}
}

func TestKilledByGovernor(t *testing.T) {
	tests := []struct {
		name       string
		script     string
		killFirst  bool // SIGKILL the group before sending KILLED
		termSignal syscall.Signal
	}{
		{name: "KILLED then SIGKILL", script: "sleep 30", termSignal: syscall.SIGKILL},
		{name: "SIGKILL then KILLED", script: "sleep 30", killFirst: true, termSignal: syscall.SIGKILL},
		{name: "child exits 0 on TERM", script: `trap 'exit 0' TERM; while :; do sleep 0.01; done`, termSignal: syscall.SIGTERM},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			f := newFake(t, e.sock)
			killed := proto.Event{Verb: proto.VerbKilled, Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(9771050598), BudgetBytes: proto.I64(6 << 30)}
			f.onSubmit = func(s *jobStream) {
				s.send(proto.Event{Verb: proto.VerbAccepted})
				s.send(proto.Event{Verb: proto.VerbAdmitted})
				st := s.waitStarted(t)
				if st.PGID <= 0 {
					return
				}
				if tc.killFirst {
					syscall.Kill(-st.PGID, tc.termSignal)
					time.Sleep(50 * time.Millisecond)
					s.send(killed)
				} else {
					s.send(proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(6012954214), BudgetBytes: proto.I64(6 << 30), Next: "kill", InMS: proto.I64(0)})
					s.send(killed)
					time.Sleep(20 * time.Millisecond)
					syscall.Kill(-st.PGID, tc.termSignal)
				}
				s.hold()
			}
			f.start()
			status := filepath.Join(e.dir, "st.json")
			res := e.run(Options{Argv: sh(tc.script), StatusFile: status})
			if res.code != proto.ExitKilled {
				t.Fatalf("code = %d stderr=%q", res.code, res.stderr)
			}
			if len(res.raised) != 0 {
				t.Fatalf("raised %v for a governor kill", res.raised)
			}
			g := res.pace()
			last := g[len(g)-1]
			if last.raw != "pace: EXIT code=201 source=pace job=j1" {
				t.Fatalf("last line %q", last.raw)
			}
			k, ok := res.find(proto.VerbKilled)
			if !ok || k.kv["policy"] != "mem_budget" || k.kv["used"] != "9.1G" || k.kv["budget"] != "6.0G" {
				t.Fatalf("KILLED line %+v", k)
			}
			if h := res.hints(); len(h) == 0 || h[len(h)-1] != "retry with lower parallelism or request a larger budget" {
				t.Fatalf("hints %v", h)
			}
			var st statusRecord
			b, _ := os.ReadFile(status)
			json.Unmarshal(b, &st)
			if st.Outcome != OutcomeKilled || st.ExitCode != 201 || st.Source != "pace" || st.KilledBy != "mem_budget" ||
				st.ID != "j1" || st.State != "done" || st.Stage != "killed" {
				t.Fatalf("status %+v", st)
			}
			// The daemon's record must agree with pace run: 201, never 137/143
			// (the signal is kept for diagnostics only).
			// A KILLED that arrives after the exit report corrects it.
			f.job("j1").exitReq(t)
			exits := f.job("j1").exitList()
			ex := exits[len(exits)-1]
			if ex.Code != proto.ExitKilled || ex.Signaled {
				t.Fatalf("exit POSTs %+v; want the last with code 201, not signaled", exits)
			}
			if tc.termSignal == syscall.SIGKILL && ex.Signal != int(syscall.SIGKILL) {
				t.Fatalf("exit POST %+v", ex)
			}
		})
	}
}

func TestUnrelatedSignalReraised(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	pidf := filepath.Join(e.dir, "pid")
	status := filepath.Join(e.dir, "st.json")
	done := make(chan result, 1)
	go func() { done <- e.run(Options{Argv: sh("echo $$ > " + pidf + "; exec sleep 30"), StatusFile: status}) }()
	pid := readPID(t, pidf)
	time.Sleep(50 * time.Millisecond) // let exec happen
	syscall.Kill(pid, syscall.SIGKILL)
	res := <-done
	if res.code != 128+9 || len(res.raised) != 1 || res.raised[0] != syscall.SIGKILL {
		t.Fatalf("code %d raised %v", res.code, res.raised)
	}
	if res.stderr != "" {
		t.Fatalf("unexpected stderr %q", res.stderr)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Outcome != OutcomeAdmitted || st.Signal != 9 || st.ExitCode != 137 || st.Source != "child" {
		t.Fatalf("status %+v", st)
	}
}

func TestWarningsInterleave(t *testing.T) {
	warn := proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(6012954214), BudgetBytes: proto.I64(6 << 30), Next: "kill", InMS: proto.I64(20000), Hint: "lower parallelism now or the job will be killed"}
	warnLines := "pace: WARN policy=mem_budget used=5.6G budget=6.0G next=kill in=20s job=j1\npace: hint: lower parallelism now or the job will be killed\n"

	t.Run("spliced at the child's newline", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		f := newFake(t, e.sock)
		errPath := filepath.Join(e.dir, "stderr")
		goFile := filepath.Join(e.dir, "go")
		f.onSubmit = func(s *jobStream) {
			s.send(proto.Event{Verb: proto.VerbAccepted})
			s.send(proto.Event{Verb: proto.VerbAdmitted})
			s.waitStarted(t)
			waitContent(t, errPath, "compiling foo", 5*time.Second)
			s.send(warn)
			time.Sleep(50 * time.Millisecond) // let the wrapper receive it
			os.WriteFile(goFile, nil, 0o600)
			s.hold()
		}
		f.start()
		errF, _ := os.Create(errPath)
		defer errF.Close()
		o := Options{Stderr: errF, t: fastTimings(),
			Argv: sh("printf 'compiling foo' >&2; while [ ! -e " + goFile + " ]; do sleep 0.01; done; printf ' done\\nnext line\\n' >&2")}
		o.t.MuxHold = 5 * time.Second // never expires in this test
		res := e.run(o)
		b, _ := os.ReadFile(errPath)
		want := "compiling foo done\n" + warnLines + "next line\n"
		if res.code != 0 || string(b) != want {
			t.Fatalf("code %d stderr:\n%q\nwant:\n%q", res.code, b, want)
		}
	})

	t.Run("forced newline after hold", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		f := newFake(t, e.sock)
		errPath := filepath.Join(e.dir, "stderr")
		goFile := filepath.Join(e.dir, "go")
		f.onSubmit = func(s *jobStream) {
			s.send(proto.Event{Verb: proto.VerbAccepted})
			s.send(proto.Event{Verb: proto.VerbAdmitted})
			s.waitStarted(t)
			waitContent(t, errPath, "50%", 5*time.Second)
			s.send(warn)
			waitContent(t, errPath, "pace: hint:", 5*time.Second)
			os.WriteFile(goFile, nil, 0o600)
			s.hold()
		}
		f.start()
		errF, _ := os.Create(errPath)
		defer errF.Close()
		start := time.Now()
		res := e.run(Options{Stderr: errF,
			Argv: sh("printf 'progress 50%%' >&2; while [ ! -e " + goFile + " ]; do sleep 0.01; done; printf ' 100%%\\n' >&2")})
		b, _ := os.ReadFile(errPath)
		want := "progress 50%\n" + warnLines + " 100%\n"
		if res.code != 0 || string(b) != want {
			t.Fatalf("code %d stderr:\n%q\nwant:\n%q", res.code, b, want)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("too slow")
		}
	})

	t.Run("every event verb relayed at line boundaries", func(t *testing.T) {
		t.Parallel()
		e := newEnv(t)
		f := newFake(t, e.sock)
		evs := []proto.Event{
			{Verb: proto.VerbWarn, Policy: proto.PolicyPressure, Level: proto.LevelWarn},
			{Verb: proto.VerbThrottled, Policy: proto.PolicyCPU},
			{Verb: proto.VerbFrozen, Policy: proto.PolicyPressure},
			{Verb: proto.VerbThawed},
			{Verb: proto.VerbUnthrottled},
			{Verb: proto.VerbOK, Policy: proto.PolicyPressure},
			{Verb: proto.VerbInfo, Msg: "hello world"},
			{Verb: proto.VerbQueued, Position: proto.Int(1)}, // not shown once running
			{Verb: proto.VerbAdmitted},                       // not shown once running
			{Verb: proto.VerbAccepted},                       // never shown
			{Verb: proto.VerbWarn, Job: "other-job"},         // not ours
		}
		goFile := filepath.Join(e.dir, "go")
		f.onSubmit = func(s *jobStream) {
			s.send(proto.Event{Verb: proto.VerbAccepted})
			s.send(proto.Event{Verb: proto.VerbAdmitted})
			s.waitStarted(t)
			for _, ev := range evs {
				s.send(ev)
			}
			time.Sleep(100 * time.Millisecond)
			os.WriteFile(goFile, nil, 0o600)
			s.hold()
		}
		f.start()
		// The child writes a steady stream of complete lines.
		res := e.run(Options{Argv: sh("i=0; while [ ! -e " + goFile + " ]; do i=$((i+1)); echo \"line $i\" >&2; sleep 0.005; done; echo end >&2")})
		if res.code != 0 {
			t.Fatalf("code %d", res.code)
		}
		got := strings.Join(res.verbs(), ",")
		if got != "WARN,THROTTLED,FROZEN,THAWED,UNTHROTTLED,OK,INFO" {
			t.Fatalf("verbs %s\n%s", got, res.stderr)
		}
		for _, l := range strings.Split(strings.TrimSuffix(res.stderr, "\n"), "\n") {
			if strings.HasPrefix(l, "pace: ") || l == "end" {
				continue
			}
			if !strings.HasPrefix(l, "line ") || strings.Contains(l, "pace") {
				t.Fatalf("broken child line %q in\n%s", l, res.stderr)
			}
			if _, err := strconv.Atoi(strings.TrimPrefix(l, "line ")); err != nil {
				t.Fatalf("broken child line %q", l)
			}
		}
	})
}

func TestSameFileOrdering(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonSlots, Position: proto.Int(1)})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.hold()
	}
	f.start()
	both, _ := os.Create(filepath.Join(e.dir, "both"))
	defer both.Close()
	var script strings.Builder
	for i := 0; i < 200; i++ {
		script.WriteString("echo out" + strconv.Itoa(i) + "; echo err" + strconv.Itoa(i) + " >&2; ")
	}
	res := e.run(Options{Stdout: both, Stderr: both, Argv: sh(script.String())})
	if res.code != 0 {
		t.Fatalf("code %d", res.code)
	}
	b, _ := os.ReadFile(both.Name())
	var want strings.Builder
	want.WriteString("pace: QUEUED reason=slots position=1 job=j1\npace: ADMITTED job=j1\n")
	for i := 0; i < 200; i++ {
		want.WriteString("out" + strconv.Itoa(i) + "\nerr" + strconv.Itoa(i) + "\n")
	}
	if string(b) != want.String() {
		t.Fatalf("interleaving broken:\n%s", b)
	}
}

func TestSeparateStdoutNeverGetsPaceLines(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonSlots, Position: proto.Int(1)})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.waitStarted(t)
		s.send(proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyStuck})
		s.hold()
	}
	f.start()
	res := e.run(Options{Argv: sh("sleep 0.3; echo out; exit 201")})
	if res.stdout != "out\n" {
		t.Fatalf("stdout %q", res.stdout)
	}
	if got := strings.Join(res.verbs(), ","); got != "QUEUED,ADMITTED,WARN,EXIT" {
		t.Fatalf("verbs %s", got)
	}
}

func TestProgressHeartbeats(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock).start()
	// 1000 bytes on stdout + 500 on stderr, then idle for a while.
	res := e.run(Options{Argv: sh(`head -c 1000 /dev/zero; head -c 500 /dev/zero >&2; sleep 0.6`)})
	if res.code != 0 {
		t.Fatalf("code %d", res.code)
	}
	ps := f.job("j1").progressList()
	if len(ps) < 3 {
		t.Fatalf("only %d heartbeats", len(ps))
	}
	var prev proto.ProgressRequest
	for i, p := range ps {
		if p.OutputBytes < prev.OutputBytes || p.LastOutputMS < prev.LastOutputMS {
			t.Fatalf("heartbeat %d went backwards: %+v after %+v", i, p, prev)
		}
		prev = p
	}
	last := ps[len(ps)-1]
	if last.OutputBytes != 1500 || last.LastOutputMS <= 0 {
		t.Fatalf("last heartbeat %+v", last)
	}
	if len(res.stdout) != 1000 || len(res.stderr) != 500 {
		t.Fatalf("relayed %d/%d bytes", len(res.stdout), len(res.stderr))
	}
}

func TestDaemonLostReattach(t *testing.T) {
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
		done <- e.run(Options{Stderr: errF, Class: "test", Argv: sh("echo working; while [ ! -e " + goFile + " ]; do sleep 0.01; done; echo finished")})
	}()
	<-lostNow
	time.Sleep(100 * time.Millisecond)
	f1.stop() // daemon dies
	waitContent(t, errPath, "DEGRADED", 5*time.Second)
	time.Sleep(250 * time.Millisecond) // a few failed re-attach attempts

	f2 := newFake(t, e.sock)
	f2.onAttach = func(s *jobStream, req proto.AttachRequest) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyWallClock, ElapsedMS: proto.I64(1000), LimitMS: proto.I64(2000)})
		s.hold()
	}
	f2.start()
	waitContent(t, errPath, "reattached", 5*time.Second)
	waitContent(t, errPath, "WARN", 5*time.Second)
	os.WriteFile(goFile, nil, 0o600)
	res := <-done
	if res.code != 0 {
		t.Fatalf("code %d", res.code)
	}
	b, _ := os.ReadFile(errPath)
	g := parsePace(string(b))
	var verbs []string
	for _, l := range g {
		verbs = append(verbs, l.verb)
	}
	if strings.Join(verbs, ",") != "DEGRADED,INFO,WARN" {
		t.Fatalf("verbs %v:\n%s", verbs, b)
	}
	if g[0].kv["reason"] != "daemon_lost" || g[0].kv["job"] != "j1" {
		t.Fatalf("DEGRADED %+v", g[0])
	}
	if g[1].kv["msg"] != "reattached" || g[1].kv["job"] != "j1" {
		t.Fatalf("INFO %+v", g[1])
	}
	if strings.Count(string(b), "DEGRADED") != 1 {
		t.Fatal("DEGRADED printed more than once")
	}
	ats := f2.attachList()
	if len(ats) != 1 {
		t.Fatalf("attaches %d", len(ats))
	}
	at := ats[0]
	st := <-f1.job("j1").started
	// StartMS is the kernel's start time: on Linux boot time has whole
	// seconds, so it may read up to 1 s early.
	if at.PID != st.PID || at.PGID != st.PGID || at.StartMS != st.StartMS || at.Submit.Class != "test" ||
		len(at.Submit.Argv) != 3 || at.SubmitMS <= 0 || at.SubmitMS > st.StartMS+1000 || at.OutputBytes != int64(len("working\n")) {
		t.Fatalf("attach %+v vs started %+v", at, st)
	}
	ex := f2.job("j1").exitReq(t)
	if ex.Code != 0 {
		t.Fatalf("exit to new daemon %+v", ex)
	}
	if res.stdout != "working\nfinished\n" {
		t.Fatalf("stdout %q", res.stdout)
	}
}

func TestDaemonLostReattachRefused(t *testing.T) {
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
	status := filepath.Join(e.dir, "st.json")
	goFile := filepath.Join(e.dir, "go")
	errPath := filepath.Join(e.dir, "stderr")
	errF, _ := os.Create(errPath)
	defer errF.Close()
	done := make(chan result, 1)
	go func() {
		done <- e.run(Options{Stderr: errF, StatusFile: status, Argv: sh("while [ ! -e " + goFile + " ]; do sleep 0.01; done; exit 4")})
	}()
	<-lostNow
	f1.stop()
	f2 := newFake(t, e.sock).start() // no onAttach: 404
	waitContent(t, errPath, "refused to re-attach", 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	os.WriteFile(goFile, nil, 0o600)
	res := <-done
	if res.code != 4 {
		t.Fatalf("code %d", res.code)
	}
	if n := len(f2.attachList()); n != 1 {
		t.Fatalf("attach attempts after 404: %d", n)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Outcome != OutcomeDegraded || st.ExitCode != 4 {
		t.Fatalf("status %+v", st)
	}
}

func TestDaemonLostWhileQueuedResubmits(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f1 := newFake(t, e.sock)
	queued := make(chan struct{})
	f1.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonHeadroom, Position: proto.Int(1)})
		close(queued)
		s.hold()
	}
	f1.start()
	done := make(chan result, 1)
	go func() { done <- e.run(Options{Argv: sh(`echo "$PACE_JOB_ID"`)}) }()
	<-queued
	time.Sleep(50 * time.Millisecond)
	f1.stop()
	f2 := newFake(t, e.sock)
	f2.idPrefix = "k"
	f2.start()
	res := <-done
	if res.code != 0 || res.stdout != "k1\n" {
		t.Fatalf("code %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
	}
	if got := strings.Join(res.verbs(), ","); got != "QUEUED,DEGRADED" {
		t.Fatalf("verbs %s\n%s", got, res.stderr)
	}
	if len(f2.submitList()) != 1 {
		t.Fatal("no resubmit")
	}
}

func TestSubmitRejected(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.submitStatus = 400
	f.start()
	status := filepath.Join(e.dir, "st.json")
	res := e.run(Options{Class: "bogus", StatusFile: status, Argv: sh("exit 0")})
	if res.code != proto.ExitPaceError {
		t.Fatalf("code %d", res.code)
	}
	g := res.pace()
	if len(g) != 1 || g[0].verb != "EXIT" || g[0].kv["code"] != "202" || g[0].kv["source"] != "pace" || !strings.Contains(g[0].kv["msg"], "unknown class") {
		t.Fatalf("lines %+v", g)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Outcome != OutcomeError || st.ExitCode != 202 {
		t.Fatalf("status %+v", st)
	}
}

func TestCommandNotFound(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock).start()
	dir := e.dir
	os.WriteFile(filepath.Join(dir, "noexec"), []byte("#!/bin/sh\n"), 0o644)
	tests := []struct {
		argv []string
		code int
		msg  string
	}{
		{[]string{"pace-definitely-not-a-command"}, 127, "pace run: pace-definitely-not-a-command: command not found\n"},
		{[]string{dir + "/missing"}, 127, "pace run: " + dir + "/missing: stat " + dir + "/missing: no such file or directory\n"},
		{[]string{dir + "/noexec"}, 126, "pace run: " + dir + "/noexec: exec " + dir + "/noexec: permission denied\n"},
		{[]string{dir}, 126, "pace run: " + dir + ": exec " + dir + ": is a directory\n"},
		{nil, 202, "pace: EXIT code=202 source=pace msg=\"no command given\"\n"},
	}
	for _, tc := range tests {
		res := e.run(Options{Argv: tc.argv})
		if res.code != tc.code || res.stderr != tc.msg {
			t.Errorf("%v: code %d stderr %q; want %d %q", tc.argv, res.code, res.stderr, tc.code, tc.msg)
		}
	}
	if n := len(f.submitList()); n != 0 {
		t.Fatalf("submitted %d unrunnable commands", n)
	}
}

func TestDrainStopsWhenDescendantHoldsPipe(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	pidf := filepath.Join(e.dir, "bg")
	o := Options{t: fastTimings(), Argv: sh("sleep 20 & echo $! > " + pidf + "; echo leader-done")}
	o.t.Drain = 200 * time.Millisecond
	res := e.run(o)
	pid := readPID(t, pidf)
	syscall.Kill(pid, syscall.SIGKILL) // our grandchild
	if res.code != 0 || res.stdout != "leader-done\n" {
		t.Fatalf("code %d stdout %q", res.code, res.stdout)
	}
	if res.dur > 3*time.Second {
		t.Fatalf("wrapper hung %v on a pipe held by a descendant", res.dur)
	}
}

func TestStdinPassedThrough(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	in := filepath.Join(e.dir, "in")
	os.WriteFile(in, []byte("from stdin\n"), 0o600)
	inF, _ := os.Open(in)
	defer inF.Close()
	res := e.run(Options{Stdin: inF, Argv: []string{"cat"}})
	if res.code != 0 || res.stdout != "from stdin\n" {
		t.Fatalf("code %d stdout %q", res.code, res.stdout)
	}
	// non-file reader
	res = e.run(Options{Stdin: strings.NewReader("reader\n"), Argv: []string{"cat"}})
	if res.code != 0 || res.stdout != "reader\n" {
		t.Fatalf("code %d stdout %q", res.code, res.stdout)
	}
}

func TestStatusFileFromEnv(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	status := filepath.Join(e.dir, "env-status.json")
	res := e.run(Options{Env: testEnv(EnvStatusFile + "=" + status), Argv: sh("echo hi; exit 3")})
	if res.code != 3 {
		t.Fatalf("code %d", res.code)
	}
	var st statusRecord
	b, err := os.ReadFile(status)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.V != 1 || st.ID != "j1" || st.Outcome != OutcomeAdmitted || st.ExitCode != 3 || st.Source != "child" ||
		st.PID <= 0 || st.PGID != st.PID || st.StartTS <= 0 || st.EndTS < st.StartTS || st.SubmitTS <= 0 ||
		st.OutputBytes != 3 || len(st.Argv) != 3 || st.Events == nil {
		t.Fatalf("status %+v", st)
	}
	fi, _ := os.Stat(status)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// no temp files left behind
	ents, _ := os.ReadDir(e.dir)
	for _, de := range ents {
		if strings.Contains(de.Name(), ".tmp") {
			t.Fatalf("temp file left: %s", de.Name())
		}
	}
}

func TestStatusFileUnwritable(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	res := e.run(Options{StatusFile: filepath.Join(e.dir, "no", "such", "dir", "s.json"), Argv: sh("exit 5")})
	if res.code != 5 {
		t.Fatalf("code %d", res.code)
	}
	g, ok := res.find(proto.VerbInfo)
	if !ok || !strings.Contains(g.kv["msg"], "status file") {
		t.Fatalf("stderr %q", res.stderr)
	}
}
