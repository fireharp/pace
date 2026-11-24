package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

func TestSubmitStreamLifecycle(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	s := h.openStream("/v1/jobs", submitReq("go", "build", "./..."))
	acc := s.expect(proto.VerbAccepted)
	if !regexp.MustCompile(`^j[0-9a-z]+-[0-9a-z]+$`).MatchString(acc.Job) {
		t.Fatalf("job id %q has the wrong shape", acc.Job)
	}
	if acc.V != 1 || acc.TS == 0 || acc.Class != "build" {
		t.Fatalf("ACCEPTED %+v", acc)
	}
	id := acc.Job
	adm := s.expect(proto.VerbAdmitted)
	if adm.Job != id || adm.V != 1 || adm.TS == 0 {
		t.Fatalf("ADMITTED %+v: V and TS must be filled in", adm)
	}

	resp, b := h.post("/v1/jobs/"+id+"/started", proto.StartedRequest{PID: 5000, PGID: 5000, StartMS: 123})
	mustCode(t, resp, b, 204)
	resp, b = h.post("/v1/jobs/"+id+"/progress", proto.ProgressRequest{OutputBytes: 77, LastOutputMS: 456})
	mustCode(t, resp, b, 204)

	// started/progress are routed on the next tick.
	eventually(t, "started+progress", func() bool {
		evs := h.rec.events()
		return hasEvent(evs, policy.EvStarted, id) && hasEvent(evs, policy.EvProgress, id)
	})
	var st proto.Status
	eventually(t, "status shows pid", func() bool { st = h.status(); return len(st.Running) == 1 && st.Running[0].PID != 0 })
	if len(st.Running) != 1 || st.Running[0].ID != id || st.Running[0].PID != 5000 || st.Running[0].PGID != 5000 {
		t.Fatalf("running %+v", st.Running)
	}
	if got := strings.Join(st.Running[0].Argv, " "); got != "go build ./..." {
		t.Fatalf("argv %q", got)
	}

	resp, b = h.post("/v1/jobs/"+id+"/exit", proto.ExitRequest{Code: 3})
	mustCode(t, resp, b, 204)
	s.expectEnd()

	// Inputs carried every field, in arrival order.
	var kinds []string
	for _, e := range h.rec.events() {
		if e.Job != id {
			continue
		}
		kinds = append(kinds, e.Kind)
		switch e.Kind {
		case policy.EvSubmit:
			if e.Submit == nil || e.Submit.Class != "build" || e.Submit.WrapperPID != 4242 || len(e.Submit.Argv) != 3 {
				t.Errorf("submit %+v", e.Submit)
			}
		case policy.EvStarted:
			if e.PID != 5000 || e.PGID != 5000 || e.StartMS != 123 {
				t.Errorf("started %+v", e)
			}
		case policy.EvProgress:
			if e.OutputBytes != 77 || e.LastOutputMS != 456 {
				t.Errorf("progress %+v", e)
			}
		case policy.EvExit:
			if e.Code != 3 || e.Signaled {
				t.Errorf("exit %+v", e)
			}
		}
	}
	want := []string{policy.EvSubmit, policy.EvStarted, policy.EvProgress, policy.EvExit}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("event order %v, want %v", kinds, want)
	}
	if hasEvent(h.rec.events(), policy.EvLost, id) {
		t.Fatal("EvLost after a reported exit")
	}

	// Finished job is still served, with its events and exit code.
	resp, b = h.get("/v1/jobs/" + id)
	mustCode(t, resp, b, 200)
	var js proto.JobStatus
	if err := json.Unmarshal(b, &js); err != nil {
		t.Fatal(err)
	}
	if js.State != policy.JobDone || js.ExitCode == nil || *js.ExitCode != 3 || js.EndTS == 0 {
		t.Fatalf("job status %+v", js)
	}
	if len(js.Events) != 2 || js.Events[0].Verb != proto.VerbAccepted || js.Events[1].Verb != proto.VerbAdmitted {
		t.Fatalf("job events %+v", js.Events)
	}
	eventually(t, "recent", func() bool { st = h.status(); return len(st.Recent) == 1 })
	if len(st.Running) != 0 || len(st.Recent) != 1 || st.Recent[0].ID != id {
		t.Fatalf("status after exit: running=%v recent=%v", st.Running, st.Recent)
	}
}

func TestRefusedEndsStream(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	req := submitReq("refuse")
	req.NoWait = true
	s := h.openStream("/v1/jobs", req)
	id := s.expect(proto.VerbAccepted).Job
	ref := s.expect(proto.VerbRefused)
	if ref.Reason != proto.ReasonHeadroom || ref.RetryAfterMS == nil || *ref.RetryAfterMS != 40000 || ref.Hint == "" {
		t.Fatalf("REFUSED %+v", ref)
	}
	s.expectEnd()
	time.Sleep(50 * time.Millisecond)
	if hasEvent(h.rec.events(), policy.EvLost, id) {
		t.Fatal("refused job must not produce EvLost")
	}
	_, b := h.get("/v1/jobs/" + id)
	var js proto.JobStatus
	_ = json.Unmarshal(b, &js)
	if js.State != policy.JobDone || len(js.Events) != 2 {
		t.Fatalf("refused job status %+v", js)
	}
}

