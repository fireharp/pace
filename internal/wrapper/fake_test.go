package wrapper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// ---------------------------------------------------------------------------
// Fake daemon: an HTTP server on a unix socket that scripts job streams.

type fakeJob struct {
	id       string
	submit   proto.SubmitRequest
	started  chan proto.StartedRequest
	exit     chan proto.ExitRequest
	mu       sync.Mutex
	progress []proto.ProgressRequest
	exits    []proto.ExitRequest // every exit POST, in order
}

func (j *fakeJob) exitList() []proto.ExitRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]proto.ExitRequest(nil), j.exits...)
}

type jobStream struct {
	f   *fakeDaemon
	job *fakeJob
	w   http.ResponseWriter
	ctx context.Context
}

func (s *jobStream) send(ev proto.Event) bool {
	ev.V = 1
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	if ev.Job == "" {
		ev.Job = s.job.id
	}
	b, _ := json.Marshal(ev)
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return false
	}
	s.w.(http.Flusher).Flush()
	return true
}

// waitStarted waits for the job's started POST.
func (s *jobStream) waitStarted(t *testing.T) proto.StartedRequest {
	t.Helper()
	select {
	case st := <-s.job.started:
		s.job.started <- st // keep it readable for the test body
		return st
	case <-s.ctx.Done():
		return proto.StartedRequest{}
	case <-time.After(10 * time.Second):
		t.Errorf("job %s: no started POST", s.job.id)
		return proto.StartedRequest{}
	}
}

// hold keeps the stream open until the wrapper posts exit, the client goes
// away or the fake daemon is stopped.
func (s *jobStream) hold() {
	select {
	case ex := <-s.job.exit:
		s.job.exit <- ex
	case <-s.ctx.Done():
	case <-s.f.closed:
	}
}

type fakeDaemon struct {
	t      *testing.T
	sock   string
	srv    *http.Server
	closed chan struct{}
	once   sync.Once

	// scripts; nil = default behaviour
	onSubmit func(s *jobStream)
	onAttach func(s *jobStream, req proto.AttachRequest)
	// submitStatus > 0 makes POST /v1/jobs fail with that HTTP status.
	submitStatus int
	// progressDelay makes every progress POST hang this long (slow daemon).
	progressDelay time.Duration
	// startedFail > 0 makes that many started POSTs fail with 503.
	startedFail  atomic.Int32
	startedCalls atomic.Int32
	// healthStatus != 0 is the HTTP status GET /healthz answers with.
	healthStatus atomic.Int32
	// healthDelay delays every /healthz answer (a slow, not stalled, daemon).
	healthDelay atomic.Int64
	// jobStatus answers GET /v1/jobs/{id} (nil: 404).
	jobStatus func(id string) proto.JobStatus
	// attachStatus > 0 makes every attach fail with that HTTP status.
	attachStatus int

	mu            sync.Mutex
	nextID        int
	idPrefix      string
	jobs          map[string]*fakeJob
	submits       []proto.SubmitRequest
	attaches      []proto.AttachRequest
	streamsClosed int
}

func newFake(t *testing.T, sock string) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{t: t, sock: sock, closed: make(chan struct{}), jobs: map[string]*fakeJob{}, idPrefix: "j"}
	return f
}

func (f *fakeDaemon) start() *fakeDaemon {
	f.t.Helper()
	os.Remove(f.sock)
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		f.t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if d := time.Duration(f.healthDelay.Load()); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
		}
		if st := int(f.healthStatus.Load()); st != 0 && st != 200 {
			w.WriteHeader(st)
			w.Write([]byte("loop stalled: last step 12000ms ago\n"))
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /v1/jobs", f.handleSubmit)
	mux.HandleFunc("POST /v1/jobs/{id}/started", f.handlePost)
	mux.HandleFunc("POST /v1/jobs/{id}/progress", f.handlePost)
	mux.HandleFunc("POST /v1/jobs/{id}/exit", f.handlePost)
	mux.HandleFunc("POST /v1/jobs/{id}/attach", f.handleAttach)
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if f.jobStatus == nil {
			http.Error(w, "unknown job", 404)
			return
		}
		json.NewEncoder(w).Encode(f.jobStatus(r.PathValue("id")))
	})
	f.srv = &http.Server{Handler: mux}
	go f.srv.Serve(ln)
	f.t.Cleanup(f.stop)
	return f
}

