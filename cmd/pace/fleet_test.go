package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/fleet"
)

// fakeSSH installs a script used as ssh: destination "mini" prints a
// status, "down" exits 3 (pace daemon down), anything else exits 255.
func fakeSSH(t *testing.T, te *testEnv) string {
	t.Helper()
	st := richStatus()
	st.Mode = "observe"
	b, _ := json.Marshal(st)
	statusFile := filepath.Join(te.home, "peer-status.json")
	os.WriteFile(statusFile, b, 0o600)
	script := filepath.Join(te.home, "fake-ssh")
	os.WriteFile(script, []byte(`#!/bin/sh
while [ "$1" != "--" ]; do shift; done
shift
case "$1" in
mini) cat '`+statusFile+`' ;;
down) echo "pace status: daemon unreachable" >&2; exit 3 ;;
*) echo "ssh: Could not resolve hostname $1" >&2; exit 255 ;;
esac
`), 0o755)
	t.Setenv("PACE_FLEET_SSH", script)
	t.Setenv("PACE_FLEET", filepath.Join(te.home, "fleet.toml"))
	return script
}

func TestFleetCommands(t *testing.T) {
	te := newTestEnv(t)
	fakeSSH(t, te)
	startDaemon(t, te.sock, richStatus(), nil)

	if code := te.run("fleet", "add", "mini", "me@mini"); code != exitFail {
		t.Fatalf("unreachable add: %d %s", code, te.out.String())
	}
	if !strings.Contains(te.out.String(), "does not answer yet") || !strings.Contains(te.out.String(), "ssh -o BatchMode=yes me@mini") {
		t.Fatalf("add guidance: %s", te.out.String())
	}
	if code := te.run("fleet", "add", "mini", "mini"); code != 0 || !strings.Contains(te.out.String(), "updated mini") || !strings.Contains(te.out.String(), "mini answers: pace v0.1.0, mode observe") {
		t.Fatalf("add: %d %s %s", code, te.out.String(), te.err.String())
	}
	// A container peer: listed with its container; an update without the
	// container flags keeps them.
	if code := te.run("fleet", "add", "st", "mini", "--container", "build-worker", "--container-user", "worker", "--no-check"); code != 0 {
		t.Fatalf("add container: %d %s", code, te.err.String())
	}
	te.run("fleet", "add", "st", "elsewhere", "--container", "build-worker", "--container-user", "worker")
	if !strings.Contains(te.out.String(), "check: ssh -o BatchMode=yes elsewhere docker exec -u worker build-worker pace status") {
		t.Fatalf("container hint: %s", te.out.String())
	}
	te.run("fleet", "add", "st", "mini2", "--no-check")
	te.run("fleet", "ls")
	if !strings.Contains(te.out.String(), "st  ssh mini2  container build-worker (user worker)") {
		t.Fatalf("ls: %s", te.out.String())
	}
	if code := te.run("fleet", "add", "bad", "mini", "--container", "x;y", "--no-check"); code != exitUsage {
		t.Fatalf("bad container accepted: %d", code)
	}
	te.run("fleet", "rm", "st")
	// Updating only the destination keeps the pace path.
	te.run("fleet", "add", "keep", "h1", "--pace", "/opt/pace/bin/pace", "--no-check")
	te.run("fleet", "add", "keep", "h2", "--no-check")
	if f, _ := fleet.Load(os.Getenv("PACE_FLEET")); f.Peers[f.Find("keep")].Pace != "/opt/pace/bin/pace" || f.Peers[f.Find("keep")].SSH != "h2" {
		t.Fatalf("pace path lost on update: %+v", f.Peers)
	}
	te.run("fleet", "rm", "keep")
	if code := te.run("fleet", "add", "box", "down", "--no-check"); code != 0 {
		t.Fatalf("add --no-check: %d %s", code, te.err.String())
	}
	if code := te.run("fleet", "add", "gone", "nowhere", "--no-check"); code != 0 {
		t.Fatal(code)
	}
	if code := te.run("fleet", "add", "bad name", "x"); code != exitUsage {
		t.Fatalf("bad name: %d", code)
	}
	if code := te.run("fleet", "add", "x", "-oProxyCommand=evil"); code != exitUsage {
		t.Fatalf("option as destination: %d", code)
	}
	if code := te.run("fleet", "ls"); code != 0 || !strings.Contains(te.out.String(), "mini  ssh mini") || !strings.Contains(te.out.String(), "(this machine, socket)") {
		t.Fatalf("ls: %s", te.out.String())
	}

	if code := te.run("fleet", "--json"); code != 0 {
		t.Fatalf("fleet --json: %d %s", code, te.err.String())
	}
	var v struct {
		Machines []fleet.Machine `json:"machines"`
	}
	if err := json.Unmarshal(te.out.Bytes(), &v); err != nil {
		t.Fatalf("%v %s", err, te.out.String())
	}
	states := map[string]string{}
	for _, m := range v.Machines {
		states[m.Name] = m.State
	}
	if states["mini"] != "live" || states["box"] != "pace-down" || states["gone"] != "unreachable" || len(v.Machines) != 4 {
		t.Fatalf("states %v", states)
	}

	// The peer disappears: stale, with its snapshot, exit 0.
	if code := te.run("fleet", "add", "mini", "elsewhere", "--no-check"); code != 0 {
		t.Fatal(code)
	}
	if code := te.run("fleet", "-v"); code != 0 {
		t.Fatalf("fleet: %d", code)
	}
	o := te.out.String()
	for _, want := range []string{"MACHINE", "mini   ", "stale", "observe", "mini: ssh: Could not resolve hostname elsewhere; showing its", "== mini (stale via ssh elsewhere"} {
		if !strings.Contains(o, want) {
			t.Errorf("missing %q:\n%s", want, o)
		}
	}

	if code := te.run("fleet", "self", "mbp"); code != 0 || !strings.Contains(te.out.String(), "this machine is mbp") {
		t.Fatalf("self: %d %s", code, te.err.String())
	}
	if code := te.run("fleet", "self"); code != 0 || strings.TrimSpace(te.out.String()) != "mbp" {
		t.Fatalf("self show: %q", te.out.String())
	}
	if code := te.run("fleet", "self", "mini"); code != exitUsage {
		t.Fatalf("self = a peer: %d", code)
	}
	te.run("fleet", "--json")
	if !strings.Contains(te.out.String(), `"name":"mbp","self":true`) {
		t.Fatalf("self name not used: %s", te.out.String())
	}
	// rename: a peer (to a dotted name), this machine, and the refusals
	if code := te.run("fleet", "rename", "box", ".53"); code != 0 || !strings.Contains(te.out.String(), "renamed box to .53") {
		t.Fatalf("rename: %d %s %s", code, te.out.String(), te.err.String())
	}
	if f, _ := fleet.Load(os.Getenv("PACE_FLEET")); f.Find(".53") < 0 || f.Find("box") >= 0 {
		t.Fatalf("rename not saved: %+v", f.Peers)
	}
	for _, bad := range [][]string{{"rename", ".53", ".."}, {"rename", ".53", "."}, {"rename", ".53", "a/b"}, {"rename", ".53", "mini"}, {"rename", "nobody", "x"}, {"rename", ".53"}} {
		if code := te.run(append([]string{"fleet"}, bad...)...); code == 0 {
			t.Errorf("%v accepted", bad)
		}
	}
	if code := te.run("fleet", "rename", "mbp", "laptop"); code != 0 {
		t.Fatalf("rename self: %d %s", code, te.err.String())
	}
	te.run("fleet", "self")
	if strings.TrimSpace(te.out.String()) != "laptop" {
		t.Fatalf("self after rename: %q", te.out.String())
	}
	te.run("fleet", "rename", "laptop", "mbp")
	te.run("fleet", "rename", ".53", "box")
	if code := te.run("fleet", "rm", "gone"); code != 0 {
		t.Fatal(code)
	}
	if code := te.run("fleet", "rm", "gone"); code != exitFail {
		t.Fatalf("rm twice: %d", code)
	}
	if code := te.run("fleet", "frob"); code != exitUsage {
		t.Fatalf("unknown sub: %d", code)
	}
	if code := te.run("fleet", "-h"); code != 0 || !strings.Contains(te.out.String(), "stale") {
		t.Fatal("help")
	}
	// A broken fleet file is a usage error, not a crash.
	os.WriteFile(os.Getenv("PACE_FLEET"), []byte("nope = = 1"), 0o600)
	if code := te.run("fleet"); code != exitUsage {
		t.Fatalf("broken file: %d", code)
	}
}

