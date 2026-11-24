package integrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// shortDir returns a short temp dir for unix sockets (sun_path ≤ 103 bytes).
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func sampleStatus() proto.Status {
	return proto.Status{
		V: 1, TS: 1_700_000_000_000, Version: "test", Platform: "fake", Mode: "enforce",
		CapacityBytes: 36 << 30, AvailBytes: 20 << 30, ReserveBytes: 4 << 30, ReservedBytes: 3 << 30,
		HeadroomBytes: 13 << 30, Pressure: proto.LevelNormal, KernelLevel: 1, NCPU: 12,
		Admitting: true, SuggestedConcurrency: 4, SuggestedByClass: map[string]int{"build": 2, "test": 4},
	}
}

// fakeDaemon serves the API on a unix socket. status is read on every request.
type fakeDaemon struct {
	sock    string
	status  atomic.Value // proto.Status
	delay   atomic.Int64 // ns before answering /v1/status
	reloads atomic.Int32
	events  []proto.Event
	hold    bool // keep the event stream open after sending events
}

func startFakeDaemon(t *testing.T, fd *fakeDaemon) *fakeDaemon {
	t.Helper()
	if fd.sock == "" {
		fd.sock = filepath.Join(shortDir(t), "g.sock")
	}
	if fd.status.Load() == nil {
		fd.status.Store(sampleStatus())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		if d := fd.delay.Load(); d > 0 {
			select {
			case <-time.After(time.Duration(d)):
			case <-r.Context().Done():
				return
			}
		}
		json.NewEncoder(w).Encode(fd.status.Load().(proto.Status))
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "j1" {
			http.Error(w, "no such job", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(proto.JobStatus{ID: "j1", Class: "build", State: "done", ExitCode: proto.Int(0)})
	})
	mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, r *http.Request) {
		fd.reloads.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("follow") != "1" {
			http.Error(w, "follow required", 400)
			return
		}
		fl := w.(http.Flusher)
		for _, e := range fd.events {
			json.NewEncoder(w).Encode(e)
			fmt.Fprintln(w) // blank lines are skipped by the client
			fl.Flush()
		}
		if fd.hold {
			<-r.Context().Done()
		}
	})
	ln, err := net.Listen("unix", fd.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fd
}

func TestClientStatusJobHealthReload(t *testing.T) {
	fd := startFakeDaemon(t, &fakeDaemon{})
	c := &Client{Socket: fd.sock}
	ctx := context.Background()
	s, raw, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Mode != "enforce" || s.HeadroomBytes != 13<<30 || !strings.Contains(string(raw), `"headroom_bytes"`) {
		t.Fatalf("status %+v", s)
	}
	j, _, err := c.Job(ctx, "j1")
	if err != nil || j.ID != "j1" || j.ExitCode == nil {
		t.Fatalf("job %+v %v", j, err)
	}
	if _, _, err := c.Job(ctx, "nope/../x"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("missing job: %v", err)
	}
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Reload(ctx); err != nil || fd.reloads.Load() != 1 {
		t.Fatalf("reload %v %d", err, fd.reloads.Load())
	}
}

func TestClientUnreachable(t *testing.T) {
	dir := shortDir(t)
	cases := []string{
		filepath.Join(dir, "missing.sock"),
		"",
		"/" + strings.Repeat("x", 120),
	}
	for _, sock := range cases {
		c := &Client{Socket: sock}
		_, _, err := c.Status(context.Background())
		if !IsUnreachable(err) {
			t.Errorf("%q: want unreachable, got %v", sock, err)
		}
	}
	// A stale socket file with no listener.
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, _, err := (&Client{Socket: stale}).Status(context.Background()); !IsUnreachable(err) {
		t.Errorf("stale socket: %v", err)
	}
}

func TestClientTimeout(t *testing.T) {
	fd := startFakeDaemon(t, &fakeDaemon{})
	fd.delay.Store(int64(2 * time.Second))
	c := &Client{Socket: fd.sock, Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, _, err := c.Status(context.Background())
	if !IsUnreachable(err) {
		t.Fatalf("want unreachable on timeout, got %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout took %v", el)
	}
}

func TestClientFollow(t *testing.T) {
	evs := []proto.Event{
		{V: 1, TS: 1, Verb: proto.VerbWarn, Job: "j1", Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(5 << 30)},
		{V: 1, TS: 2, Verb: proto.VerbKilled, Job: "j1", Policy: proto.PolicyMemBudget, Hint: "lower parallelism"},
	}
	fd := startFakeDaemon(t, &fakeDaemon{events: evs})
	c := &Client{Socket: fd.sock}
	var got []proto.Event
	err := c.Follow(context.Background(), func(raw []byte, ev proto.Event) error {
		if !json.Valid(raw) {
			t.Errorf("raw not JSON: %q", raw)
		}
		got = append(got, ev)
		return nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF at end of stream, got %v", err)
	}
	if len(got) != 2 || got[1].Verb != proto.VerbKilled || *got[0].UsedBytes != 5<<30 {
		t.Fatalf("events %+v", got)
	}

	// fn error stops the stream.
	stop := errors.New("stop")
	if err := c.Follow(context.Background(), func([]byte, proto.Event) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("want stop, got %v", err)
	}

	// Context cancel ends a held stream.
	fd2 := startFakeDaemon(t, &fakeDaemon{events: evs[:1], hold: true})
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	done := make(chan error, 1)
	go func() {
		done <- (&Client{Socket: fd2.sock}).Follow(ctx, func([]byte, proto.Event) error { n++; cancel(); return nil })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || n != 1 {
			t.Fatalf("held stream: %v n=%d", err, n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Follow did not return after cancel")
	}

	// Unreachable.
	if err := (&Client{Socket: filepath.Join(shortDir(t), "x.sock")}).Follow(context.Background(), nil); !IsUnreachable(err) {
		t.Fatalf("follow unreachable: %v", err)
	}
}
