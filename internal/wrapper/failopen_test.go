package wrapper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// noDaemon returns an env whose socket never exists.
func noDaemon(t *testing.T) *env {
	e := newEnv(t)
	e.cfg.FailOpenWaitMS = 100
	return e
}

func TestFailOpenSlots(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	type running struct {
		errPath string
		goFile  string
		done    chan result
	}
	start := func(name string) running {
		r := running{errPath: filepath.Join(e.dir, name+".err"), goFile: filepath.Join(e.dir, name+".go"), done: make(chan result, 1)}
		errF, _ := os.Create(r.errPath)
		go func() {
			defer errF.Close()
			r.done <- e.run(Options{Stderr: errF, Argv: sh(`echo "$PACE_JOB_ID"; while [ ! -e ` + r.goFile + ` ]; do sleep 0.01; done; exit 3`)})
		}()
		return r
	}
	a, b := start("a"), start("b")
	waitContent(t, a.errPath, "failopen_slot=", 5*time.Second)
	waitContent(t, b.errPath, "failopen_slot=", 5*time.Second)

	// Third: no slot; waits for the (short) queue timeout, then refused.
	c := e.run(Options{QueueTimeoutMS: 300, Argv: sh("echo must-not-run")})
	if c.code != proto.ExitRefused || c.stdout != "" {
		t.Fatalf("third: code %d stdout %q stderr %q", c.code, c.stdout, c.stderr)
	}
	if got := strings.Join(c.verbs(), ","); got != "DEGRADED,QUEUED,REFUSED,EXIT" {
		t.Fatalf("third verbs %s\n%s", got, c.stderr)
	}
	q, _ := c.find(proto.VerbQueued)
	rf, _ := c.find(proto.VerbRefused)
	ex, _ := c.find(proto.VerbExit)
	// The wait ran out: queue_timeout (as documented), with retry_after.
	if q.kv["reason"] != "failopen_full" || rf.kv["reason"] != "queue_timeout" || rf.kv["waited"] == "" || rf.kv["retry_after"] != "5s" ||
		ex.kv["code"] != "200" || ex.kv["source"] != "pace" {
		t.Fatalf("third lines:\n%s", c.stderr)
	}
	if c.dur < 300*time.Millisecond || c.dur > 3*time.Second {
		t.Fatalf("third waited %v, want ~queue timeout", c.dur)
	}
	if h := c.hints(); len(h) != 2 || !strings.Contains(h[0], "pace install") || !strings.Contains(h[1], "pace install") {
		t.Fatalf("hints %v", h)
	}

	// Fourth waits, and gets a slot when a finishes.
	errD := filepath.Join(e.dir, "d.err")
	errF, _ := os.Create(errD)
	defer errF.Close()
	dDone := make(chan result, 1)
	go func() { dDone <- e.run(Options{Stderr: errF, QueueTimeoutMS: 10000, Argv: sh("exit 0")}) }()
	waitContent(t, errD, "QUEUED", 5*time.Second)
	os.WriteFile(a.goFile, nil, 0o600)
	ra := <-a.done
	d := <-dDone
	if d.code != 0 {
		t.Fatalf("fourth code %d", d.code)
	}
	dl, _ := os.ReadFile(errD)
	if got := strings.Join(verbsOf(string(dl)), ","); got != "DEGRADED,QUEUED,DEGRADED" {
		t.Fatalf("fourth verbs %s\n%s", got, dl)
	}
	os.WriteFile(b.goFile, nil, 0o600)
	rb := <-b.done

	var slots []string
	for _, r := range []result{ra, rb} {
		if r.code != 3 {
			t.Fatalf("fail-open exit code %d not passed through", r.code)
		}
		if r.stdout != fmt.Sprintf("failopen-%d\n", os.Getpid()) {
			t.Fatalf("PACE_JOB_ID %q", r.stdout)
		}
	}
	for _, p := range []string{a.errPath, b.errPath} {
		bs, _ := os.ReadFile(p)
		g := parsePace(string(bs))
		if len(g) != 1 || g[0].verb != "DEGRADED" || g[0].kv["reason"] != "daemon_unreachable" {
			t.Fatalf("lines %q", bs)
		}
		if !strings.HasPrefix(g[0].raw, "pace: DEGRADED reason=daemon_unreachable failopen_slot=") {
			t.Fatalf("line %q", g[0].raw)
		}
		slots = append(slots, g[0].kv["failopen_slot"])
		if !strings.Contains(string(bs), "pace: hint: the governor daemon is not running: start it with pace install (or run pace daemon)\n") {
			t.Fatalf("no hint in %q", bs)
		}
	}
	sort.Strings(slots)
	if strings.Join(slots, ",") != "1/2,2/2" {
		t.Fatalf("slots %v", slots)
	}
}