func TestValidationAndRouting(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	s := h.openStream("/v1/jobs", submitReq("sleep", "1"))
	id := s.expect(proto.VerbAccepted).Job
	s.expect(proto.VerbAdmitted)

	cases := []struct {
		name   string
		method string
		path   string
		body   string
		code   int
	}{
		{"submit no argv", "POST", "/v1/jobs", `{"class":"build"}`, 400},
		{"submit empty argv0", "POST", "/v1/jobs", `{"argv":[""]}`, 400},
		{"submit bad json", "POST", "/v1/jobs", `{"argv":`, 400},
		{"submit priority high", "POST", "/v1/jobs", `{"argv":["x"],"priority":10}`, 400},
		{"submit priority neg", "POST", "/v1/jobs", `{"argv":["x"],"priority":-1}`, 400},
		{"submit negative estimate", "POST", "/v1/jobs", `{"argv":["x"],"estimate_bytes":-1}`, 400},
		{"started unknown", "POST", "/v1/jobs/nope/started", `{"pid":10,"pgid":10}`, 404},
		{"progress unknown", "POST", "/v1/jobs/nope/progress", `{}`, 404},
		{"exit unknown", "POST", "/v1/jobs/nope/exit", `{"code":0}`, 404},
		{"get unknown", "GET", "/v1/jobs/nope", ``, 404},
		{"started pid 1", "POST", "/v1/jobs/" + id + "/started", `{"pid":1,"pgid":1}`, 400},
		{"started pgid 0", "POST", "/v1/jobs/" + id + "/started", `{"pid":500,"pgid":0}`, 400},
		{"started bad json", "POST", "/v1/jobs/" + id + "/started", `nope`, 400},
		{"attach bad id", "POST", "/v1/jobs/bad%20id/attach", `{"submit":{"argv":["x"]}}`, 400},
		{"attach unknown no argv", "POST", "/v1/jobs/jzz-1/attach", `{"pid":500,"pgid":500}`, 400},
		{"attach pgid 1", "POST", "/v1/jobs/jzz-1/attach", `{"submit":{"argv":["x"]},"pid":500,"pgid":1}`, 400},
		{"wrong method status", "POST", "/v1/status", ``, 405},
		{"wrong method jobs", "GET", "/v1/jobs", ``, 405},
		{"unknown path", "GET", "/v1/nothing", ``, 404},
		{"healthz", "GET", "/healthz", ``, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, h.url(c.path), strings.NewReader(c.body))
			resp, err := h.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			if resp.StatusCode != c.code {
				t.Fatalf("status %d (%s), want %d", resp.StatusCode, buf.String(), c.code)
			}
			if c.code == 400 || (c.code == 404 && strings.HasPrefix(c.path, "/v1/jobs")) {
				var eb errorBody
				if err := json.Unmarshal(buf.Bytes(), &eb); err != nil || eb.Error == "" {
					t.Fatalf("error body %q", buf.String())
				}
			}
		})
	}
	// None of the rejected requests reached the policy.
	for _, e := range h.rec.events() {
		if e.Job != id {
			t.Fatalf("rejected request reached the policy: %+v", e)
		}
	}
}

func TestClientDisconnectInjectsLost(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	for _, argv0 := range []string{"run", "queue"} {
		s := h.openStream("/v1/jobs", submitReq(argv0))
		id := s.expect(proto.VerbAccepted).Job
		if argv0 == "queue" {
			s.expect(proto.VerbQueued)
		} else {
			s.expect(proto.VerbAdmitted)
		}
		s.close()
		eventually(t, "EvLost for "+argv0, func() bool { return hasEvent(h.rec.events(), policy.EvLost, id) })
		n := 0
		for _, e := range h.rec.events() {
			if e.Kind == policy.EvLost && e.Job == id {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d EvLost for %s", n, id)
		}
	}
	// queued + lost -> removed by policy -> job done.
	eventually(t, "queued job gone", func() bool { return len(h.status().Queued) == 0 })
}

func TestStreamCloseAfterExitIsNotLost(t *testing.T) {
	h := newHarness(t, hopt{fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		// Keep the job running after exit (stragglers): the stream still ends.
		s2, out := miniPolicy(p, s, in)
		for _, ev := range in.Events {
			if ev.Kind == policy.EvExit {
				s2 = s
				s2.LastMS = in.NowMS
			}
		}
		return s2, out
	}})
	s := h.openStream("/v1/jobs", submitReq("x"))
	id := s.expect(proto.VerbAccepted).Job
	s.expect(proto.VerbAdmitted)
	resp, b := h.post("/v1/jobs/"+id+"/exit", proto.ExitRequest{Signaled: true, Signal: 9})
	mustCode(t, resp, b, 204)
	s.expectEnd()
	s.close()
	time.Sleep(100 * time.Millisecond)
	if hasEvent(h.rec.events(), policy.EvLost, id) {
		t.Fatal("EvLost after exit")
	}
	_, b = h.get("/v1/jobs/" + id)
	var js proto.JobStatus
	_ = json.Unmarshal(b, &js)
	if js.ExitCode == nil || *js.ExitCode != 137 || js.State != policy.JobRunning {
		t.Fatalf("status %+v (still running with stragglers, exit 128+9)", js)
	}
}

func TestImmediateStepOnSubmit(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy, cfg: func(c *config.Resolved) { c.SampleMS, c.FastMS = 5000, 5000 }})
	h.waitPublished()
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		s := h.openStream("/v1/jobs", submitReq("x"))
		id := s.expect(proto.VerbAccepted).Job
		s.expect(proto.VerbAdmitted)
		if lat := time.Since(t0); lat > 100*time.Millisecond {
			t.Fatalf("admission took %v with a 5s tick", lat)
		}
		t1 := time.Now()
		resp, b := h.post("/v1/jobs/"+id+"/exit", proto.ExitRequest{})
		mustCode(t, resp, b, 204)
		s.expectEnd()
		if lat := time.Since(t1); lat > 100*time.Millisecond {
			t.Fatalf("exit processing took %v with a 5s tick", lat)
		}
	}
}

func TestActionsExecutedOnBackend(t *testing.T) {
	var once sync.Once
	h := newHarness(t, hopt{})
	h.fake.Lock()
	h.fake.Procs = []platform.Proc{
		{PID: 600, PPID: 1, PGID: 600, UID: os.Getuid(), Comm: "victim"},
		{PID: 601, PPID: 600, PGID: 600, UID: os.Getuid(), Comm: "child"},
		{PID: 700, PPID: 1, PGID: 700, UID: os.Getuid(), Comm: "other"},
	}
	h.fake.Unlock()
	h.rec.setFn(func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		var out policy.Output
		once.Do(func() {
			out.Actions = []policy.Action{
				// Unit keys as attribution forms them (pg:<pgid>:<leader start>):
				// actions reach only the members of the unit they name.
				{Op: policy.OpSignalGroup, Unit: "pg:600:0", PGID: 600, Signal: int(syscall.SIGTERM)},
				{Op: policy.OpSignal, Unit: "pg:700:0", PIDs: []int{700}, Signal: int(syscall.SIGSTOP)},
				{Op: policy.OpBackground, Unit: "pg:700:0", PGID: 700},
				{Op: policy.OpSignalGroup, Unit: "pg:1", PGID: 1, Signal: int(syscall.SIGKILL)},
				{Op: policy.OpSignal, PIDs: []int{1, os.Getpid()}, Signal: int(syscall.SIGKILL)},
				{Op: policy.OpSignalGroup, PGID: syscall.Getpgrp(), Signal: int(syscall.SIGKILL)},
			}
			out.Would = []policy.Action{{Op: policy.OpSignalGroup, PGID: 700, Signal: int(syscall.SIGKILL)}}
			// The state records the freeze; otherwise the actuator releases
			// what no target in the state holds.
			s.Units = []policy.Unit{{Key: "pg:700:0", PGID: 700, LeaderPID: 700, Enf: policy.Enforcement{Stage: policy.StageFrozen}}}
		})
		s.LastMS = in.NowMS
		return s, out
	})
	eventually(t, "actions", func() bool { return len(h.fake.Signals()) >= 2 })
	time.Sleep(100 * time.Millisecond)
	got := h.fake.Signals()
	want := []platform.SentSignal{{PGID: 600, Sig: syscall.SIGTERM}, {PID: 700, Sig: syscall.SIGSTOP}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sent %s, want %s (Would must not run; unsafe targets refused)", fmtSent(got), fmtSent(want))
	}
	h.fake.Lock()
	bg := h.fake.Background[700]
	h.fake.Unlock()
	if !bg {
		t.Fatal("background not applied to pgid 700 members")
	}
	if n := h.d.faults.count("unsafe_action"); n != 4 {
		t.Fatalf("unsafe_action faults %d, want 4 (pgid 1, pid 1, own pid, own pgid)", n)
	}
	var st proto.Status
	eventually(t, "faults published", func() bool { st = h.status(); return len(st.Faults) > 0 })
	if !strings.Contains(strings.Join(st.Faults, "\n"), "unsafe_action") {
		t.Fatalf("faults not in status: %v", st.Faults)
	}
}

