package wrapper

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// Finding 38: the wrapper creates the job container, places the child in it
// and tells the daemon where it is.

// fakeContainers opens c at its base only (the base ADMITTED names).
func fakeContainers(c *platform.FakeContainers) func(string) (platform.JobContainer, error) {
	return func(base string) (platform.JobContainer, error) {
		if base != c.Base {
			return nil, errors.New("no such base " + base)
		}
		return c, nil
	}
}

func TestContainerCreatedAttachedReported(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	var started proto.StartedRequest
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted, MemMaxBytes: proto.I64(440 << 20), Cgroup: "/cg/pace/job-j1"})
		started = s.waitStarted(t)
		s.hold()
	}
	f.start()
	c := &platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{}}
	res := e.run(Options{Argv: sh("exit 3"), h: hooks{containers: fakeContainers(c)}})
	if res.code != 3 {
		t.Fatalf("code %d: %s", res.code, res.stderr)
	}
	if started.Cgroup != "/cg/pace/job-j1" {
		t.Fatalf("started request cgroup %q", started.Cgroup)
	}
	g, ok := c.Get("j1")
	if !ok || len(g.PIDs) != 1 || g.PIDs[0] != started.PID {
		t.Fatalf("child %d not placed in the container: %+v", started.PID, g)
	}
	if g.MemMax != 440<<20 {
		t.Errorf("container created with limit %d, want ADMITTED's 440M (in place before the child runs)", g.MemMax)
	}
	if _, ok := res.find(proto.VerbInfo); ok {
		t.Errorf("INFO on a normal run: %s", res.stderr)
	}
}

// The kernel killed the job at its hard limit and the daemon's KILLED never
// came: the exit report says so, and the agent still gets KILLED
// policy=mem_budget and exit 201 (never a bare 137).
func TestContainerKernelOOMKill(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted, BudgetBytes: proto.I64(300 << 20)})
		s.send(proto.Event{Verb: proto.VerbAdmitted, Cgroup: "/cg/pace/job-j1"})
		s.waitStarted(t)
		s.hold()
	}
	f.start()
	c := &platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{}}
	c.Set("j1", platform.FakeCgroup{OOMKills: 1, Peak: 330 << 20})
	res := e.run(Options{Argv: sh("kill -9 $$"), h: hooks{containers: fakeContainers(c)}})
	if res.code != proto.ExitKilled || len(res.raised) != 0 {
		t.Fatalf("code %d raised %v: %s", res.code, res.raised, res.stderr)
	}
	k, ok := res.find(proto.VerbKilled)
	if !ok || k.kv["policy"] != proto.PolicyMemBudget || k.kv["budget"] != "300M" || k.kv["used"] != "330M" || k.kv["msg"] != proto.MsgKernelMemMax {
		t.Fatalf("KILLED line %+v in:\n%s", k, res.stderr)
	}
	if len(res.hints()) == 0 {
		t.Errorf("no hint: %s", res.stderr)
	}
	ex := f.job("j1").exitList()
	if len(ex) == 0 || !ex[0].OOMKill || ex[0].PeakBytes != 330<<20 {
		t.Fatalf("exit reports %+v: want oom_kill with the peak", ex)
	}
	if last := ex[len(ex)-1]; last.Code != proto.ExitKilled {
		t.Errorf("daemon record not corrected to 201: %+v", ex)
	}

	// The same death without an OOM at the job's own limit is not a kill.
	c2 := &platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{}}
	res = e.run(Options{Argv: sh("kill -9 $$"), h: hooks{containers: fakeContainers(c2)}})
	if res.code == proto.ExitKilled || len(res.raised) != 1 {
		t.Fatalf("plain SIGKILL: code %d raised %v", res.code, res.raised)
	}
}

// The same kernel kill after a mem_budget WARN whose deadline (in=20s) has
// not passed: the wrapper's own KILLED line says, as the daemon's would, that
// it came before the WARN's deadline.
func TestContainerKernelOOMKillBeforeWarnDeadline(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	goFile := filepath.Join(e.dir, "go")
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted, BudgetBytes: proto.I64(300 << 20)})
		s.send(proto.Event{Verb: proto.VerbAdmitted, Cgroup: "/cg/pace/job-j1"})
		s.waitStarted(t)
		s.send(proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(285 << 20),
			BudgetBytes: proto.I64(300 << 20), Next: "kill", InMS: proto.I64(20_000)})
		time.Sleep(50 * time.Millisecond)
		os.WriteFile(goFile, nil, 0o600)
		s.hold()
	}
	f.start()
	c := &platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{}}
	c.Set("j1", platform.FakeCgroup{OOMKills: 1, Peak: 330 << 20})
	res := e.run(Options{Argv: sh("while [ ! -e " + goFile + " ]; do sleep 0.01; done; kill -9 $$"), h: hooks{containers: fakeContainers(c)}})
	if res.code != proto.ExitKilled {
		t.Fatalf("code %d: %s", res.code, res.stderr)
	}
	if _, ok := res.find(proto.VerbWarn); !ok {
		t.Fatalf("no WARN line: %s", res.stderr)
	}
	k, ok := res.find(proto.VerbKilled)
	if want := proto.MsgKernelMemMax + proto.MsgBeforeWarnDeadline; !ok || k.kv["msg"] != want {
		t.Fatalf("KILLED line %+v, want msg %q in:\n%s", k, want, res.stderr)
	}
}

