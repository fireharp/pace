package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

func status(mode string, avail int64) proto.Status {
	return proto.Status{V: 1, Version: "v0.1.0", Platform: "darwin", Mode: mode, Pressure: "normal",
		CapacityBytes: 16 << 30, AvailBytes: avail, HeadroomBytes: avail / 2, NCPU: 8, Load1Milli: 2500,
		Admitting: true, Running: []proto.JobStatus{{ID: "j1", Class: "test", Argv: []string{"/usr/bin/go", "test"}, UsedBytes: 1 << 30}},
		Unmanaged: []proto.UnitStatus{{Comm: "Google Chrome", NProcs: 40, UsedBytes: 3 << 30}}}
}

// fakeRunner answers per ssh destination (the argument after "--").
type fakeRunner struct {
	answers map[string]func(ctx context.Context) ([]byte, []byte, int, error)
	calls   atomic.Int32
}

func (f *fakeRunner) run(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	f.calls.Add(1)
	dest := ""
	for i, a := range args {
		if a == "--" && i+1 < len(args) {
			dest = args[i+1]
		}
	}
	if fn, ok := f.answers[dest]; ok {
		return fn(ctx)
	}
	return nil, []byte("ssh: Could not resolve hostname " + dest), 255, nil
}

func jsonAnswer(s proto.Status) func(context.Context) ([]byte, []byte, int, error) {
	return func(context.Context) ([]byte, []byte, int, error) {
		b, _ := json.Marshal(s)
		return append(b, '\n'), nil, 0, nil
	}
}

func TestFetchStates(t *testing.T) {
	cache := t.TempDir()
	fr := &fakeRunner{answers: map[string]func(context.Context) ([]byte, []byte, int, error){
		"mini": jsonAnswer(status("observe", 9<<30)),
		"down": func(context.Context) ([]byte, []byte, int, error) {
			return nil, []byte("pace status: daemon unreachable: dial unix ...: connect: no such file or directory\n"), 3, nil
		},
		"nopace": func(context.Context) ([]byte, []byte, int, error) {
			return nil, []byte("zsh:1: command not found: pace\n"), 127, nil
		},
		"slow": func(ctx context.Context) ([]byte, []byte, int, error) {
			<-ctx.Done()
			return nil, nil, -1, ctx.Err()
		},
		"junk": func(context.Context) ([]byte, []byte, int, error) { return []byte("hello"), nil, 0, nil },
	}}
	f := &Fetcher{
		File: File{Self: "mbp", Timeout: "300ms", Peers: []Peer{
			{Name: "mini", SSH: "mini"}, {Name: "down", SSH: "down"}, {Name: "nopace", SSH: "nopace"},
			{Name: "slow", SSH: "slow"}, {Name: "gone", SSH: "gone"}, {Name: "junk", SSH: "junk"},
			{Name: "mbp", SSH: "mbp-over-ssh"}, // this machine: read via the socket, not ssh
		}},
		CacheDir: cache,
		Local:    func(context.Context) (proto.Status, error) { return status("enforce", 20<<30), nil },
		Run:      fr.run,
	}
	t0 := time.Now()
	ms := f.Fetch(context.Background())
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("fetch took %v; peers must be fetched in parallel within the timeout", el)
	}
	want := map[string]string{"mbp": StateLive, "mini": StateLive, "down": StatePaceDown, "nopace": StateNoPace,
		"slow": StateUnreachable, "gone": StateUnreachable, "junk": StateError}
	if len(ms) != len(want) {
		t.Fatalf("got %d machines: %+v", len(ms), ms)
	}
	for _, m := range ms {
		if m.State != want[m.Name] {
			t.Errorf("%s: state %s (%s), want %s", m.Name, m.State, m.Error, want[m.Name])
		}
		if m.Name == "mbp" && (!m.Self || m.Via != "socket") {
			t.Errorf("self row: %+v", m)
		}
	}
	if int(fr.calls.Load()) != 6 {
		t.Errorf("ssh calls %d, want 6 (not to this machine)", fr.calls.Load())
	}
	if m := find(ms, "gone"); !strings.Contains(m.Error, "Could not resolve") {
		t.Errorf("ssh error not kept: %q", m.Error)
	}
	if m := find(ms, "slow"); !strings.Contains(m.Error, "no answer within 300ms") {
		t.Errorf("timeout error: %q", m.Error)
	}

	// Next round: mini is gone. It is stale, with its last snapshot.
	delete(fr.answers, "mini")
	ms = f.Fetch(context.Background())
	m := find(ms, "mini")
	if m.State != StateStale || m.Status == nil || m.Status.Mode != "observe" || m.SeenMS == 0 {
		t.Fatalf("mini after loss: %+v", m)
	}
	// The daemon on "down" was seen never: no snapshot. After one good
	// answer and a daemon failure, the state is pace-down with a snapshot.
	fr.answers["down"] = jsonAnswer(status("warn-only", 1<<30))
	f.Fetch(context.Background())
	fr.answers["down"] = func(context.Context) ([]byte, []byte, int, error) { return nil, []byte("x"), 3, nil }
	ms = f.Fetch(context.Background())
	if m := find(ms, "down"); m.State != StatePaceDown || m.Status == nil || m.Status.Mode != "warn-only" {
		t.Fatalf("down: %+v", m)
	}

	// Cached() needs no network.
	before := fr.calls.Load()
	cached := f.Cached()
	if fr.calls.Load() != before {
		t.Fatal("Cached() ran ssh")
	}
	if m := find(cached, "mini"); m.State != StateStale || m.Status == nil {
		t.Fatalf("cached mini: %+v", m)
	}
	if m := find(cached, "gone"); m.State != StateUnreachable {
		t.Fatalf("cached gone: %+v", m)
	}

	// The local daemon down: pace-down with its last snapshot.
	f.Local = func(context.Context) (proto.Status, error) { return proto.Status{}, errors.New("connection refused") }
	ms = f.Fetch(context.Background())
	if m := find(ms, "mbp"); m.State != StatePaceDown || m.Status == nil || !strings.Contains(m.Error, "connection refused") {
		t.Fatalf("local down: %+v", m)
	}
}