func TestEventFanout(t *testing.T) {
	var once sync.Once
	h := newHarness(t, hopt{fn: miniPolicy})
	follow, _ := http.NewRequest("GET", h.url("/v1/events?follow=1"), nil)
	fs := h.stream(follow)
	eventually(t, "follower registered", func() bool { return h.d.feed.followers() == 1 })

	js := h.openStream("/v1/jobs", submitReq("x"))
	id := js.expect(proto.VerbAccepted).Job
	js.expect(proto.VerbAdmitted)
	h.rec.setFn(func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		s, out := miniPolicy(p, s, in)
		once.Do(func() {
			out.Events = append(out.Events,
				proto.Event{Verb: proto.VerbWarn, Job: id, Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(5 << 30), BudgetBytes: proto.I64(6 << 30), Next: "kill", InMS: proto.I64(20000)},
				proto.Event{Verb: proto.VerbKilled, Unit: "pg:900:1", PGID: 900, Comm: "chrome", Policy: proto.PolicyPressure},
				proto.Event{Verb: proto.VerbOK, Job: id, Policy: proto.PolicyMemBudget},
			)
		})
		return s, out
	})
	w := js.expect(proto.VerbWarn)
	if w.Policy != proto.PolicyMemBudget || *w.UsedBytes != 5<<30 {
		t.Fatalf("WARN %+v", w)
	}
	js.expect(proto.VerbOK) // the unit KILLED is not on the job stream

	var verbs []string
	for len(verbs) < 5 {
		verbs = append(verbs, fs.next().Verb)
	}
	if strings.Join(verbs, ",") != "ACCEPTED,ADMITTED,WARN,KILLED,OK" {
		t.Fatalf("follower saw %v", verbs)
	}

	// alerts.jsonl: job WARN and unit KILLED; not ACCEPTED/ADMITTED/OK.
	path := filepath.Join(h.dir, "logs", "alerts.jsonl")
	eventually(t, "alerts written", func() bool {
		b, _ := os.ReadFile(path)
		return bytes.Count(b, []byte("\n")) >= 2
	})
	h.stop()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var av []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		var e proto.Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		av = append(av, e.Verb)
	}
	if strings.Join(av, ",") != "WARN,KILLED" {
		t.Fatalf("alerts %v", av)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("alerts mode %v", fi.Mode())
	}
}

func TestEventsRecentBuffer(t *testing.T) {
	var once sync.Once
	h := newHarness(t, hopt{fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		var out policy.Output
		once.Do(func() {
			for i := 0; i < 300; i++ {
				out.Events = append(out.Events, proto.Event{Verb: proto.VerbInfo, Msg: "m", PID: i + 2})
			}
		})
		s.LastMS = in.NowMS
		return s, out
	}})
	eventually(t, "events", func() bool { l := h.d.feed.last(1); return len(l) == 1 && l[0].PID == 301 })
	resp, b := h.get("/v1/events")
	mustCode(t, resp, b, 200)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != recentEvents {
		t.Fatalf("%d recent events, want %d", len(lines), recentEvents)
	}
	var first, last proto.Event
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if first.PID != 102 || last.PID != 301 {
		t.Fatalf("recent window [%d..%d], want [102..301]", first.PID, last.PID)
	}
	// follow with backlog replays the tail first.
	req, _ := http.NewRequest("GET", h.url("/v1/events?follow=1&backlog=3"), nil)
	fs := h.stream(req)
	for _, want := range []int{299, 300, 301} {
		if e := fs.next(); e.PID != want {
			t.Fatalf("backlog pid %d, want %d", e.PID, want)
		}
	}
}

func TestStatusShape(t *testing.T) {
	ft := &fakeTracker{}
	for i := 0; i < 25; i++ {
		ft.res.Units = append(ft.res.Units, policy.UnitSample{Key: "pg:" + string(rune('a'+i)), PGID: 1000 + i, LeaderPID: 1000 + i, Comm: "u", UsedBytes: int64(i) << 20, NProcs: 1})
	}
	ft.res.ScopedUsedBytes = 3 << 30
	h := newHarness(t, hopt{fn: miniPolicy, opts: func(o *Options) { o.Tracker = ft }})
	s := h.openStream("/v1/jobs", proto.SubmitRequest{Class: "build", Argv: []string{"make"}, EstimateBytes: 1 << 30, WrapperPID: 777})
	s.expect(proto.VerbAccepted)
	s.expect(proto.VerbAdmitted)
	q := h.openStream("/v1/jobs", submitReq("queue"))
	q.expect(proto.VerbAccepted)
	q.expect(proto.VerbQueued)

	eventually(t, "status", func() bool { st := h.status(); return len(st.Running) == 1 && len(st.Queued) == 1 })
	resp, b := h.get("/v1/status")
	mustCode(t, resp, b, 200)
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"v", "ts", "version", "platform", "mode", "capacity_bytes", "avail_bytes", "reserve_bytes", "reserved_bytes", "headroom_bytes", "pressure", "admitting", "suggested_concurrency", "suggested_by_class", "running", "queued", "unmanaged"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("status JSON lacks %q", k)
		}
	}
	st := h.status()
	if st.V != 1 || st.Version != "test" || st.Platform != "fake" || st.Mode != policy.ModeEnforce {
		t.Fatalf("header %+v", st)
	}
	if st.CapacityBytes != 8<<30 || st.AvailBytes != 5<<30 || st.ReserveBytes != 1<<30 || st.HeadroomBytes != 4<<30 || st.Pressure != proto.LevelNormal {
		t.Fatalf("figures cap=%d avail=%d reserve=%d headroom=%d level=%s", st.CapacityBytes, st.AvailBytes, st.ReserveBytes, st.HeadroomBytes, st.Pressure)
	}
	if !st.Admitting || st.SuggestedConcurrency != 4 || st.SuggestedByClass["build"] != 1 {
		t.Fatalf("admitting=%v suggested=%d byClass=%v", st.Admitting, st.SuggestedConcurrency, st.SuggestedByClass)
	}
	if len(st.Unmanaged) != topUnmanaged || st.Unmanaged[0].UsedBytes != 24<<20 || st.Unmanaged[19].UsedBytes != 5<<20 {
		t.Fatalf("unmanaged: %d entries, first %+v", len(st.Unmanaged), st.Unmanaged[0])
	}
	if st.Unmanaged[0].Priority != h.d.cfg.Params.UnmanagedPriority {
		t.Fatal("unit priority")
	}
	if st.Queued[0].Position != 1 || st.Queued[0].State != policy.JobQueued {
		t.Fatalf("queued %+v", st.Queued[0])
	}
	// Wrapper pids of live jobs (running 777, queued 4242) are protected.
	eventually(t, "protected", func() bool {
		p := ft.prot()
		return len(p) == 2 && p[0] == 777 && p[1] == 4242
	})
}