func (f *fakeDaemon) stop() {
	f.once.Do(func() {
		close(f.closed)
		if f.srv != nil {
			f.srv.Close()
		}
	})
}

func (f *fakeDaemon) job(id string) *fakeJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[id]
	if j == nil {
		j = &fakeJob{id: id, started: make(chan proto.StartedRequest, 1), exit: make(chan proto.ExitRequest, 1)}
		f.jobs[id] = j
	}
	return j
}

func (f *fakeDaemon) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var req proto.SubmitRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	f.mu.Lock()
	f.submits = append(f.submits, req)
	f.mu.Unlock()
	if f.submitStatus > 0 {
		http.Error(w, "unknown class", f.submitStatus)
		return
	}
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("%s%d", f.idPrefix, f.nextID)
	f.mu.Unlock()
	j := f.job(id)
	j.submit = req
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	s := &jobStream{f: f, job: j, w: w, ctx: r.Context()}
	if f.onSubmit != nil {
		f.onSubmit(s)
	} else {
		s.send(proto.Event{Verb: proto.VerbAccepted})
		s.send(proto.Event{Verb: proto.VerbAdmitted})
		s.hold()
	}
	f.mu.Lock()
	f.streamsClosed++
	f.mu.Unlock()
}

func (f *fakeDaemon) handleAttach(w http.ResponseWriter, r *http.Request) {
	var req proto.AttachRequest
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.attaches = append(f.attaches, req)
	f.mu.Unlock()
	if f.attachStatus > 0 {
		http.Error(w, "busy", f.attachStatus)
		return
	}
	if f.onAttach == nil {
		http.Error(w, "no such job", 404)
		return
	}
	j := f.job(r.PathValue("id"))
	w.WriteHeader(200)
	s := &jobStream{f: f, job: j, w: w, ctx: r.Context()}
	f.onAttach(s, req)
}

func (f *fakeDaemon) handlePost(w http.ResponseWriter, r *http.Request) {
	j := f.job(r.PathValue("id"))
	switch op := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]; op {
	case "started":
		var req proto.StartedRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.startedCalls.Add(1)
		if f.startedFail.Add(-1) >= 0 {
			http.Error(w, "event queue full", http.StatusServiceUnavailable)
			return
		}
		select {
		case j.started <- req:
		default:
		}
	case "progress":
		var req proto.ProgressRequest
		json.NewDecoder(r.Body).Decode(&req)
		if f.progressDelay > 0 {
			select {
			case <-time.After(f.progressDelay):
			case <-r.Context().Done():
				return
			}
		}
		j.mu.Lock()
		j.progress = append(j.progress, req)
		j.mu.Unlock()
	case "exit":
		var req proto.ExitRequest
		json.NewDecoder(r.Body).Decode(&req)
		j.mu.Lock()
		j.exits = append(j.exits, req)
		j.mu.Unlock()
		select {
		case j.exit <- req:
		default:
		}
	}
	w.WriteHeader(204)
}

func (f *fakeDaemon) submitList() []proto.SubmitRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.SubmitRequest(nil), f.submits...)
}

func (f *fakeDaemon) attachList() []proto.AttachRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]proto.AttachRequest(nil), f.attaches...)
}

func (j *fakeJob) exitReq(t *testing.T) proto.ExitRequest {
	t.Helper()
	select {
	case ex := <-j.exit:
		j.exit <- ex
		return ex
	case <-time.After(5 * time.Second):
		t.Fatalf("job %s: no exit POST", j.id)
		return proto.ExitRequest{}
	}
}

func (j *fakeJob) hasStarted() bool {
	select {
	case st := <-j.started:
		j.started <- st
		return true
	default:
		return false
	}
}

func (j *fakeJob) progressList() []proto.ProgressRequest {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]proto.ProgressRequest(nil), j.progress...)
}

// ---------------------------------------------------------------------------
// Harness.

// shortDir is a short temp dir under /tmp (unix socket paths <= 103 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pace-w")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// testEnv is the environment for children: never governed, no status file.
func testEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == EnvJobID || k == EnvStatusFile || k == "PACE_WRAPPER_HELPER" || k == "PACE_WRAPPER_SPEC" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

