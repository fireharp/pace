package wrapper

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

type syncBuf struct {
	mu  sync.Mutex
	b   bytes.Buffer
	err error
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestMux(t *testing.T) {
	type step struct {
		child string
		pace  []string
		sleep time.Duration
		close bool
	}
	tests := []struct {
		name   string
		direct bool
		steps  []step
		want   string
	}{
		{"pace at start", false, []step{{pace: []string{"pace: A"}}}, "pace: A\n"},
		{"pace after full line", false, []step{{child: "x\n"}, {pace: []string{"pace: A"}}}, "x\npace: A\n"},
		{"held until newline, spliced", false,
			[]step{{child: "abc"}, {pace: []string{"pace: A"}}, {child: "def\nxyz"}, {child: "\n"}},
			"abcdef\npace: A\nxyz\n"},
		{"held lines keep order", false,
			[]step{{child: "abc"}, {pace: []string{"pace: A", "pace: hint: h"}}, {pace: []string{"pace: B"}}, {child: "\n"}},
			"abc\npace: A\npace: hint: h\npace: B\n"},
		{"newline exactly at chunk end", false,
			[]step{{child: "ab"}, {pace: []string{"pace: A"}}, {child: "c\n"}, {child: "d\n"}},
			"abc\npace: A\nd\n"},
		{"forced newline after hold", false,
			[]step{{child: "50%"}, {pace: []string{"pace: A"}}, {sleep: 120 * time.Millisecond}, {child: " 100%\n"}},
			"50%\npace: A\n 100%\n"},
		{"direct mode never holds", true,
			[]step{{child: "abc"}, {pace: []string{"pace: A"}}},
			"abcpace: A\n"},
		{"close flushes held with newline", false,
			[]step{{child: "abc"}, {pace: []string{"pace: A"}}, {close: true}},
			"abc\npace: A\n"},
		{"close without pending adds nothing", false,
			[]step{{child: "abc"}, {close: true}},
			"abc"},
		{"pace after close mid-line breaks line", false,
			[]step{{child: "abc"}, {close: true}, {pace: []string{"pace: EXIT"}}},
			"abc\npace: EXIT\n"},
		{"pace after close at bol", false,
			[]step{{child: "abc\n"}, {close: true}, {pace: []string{"pace: EXIT"}}},
			"abc\npace: EXIT\n"},
		{"empty child write ignored", false,
			[]step{{child: "abc"}, {child: ""}, {pace: []string{"pace: A"}}, {child: "\n"}},
			"abc\npace: A\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var w syncBuf
			m := newMux(&w, 60*time.Millisecond, tc.direct)
			for _, s := range tc.steps {
				if s.child != "" {
					if err := m.Child([]byte(s.child)); err != nil {
						t.Fatal(err)
					}
				}
				if s.child == "" && s.pace == nil && s.sleep == 0 && !s.close {
					m.Child(nil)
				}
				if s.pace != nil {
					m.Pace(s.pace...)
				}
				if s.sleep > 0 {
					time.Sleep(s.sleep)
				}
				if s.close {
					m.Close()
				}
			}
			if got := w.String(); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestMuxChildWriteError(t *testing.T) {
	w := &syncBuf{err: syscall.EPIPE}
	m := newMux(w, time.Second, false)
	if err := m.Child([]byte("x")); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("err %v", err)
	}
	m.Pace("pace: A") // must not panic or block
}

func TestMuxConcurrentLinesStayWhole(t *testing.T) {
	var w syncBuf
	m := newMux(&w, 20*time.Millisecond, false)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			m.Child([]byte("child-"))
			m.Child([]byte("line\n"))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			m.Pace("pace: WARN policy=x")
			time.Sleep(100 * time.Microsecond)
		}
	}()
	wg.Wait()
	m.Close()
	nPace := 0
	for _, l := range strings.Split(strings.TrimSuffix(w.String(), "\n"), "\n") {
		switch l {
		case "child-line":
		case "pace: WARN policy=x":
			nPace++
		case "child-", "line":
			// a forced break (hold expired mid-line) is allowed, a merged line is not
		default:
			t.Fatalf("mangled line %q", l)
		}
	}
	if nPace != 100 {
		t.Fatalf("%d pace lines", nPace)
	}
}

func TestLookPath(t *testing.T) {
	d := t.TempDir()
	exe := filepath.Join(d, "tool")
	os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755)
	noexe := filepath.Join(d, "data")
	os.WriteFile(noexe, nil, 0o644)
	d2 := t.TempDir()
	os.WriteFile(filepath.Join(d2, "data"), []byte("#!/bin/sh\n"), 0o755)
	tests := []struct {
		file, path string
		want       string
		err        error
	}{
		{"tool", d, exe, nil},
		{"tool", "/nonexistent:" + d, exe, nil},
		{"data", d + ":" + d2, filepath.Join(d2, "data"), nil}, // skip non-executable
		{"data", d, "", fs.ErrPermission},
		{"missing", d, "", errNotFound},
		{exe, "", exe, nil},
		{noexe, "", "", fs.ErrPermission},
		{filepath.Join(d, "nope"), "", "", fs.ErrNotExist},
		{"sh", "", defaultSh(), nil}, // default PATH
	}
	for _, tc := range tests {
		got, err := lookPath(tc.file, tc.path)
		if got != tc.want || (tc.err == nil) != (err == nil) || (tc.err != nil && !errors.Is(err, tc.err)) {
			t.Errorf("lookPath(%q, %q) = %q, %v; want %q, %v", tc.file, tc.path, got, err, tc.want, tc.err)
		}
	}
}