func TestStatusProtectedPIDs(t *testing.T) {
	ft := &fakeTracker{}
	h := newHarness(t, hopt{fn: miniPolicy, opts: func(o *Options) { o.Tracker = ft }})
	a := h.openStream("/v1/jobs", proto.SubmitRequest{Argv: []string{"a"}, WrapperPID: 777})
	idA := a.expect(proto.VerbAccepted).Job
	a.expect(proto.VerbAdmitted)
	b := h.openStream("/v1/jobs", proto.SubmitRequest{Argv: []string{"b"}, WrapperPID: 888})
	b.expect(proto.VerbAccepted)
	b.expect(proto.VerbAdmitted)
	eventually(t, "both protected", func() bool { p := ft.prot(); return len(p) == 2 && p[0] == 777 && p[1] == 888 })
	h.post("/v1/jobs/"+idA+"/exit", proto.ExitRequest{})
	a.expectEnd()
	eventually(t, "only live wrapper protected", func() bool { p := ft.prot(); return len(p) == 1 && p[0] == 888 })
}

func TestSamplerFaultKeepsLastSample(t *testing.T) {
	be := &splitBackend{Fake: platform.NewFake()}
	h := newHarness(t, hopt{fn: miniPolicy, backend: be})
	eventually(t, "a good sample", func() bool { return h.rec.count() >= 1 })
	good := h.rec.inputs()[0].Machine
	be.set(errors.New("sysctl broke"), errors.New("proc list broke"))
	s := h.openStream("/v1/jobs", submitReq("x"))
	s.expect(proto.VerbAccepted)
	s.expect(proto.VerbAdmitted) // admission still works on the last good sample
	eventually(t, "faults", func() bool {
		f := strings.Join(h.status().Faults, "\n")
		return strings.Contains(f, "sample_machine: sysctl broke") && strings.Contains(f, "list_procs: proc list broke")
	})
	ins := h.rec.inputs()
	if m := ins[len(ins)-1].Machine; m != good {
		t.Fatalf("machine %+v, want last good %+v", m, good)
	}
	be.set(nil, nil)
	eventually(t, "faults age out of nothing but count stays", func() bool { return h.d.faults.total.Load() >= 2 })
}

func TestNoSampleHoldsEvents(t *testing.T) {
	be := &splitBackend{Fake: platform.NewFake()}
	be.set(errors.New("down"), nil)
	h := newHarness(t, hopt{fn: miniPolicy, backend: be, noStart: true})
	h.d.pushTimeout = 50 * time.Millisecond
	h.d.evCh = make(chan policy.InEvent, 4)
	h.start()
	time.Sleep(150 * time.Millisecond)
	if h.rec.count() != 0 {
		t.Fatal("policy stepped without any machine sample")
	}
	// Fill the queue: submits wait in the channel (never stepped); the
	// overflow gets 503 so the wrapper can fail open.
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := json.Marshal(submitReq("x"))
			resp, err := h.client.Post(h.url("/v1/jobs"), "application/json", bytes.NewReader(b))
			if err != nil {
				codes <- -1
				return
			}
			codes <- resp.StatusCode
			resp.Body.Close()
		}()
	}
	n503 := 0
	deadline := time.After(10 * time.Second)
	for n503 == 0 {
		select {
		case c := <-codes:
			if c == 503 {
				n503++
			}
		case <-deadline:
			t.Fatal("no 503 while the queue was full")
		}
	}
	be.set(nil, nil) // sampler recovers: queued submits are admitted
	eventually(t, "stepped after recovery", func() bool { return h.rec.count() >= 1 })
	h.stop()
	wg.Wait()
}