func fastTimings() timings {
	return timings{
		MuxHold:      100 * time.Millisecond,
		Progress:     100 * time.Millisecond,
		Reattach:     100 * time.Millisecond,
		FailOpenPoll: 50 * time.Millisecond,
		Drain:        500 * time.Millisecond,
		KillWait:     300 * time.Millisecond,
	}
}

type env struct {
	t    *testing.T
	dir  string
	sock string
	cfg  config.Resolved
}

func newEnv(t *testing.T) *env {
	d := shortDir(t)
	e := &env{t: t, dir: d, sock: filepath.Join(d, "pace.sock")}
	e.cfg = config.Resolved{Socket: e.sock, Home: d, FailOpenWaitMS: 300, FailOpenSlots: 2}
	return e
}

type result struct {
	code   int
	stdout string
	stderr string
	raised []syscall.Signal
	dur    time.Duration
}

// paceLine is one parsed governor line.
type paceLine struct {
	verb string
	kv   map[string]string
	raw  string
}

func (r result) pace() []paceLine { return parsePace(r.stderr) }

func parsePace(s string) []paceLine {
	var out []paceLine
	for _, l := range strings.Split(s, "\n") {
		if v, kv, ok := proto.ParseLine(l); ok {
			out = append(out, paceLine{v, kv, l})
		}
	}
	return out
}

func (r result) verbs() []string {
	var v []string
	for _, g := range r.pace() {
		v = append(v, g.verb)
	}
	return v
}

func (r result) hints() []string {
	var h []string
	for _, l := range strings.Split(r.stderr, "\n") {
		if strings.HasPrefix(l, "pace: hint: ") {
			h = append(h, strings.TrimPrefix(l, "pace: hint: "))
		}
	}
	return h
}

func (r result) find(verb string) (paceLine, bool) {
	for _, g := range r.pace() {
		if g.verb == verb {
			return g, true
		}
	}
	return paceLine{}, false
}

// run executes Run in-process with temp-file stdio. Signal re-raising is
// recorded instead of performed.
func (e *env) run(o Options) result {
	e.t.Helper()
	outF, err := os.Create(filepath.Join(e.dir, fmt.Sprintf("out-%d", time.Now().UnixNano())))
	if err != nil {
		e.t.Fatal(err)
	}
	defer outF.Close()
	errF, err := os.Create(outF.Name() + ".err")
	if err != nil {
		e.t.Fatal(err)
	}
	defer errF.Close()
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if o.Stdout == nil {
		o.Stdout = outF
	}
	if o.Stderr == nil {
		o.Stderr = errF
	}
	if o.Stdin == nil {
		o.Stdin = devnull
	}
	if o.Env == nil {
		o.Env = testEnv()
	}
	if o.Config.Socket == "" && o.Config.Home == "" {
		o.Config = e.cfg
	}
	if o.t == (timings{}) {
		o.t = fastTimings()
	}
	var mu sync.Mutex
	var raised []syscall.Signal
	o.h.raise = func(s syscall.Signal) { mu.Lock(); raised = append(raised, s); mu.Unlock() }
	if o.h.quickPressure == nil {
		o.h.quickPressure = func() int { return 1 }
	}
	if o.h.openTTY == nil {
		// Never take over the terminal `go test` may be running on.
		o.h.openTTY = noTTY
	}
	if o.h.containers == nil {
		// Never create cgroups on the machine running the tests.
		o.h.containers = func(string) (platform.JobContainer, error) { return nil, errors.New("no containers in tests") }
	}
	start := time.Now()
	code := Run(o)
	res := result{code: code, dur: time.Since(start), raised: raised}
	b, _ := os.ReadFile(outF.Name())
	res.stdout = string(b)
	b, _ = os.ReadFile(errF.Name())
	res.stderr = string(b)
	return res
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

// waitFile polls until path exists (or fails the test).
func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// waitContent polls until the file contains sub.
func waitContent(t *testing.T, path, sub string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), sub) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %q in %s (have %q)", sub, path, b)
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	waitFile(t, path, 5*time.Second)
	for i := 0; i < 200; i++ {
		b, _ := os.ReadFile(path)
		var pid int
		if _, err := fmt.Sscan(strings.TrimSpace(string(b)), &pid); err == nil && pid > 0 {
			return pid
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// alive reports whether pid exists (signal 0 delivers nothing).
func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitGone(t *testing.T, pid int, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive", pid)
}