func TestFleetNoPeers(t *testing.T) {
	te := newTestEnv(t)
	t.Setenv("PACE_FLEET", filepath.Join(te.home, "none.toml"))
	// Daemon down and no peers: still a table, exit 0.
	if code := te.run("fleet"); code != 0 {
		t.Fatalf("%d %s", code, te.err.String())
	}
	if !strings.Contains(te.out.String(), "pace-down") || !strings.Contains(te.out.String(), "No peers yet") {
		t.Fatalf("%s", te.out.String())
	}
}

func TestFleetServe(t *testing.T) {
	te := newTestEnv(t)
	fakeSSH(t, te)
	startDaemon(t, te.sock, richStatus(), nil)
	te.run("fleet", "add", "mini", "mini", "--no-check")
	for _, l := range []string{"0.0.0.0:0", "[::]:0", ":0", "0:0", "0x0:0"} {
		if code := te.run("fleet", "serve", "--listen", l); code != exitUsage {
			t.Errorf("all interfaces via %q: %d %s", l, code, te.err.String())
		}
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	done := make(chan int, 1)
	out := &syncBuf{}
	c := &cli{stdin: strings.NewReader(""), stdout: out, stderr: out, env: os.Getenv, exe: "/opt/pace/bin/pace"}
	go func() { done <- c.main([]string{"fleet", "serve", "--listen", addr, "--every", "1s"}) }()
	var body string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + addr + "/fleet.json")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			if strings.Contains(body, `"state":"live"`) && strings.Contains(body, `"name":"mini"`) && !strings.Contains(body, "not fetched yet") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(body, `"name":"mini"`) || strings.Contains(body, "not fetched yet") {
		t.Fatalf("fleet.json: %s", body)
	}
	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<title>pace fleet</title>") || !strings.Contains(string(b), "mini") {
		t.Fatalf("page: %s", b)
	}
	if resp, err := http.Get("http://" + addr + "/nope"); err != nil || resp.StatusCode != 404 {
		t.Fatalf("404 expected: %v", err)
	}
	// DNS rebinding: a request naming another host is refused; localhost and
	// IP literals are served.
	_, port, _ := net.SplitHostPort(addr)
	for host, want := range map[string]int{"evil.example:" + port: 421, "evil.example": 421, "localhost:" + port: 200, "127.0.0.1:" + port: 200,
		"localhost:1": 200, "LOCALHOST": 200, "[::1]:7720": 200, "mini:" + port: 200, "mini.tail1234.ts.net": 200, "evil.ts.net.example.com": 421} {
		req, _ := http.NewRequest("GET", "http://"+addr+"/fleet.json", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("Host %s: status %d, want %d", host, resp.StatusCode, want)
		}
	}
	p, _ := os.FindProcess(os.Getpid())
	p.Signal(os.Interrupt)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit %d: %s", code, out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop on interrupt")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