func TestPersistenceAndRestart(t *testing.T) {
	dir := shortDir(t)
	h := newHarness(t, hopt{dir: dir, fn: miniPolicy})
	s := h.openStream("/v1/jobs", submitReq("make", "-j8"))
	id := s.expect(proto.VerbAccepted).Job
	s.expect(proto.VerbAdmitted)
	h.post("/v1/jobs/"+id+"/started", proto.StartedRequest{PID: 5100, PGID: 5100})
	// A finished job, for history and the recent list.
	s2 := h.openStream("/v1/jobs", submitReq("done-job"))
	id2 := s2.expect(proto.VerbAccepted).Job
	s2.expect(proto.VerbAdmitted)
	h.post("/v1/jobs/"+id2+"/exit", proto.ExitRequest{Code: 0})
	s2.expectEnd()
	eventually(t, "started routed", func() bool { return hasEvent(h.rec.events(), policy.EvStarted, id) })
	h.stop()
	s.expectEnd() // daemon shutdown ends the stream ...
	if hasEvent(h.rec.events(), policy.EvLost, id) {
		t.Fatal("shutdown must not inject EvLost") // ... without declaring the wrapper lost
	}
	if len(h.fake.Signals()) != 0 {
		t.Fatal("shutdown signalled processes")
	}
	if !h.log.closed {
		t.Fatal("log not closed")
	}

	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		t.Fatal(err)
	}
	if sf.V != 1 || len(sf.State.Jobs) != 1 || sf.State.Jobs[0].ID != id || sf.State.Jobs[0].PGID != 5100 || len(sf.State.History) != 1 {
		t.Fatalf("state file %+v", sf.State)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "state.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("state.json mode %v", fi.Mode())
	}

	// Restart: EvLost for the running job in the first step, history kept.
	h2 := newHarness(t, hopt{dir: dir, fn: miniPolicy})
	eventually(t, "restart step", func() bool { return h2.rec.count() >= 1 })
	first := h2.rec.inputs()[0]
	if len(first.Events) != 1 || first.Events[0].Kind != policy.EvLost || first.Events[0].Job != id {
		t.Fatalf("first step after restart events %+v", first.Events)
	}
	starts := h2.log.kinds(dlog.KindStart)
	if len(starts) != 1 || starts[0].State == nil || len(starts[0].State.History) != 1 || starts[0].Params == nil {
		t.Fatalf("start record %+v", starts)
	}
	if starts[0].Seq <= h.log.records()[len(h.log.records())-1].Seq {
		t.Fatal("decision log seq must continue across restarts")
	}
	_, jb := h2.get("/v1/jobs/" + id)
	var js proto.JobStatus
	_ = json.Unmarshal(jb, &js)
	if js.State != policy.JobRunning || len(js.Events) != 2 || js.Events[1].Verb != proto.VerbAdmitted || js.Argv[0] != "make" {
		t.Fatalf("restored job %+v", js)
	}
	_, jb = h2.get("/v1/jobs/" + id2)
	js = proto.JobStatus{}
	_ = json.Unmarshal(jb, &js)
	if js.State != policy.JobDone || js.ExitCode == nil {
		t.Fatalf("restored finished job %+v", js)
	}
	// Exit without attach works for a restored job.
	resp, eb := h2.post("/v1/jobs/"+id+"/progress", proto.ProgressRequest{OutputBytes: 1})
	mustCode(t, resp, eb, 204)

	// Re-attach: stream resumes with ACCEPTED, the policy's reply, then ends on exit.
	a := h2.openStream("/v1/jobs/"+id+"/attach", proto.AttachRequest{PID: 5100, PGID: 5100, StartMS: 1})
	a.expect(proto.VerbAccepted)
	if e := a.expect(proto.VerbInfo); e.Msg != "attached" {
		t.Fatalf("attach reply %+v", e)
	}
	var att policy.InEvent
	for _, e := range h2.rec.events() {
		if e.Kind == policy.EvAttach {
			att = e
		}
	}
	if att.Submit == nil || att.Submit.Argv[0] != "make" || att.PID != 5100 || att.PGID != 5100 {
		t.Fatalf("attach event %+v (submit info must come from the restored job table)", att)
	}
	h2.post("/v1/jobs/"+id+"/exit", proto.ExitRequest{})
	a.expectEnd()
}

func TestAttachUnknownJob(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	a := h.openStream("/v1/jobs/jabc-7/attach", proto.AttachRequest{Submit: proto.SubmitRequest{Class: "test", Argv: []string{"pytest"}, WrapperPID: 31}, PID: 900, PGID: 900, SubmitMS: 5})
	if e := a.expect(proto.VerbAccepted); e.Job != "jabc-7" {
		t.Fatalf("accepted %+v", e)
	}
	a.expect(proto.VerbInfo)
	var st proto.Status
	eventually(t, "attached job running", func() bool { st = h.status(); return len(st.Running) == 1 })
	if len(st.Running) != 1 || st.Running[0].ID != "jabc-7" || st.Running[0].Argv[0] != "pytest" {
		t.Fatalf("running %+v", st.Running)
	}
	// A second attach replaces the first stream without EvLost.
	a2 := h.openStream("/v1/jobs/jabc-7/attach", proto.AttachRequest{PID: 900, PGID: 900})
	a2.expect(proto.VerbAccepted)
	a.expectEnd()
	a2.expect(proto.VerbInfo)
	time.Sleep(50 * time.Millisecond)
	if hasEvent(h.rec.events(), policy.EvLost, "jabc-7") {
		t.Fatal("replaced stream produced EvLost")
	}
	a2.close()
	eventually(t, "EvLost for the current stream", func() bool { return hasEvent(h.rec.events(), policy.EvLost, "jabc-7") })
}

func TestCorruptStateMovedAside(t *testing.T) {
	for _, content := range []string{"{not json", `{"v":99,"state":{}}`} {
		dir := shortDir(t)
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, hopt{dir: dir, fn: miniPolicy})
		if f := strings.Join(h.status().Faults, "\n"); !strings.Contains(f, "state_load") {
			t.Fatalf("faults %q", f)
		}
		m, _ := filepath.Glob(filepath.Join(dir, "state.json.corrupt-*"))
		if len(m) != 1 {
			t.Fatalf("corrupt file not moved aside: %v", m)
		}
		if b, _ := os.ReadFile(m[0]); string(b) != content {
			t.Fatal("corrupt file content changed")
		}
		// Fresh start works.
		s := h.openStream("/v1/jobs", submitReq("x"))
		s.expect(proto.VerbAccepted)
		s.expect(proto.VerbAdmitted)
		h.stop()
	}
}

func TestStaleSocketAndAlreadyRunning(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "s.sock")
	// A stale socket: bound, then the listener closed without unlinking.
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Lstat(sock); err != nil {
		t.Fatal("stale socket missing")
	}
	h := newHarness(t, hopt{dir: dir, fn: miniPolicy})
	if resp, b := h.get("/healthz"); resp.StatusCode != 200 || string(b) != "ok\n" {
		t.Fatalf("healthz %d %q", resp.StatusCode, b)
	}
	fi, err := os.Stat(sock)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v %v", fi, err)
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", di.Mode())
	}

	// Second daemon on the same paths fails clearly and leaves the first alone.
	err = Run(context.Background(), Options{Config: baseConfig(dir), Backend: platform.NewFake(), Log: &memLog{}, NoSignals: true, NoSelfTune: true})
	if err == nil || !strings.Contains(err.Error(), "another pace daemon is running") {
		t.Fatalf("second daemon: %v", err)
	}
	if resp, _ := h.get("/healthz"); resp.StatusCode != 200 {
		t.Fatal("first daemon broken by the second")
	}
	h.stop()
	if _, err := os.Lstat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket not removed on shutdown: %v", err)
	}
	// After a clean stop a new daemon starts on the same paths.
	h3 := newHarness(t, hopt{dir: dir, fn: miniPolicy})
	if resp, _ := h3.get("/healthz"); resp.StatusCode != 200 {
		t.Fatal("restart failed")
	}
}

