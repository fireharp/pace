package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
)

var update = flag.Bool("update", false, "rewrite golden files")

// testEnv isolates a test from the user's real pace setup: HOME, PACE_HOME,
// PACE_SOCK and PACE_CONFIG point into short temp dirs (sockets ≤ 103 bytes).
type testEnv struct {
	home, paceHome, sock string
	out, err             *bytes.Buffer
	c                    *cli
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	short, err := os.MkdirTemp("/tmp", "pace-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	te := &testEnv{home: filepath.Join(short, "home"), paceHome: filepath.Join(short, "gh"), sock: filepath.Join(short, "g.sock")}
	os.MkdirAll(te.home, 0o755)
	cfg := filepath.Join(short, "config.toml")
	os.WriteFile(cfg, []byte("# empty\n"), 0o644)
	t.Setenv("HOME", te.home)
	t.Setenv("PACE_HOME", te.paceHome)
	t.Setenv("PACE_SOCK", te.sock)
	t.Setenv("PACE_CONFIG", cfg)
	t.Setenv("PACE_SHIM_DIR", "")
	t.Setenv("PACE_JOB_ID", "")
	t.Setenv("PACE_SHIM_DISABLE", "")
	t.Setenv("PACE_STATUS_FILE", "")
	t.Setenv("CLAUDE_ENV_FILE", "")
	te.out, te.err = &bytes.Buffer{}, &bytes.Buffer{}
	te.c = &cli{stdin: strings.NewReader(""), stdout: te.out, stderr: te.err, env: os.Getenv, exe: "/opt/pace/bin/pace"}
	// Never run launchctl/systemctl from tests.
	old := newRunner
	newRunner = func() integrate.Runner { return &recRunner{} }
	t.Cleanup(func() { newRunner = old })
	return te
}

func (te *testEnv) run(args ...string) int {
	te.out.Reset()
	te.err.Reset()
	return te.c.main(args)
}

// recRunner records commands; read-only queries fail (nothing is loaded),
// everything else succeeds.
type recRunner struct{ calls []string }

func (r *recRunner) Run(name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	if strings.HasPrefix(cmd, "launchctl print") || strings.Contains(cmd, "is-active") || strings.Contains(cmd, " show ") {
		return []byte("not found"), fmt.Errorf("exit status 113")
	}
	return nil, nil
}

// fakeDaemon serves a minimal API on te.sock.
type fakeDaemon struct {
	status atomic.Value
	events []proto.Event
}

func startDaemon(t *testing.T, sock string, s proto.Status, events []proto.Event) *fakeDaemon {
	t.Helper()
	fd := &fakeDaemon{events: events}
	fd.status.Store(s)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(fd.status.Load().(proto.Status))
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "j7" {
			http.Error(w, "not found", 404)
			return
		}
		json.NewEncoder(w).Encode(sampleJob())
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		for _, e := range fd.events {
			json.NewEncoder(w).Encode(e)
		}
		io.WriteString(w, "{\"weird\":true}\n")
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fd
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("golden %s: %v (run go test -update)", p, err)
	}
	if string(want) != got {
		t.Fatalf("%s mismatch:\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

func TestDispatch(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run(); code != exitUsage || !strings.Contains(te.err.String(), "Commands:") {
		t.Fatalf("no args: %d %q", code, te.err.String())
	}
	if code := te.run("bogus"); code != exitUsage || !strings.Contains(te.err.String(), `unknown command "bogus"`) {
		t.Fatalf("unknown: %d %q", code, te.err.String())
	}
	for _, h := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		if code := te.run(h...); code != 0 || !strings.Contains(te.out.String(), "Commands:") {
			t.Fatalf("%v: %d", h, code)
		}
		if strings.Contains(te.out.String(), "shim-exec") {
			t.Fatal("hidden command listed")
		}
	}
	if code := te.run("help", "nope"); code != exitUsage {
		t.Fatalf("help nope: %d", code)
	}
	if code := te.run("version"); code != 0 || !strings.HasPrefix(te.out.String(), "pace ") {
		t.Fatalf("version: %d %q", code, te.out.String())
	}
	if code := te.run("--version"); code != 0 || !strings.HasPrefix(te.out.String(), "pace ") {
		t.Fatalf("--version: %d", code)
	}
	if code := te.run("version", "x"); code != exitUsage {
		t.Fatalf("version x: %d", code)
	}
}

// Every visible command answers -h on stdout with exit 0 and a Usage line,
// and 'pace help CMD' does the same.
func TestEveryCommandHasHelp(t *testing.T) {
	te := newTestEnv(t)
	for _, cmd := range commands {
		for _, args := range [][]string{{cmd.name, "-h"}, {"help", cmd.name}} {
			code := te.run(args...)
			out := te.out.String() + te.err.String()
			if code != 0 {
				t.Errorf("%v: exit %d: %s", args, code, out)
			}
			if !strings.Contains(out, "Usage") {
				t.Errorf("%v: no usage text: %s", args, out)
			}
		}
	}
}

func TestBadFlagsAreUsageErrors(t *testing.T) {
	te := newTestEnv(t)
	for _, args := range [][]string{
		{"status", "--bogus"}, {"status", "extra"}, {"watch", "--nope"}, {"daemon", "--x"}, {"daemon", "extra"},
		{"install", "--mode", "turbo"}, {"install", "extra"}, {"uninstall", "x"}, {"shims"}, {"shims", "frob"},
		{"shims", "list", "--bogus"}, {"shims", "list", "extra"}, {"integrate"}, {"integrate", "cursor"}, {"config"}, {"config", "frob"},
		{"config", "print", "check"}, {"replay"}, {"doctor", "x"}, {"hook"},
	} {
		if code := te.run(args...); code != exitUsage {
			t.Errorf("%v: exit %d, want %d (stderr %q)", args, code, exitUsage, te.err.String())
		}
	}
}