func find(ms []Machine, name string) Machine {
	for _, m := range ms {
		if m.Name == name {
			return m
		}
	}
	return Machine{}
}

func TestSSHArgs(t *testing.T) {
	args := SSHArgs(Peer{Name: "m", SSH: "me@mini.tail.ts.net", SSHOptions: []string{"-p", "2222"}}, 8*time.Second)
	s := strings.Join(args, " ")
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=4", "-p 2222 -- me@mini.tail.ts.net", "exec pace status --json", "$HOME/.local/bin"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in %s", want, s)
		}
	}
	if args[len(args)-2] != "me@mini.tail.ts.net" {
		t.Errorf("destination must come right before the remote command: %v", args)
	}
	if got := SSHArgs(Peer{SSH: "h"}, time.Second); !strings.Contains(strings.Join(got, " "), "ConnectTimeout=1") {
		t.Errorf("min connect timeout: %v", got)
	}
}

func TestLoadSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pace", "fleet.toml")
	f, err := Load(path)
	if err != nil || len(f.Peers) != 0 {
		t.Fatalf("missing file: %+v %v", f, err)
	}
	f = File{Self: "mbp", Peers: []Peer{{Name: "mini", SSH: "me@192.0.2.1", Pace: "~/.local/bin/pace"}, {Name: "box", SSH: "box", SSHOptions: []string{"-p", "22"}}}}
	if err := Save(path, f); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	got, err := Load(path)
	if err != nil || got.Self != "mbp" || len(got.Peers) != 2 || got.Peers[0].Pace != "~/.local/bin/pace" || got.Peers[1].SSHOptions[0] != "-p" {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), "# pace fleet") {
		t.Errorf("header missing:\n%s", b)
	}
	for _, bad := range []string{
		"[[peer]]\nname = \"a b\"\nssh = \"x\"\n",
		"[[peer]]\nname = \"a\"\nssh = \"-oProxyCommand=evil\"\n",
		"[[peer]]\nname = \"a\"\nssh = \"x\"\n[[peer]]\nname = \"a\"\nssh = \"y\"\n",
		"timeout = \"soon\"\n",
		"colour = \"blue\"\n",
		"[[peer]]\nname = \"../x\"\nssh = \"x\"\n",
		"[[peer]]\nname = \"a\"\nssh = \"x\"\npace = \"/a!b/pace\"\n",
	} {
		os.WriteFile(path, []byte(bad), 0o600)
		if _, err := Load(path); err == nil {
			t.Errorf("want error for:\n%s", bad)
		}
	}
}

func TestPaths(t *testing.T) {
	env := func(m map[string]string) Env { return func(k string) string { return m[k] } }
	if got := DefaultPath(env(map[string]string{"HOME": "/h"})); got != "/h/.config/pace/fleet.toml" {
		t.Error(got)
	}
	if got := DefaultPath(env(map[string]string{"HOME": "/h", "PACE_FLEET": "/x.toml"})); got != "/x.toml" {
		t.Error(got)
	}
	if got := DefaultCacheDir(env(map[string]string{"HOME": "/h", "PACE_HOME": "/g"})); got != "/g/cache/fleet" {
		t.Error(got)
	}
	if n := SelfName(File{}); !ValidName(n) {
		t.Errorf("self name %q invalid", n)
	}
}

