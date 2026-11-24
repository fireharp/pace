package client

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
	"sync"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// shortDir returns a short temp dir under /tmp so unix socket paths stay
// below the 103-byte sun_path limit.
func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "pace-c")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type server struct {
	srv  *http.Server
	ln   net.Listener
	sock string
	mu   sync.Mutex
	reqs []string // "METHOD path body"
}

func serve(t *testing.T, sock string, h func(s *server) http.Handler) *server {
	t.Helper()
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{ln: ln, sock: sock}
	s.srv = &http.Server{Handler: h(s)}
	go s.srv.Serve(ln)
	t.Cleanup(func() { s.srv.Close() })
	return s
}

func (s *server) record(r *http.Request) string {
	b, _ := io.ReadAll(r.Body)
	line := r.Method + " " + r.URL.RequestURI() + " " + string(b)
	s.mu.Lock()
	s.reqs = append(s.reqs, line)
	s.mu.Unlock()
	return string(b)
}

func (s *server) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reqs...)
}

func writeEvents(w http.ResponseWriter, evs ...proto.Event) {
	f := w.(http.Flusher)
	for _, e := range evs {
		b, _ := json.Marshal(e)
		w.Write(append(b, '\n'))
		f.Flush()
	}
}

func TestDialInvalidSocket(t *testing.T) {
	for _, tc := range []struct{ name, sock string }{
		{"empty", ""},
		{"too long", "/tmp/" + strings.Repeat("x", 120) + ".sock"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			_, err := Dial(context.Background(), tc.sock, time.Second)
			if !errors.Is(err, ErrSocketPath) {
				t.Fatalf("err = %v, want ErrSocketPath", err)
			}
			if time.Since(start) > 100*time.Millisecond {
				t.Fatalf("invalid path should fail at once, took %v", time.Since(start))
			}
		})
	}
}

func TestDialUnreachableTimesOut(t *testing.T) {
	sock := filepath.Join(shortDir(t), "none.sock")
	start := time.Now()
	_, err := Dial(context.Background(), sock, 150*time.Millisecond)
	el := time.Since(start)
	if err == nil {
		t.Fatal("expected error")
	}
	if !IsUnreachable(err) {
		t.Fatalf("IsUnreachable(%v) = false", err)
	}
	if el < 140*time.Millisecond || el > 600*time.Millisecond {
		t.Fatalf("Dial waited %v, want ~150ms", el)
	}
}

func TestDialContextCancel(t *testing.T) {
	sock := filepath.Join(shortDir(t), "none.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := Dial(ctx, sock, 5*time.Second)
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("Dial err=%v after %v; want quick ctx error", err, time.Since(start))
	}
}