func verbsOf(s string) []string {
	var v []string
	for _, g := range parsePace(s) {
		v = append(v, g.verb)
	}
	return v
}

func holdAllSlots(t *testing.T, e *env) {
	t.Helper()
	dirs := []string{filepath.Join(e.dir, "failopen")}
	for i := 0; i < e.cfg.FailOpenSlots; i++ {
		f, _, err := acquireSlot(dirs, e.cfg.FailOpenSlots)
		if err != nil || f == nil {
			t.Fatalf("hold slot %d: %v", i, err)
		}
		t.Cleanup(func() { f.Close() })
	}
}

func TestFailOpenNoWait(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	holdAllSlots(t, e)
	res := e.run(Options{NoWait: true, Argv: sh("echo no")})
	if res.code != 200 || res.dur > 2*time.Second {
		t.Fatalf("code %d after %v", res.code, res.dur)
	}
	// Nothing waits under --no-wait: no QUEUED line before the refusal.
	if got := strings.Join(res.verbs(), ","); got != "DEGRADED,REFUSED,EXIT" {
		t.Fatalf("verbs %s", got)
	}
	rf, _ := res.find(proto.VerbRefused)
	if rf.kv["reason"] != "failopen_full" || rf.kv["retry_after"] != "5s" {
		t.Fatalf("REFUSED %q", rf.raw)
	}
}

func TestFailOpenCriticalPressure(t *testing.T) {
	t.Run("waits until pressure drops", func(t *testing.T) {
		t.Parallel()
		e := noDaemon(t)
		var calls atomic.Int32
		o := Options{Argv: sh("echo ran")}
		o.h.quickPressure = func() int {
			if calls.Add(1) <= 3 {
				return 4
			}
			return 2
		}
		res := e.run(o)
		if res.code != 0 || res.stdout != "ran\n" {
			t.Fatalf("code %d out %q err %q", res.code, res.stdout, res.stderr)
		}
		g := res.pace()
		if len(g) != 2 || g[0].kv["level"] != "critical" || g[1].kv["failopen_slot"] == "" {
			t.Fatalf("lines:\n%s", res.stderr)
		}
		if calls.Load() < 4 {
			t.Fatalf("pressure polled %d times", calls.Load())
		}
	})
	t.Run("refused at timeout", func(t *testing.T) {
		t.Parallel()
		e := noDaemon(t)
		o := Options{QueueTimeoutMS: 200, Argv: sh("echo ran")}
		o.h.quickPressure = func() int { return 4 }
		res := e.run(o)
		rf, ok := res.find(proto.VerbRefused)
		if res.code != 200 || res.stdout != "" || !ok || rf.kv["reason"] != "queue_timeout" || rf.kv["level"] != "critical" ||
			rf.kv["retry_after"] == "" {
			t.Fatalf("code %d stderr %q", res.code, res.stderr)
		}
	})
	t.Run("no-wait refused at once", func(t *testing.T) {
		t.Parallel()
		e := noDaemon(t)
		o := Options{NoWait: true, Argv: sh("echo ran")}
		o.h.quickPressure = func() int { return 4 }
		res := e.run(o)
		if res.code != 200 || res.stdout != "" || res.dur > 2*time.Second {
			t.Fatalf("code %d after %v", res.code, res.dur)
		}
		// slot must have been released: a normal run succeeds.
		ok := e.run(Options{Argv: sh("exit 0")})
		ok2 := e.run(Options{Argv: sh("exit 0")})
		if ok.code != 0 || ok2.code != 0 {
			t.Fatal("slots leaked")
		}
	})
}

