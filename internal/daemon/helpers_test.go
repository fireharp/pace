package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// shortDir returns a fresh directory under /tmp (t.TempDir paths are too
// long for unix sockets on macOS).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// memLog is an in-memory LogWriter.
type memLog struct {
	mu     sync.Mutex
	recs   []dlog.Record
	closed bool
}

func (m *memLog) Write(r dlog.Record) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Marshal now, like an async writer eventually would, to catch races
	// on shared state under -race.
	b, _ := json.Marshal(r)
	var c dlog.Record
	_ = json.Unmarshal(b, &c)
	m.recs = append(m.recs, c)
}

func (m *memLog) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return nil
}

func (m *memLog) records() []dlog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]dlog.Record(nil), m.recs...)
}

func (m *memLog) kinds(kind string) []dlog.Record {
	var out []dlog.Record
	for _, r := range m.records() {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// recorder wraps a step function and records every Inputs it saw.
type recorder struct {
	mu    sync.Mutex
	ins   []policy.Inputs
	fn    StepFunc
	calls int
}

func (r *recorder) step(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
	r.mu.Lock()
	r.ins = append(r.ins, cloneInputs(in))
	r.calls++
	fn := r.fn
	r.mu.Unlock()
	if fn == nil {
		s.LastMS = in.NowMS
		return s, policy.Output{}
	}
	return fn(p, s, in)
}

func (r *recorder) setFn(fn StepFunc) {
	r.mu.Lock()
	r.fn = fn
	r.mu.Unlock()
}

// events returns every InEvent seen so far, in order.
func (r *recorder) events() []policy.InEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []policy.InEvent
	for _, in := range r.ins {
		out = append(out, in.Events...)
	}
	return out
}

func (r *recorder) inputs() []policy.Inputs {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]policy.Inputs(nil), r.ins...)
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// miniPolicy is a tiny deterministic policy for daemon tests:
//   - submit: argv[0]=="refuse" -> REFUSED; argv[0]=="queue" -> queued
//     (QUEUED event); else running + ADMITTED
//   - started: pid/pgid; progress: output bytes
//   - exit: job removed, History "class:<c>" gets a peak
//   - lost: queued -> removed; running -> WrapperLostMS set
//   - attach: job (re)created running, INFO "attached"
func miniPolicy(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
	var out policy.Output
	s.LastMS = in.NowMS
	find := func(id string) int {
		for i := range s.Jobs {
			if s.Jobs[i].ID == id {
				return i
			}
		}
		return -1
	}
	remove := func(i int) { s.Jobs = append(s.Jobs[:i:i], s.Jobs[i+1:]...) }
	for _, ev := range in.Events {
		i := find(ev.Job)
		switch ev.Kind {
		case policy.EvSubmit:
			if i >= 0 {
				continue
			}
			cls := ev.Submit.Class
			if ev.Submit.Argv[0] == "refuse" {
				out.Events = append(out.Events, proto.Event{Verb: proto.VerbRefused, Job: ev.Job, Class: cls, Reason: proto.ReasonHeadroom, RetryAfterMS: proto.I64(40000), Hint: "retry later"})
				continue
			}
			j := policy.Job{ID: ev.Job, Seq: s.NextSeq, Class: cls, State: policy.JobRunning, SubmitMS: in.NowMS, AdmitMS: in.NowMS, EstimateBytes: ev.Submit.EstimateBytes, BudgetBytes: ev.Submit.BudgetBytes}
			s.NextSeq++
			if ev.Submit.Argv[0] == "queue" {
				j.State, j.AdmitMS, j.LastPosition = policy.JobQueued, 0, 1
				out.Events = append(out.Events, proto.Event{Verb: proto.VerbQueued, Job: ev.Job, Reason: proto.ReasonHeadroom, Position: proto.Int(1)})
			} else {
				out.Events = append(out.Events, proto.Event{Verb: proto.VerbAdmitted, Job: ev.Job, Class: cls})
			}
			s.Jobs = append(s.Jobs, j)
		case policy.EvStarted:
			if i >= 0 {
				s.Jobs[i].PID, s.Jobs[i].PGID, s.Jobs[i].StartMS = ev.PID, ev.PGID, ev.StartMS
			}
		case policy.EvProgress:
			if i >= 0 {
				s.Jobs[i].OutputBytes = ev.OutputBytes
			}
		case policy.EvExit:
			if i >= 0 {
				cls := s.Jobs[i].Class
				remove(i)
				s.History = addPeak(s.History, "class:"+cls, 1<<20)
			}
		case policy.EvLost:
			if i >= 0 {
				if s.Jobs[i].State == policy.JobQueued {
					remove(i)
				} else {
					s.Jobs[i].WrapperLostMS = in.NowMS
				}
			}
		case policy.EvAttach:
			if i < 0 {
				s.Jobs = append(s.Jobs, policy.Job{ID: ev.Job, Seq: s.NextSeq, Class: ev.Submit.Class, State: policy.JobRunning, SubmitMS: in.NowMS})
				s.NextSeq++
				i = len(s.Jobs) - 1
			}
			s.Jobs[i].WrapperLostMS = 0
			s.Jobs[i].PID, s.Jobs[i].PGID = ev.PID, ev.PGID
			out.Events = append(out.Events, proto.Event{Verb: proto.VerbInfo, Job: ev.Job, Msg: "attached"})
		}
	}
	sort.Slice(s.Jobs, func(a, b int) bool { return s.Jobs[a].Seq < s.Jobs[b].Seq })
	return s, out
}