func TestRemoveStaleSocket(t *testing.T) {
	dir := shortDir(t)
	live := filepath.Join(dir, "live.sock")
	ln, err := net.Listen("unix", live)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	regular := filepath.Join(dir, "file")
	os.WriteFile(regular, []byte("x"), 0o600)
	stale := filepath.Join(dir, "stale.sock")
	sl, _ := net.Listen("unix", stale)
	sl.(*net.UnixListener).SetUnlinkOnClose(false)
	sl.Close()

	cases := []struct {
		name, path, errSub string
		gone               bool
	}{
		{"missing", filepath.Join(dir, "none.sock"), "", true},
		{"live", live, "another pace daemon is running", false},
		{"regular file", regular, "is not a socket", false},
		{"stale", stale, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := removeStaleSocket(c.path)
			if c.errSub == "" && err != nil || c.errSub != "" && (err == nil || !strings.Contains(err.Error(), c.errSub)) {
				t.Fatalf("err %v, want %q", err, c.errSub)
			}
			_, serr := os.Lstat(c.path)
			if gone := errors.Is(serr, os.ErrNotExist); gone != c.gone {
				t.Fatalf("gone=%v want %v", gone, c.gone)
			}
		})
	}
}

func TestSocketPathTooLong(t *testing.T) {
	dir := shortDir(t)
	cfg := baseConfig(dir)
	cfg.Socket = filepath.Join(dir, strings.Repeat("x", 120)+".sock")
	err := Run(context.Background(), Options{Config: cfg, Backend: platform.NewFake(), Log: &memLog{}, NoSignals: true, NoSelfTune: true})
	if err == nil || !strings.Contains(err.Error(), "limit is 103") {
		t.Fatalf("err %v", err)
	}
}

func TestPeerCredCheck(t *testing.T) {
	dir := shortDir(t)
	for i, c := range []struct {
		name  string
		uid   int
		allow bool
	}{{"same uid", os.Geteuid(), true}, {"other uid", os.Geteuid() + 1, false}} {
		t.Run(c.name, func(t *testing.T) {
			sock := filepath.Join(dir, "p"+string(rune('0'+i))+".sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var rejected []string
			reject := func(kind, msg string) { mu.Lock(); rejected = append(rejected, kind+": "+msg); mu.Unlock() }
			cl := &credListener{Listener: ln, uid: c.uid, reject: reject}
			srv := &http.Server{ConnContext: connContext, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pc, ok := peerFrom(r.Context())
				if !ok || !pc.Known || pc.UID != os.Geteuid() || pc.PID != os.Getpid() {
					http.Error(w, "bad cred", 500)
					return
				}
				w.Write([]byte("ok"))
			})}
			done := make(chan struct{})
			go func() { srv.Serve(cl); close(done) }()
			client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			}}}
			resp, err := client.Get("http://pace/")
			if c.allow {
				if err != nil {
					t.Fatal(err)
				}
				var b bytes.Buffer
				b.ReadFrom(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || b.String() != "ok" {
					t.Fatalf("%d %s", resp.StatusCode, b.String())
				}
			} else {
				if err == nil {
					resp.Body.Close()
					t.Fatal("connection from another uid accepted")
				}
				mu.Lock()
				defer mu.Unlock()
				if len(rejected) == 0 || !strings.Contains(rejected[0], "peer_cred: rejected connection from uid") {
					t.Fatalf("rejections %v", rejected)
				}
			}
			client.CloseIdleConnections()
			srv.Close()
			<-done
		})
	}
}

func TestReload(t *testing.T) {
	var mu sync.Mutex
	var next config.Resolved
	var nextErr error
	calls := 0
	sigs := make(chan os.Signal, 1)
	h := newHarness(t, hopt{fn: miniPolicy, opts: func(o *Options) {
		o.ConfigPath = "/nonexistent/config.toml"
		o.Signals = sigs
		o.LoadConfig = func(path string, total int64, plat string) (config.Resolved, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if path != "/nonexistent/config.toml" || total != 16<<30 {
				return config.Resolved{}, errors.New("unexpected args")
			}
			return next, nextErr
		}
	}})
	h.waitPublished()

	good := baseConfig("/elsewhere") // home/socket changes are ignored
	good.Params.Mode = policy.ModeWarnOnly
	good.Hash = "sha256:new"
	good.Source = "reloaded.toml"
	mu.Lock()
	next = good
	mu.Unlock()
	resp, b := h.post("/v1/reload", nil)
	mustCode(t, resp, b, 204)
	eventually(t, "mode applied", func() bool { return h.status().Mode == policy.ModeWarnOnly })
	cr := h.log.kinds(dlog.KindConfig)
	if len(cr) != 1 || cr[0].CfgHash != "sha256:new" || cr[0].Params == nil || cr[0].Params.Mode != policy.ModeWarnOnly || !strings.Contains(cr[0].Msg, "need a restart") {
		t.Fatalf("config record %+v", cr)
	}
	if h.d.sockPath != filepath.Join(h.dir, "s.sock") {
		t.Fatal("socket path changed on reload")
	}

	mu.Lock()
	nextErr = errors.New(`config: unknown key "solts"`)
	mu.Unlock()
	resp, b = h.post("/v1/reload", nil)
	mustCode(t, resp, b, 400)
	var eb errorBody
	_ = json.Unmarshal(b, &eb)
	if !strings.Contains(eb.Error, "solts") {
		t.Fatalf("error body %q", b)
	}
	time.Sleep(100 * time.Millisecond)
	if h.status().Mode != policy.ModeWarnOnly {
		t.Fatal("bad reload changed the config")
	}
	cr = h.log.kinds(dlog.KindConfig)
	if len(cr) != 2 || !strings.Contains(cr[1].Msg, "reload failed") || cr[1].CfgHash != "sha256:new" {
		t.Fatalf("failed reload record %+v", cr[len(cr)-1])
	}

	// SIGHUP reloads too.
	mu.Lock()
	nextErr = nil
	good.Params.Mode = policy.ModeObserve
	next = good
	mu.Unlock()
	sigs <- syscall.SIGHUP
	eventually(t, "SIGHUP reload", func() bool { return h.status().Mode == policy.ModeObserve })
	mu.Lock()
	if calls != 3 {
		t.Errorf("LoadConfig calls %d", calls)
	}
	mu.Unlock()
	// SIGTERM shuts down.
	sigs <- syscall.SIGTERM
	select {
	case err := <-h.errc:
		h.stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not stop the daemon")
	}
}