func TestFailOpenWaitsBeforeDegrading(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.cfg.FailOpenWaitMS = 400
	res := e.run(Options{Argv: sh("exit 0")})
	if res.code != 0 || res.dur < 400*time.Millisecond {
		t.Fatalf("code %d dur %v", res.code, res.dur)
	}
	// Daemon appears during the wait: no degradation at all.
	e2 := newEnv(t)
	e2.cfg.FailOpenWaitMS = 3000
	go func() {
		time.Sleep(200 * time.Millisecond)
		newFake(t, e2.sock).start()
	}()
	res = e2.run(Options{Argv: sh(`echo "$PACE_JOB_ID"`)})
	if res.code != 0 || res.stdout != "j1\n" || res.stderr != "" {
		t.Fatalf("code %d out %q err %q", res.code, res.stdout, res.stderr)
	}
}

func TestFailOpenDaemonReturnsWhileWaiting(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	holdAllSlots(t, e)
	status := filepath.Join(e.dir, "st.json")
	go func() {
		time.Sleep(250 * time.Millisecond)
		newFake(t, e.sock).start()
	}()
	res := e.run(Options{QueueTimeoutMS: 10000, StatusFile: status, Argv: sh(`echo "$PACE_JOB_ID"; exit 9`)})
	if res.code != 9 || res.stdout != "j1\n" {
		t.Fatalf("code %d out %q err %q", res.code, res.stdout, res.stderr)
	}
	if got := strings.Join(res.verbs(), ","); got != "DEGRADED,QUEUED,INFO" {
		t.Fatalf("verbs %s", got)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Outcome != OutcomeAdmitted || st.ID != "j1" || st.FailOpenSlot != "" {
		t.Fatalf("status %+v", st)
	}
}

func TestFailOpenStatusAndReservedCode(t *testing.T) {
	t.Parallel()
	e := noDaemon(t)
	e.cfg.Socket = "" // unresolved config: fails open at once
	status := filepath.Join(e.dir, "st.json")
	res := e.run(Options{StatusFile: status, Argv: sh("exit 201")})
	if res.code != 201 {
		t.Fatalf("code %d", res.code)
	}
	// No daemon knows a fail-open run: its lines and status carry no job id
	// (`pace status --job` could never find failopen-<pid>).
	ex, ok := res.find(proto.VerbExit)
	if _, hasJob := ex.kv["job"]; !ok || ex.kv["source"] != "child" || ex.kv["code"] != "201" || hasJob {
		t.Fatalf("stderr %q", res.stderr)
	}
	var st statusRecord
	b, _ := os.ReadFile(status)
	json.Unmarshal(b, &st)
	if st.Outcome != OutcomeDegraded || st.ExitCode != 201 || st.Source != "child" || st.FailOpenSlot == "" || st.Error == "" || st.ID != "" {
		t.Fatalf("status %+v", st)
	}
}

func TestAcquireSlot(t *testing.T) {
	t.Parallel()
	d := shortDir(t)
	dirs := []string{filepath.Join(d, "fo")}
	var held []*os.File
	for k := 1; k <= 3; k++ {
		f, n, err := acquireSlot(dirs, 3)
		if err != nil || f == nil || n != k {
			t.Fatalf("slot %d: f=%v n=%d err=%v", k, f, n, err)
		}
		held = append(held, f)
	}
	if f, n, err := acquireSlot(dirs, 3); f != nil || n != 0 || err != nil {
		t.Fatalf("4th slot: %v %d %v", f, n, err)
	}
	held[1].Close() // release slot 2
	// A process forked concurrently by a parallel test briefly shares the
	// open file description (until its exec closes it), so the release can
	// take a moment to be visible.
	var f *os.File
	var n int
	var err error
	for i := 0; i < 100; i++ {
		if f, n, err = acquireSlot(dirs, 3); f != nil || err != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil || n != 2 {
		t.Fatalf("reacquire: %d %v", n, err)
	}
	f.Close()
	held[0].Close()
	held[2].Close()

	// Unusable first dir falls back to the next.
	blocker := filepath.Join(d, "file")
	os.WriteFile(blocker, nil, 0o600)
	f, n, err = acquireSlot([]string{filepath.Join(blocker, "sub"), filepath.Join(d, "fb")}, 1)
	if err != nil || f == nil || n != 1 || !strings.HasPrefix(f.Name(), filepath.Join(d, "fb")) {
		t.Fatalf("fallback: %v %d %v", f, n, err)
	}
	f.Close()
	// Nothing usable at all.
	if f, _, err := acquireSlot([]string{filepath.Join(blocker, "x")}, 1); f != nil || err == nil {
		t.Fatal("expected error")
	}
}