func addPeak(h []policy.History, key string, peak int64) []policy.History {
	h = append([]policy.History(nil), h...)
	for i := range h {
		if h[i].Key == key {
			h[i].Peaks = append(append([]int64(nil), h[i].Peaks...), peak)
			return h
		}
	}
	h = append(h, policy.History{Key: key, Peaks: []int64{peak}, DurMS: []int64{0}})
	sort.Slice(h, func(a, b int) bool { return h[a].Key < h[b].Key })
	return h
}

// harness is one running daemon under test.
type harness struct {
	t       *testing.T
	d       *daemon
	dir     string
	sock    string
	fake    *platform.Fake
	log     *memLog
	rec     *recorder
	cancel  context.CancelFunc
	errc    chan error
	client  *http.Client
	stopped bool
}

type hopt struct {
	dir     string
	cfg     func(*config.Resolved)
	opts    func(*Options)
	backend platform.Backend
	fn      StepFunc
	noStart bool
}

func baseConfig(dir string) config.Resolved {
	return config.Resolved{
		Home: dir, Socket: filepath.Join(dir, "s.sock"), LogDir: filepath.Join(dir, "logs"),
		SampleMS: 50, FastMS: 10, HeartbeatMS: 60_000, Hash: "sha256:test", Source: "test",
		Params: policy.Params{
			// AdmitBelowWarn keeps the plain 1G reserve these tests' figures use.
			Mode: policy.ModeEnforce, CapacityBytes: 8 << 30, ReserveBytes: 1 << 30, AdmitBelowWarn: true,
			WarnAvailPermille: 150, CritAvailPermille: 70, DefaultClass: "default",
			Classes: []policy.ClassParams{{Name: "build", EstimateBytes: 2 << 30, Slots: 2}, {Name: "default", EstimateBytes: 1 << 30}},
		},
	}
}

func newHarness(t *testing.T, h hopt) *harness {
	t.Helper()
	dir := h.dir
	if dir == "" {
		dir = shortDir(t)
	}
	cfg := baseConfig(dir)
	if h.cfg != nil {
		h.cfg(&cfg)
	}
	fake := platform.NewFake()
	var be platform.Backend = fake
	if h.backend != nil {
		be = h.backend
	}
	rec := &recorder{fn: h.fn}
	ml := &memLog{}
	o := Options{
		Config: cfg, Backend: be, UID: os.Getuid(), Version: "test", Log: ml,
		NoSignals: true, NoSelfTune: true, Step: rec.step,
	}
	if h.opts != nil {
		h.opts(&o)
	}
	d, err := newDaemon(o)
	if err != nil {
		t.Fatal(err)
	}
	hs := &harness{t: t, d: d, dir: dir, sock: d.sockPath, fake: fake, log: ml, rec: rec}
	hs.client = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dd net.Dialer
			return dd.DialContext(ctx, "unix", hs.sock)
		},
		DisableKeepAlives: true,
	}}
	if !h.noStart {
		hs.start()
	}
	return hs
}

func (h *harness) start() {
	h.t.Helper()
	ready := make(chan struct{})
	h.d.o.Ready = func(string) { close(ready) }
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.errc = make(chan error, 1)
	go func() { h.errc <- h.d.run(ctx) }()
	select {
	case <-ready:
	case err := <-h.errc:
		h.t.Fatalf("daemon exited: %v", err)
	case <-time.After(5 * time.Second):
		h.t.Fatal("daemon not ready")
	}
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.stopped {
		return
	}
	h.stopped = true
	h.cancel()
	select {
	case err := <-h.errc:
		if err != nil {
			h.t.Errorf("daemon run: %v", err)
		}
	case <-time.After(10 * time.Second):
		h.t.Error("daemon did not stop")
	}
}

func (h *harness) url(p string) string { return "http://pace" + p }