func TestMetrics(t *testing.T) {
	var once sync.Once
	h := newHarness(t, hopt{fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		s, out := miniPolicy(p, s, in)
		if len(s.Jobs) > 0 {
			once.Do(func() {
				out.Events = append(out.Events, proto.Event{Verb: proto.VerbKilled, Job: s.Jobs[0].ID, Policy: proto.PolicyMemBudget})
			})
		}
		return s, out
	}})
	s := h.openStream("/v1/jobs", submitReq("x"))
	s.expect(proto.VerbAccepted)
	s.expect(proto.VerbAdmitted)
	s.expect(proto.VerbKilled)
	h.d.fault("test", "a fault")
	eventually(t, "metrics", func() bool {
		_, b := h.get("/metrics")
		return strings.Contains(string(b), `pace_jobs{state="running"} 1`)
	})
	resp, b := h.get("/metrics")
	mustCode(t, resp, b, 200)
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content type %q", ct)
	}
	text := string(b)
	for _, want := range []string{
		"pace_capacity_bytes 8589934592\n", "pace_headroom_bytes ", "pace_avail_bytes ", "pace_pressure_level 0\n",
		`pace_jobs{state="queued"} 0`, `pace_events_total{verb="ACCEPTED"} 1`, `pace_events_total{verb="ADMITTED"} 1`,
		`pace_kills_total{policy="mem_budget"} 1`, "pace_sample_seconds ", "pace_faults_total ",
		"# TYPE pace_events_total counter",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	line := regexp.MustCompile(`^[a-z_]+(\{[a-z_]+="[^"]*"\})? [0-9.e+-]+$`)
	for _, l := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.HasPrefix(l, "# HELP ") || strings.HasPrefix(l, "# TYPE ") {
			continue
		}
		if !line.MatchString(l) {
			t.Errorf("bad exposition line %q", l)
		}
	}
}

func TestDecisionLogRecords(t *testing.T) {
	var mu sync.Mutex
	emit := false
	h := newHarness(t, hopt{cfg: func(c *config.Resolved) { c.HeartbeatMS = 150 }, fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		s, out := miniPolicy(p, s, in)
		// Mutate a telemetry field every step (not material) ...
		for i := range s.Jobs {
			s.Jobs[i].UsedBytes = in.NowMS
		}
		mu.Lock()
		if emit {
			emit = false
			out.Events = append(out.Events, proto.Event{Verb: proto.VerbInfo, Msg: "x"})
		}
		mu.Unlock()
		return s, out
	}})
	s := h.openStream("/v1/jobs", submitReq("x"))
	id := s.expect(proto.VerbAccepted).Job
	s.expect(proto.VerbAdmitted)
	time.Sleep(100 * time.Millisecond) // let the submit step finish logging
	nSteps := len(h.log.kinds(dlog.KindStep))
	// ... which must not produce a step record per tick, only heartbeats.
	time.Sleep(400 * time.Millisecond)
	if got := len(h.log.kinds(dlog.KindStep)); got != nSteps {
		t.Fatalf("telemetry-only steps logged: %d -> %d", nSteps, got)
	}
	if hb := h.log.kinds(dlog.KindHeartbeat); len(hb) < 1 || hb[0].State == nil || hb[0].In == nil {
		t.Fatalf("heartbeats %+v", hb)
	}
	mu.Lock()
	emit = true
	mu.Unlock()
	eventually(t, "event step logged", func() bool { return len(h.log.kinds(dlog.KindStep)) == nSteps+1 })

	recs := h.log.records()
	if recs[0].Kind != dlog.KindStart || recs[0].Params == nil || recs[0].Version != "test" || recs[0].CfgHash != "sha256:test" {
		t.Fatalf("first record %+v", recs[0])
	}
	var submitRec *dlog.Record
	for i := range recs {
		r := &recs[i]
		if i > 0 && r.Seq != recs[i-1].Seq+1 {
			t.Fatalf("seq gap %d -> %d", recs[i-1].Seq, r.Seq)
		}
		if r.Kind == dlog.KindStep || r.Kind == dlog.KindHeartbeat {
			if r.State == nil || r.In == nil || r.Out == nil || r.Params == nil || !strings.HasPrefix(r.StateHash, "sha256:") || r.TS != r.In.NowMS {
				t.Fatalf("incomplete record %+v", r)
			}
			// Self-contained: replaying the step gives the logged hash.
			if r.Kind == dlog.KindStep && hasEvent(r.In.Events, policy.EvSubmit, id) {
				submitRec = r
			}
		}
	}
	if submitRec == nil {
		t.Fatal("no record for the submit step")
	}
	if len(submitRec.State.Jobs) != 0 || len(submitRec.Out.Events) != 1 || submitRec.Out.Events[0].Verb != proto.VerbAdmitted {
		t.Fatalf("submit record: before=%+v out=%+v", submitRec.State.Jobs, submitRec.Out)
	}
	after, _ := miniPolicy(*submitRec.Params, *submitRec.State, *submitRec.In)
	for i := range after.Jobs {
		after.Jobs[i].UsedBytes = submitRec.In.NowMS
	}
	if stateHash(after) != submitRec.StateHash {
		t.Fatal("replaying the record does not reproduce its state hash")
	}
}

func TestStateBeforeIsolatedFromPolicyMutation(t *testing.T) {
	// A policy that mutates its input in place must not corrupt the
	// logged "before" state.
	h := newHarness(t, hopt{fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		s, out := miniPolicy(p, s, in)
		if len(s.Jobs) > 0 {
			s.Jobs[0].Enf.Warnings = append(s.Jobs[0].Enf.Warnings[:0], policy.Warning{Policy: "p", IssuedMS: in.NowMS})
			s.Jobs[0].Class = "mutated-" + time.Now().String()
		}
		return s, out
	}})
	s := h.openStream("/v1/jobs", submitReq("x"))
	s.expect(proto.VerbAccepted)
	s.expect(proto.VerbAdmitted)
	time.Sleep(200 * time.Millisecond) // -race checks concurrent access
	for _, r := range h.log.kinds(dlog.KindStep) {
		for _, j := range r.State.Jobs {
			if j.Class == "build" {
				continue
			}
			if !strings.HasPrefix(j.Class, "mutated-") {
				t.Fatalf("before state %+v", j)
			}
		}
	}
}