type failCreate struct{ *platform.FakeContainers }

func (failCreate) Create(string, int64) (string, error) { return "", errors.New("permission denied") }

// A container facility that cannot take this job: one INFO line, and the job
// runs governed by its process group as before.
func TestContainerUnavailableInfo(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	f := newFake(t, e.sock)
	var started proto.StartedRequest
	f.onSubmit = func(s *jobStream) {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted, Cgroup: "/cg/pace/job-j1"})
		started = s.waitStarted(t)
		s.hold()
	}
	f.start()
	fc := failCreate{&platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{}}}
	res := e.run(Options{Argv: sh("exit 0"), h: hooks{containers: func(string) (platform.JobContainer, error) { return fc, nil }}})
	if res.code != 0 {
		t.Fatalf("code %d: %s", res.code, res.stderr)
	}
	in, ok := res.find(proto.VerbInfo)
	if !ok || in.kv["msg"] != "no job container (permission denied): governed by process group" {
		t.Fatalf("INFO %+v:\n%s", in, res.stderr)
	}
	if started.Cgroup != "" || started.PID == 0 {
		t.Fatalf("started %+v", started)
	}
}

// Review: the daemon runs jobs in containers (ADMITTED names the path) but
// this pace run cannot use it (started from a root-owned ssh session scope,
// PACE_LINUX_CGROUP=0): the agent gets an INFO line saying so instead of a
// silent fallback. Without a path (the daemon runs none) nothing is tried
// and nothing is printed.
func TestContainerFacilityUnavailableInfo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, path string
		info       string
	}{
		{"facility error", "/cg/pace/job-j1", "no job container (cgroup: own cgroup /sys/fs/cgroup/session1 is not delegated to us): governed by process group"},
		{"not this job's", "/cg/pace/job-other", `no job container (container path "/cg/pace/job-other" is not this job's): governed by process group`},
		{"daemon runs none", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			f := newFake(t, e.sock)
			var started proto.StartedRequest
			f.onSubmit = func(s *jobStream) {
				s.send(proto.Event{Verb: proto.VerbAccepted})
				s.send(proto.Event{Verb: proto.VerbAdmitted, Cgroup: tc.path, MemMaxBytes: proto.I64(1 << 30)})
				started = s.waitStarted(t)
				s.hold()
			}
			f.start()
			called := false
			open := func(string) (platform.JobContainer, error) {
				called = true
				return nil, errors.New("cgroup: own cgroup /sys/fs/cgroup/session1 is not delegated to us")
			}
			res := e.run(Options{Argv: sh("exit 0"), h: hooks{containers: open}})
			if res.code != 0 {
				t.Fatalf("code %d: %s", res.code, res.stderr)
			}
			in, ok := res.find(proto.VerbInfo)
			if tc.info == "" {
				if ok || called {
					t.Fatalf("no container path, yet facility called=%v / INFO:\n%s", called, res.stderr)
				}
			} else if !ok || in.kv["msg"] != tc.info {
				t.Fatalf("INFO %+v:\n%s", in, res.stderr)
			}
			if started.Cgroup != "" || started.PID == 0 {
				t.Fatalf("started %+v", started)
			}
			if strings.Contains(res.stderr, "cgroup=") || strings.Contains(res.stderr, "mem_max") {
				t.Fatalf("stream-only fields printed:\n%s", res.stderr)
			}
		})
	}
}

// Linux procStartMS places the kernel's start ticks on the wall clock
// through CLOCK_BOOTTIME (not the whole-second btime, which made starts up
// to 1 s early and wall-clock kills up to 1 s premature).
func TestStartFromBoot(t *testing.T) {
	// Booted 1000.5 s before wall time 2_000_000 ms; started 12.34 s after boot.
	if got := startFromBoot(2_000_000, 1_000_500, 1234, 100); got != 2_000_000-1_000_500+12_340 {
		t.Fatalf("got %d", got)
	}
}