func TestDialRetriesUntilServerAppears(t *testing.T) {
	sock := filepath.Join(shortDir(t), "late.sock")
	done := make(chan *server, 1)
	go func() {
		time.Sleep(120 * time.Millisecond)
		done <- serve(t, sock, func(s *server) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		})
	}()
	c, err := Dial(context.Background(), sock, 2*time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	<-done
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestDialAcceptsAnyHTTPAnswer(t *testing.T) {
	sock := filepath.Join(shortDir(t), "404.sock")
	serve(t, sock, func(s *server) http.Handler { return http.NotFoundHandler() })
	c, err := Dial(context.Background(), sock, time.Second)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	err = c.Ping(context.Background())
	if !IsStatus(err, 404) || IsUnreachable(err) {
		t.Fatalf("Ping err = %v, want APIError 404 (reachable)", err)
	}
}

func TestRequests(t *testing.T) {
	sock := filepath.Join(shortDir(t), "api.sock")
	srv := serve(t, sock, func(s *server) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/jobs/{id}/{op}", func(w http.ResponseWriter, r *http.Request) {
			s.record(r)
			if r.PathValue("id") == "bad" {
				http.Error(w, "no such job", 404)
				return
			}
			w.WriteHeader(204)
		})
		mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
			s.record(r)
			json.NewEncoder(w).Encode(proto.Status{V: 1, Mode: "enforce", HeadroomBytes: 5 << 30, NCPU: 12})
		})
		mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
			s.record(r)
			json.NewEncoder(w).Encode(proto.JobStatus{ID: r.PathValue("id"), State: "done", ExitCode: proto.Int(3)})
		})
		mux.HandleFunc("POST /v1/reload", func(w http.ResponseWriter, r *http.Request) { s.record(r); w.WriteHeader(204) })
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("pace_up 1\n")) })
		mux.HandleFunc("GET /v1/garbage", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{not json")) })
		return mux
	})
	c := New(sock)
	ctx := context.Background()
	if err := c.Started(ctx, "j1", proto.StartedRequest{PID: 10, PGID: 10, StartMS: 99}); err != nil {
		t.Fatal(err)
	}
	if err := c.Progress(ctx, "j1", proto.ProgressRequest{OutputBytes: 5, LastOutputMS: 7}); err != nil {
		t.Fatal(err)
	}
	if err := c.Exit(ctx, "j1", proto.ExitRequest{Code: 0, Signaled: true, Signal: 9}); err != nil {
		t.Fatal(err)
	}
	err := c.Exit(ctx, "bad", proto.ExitRequest{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 404 || !strings.Contains(ae.Body, "no such job") {
		t.Fatalf("Exit(bad) = %v, want 404 APIError", err)
	}
	if !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("error text %q", err.Error())
	}
	st, err := c.Status(ctx)
	if err != nil || st.HeadroomBytes != 5<<30 || st.NCPU != 12 {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	j, err := c.Job(ctx, "j/odd id")
	if err != nil || j.ID != "j/odd id" || j.ExitCode == nil || *j.ExitCode != 3 {
		t.Fatalf("Job = %+v, %v", j, err)
	}
	if err := c.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	m, err := c.Metrics(ctx)
	if err != nil || m != "pace_up 1\n" {
		t.Fatalf("Metrics = %q, %v", m, err)
	}
	var out map[string]any
	err = c.do(ctx, "GET", "/v1/garbage", nil, &out)
	var de *DecodeError
	if !errors.As(err, &de) || IsUnreachable(err) {
		t.Fatalf("garbage decode err = %v", err)
	}

	want := []string{
		`POST /v1/jobs/j1/started {"pid":10,"pgid":10,"start_ms":99}`,
		`POST /v1/jobs/j1/progress {"output_bytes":5,"last_output_ms":7}`,
		`POST /v1/jobs/j1/exit {"code":0,"signaled":true,"signal":9}`,
		`POST /v1/jobs/bad/exit {"code":0,"signaled":false}`,
		`GET /v1/status `,
		`GET /v1/jobs/j%2Fodd%20id `,
		`POST /v1/reload `,
	}
	got := srv.requests()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestRequestTimeout(t *testing.T) {
	sock := filepath.Join(shortDir(t), "slow.sock")
	release := make(chan struct{})
	serve(t, sock, func(s *server) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
	})
	defer close(release)
	c := New(sock)
	c.Timeout = 80 * time.Millisecond
	start := time.Now()
	err := c.Progress(context.Background(), "j", proto.ProgressRequest{})
	if err == nil || !IsUnreachable(err) {
		t.Fatalf("err = %v, want timeout (unreachable)", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout took %v", el)
	}
}

func TestSubmitStream(t *testing.T) {
	sock := filepath.Join(shortDir(t), "sub.sock")
	gotReq := make(chan proto.SubmitRequest, 1)
	clientGone := make(chan struct{})
	serve(t, sock, func(s *server) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
			var req proto.SubmitRequest
			json.NewDecoder(r.Body).Decode(&req)
			gotReq <- req
			writeEvents(w,
				proto.Event{V: 1, Verb: proto.VerbAccepted, Job: "j7"},
				proto.Event{V: 1, Verb: proto.VerbQueued, Job: "j7", Reason: proto.ReasonHeadroom, Position: proto.Int(2)},
			)
			w.Write([]byte("\n   \n{broken\n"))
			writeEvents(w, proto.Event{V: 1, Verb: proto.VerbAdmitted, Job: "j7", WaitedMS: proto.I64(1500)})
			<-r.Context().Done()
			close(clientGone)
		})
		return mux
	})
	c := New(sock)
	st, err := c.Submit(context.Background(), proto.SubmitRequest{Class: "build", Argv: []string{"make", "-j8"}, WrapperPID: 42, Priority: proto.Int(0)})
	if err != nil {
		t.Fatal(err)
	}
	req := <-gotReq
	if req.Class != "build" || req.WrapperPID != 42 || len(req.Argv) != 2 || req.Priority == nil || *req.Priority != 0 {
		t.Fatalf("server got %+v", req)
	}
	ev, err := st.Next()
	if err != nil || ev.Verb != proto.VerbAccepted || ev.Job != "j7" {
		t.Fatalf("1: %+v %v", ev, err)
	}
	ev, err = st.Next()
	if err != nil || ev.Verb != proto.VerbQueued || ev.Position == nil || *ev.Position != 2 {
		t.Fatalf("2: %+v %v", ev, err)
	}
	_, err = st.Next()
	var de *DecodeError
	if !errors.As(err, &de) || !strings.Contains(de.Line, "broken") {
		t.Fatalf("3: want DecodeError, got %v", err)
	}
	ev, err = st.Next()
	if err != nil || ev.Verb != proto.VerbAdmitted || *ev.WaitedMS != 1500 {
		t.Fatalf("4: %+v %v", ev, err)
	}
	// Closing the stream must end the server-side request (wrapper liveness).
	st.Close()
	select {
	case <-clientGone:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe stream close")
	}
}