func TestRender(t *testing.T) {
	now := time.Now()
	s := status("observe", 9<<30)
	ms := []Machine{
		{Name: "mbp", Self: true, Via: "socket", State: StateLive, SeenMS: now.UnixMilli(), Status: &s},
		{Name: "mini", Via: "ssh", Target: "mini", State: StateStale, SeenMS: now.Add(-5 * time.Minute).UnixMilli(), Status: &s, Error: "ssh: connect timed out"},
		{Name: "box", Via: "ssh", State: StateUnreachable, Error: "ssh: Could not resolve hostname box"},
	}
	var b bytes.Buffer
	RenderText(&b, ms, now, true)
	out := b.String()
	for _, want := range []string{"mbp*", "observe", "9.0G/16.0G", "2.5/8", "1/0", "Google Chrome 3.0G",
		"mini: ssh: connect timed out; showing its snapshot from 5m ago", "box: ssh: Could not resolve hostname box; no snapshot yet",
		"== mini (stale via ssh mini)", "running j1"} {
		if !strings.Contains(out, want) {
			t.Errorf("text missing %q:\n%s", want, out)
		}
	}
	b.Reset()
	if err := RenderHTML(&b, ms, now, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	h := b.String()
	for _, want := range []string{`content="10"`, `class="state stale"`, "live=1 stale=1 unreachable=1", "ssh: Could not resolve hostname box"} {
		if !strings.Contains(h, want) {
			t.Errorf("html missing %q", want)
		}
	}
	// Control characters from a peer cannot break the table or reach the terminal.
	evilT := status("observe", 1)
	evilT.Running[0].Argv = []string{"sh", "a\tb\nEVIL\t\x1b[31m"}
	evilT.Mode = "obs\x1b]0;pwned\x07"
	b.Reset()
	RenderText(&b, []Machine{{Name: "x", State: StateStale, Status: &evilT, Error: "ssh: bad\x1b[2J\nline"}}, now, true)
	if strings.ContainsAny(b.String(), "\x1b\x07\t") || strings.Count(b.String(), "\n") > 20 || strings.Contains(b.String(), "\nEVIL") {
		t.Errorf("control characters passed through:\n%q", b.String())
	}
	// Machine-controlled strings are escaped.
	evil := status("<script>", 1)
	b.Reset()
	RenderHTML(&b, []Machine{{Name: "x", State: StateLive, Status: &evil}}, now, time.Second)
	if strings.Contains(b.String(), "<script>") {
		t.Error("HTML not escaped")
	}
	if Ago(now, 0) != "never" || Ago(now, now.Add(-90*time.Second).UnixMilli()) != "1m" || Ago(now, now.Add(-72*time.Hour).UnixMilli()) != "3d" {
		t.Error("Ago")
	}
}

// ExecRunner returns even when the child leaves a pipe open (like an ssh
// ControlPersist master holding stderr).
func TestExecRunnerPipeHeldOpen(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t0 := time.Now()
	out, _, code, err := ExecRunner(ctx, "/bin/sh", "-c", "echo hi; (sleep 3 >&2 &) ; exit 0")
	if err != nil || code != 0 || strings.TrimSpace(string(out)) != "hi" {
		t.Fatalf("%q %d %v", out, code, err)
	}
	if el := time.Since(t0); el > 2*time.Second {
		t.Fatalf("Wait blocked on the held pipe for %v", el)
	}
	_, _, code, _ = ExecRunner(context.Background(), "/bin/sh", "-c", "exit 7")
	if code != 7 {
		t.Fatalf("exit code %d", code)
	}
}

// The remote command must work whatever the peer's login shell is: sshd
// runs it with "<shell> -c <command>". Every shell installed here is tried,
// with a fake pace in ~/.local/bin (not on PATH) and at a path with a space.
func TestRemoteCommandAcrossShells(t *testing.T) {
	home := t.TempDir()
	fake := "#!/bin/sh\n[ \"$1 $2\" = \"status --json\" ] || exit 9\necho '{\"v\":1,\"mode\":\"observe\"}'\n"
	for _, p := range []string{filepath.Join(home, ".local", "bin", "pace"), filepath.Join(home, "my bin", "pace")} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(fake), 0o755)
	}
	shells := 0
	for _, sh := range []string{"/bin/sh", "/bin/bash", "/bin/zsh", "/bin/dash", "/bin/csh", "/bin/tcsh", "/opt/homebrew/bin/fish", "/usr/bin/fish", "/usr/local/bin/fish"} {
		if _, err := os.Stat(sh); err != nil {
			continue
		}
		shells++
		for _, pace := range []string{"", "~/my bin/pace", filepath.Join(home, "my bin", "pace")} {
			cmd := exec.Command(sh, "-c", remoteCommand(pace))
			cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if s, ok := parseStatus(out); err != nil || !ok || s.Mode != "observe" {
				t.Errorf("%s, pace=%q: %v %q", sh, pace, err, out)
			}
		}
		// No pace anywhere: exit 127 (no-pace), not some other failure.
		cmd := exec.Command(sh, "-c", remoteCommand(""))
		cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
		err := cmd.Run()
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 127 {
			t.Errorf("%s without pace: %v, want exit 127", sh, err)
		}
	}
	if shells < 2 {
		t.Skip("fewer than two shells installed")
	}
}