// defaultSh is where the default PATH (/usr/bin first) finds sh: /usr/bin/sh
// on a merged-/usr Linux, /bin/sh elsewhere.
func defaultSh() string {
	if checkExec("/usr/bin/sh") == nil {
		return "/usr/bin/sh"
	}
	return "/bin/sh"
}

func waitStatus(t *testing.T, script string) syscall.WaitStatus {
	t.Helper()
	p, err := os.StartProcess("/bin/sh", []string{"sh", "-c", script}, &os.ProcAttr{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(syscall.WaitStatus)
}

func TestExitCode(t *testing.T) {
	exit3 := waitStatus(t, "exit 3")
	exit201 := waitStatus(t, "exit 201")
	killed := waitStatus(t, "kill -9 $$")
	termed := waitStatus(t, "kill -15 $$")
	tests := []struct {
		name   string
		ws     syscall.WaitStatus
		pace   bool
		code   int
		source string
		sig    syscall.Signal
	}{
		{"exit 3", exit3, false, 3, "child", 0},
		{"exit 201 by child", exit201, false, 201, "child", 0},
		{"sigkill unrelated", killed, false, 137, "child", syscall.SIGKILL},
		{"sigterm unrelated", termed, false, 143, "child", syscall.SIGTERM},
		{"sigkill by governor", killed, true, 201, "pace", 0},
		{"clean exit after KILLED", exit3, true, 201, "pace", 0},
	}
	for _, tc := range tests {
		code, source, sig := exitCode(tc.ws, tc.pace)
		if code != tc.code || source != tc.source || sig != tc.sig {
			t.Errorf("%s: got %d %s %v; want %d %s %v", tc.name, code, source, sig, tc.code, tc.source, tc.sig)
		}
	}
	if r := exitRequest(killed, false); !r.Signaled || r.Signal != 9 || r.Code != 137 {
		t.Errorf("exitRequest(killed) = %+v", r)
	}
	if r := exitRequest(exit3, false); r.Signaled || r.Code != 3 {
		t.Errorf("exitRequest(exit3) = %+v", r)
	}
}

func TestEnvHelpers(t *testing.T) {
	env := []string{"A=1", "PACE_JOB_ID=old", "B=x=y", "PACE_JOB_ID=older", "NOEQ"}
	if getenv(env, "B") != "x=y" || getenv(env, "PACE_JOB_ID") != "older" || getenv(env, "C") != "" || getenv(env, "NOEQ") != "" {
		t.Fatal("getenv")
	}
	out := setenv(env, "PACE_JOB_ID", "j9")
	if strings.Count(strings.Join(out, "\n"), "PACE_JOB_ID") != 1 || getenv(out, "PACE_JOB_ID") != "j9" || len(out) != 4 {
		t.Fatalf("setenv %v", out)
	}
	if len(env) != 5 || env[1] != "PACE_JOB_ID=old" {
		t.Fatal("setenv mutated input")
	}
}

func TestSameDest(t *testing.T) {
	d := t.TempDir()
	a, _ := os.Create(filepath.Join(d, "a"))
	defer a.Close()
	a2, _ := os.OpenFile(a.Name(), os.O_WRONLY, 0)
	defer a2.Close()
	b, _ := os.Create(filepath.Join(d, "b"))
	defer b.Close()
	var buf1, buf2 bytes.Buffer
	type nc struct{ s []int }
	tests := []struct {
		name string
		x, y interface{ Write([]byte) (int, error) }
		want bool
	}{
		{"same *os.File", a, a, true},
		{"two fds same file", a, a2, true},
		{"different files", a, b, false},
		{"file vs buffer", a, &buf1, false},
		{"same buffer", &buf1, &buf1, true},
		{"different buffers", &buf1, &buf2, false},
	}
	for _, tc := range tests {
		if got := sameDest(tc.x, tc.y); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// non-comparable dynamic types must not panic
	if sameDest(ncWriter{[]int{1}}, ncWriter{[]int{1}}) {
		t.Error("non-comparable writers reported same")
	}
	_ = nc{}
}

type ncWriter struct{ s []int }

func (ncWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestDefaultHints(t *testing.T) {
	for _, r := range []string{proto.ReasonHeadroom, proto.ReasonPressure, proto.ReasonSlots, proto.ReasonDisk, proto.ReasonTooBig,
		proto.ReasonQueueTimeout, proto.ReasonFailOpenFull, "unknown"} {
		if defaultHint(proto.Event{Verb: proto.VerbRefused, Reason: r}) == "" {
			t.Errorf("no REFUSED hint for %s", r)
		}
	}
	for _, p := range []string{proto.PolicyMemBudget, proto.PolicyPressure, proto.PolicyStuck, proto.PolicyWallClock,
		proto.PolicyOrphaned, proto.PolicyDisk, "unknown"} {
		if defaultHint(proto.Event{Verb: proto.VerbKilled, Policy: p}) == "" {
			t.Errorf("no KILLED hint for %s", p)
		}
	}
	if h := defaultHint(proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonHeadroom, RetryAfterMS: proto.I64(40000)}); h != "retry in 40s, or run with lower parallelism" {
		t.Errorf("hint %q", h)
	}
	for _, v := range []string{proto.VerbWarn, proto.VerbOK, proto.VerbInfo, proto.VerbQueued, proto.VerbDegraded} {
		if defaultHint(proto.Event{Verb: v}) != "" {
			t.Errorf("%s got a default hint", v)
		}
	}
}

func TestStatusEventsCapped(t *testing.T) {
	var s statusRecord
	for i := 0; i < maxStatusEvents+10; i++ {
		s.addEvent(proto.Event{TS: int64(i)})
	}
	if len(s.Events) != maxStatusEvents || s.Events[0].TS != 10 || s.Events[maxStatusEvents-1].TS != maxStatusEvents+9 {
		t.Fatalf("ring: len %d first %d last %d", len(s.Events), s.Events[0].TS, s.Events[len(s.Events)-1].TS)
	}
}

func TestTimingsDefaults(t *testing.T) {
	d := timings{}.withDefaults()
	if d.MuxHold != 100*time.Millisecond || d.Progress != time.Second || d.Reattach != time.Second ||
		d.FailOpenPoll != 500*time.Millisecond || d.KillWait != 200*time.Millisecond {
		t.Fatalf("defaults %+v", d)
	}
	c := timings{Progress: 5 * time.Millisecond}.withDefaults()
	if c.Progress != 5*time.Millisecond || c.MuxHold != 100*time.Millisecond {
		t.Fatalf("override %+v", c)
	}
}

func TestProcStartMS(t *testing.T) {
	now := time.Now().UnixMilli()
	got := procStartMS(os.Getpid())
	if got == 0 {
		t.Skip("no kernel start time on this OS")
	}
	if got > now || now-got > 24*3600*1000 {
		t.Fatalf("start %d vs now %d", got, now)
	}
	if procStartMS(1<<30) != 0 {
		t.Fatal("nonexistent pid has a start time")
	}
}