func TestStreamEOFAndServerDeath(t *testing.T) {
	sock := filepath.Join(shortDir(t), "eof.sock")
	kill := make(chan struct{})
	srv := serve(t, sock, func(s *server) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
			writeEvents(w, proto.Event{V: 1, Verb: proto.VerbAccepted, Job: "a"}, proto.Event{V: 1, Verb: proto.VerbRefused, Job: "a"})
		})
		mux.HandleFunc("POST /v1/jobs/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
			writeEvents(w, proto.Event{V: 1, Verb: proto.VerbAccepted, Job: r.PathValue("id")})
			<-kill
		})
		return mux
	})
	c := New(sock)
	st, err := c.Submit(context.Background(), proto.SubmitRequest{Argv: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for {
		ev, err := st.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		verbs = append(verbs, ev.Verb)
	}
	st.Close()
	if strings.Join(verbs, ",") != "ACCEPTED,REFUSED" {
		t.Fatalf("verbs %v", verbs)
	}

	at, err := c.Attach(context.Background(), "j9", proto.AttachRequest{PID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := at.Next(); err != nil || ev.Job != "j9" {
		t.Fatalf("attach first: %+v %v", ev, err)
	}
	errc := make(chan error, 1)
	go func() { _, err := at.Next(); errc <- err }()
	srv.srv.Close() // daemon dies mid-stream
	close(kill)
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected error after server death")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return after server death")
	}
	at.Close()
}

func TestStreamHeaderTimeout(t *testing.T) {
	sock := filepath.Join(shortDir(t), "hdr.sock")
	release := make(chan struct{})
	serve(t, sock, func(s *server) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		})
	})
	defer close(release)
	c := New(sock)
	c.HeaderTimeout = 80 * time.Millisecond
	start := time.Now()
	_, err := c.Submit(context.Background(), proto.SubmitRequest{})
	if err == nil || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestStreamAPIError(t *testing.T) {
	sock := filepath.Join(shortDir(t), "400.sock")
	serve(t, sock, func(s *server) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unknown class \"x\"", 400)
		})
	})
	_, err := New(sock).Submit(context.Background(), proto.SubmitRequest{Class: "x"})
	if !IsStatus(err, 400) || IsUnreachable(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestFollow(t *testing.T) {
	sock := filepath.Join(shortDir(t), "fol.sock")
	serve(t, sock, func(s *server) http.Handler {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("follow") != "1" {
				http.Error(w, "need follow", 400)
				return
			}
			for i := 0; i < 3; i++ {
				writeEvents(w, proto.Event{V: 1, Verb: proto.VerbWarn, Unit: fmt.Sprintf("pg:%d", i)})
			}
		})
		return mux
	})
	st, err := New(sock).Follow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for i := 0; i < 3; i++ {
		ev, err := st.Next()
		if err != nil || ev.Unit != fmt.Sprintf("pg:%d", i) {
			t.Fatalf("%d: %+v %v", i, ev, err)
		}
	}
	if _, err := st.Next(); err != io.EOF {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestStreamParsing(t *testing.T) {
	long := `{"v":1,"verb":"INFO","msg":"` + strings.Repeat("a", MaxLine) + `"}`
	tests := []struct {
		name  string
		in    string
		verbs []string // "!" = DecodeError
		final error
	}{
		{"empty", "", nil, io.EOF},
		{"one", `{"v":1,"verb":"OK"}` + "\n", []string{"OK"}, io.EOF},
		{"crlf", `{"v":1,"verb":"OK"}` + "\r\n", []string{"OK"}, io.EOF},
		{"unterminated last", `{"v":1,"verb":"OK"}` + "\n" + `{"v":1,"verb":"WARN"}`, []string{"OK", "WARN"}, io.EOF},
		{"blank lines", "\n\n" + `{"verb":"KILLED"}` + "\n\n", []string{"KILLED"}, io.EOF},
		{"garbage then ok", "nope\n" + `{"verb":"OK"}` + "\n", []string{"!", "OK"}, io.EOF},
		{"too long then ok", long + "\n" + `{"verb":"OK"}` + "\n", []string{"!", "OK"}, io.EOF},
		{"big but legal", `{"verb":"INFO","msg":"` + strings.Repeat("b", 100000) + `"}` + "\n", []string{"INFO"}, io.EOF},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStream(io.NopCloser(strings.NewReader(tc.in)))
			var got []string
			for {
				ev, err := s.Next()
				var de *DecodeError
				if errors.As(err, &de) {
					got = append(got, "!")
					continue
				}
				if err != nil {
					if err != tc.final {
						t.Fatalf("final err %v want %v", err, tc.final)
					}
					break
				}
				got = append(got, ev.Verb)
			}
			if strings.Join(got, ",") != strings.Join(tc.verbs, ",") {
				t.Fatalf("got %v want %v", got, tc.verbs)
			}
		})
	}
}

func TestIsUnreachable(t *testing.T) {
	if IsUnreachable(nil) {
		t.Fatal("nil")
	}
	if IsUnreachable(&APIError{Status: 500}) {
		t.Fatal("api error is reachable")
	}
	if IsUnreachable(fmt.Errorf("wrap: %w", &DecodeError{})) {
		t.Fatal("decode error is reachable")
	}
	if !IsUnreachable(errors.New("dial unix: connect: no such file")) {
		t.Fatal("transport error")
	}
}