func TestParseStatusWithBanner(t *testing.T) {
	for _, out := range []string{
		`{"v":1,"mode":"enforce"}`,
		"Welcome to box\nLast login: today\n{\"v\":1,\"mode\":\"enforce\"}\n",
	} {
		if s, ok := parseStatus([]byte(out)); !ok || s.Mode != "enforce" {
			t.Errorf("%q: %v %v", out, s, ok)
		}
	}
	for _, out := range []string{"", "hello", "{\"v\":0}", "{not json"} {
		if _, ok := parseStatus([]byte(out)); ok {
			t.Errorf("%q accepted", out)
		}
	}
}

// A container peer runs `docker exec [-u USER] CONTAINER pace ARGS` on the
// ssh host, in any login shell; a fake docker checks the exact argv.
func TestContainerPeerCommand(t *testing.T) {
	home := t.TempDir() // the fake docker goes first on the command's own PATH
	bin := filepath.Join(home, ".local", "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n[ \"$*\" = \"exec -u worker build-worker pace status --json\" ] || { echo \"argv: $*\" >&2; exit 9; }\necho '{\"v\":1,\"mode\":\"observe\"}'\n"), 0o755)
	p := Peer{Name: "dev-worker", SSH: "root@h", Container: "build-worker", ContainerUser: "worker"}
	n := 0
	for _, sh := range []string{"/bin/sh", "/bin/bash", "/bin/zsh", "/bin/csh", "/bin/tcsh"} {
		if _, err := os.Stat(sh); err != nil {
			continue
		}
		n++
		cmd := exec.Command(sh, "-c", p.Command("status", "--json"))
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if s, ok := parseStatus(out); err != nil || !ok || s.Mode != "observe" {
			t.Errorf("%s: %v %q", sh, err, out)
		}
	}
	if n == 0 {
		t.Skip("no shells")
	}
	if got := (Peer{SSH: "h", Container: "c1", Pace: "/opt/pace"}).Command("status", "--json"); !strings.HasSuffix(got, "exec docker exec c1 /opt/pace status --json'") {
		t.Errorf("no user, explicit path: %s", got)
	}
	// docker in Homebrew's dir (not on sshd's PATH) is found; no docker at
	// all exits 127 with the marker, told apart from pace missing inside.
	os.MkdirAll(filepath.Join(home, ".orbstack", "bin"), 0o755)
	os.Rename(filepath.Join(bin, "docker"), filepath.Join(home, ".orbstack", "bin", "docker"))
	cmd := exec.Command("/bin/sh", "-c", p.Command("status", "--json"))
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("docker under ~/.orbstack/bin not found: %v %q", err, out)
	}
	for _, d := range []string{"/opt/homebrew/bin/docker", "/usr/local/bin/docker", "/usr/bin/docker", "/bin/docker"} {
		if _, err := os.Stat(d); err == nil {
			t.Skipf("a real docker CLI at %s: the no-docker case cannot be run here", d)
		}
	}
	cmd = exec.Command("/bin/sh", "-c", p.Command("status", "--json"))
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 127 || !strings.Contains(string(out), noDockerMsg) {
		t.Errorf("no docker: %v %q", err, out)
	}
	for _, bad := range []Peer{
		{Name: "a", SSH: "h", Container: "-it"},
		{Name: "a", SSH: "h", Container: "c; rm"},
		{Name: "a", SSH: "h", Container: "c", ContainerUser: "u -v"},
		{Name: "a", SSH: "h", Container: "c", Pace: "~/bin/pace"},
		{Name: "a", SSH: "h", ContainerUser: "worker"},
	} {
		if err := (&File{Peers: []Peer{bad}}).Validate(); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