func TestFastIntervalSwitching(t *testing.T) {
	var mu sync.Mutex
	level := proto.LevelNormal
	be := &splitBackend{Fake: platform.NewFake()}
	h := newHarness(t, hopt{backend: be, cfg: func(c *config.Resolved) { c.SampleMS, c.FastMS = 200, 20 }, fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		mu.Lock()
		s.Pressure.Level = level
		mu.Unlock()
		s.LastMS = in.NowMS
		return s, policy.Output{}
	}})
	eventually(t, "normal interval", func() bool { return h.d.curIvMS.Load() == 200 })
	n0 := be.sampleCount()
	time.Sleep(450 * time.Millisecond)
	slow := be.sampleCount() - n0
	mu.Lock()
	level = proto.LevelWarn
	mu.Unlock()
	eventually(t, "fast interval", func() bool { return h.d.curIvMS.Load() == 20 })
	n1 := be.sampleCount()
	time.Sleep(450 * time.Millisecond)
	fast := be.sampleCount() - n1
	if slow > 4 || fast < 10 {
		t.Fatalf("samples in 450ms: normal=%d fast=%d", slow, fast)
	}
	mu.Lock()
	level = proto.LevelNormal
	mu.Unlock()
	eventually(t, "back to normal", func() bool { return h.d.curIvMS.Load() == 200 })
}

func TestConcurrentSubmits(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	const n = 40
	var wg sync.WaitGroup
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b, _ := json.Marshal(submitReq("job", string(rune('a'+i%26))))
			resp, err := h.client.Post(h.url("/v1/jobs"), "application/json", bytes.NewReader(b))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			dec := json.NewDecoder(resp.Body)
			var acc, adm proto.Event
			if err := dec.Decode(&acc); err != nil || acc.Verb != proto.VerbAccepted {
				errs <- errors.New("no ACCEPTED")
				return
			}
			if err := dec.Decode(&adm); err != nil || adm.Verb != proto.VerbAdmitted || adm.Job != acc.Job {
				errs <- errors.New("no ADMITTED")
				return
			}
			if r, err := h.client.Get(h.url("/v1/status")); err == nil { // concurrent readers
				r.Body.Close()
			}
			eb, _ := json.Marshal(proto.ExitRequest{Code: i})
			r2, err := h.client.Post(h.url("/v1/jobs/"+acc.Job+"/exit"), "application/json", bytes.NewReader(eb))
			if err != nil {
				errs <- err
				return
			}
			r2.Body.Close()
			var extra proto.Event
			if err := dec.Decode(&extra); err == nil {
				errs <- errors.New("event after exit: " + extra.Verb)
				return
			}
			ids <- acc.Job
		}(i)
	}
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate id %s", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("%d jobs completed", len(seen))
	}
	eventually(t, "all done", func() bool { st := h.status(); return len(st.Running) == 0 && len(st.Queued) == 0 })
	for _, e := range h.rec.events() {
		if e.Kind == policy.EvLost {
			t.Fatalf("EvLost %+v", e)
		}
	}
}

func TestNotifyOnKill(t *testing.T) {
	var mu sync.Mutex
	var notes []string
	level := proto.LevelNormal
	kills := 0
	h := newHarness(t, hopt{
		cfg: func(c *config.Resolved) { c.NotifyMacOS = true },
		opts: func(o *Options) {
			o.Notify = func(title, msg string) { mu.Lock(); notes = append(notes, title+"|"+msg); mu.Unlock() }
		},
		fn: func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
			var out policy.Output
			mu.Lock()
			s.Pressure.Level = level
			if kills > 0 {
				kills--
				out.Events = append(out.Events, proto.Event{Verb: proto.VerbKilled, Unit: "pg:5", Policy: proto.PolicyPressure})
			}
			mu.Unlock()
			s.LastMS = in.NowMS
			return s, out
		},
	})
	mu.Lock()
	kills = 3
	mu.Unlock()
	eventually(t, "kills emitted", func() bool { mu.Lock(); defer mu.Unlock(); return kills == 0 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	if len(notes) != 1 || !strings.Contains(notes[0], "pace: KILLED policy=pressure") {
		t.Fatalf("notes %v (rate limit: one per 10s)", notes)
	}
	mu.Unlock()

	// At critical pressure: never.
	h.d2resetNotify()
	mu.Lock()
	level, kills = proto.LevelCritical, 2
	mu.Unlock()
	eventually(t, "kills emitted", func() bool { mu.Lock(); defer mu.Unlock(); return kills == 0 })
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 {
		t.Fatalf("notified at critical pressure: %v", notes)
	}
}

// d2resetNotify clears the notification rate limiter on the loop goroutine.
func (h *harness) d2resetNotify() {
	h.testInLoop(func(d *daemon) { d.lastNotify = time.Time{} })
}

// testInLoop runs fn on the loop goroutine (race-free access to loop state).
func (h *harness) testInLoop(fn func(*daemon)) {
	done := make(chan struct{})
	h.d.reloadCh <- reloadReq{hook: fn, done: done}
	<-done
}

func TestShutdownKeepsJobs(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy})
	h.fake.Lock()
	h.fake.Procs = []platform.Proc{{PID: 5300, PGID: 5300, UID: os.Getuid(), Comm: "job"}}
	h.fake.Unlock()
	s := h.openStream("/v1/jobs", submitReq("x"))
	id := s.expect(proto.VerbAccepted).Job
	s.expect(proto.VerbAdmitted)
	h.post("/v1/jobs/"+id+"/started", proto.StartedRequest{PID: 5300, PGID: 5300})
	h.stop()
	s.expectEnd()
	if len(h.fake.Signals()) != 0 {
		t.Fatal("shutdown signalled the job")
	}
	h.fake.Lock()
	defer h.fake.Unlock()
	if len(h.fake.Procs) != 1 {
		t.Fatal("job gone")
	}
}

var _ = attrib.Result{}

// notedBackend is a Fake whose throttle is not fully reversible.
type notedBackend struct{ *platform.Fake }

func (notedBackend) ThrottleNote(bool) string { return "throttle keeps nice 10" }

// Review of finding 42: a backend whose UNTHROTTLED cannot undo everything
// (Linux unprivileged nice) says so in status, not only in code comments.
func TestStatusCarriesThrottleNote(t *testing.T) {
	h := newHarness(t, hopt{fn: miniPolicy, backend: notedBackend{platform.NewFake()}})
	eventually(t, "note in status", func() bool {
		n := h.status().Notes
		return len(n) == 1 && n[0] == "throttle keeps nice 10"
	})
	h2 := newHarness(t, hopt{fn: miniPolicy, backend: platform.NewFake()})
	eventually(t, "a sample", func() bool { return h2.rec.count() >= 1 })
	if n := h2.status().Notes; len(n) != 0 {
		t.Errorf("reversible backend has notes %v", n)
	}
}