func (h *harness) post(p string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
		r = nil
	case string:
		r = strings.NewReader(b)
	default:
		bb, _ := json.Marshal(b)
		r = bytes.NewReader(bb)
	}
	resp, err := h.client.Post(h.url(p), "application/json", r)
	if err != nil {
		h.t.Fatalf("POST %s: %v", p, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (h *harness) get(p string) (*http.Response, []byte) {
	h.t.Helper()
	resp, err := h.client.Get(h.url(p))
	if err != nil {
		h.t.Fatalf("GET %s: %v", p, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, data
}

func (h *harness) status() proto.Status {
	h.t.Helper()
	resp, b := h.get("/v1/status")
	if resp.StatusCode != 200 {
		h.t.Fatalf("status: %d %s", resp.StatusCode, b)
	}
	var st proto.Status
	if err := json.Unmarshal(b, &st); err != nil {
		h.t.Fatal(err)
	}
	return st
}

// evStream reads an NDJSON event stream in the background.
type evStream struct {
	t    *testing.T
	resp *http.Response
	ch   chan proto.Event
	done chan struct{}
}

func (h *harness) openStream(path string, body any) *evStream {
	h.t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", h.url(path), bytes.NewReader(b))
	return h.stream(req)
}

func (h *harness) stream(req *http.Request) *evStream {
	h.t.Helper()
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		h.t.Fatalf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, b)
	}
	s := &evStream{t: h.t, resp: resp, ch: make(chan proto.Event, 1024), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.ch)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			var e proto.Event
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				s.t.Errorf("bad stream line %q: %v", sc.Text(), err)
				return
			}
			s.ch <- e
		}
	}()
	h.t.Cleanup(func() { resp.Body.Close() })
	return s
}

func (s *evStream) next() proto.Event {
	s.t.Helper()
	select {
	case e, ok := <-s.ch:
		if !ok {
			s.t.Fatal("stream ended")
		}
		return e
	case <-time.After(5 * time.Second):
		s.t.Fatal("timeout waiting for stream event")
	}
	return proto.Event{}
}

func (s *evStream) expect(verb string) proto.Event {
	s.t.Helper()
	e := s.next()
	if e.Verb != verb {
		s.t.Fatalf("got %s (%+v), want %s", e.Verb, e, verb)
	}
	return e
}

// expectEnd waits for EOF, failing on any further event.
func (s *evStream) expectEnd() {
	s.t.Helper()
	select {
	case e, ok := <-s.ch:
		if ok {
			s.t.Fatalf("unexpected event %+v, want end of stream", e)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("stream did not end")
	}
}

func (s *evStream) close() { s.resp.Body.Close() }

// eventually polls cond until it holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hasEvent(evs []policy.InEvent, kind, job string) bool {
	for _, e := range evs {
		if e.Kind == kind && e.Job == job {
			return true
		}
	}
	return false
}

// splitBackend lets tests fail SampleMachine and ListProcs independently
// and counts calls.
type splitBackend struct {
	*platform.Fake
	mu       sync.Mutex
	machErr  error
	procsErr error
	samples  int
}

func (b *splitBackend) SampleMachine(p []string) (platform.Machine, error) {
	b.mu.Lock()
	b.samples++
	err := b.machErr
	b.mu.Unlock()
	if err != nil {
		return platform.Machine{}, err
	}
	return b.Fake.SampleMachine(p)
}

func (b *splitBackend) ListProcs(uid int) ([]platform.Proc, error) {
	b.mu.Lock()
	err := b.procsErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.Fake.ListProcs(uid)
}

func (b *splitBackend) set(machErr, procsErr error) {
	b.mu.Lock()
	b.machErr, b.procsErr = machErr, procsErr
	b.mu.Unlock()
}

func (b *splitBackend) sampleCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.samples
}

// fakeTracker returns a fixed attribution result.
type fakeTracker struct {
	mu        sync.Mutex
	res       attrib.Result
	protected []int
	opts      []attrib.Options
}

func (f *fakeTracker) SetOptions(o attrib.Options) {
	f.mu.Lock()
	f.opts = append(f.opts, o)
	f.mu.Unlock()
}
func (f *fakeTracker) SetProtectedPIDs(p []int) {
	f.mu.Lock()
	f.protected = append([]int(nil), p...)
	f.mu.Unlock()
}
func (f *fakeTracker) Attribute(procs []platform.Proc, jobs []policy.Job, tracked []policy.Unit) attrib.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.res
}
func (f *fakeTracker) prot() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.protected...)
}

func submitReq(argv ...string) proto.SubmitRequest {
	return proto.SubmitRequest{Class: "build", Argv: argv, WrapperPID: 4242}
}

func mustCode(t *testing.T, resp *http.Response, body []byte, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: status %d (%s), want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, body, want)
	}
}

// waitPublished waits until the loop has stepped and published a status
// built from a real sample.
func (h *harness) waitPublished() {
	h.t.Helper()
	eventually(h.t, "first published step", func() bool {
		s := h.d.snap.Load()
		return s != nil && s.totalBytes > 0
	})
}

func fmtSent(s []platform.SentSignal) string {
	var b strings.Builder
	for _, x := range s {
		fmt.Fprintf(&b, "{pid=%d pgid=%d sig=%d} ", x.PID, x.PGID, x.Sig)
	}
	return b.String()
}
