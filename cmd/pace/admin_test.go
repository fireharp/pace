package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
)

func TestInstallDryRun(t *testing.T) {
	te := newTestEnv(t)
	rr := &recRunner{}
	newRunner = func() integrate.Runner { return rr }
	if code := te.run("install", "--mode", "observe", "--dry-run"); code != 0 {
		t.Fatalf("install --dry-run: %d %s", code, te.err.String())
	}
	o := te.out.String()
	for _, want := range []string{"would write", "/opt/pace/bin/pace", "would run:", "PACE_MODE", "service environment: PACE_SOCK=" + te.sock} {
		if !strings.Contains(o, want) {
			t.Errorf("dry run output missing %q:\n%s", want, o)
		}
	}
	for _, c := range rr.calls {
		if !strings.HasPrefix(c, "launchctl print") && !strings.Contains(c, "is-active") {
			t.Fatalf("dry run executed %q", c)
		}
	}
	ents, _ := os.ReadDir(te.home)
	if len(ents) != 0 {
		t.Fatalf("dry run wrote into HOME: %v", ents)
	}
	if code := te.run("uninstall", "--dry-run"); code != 0 {
		t.Fatalf("uninstall --dry-run: %d %s", code, te.err.String())
	}
}

func TestInstallForRealWithFakeRunner(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("service install supported on darwin/linux only")
	}
	te := newTestEnv(t)
	rr := &recRunner{}
	newRunner = func() integrate.Runner { return rr }
	startDaemon(t, te.sock, richStatus(), nil) // health check passes
	if code := te.run("install", "--mode", "warn-only"); code != 0 {
		t.Fatalf("install: %d %s", code, te.err.String())
	}
	var file string
	if runtime.GOOS == "darwin" {
		file = integrate.PlistPath(te.home)
	} else {
		file = integrate.UnitPath(te.home, nil)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("service file: %v", err)
	}
	if !strings.Contains(string(b), "/opt/pace/bin/pace") || !strings.Contains(string(b), "warn-only") || !strings.Contains(string(b), te.sock) {
		t.Fatalf("service file content:\n%s", b)
	}
	if !strings.Contains(te.out.String(), "daemon is answering") {
		t.Fatalf("no health confirmation: %s", te.out.String())
	}
	if code := te.run("uninstall"); code != 0 {
		t.Fatalf("uninstall: %d %s", code, te.err.String())
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("service file not removed")
	}
}

func TestInstallRefusesGoRunBinary(t *testing.T) {
	te := newTestEnv(t)
	te.c.exe = "/private/var/folders/xx/T/go-build1234/b001/exe/pace"
	if code := te.run("install", "--dry-run"); code != exitUsage || !strings.Contains(te.err.String(), "go run") {
		t.Fatalf("%d %q", code, te.err.String())
	}
	if code := te.run("shims", "install", "--dry-run"); code != exitUsage {
		t.Fatalf("shims with go-run binary: %d", code)
	}
}

func TestShimsCommands(t *testing.T) {
	te := newTestEnv(t)
	dir := filepath.Join(t.TempDir(), "shims")
	if code := te.run("shims", "list", "--dir", dir); code != 0 || !strings.Contains(te.out.String(), "no shims") {
		t.Fatalf("empty list: %d %q", code, te.out.String())
	}
	if code := te.run("shims", "install", "--dir", dir, "--tools", "go, npm,cargo", "--dry-run"); code != 0 {
		t.Fatalf("dry: %d %s", code, te.err.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("dry run created dir")
	}
	if code := te.run("shims", "install", "--dir", dir, "--tools", "go,npm"); code != 0 {
		t.Fatalf("install: %d %s", code, te.err.String())
	}
	if !integrate.IsShim(filepath.Join(dir, "go")) || !integrate.IsShim(filepath.Join(dir, "npm")) {
		t.Fatal("shims missing")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "go"))
	if !strings.Contains(string(b), "g=/opt/pace/bin/pace") {
		t.Fatalf("shim does not point at this binary:\n%s", b)
	}
	if !strings.Contains(te.out.String(), "not first on PATH") {
		t.Fatalf("missing PATH advice: %q", te.out.String())
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	if code := te.run("shims", "list", "--dir", dir); code != 0 || !strings.Contains(te.out.String(), "2 shims, PATH entry 1") {
		t.Fatalf("list: %d %q", code, te.out.String())
	}
	if code := te.run("shims", "install", "--dir", dir, "--tools", "bad/name"); code != exitFail {
		t.Fatalf("bad tool: %d", code)
	}
	if code := te.run("shims", "uninstall", "--dir", dir); code != 0 {
		t.Fatalf("uninstall: %d", code)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("shim dir not removed")
	}
	// Default dir honours PACE_SHIM_DIR.
	def := filepath.Join(t.TempDir(), "def")
	t.Setenv("PACE_SHIM_DIR", def)
	if code := te.run("shims", "install", "--tools", "go"); code != 0 || !integrate.IsShim(filepath.Join(def, "go")) {
		t.Fatalf("default dir: %d", code)
	}
}

func TestHookCommands(t *testing.T) {
	te := newTestEnv(t)
	shimDir := filepath.Join(t.TempDir(), "s")
	t.Setenv("PACE_SHIM_DIR", shimDir)
	integrate.InstallShims(integrate.ShimOptions{Dir: shimDir, PaceBin: "/opt/pace/bin/pace", Tools: []string{"go"}})
	envFile := filepath.Join(t.TempDir(), "env")
	t.Setenv("CLAUDE_ENV_FILE", envFile)

	// Daemon down: both hooks exit 0 quickly.
	te.c.stdin = strings.NewReader(`{"hook_event_name":"SessionStart","source":"startup"}`)
	start := time.Now()
	if code := te.run("hook", "claude-session-start"); code != 0 {
		t.Fatalf("session-start: %d", code)
	}
	if b, _ := os.ReadFile(envFile); !strings.Contains(string(b), shimDir) {
		t.Fatalf("env file: %q", b)
	}
	if !strings.Contains(te.out.String(), "daemon unreachable") {
		t.Fatalf("context: %q", te.out.String())
	}
	te.c.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)
	if code := te.run("hook", "claude-pre-tool-use"); code != 0 || te.out.Len() != 0 {
		t.Fatalf("pre-tool-use down: %d %q", code, te.out.String())
	}
	if el := time.Since(start); el > 400*time.Millisecond {
		t.Fatalf("hooks took %v with the daemon down", el)
	}
	// Unknown hook still exits 0.
	if code := te.run("hook", "nope"); code != 0 {
		t.Fatalf("unknown hook: %d", code)
	}

	// Under pressure: additionalContext.
	warn := richStatus()
	startDaemon(t, te.sock, warn, nil)
	te.c.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"go test ./..."}}`)
	start = time.Now()
	if code := te.run("hook", "claude-pre-tool-use"); code != 0 {
		t.Fatalf("pre-tool-use: %d", code)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("pre-tool-use took %v", el)
	}
	var o integrate.PreToolUseOutput
	if err := json.Unmarshal(te.out.Bytes(), &o); err != nil || !strings.Contains(o.HookSpecificOutput.AdditionalContext, "pressure=warn") {
		t.Fatalf("output %q %v", te.out.String(), err)
	}
	te.c.stdin = strings.NewReader(`{}`)
	if code := te.run("hook", "claude-session-start", "--quiet"); code != 0 || te.out.Len() != 0 {
		t.Fatalf("quiet: %d %q", code, te.out.String())
	}
}

func TestIntegrateClaude(t *testing.T) {
	te := newTestEnv(t)
	for _, args := range [][]string{{"integrate", "claude"}, {"integrate", "claude", "--print"}} {
		if code := te.run(args...); code != 0 {
			t.Fatalf("%v: %d", args, code)
		}
		var v map[string]any
		if err := json.Unmarshal(te.out.Bytes(), &v); err != nil {
			t.Fatalf("snippet not JSON: %v %q", err, te.out.String())
		}
		if !strings.Contains(te.out.String(), "/opt/pace/bin/pace hook claude-pre-tool-use") {
			t.Fatalf("snippet: %s", te.out.String())
		}
		if !strings.Contains(te.err.String(), "yourself") {
			t.Fatalf("guidance: %s", te.err.String())
		}
	}
	if _, err := os.Stat(filepath.Join(te.home, ".claude")); !os.IsNotExist(err) {
		t.Fatal("integrate touched ~/.claude")
	}
}

func TestConfigCommands(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("config", "example"); code != 0 || te.out.String() != integrate.ExampleConfig {
		t.Fatalf("example: %d", code)
	}
	if code := te.run("config", "check"); code != 0 || !strings.HasPrefix(te.out.String(), "ok ") || !strings.Contains(te.out.String(), "socket="+te.sock) {
		t.Fatalf("check: %d %q %q", code, te.out.String(), te.err.String())
	}
	if code := te.run("config", "print"); code != 0 {
		t.Fatalf("print: %d %s", code, te.err.String())
	}
	var r map[string]any
	if err := json.Unmarshal(te.out.Bytes(), &r); err != nil || r["Socket"] != te.sock {
		t.Fatalf("print json: %v %q", err, te.out.String())
	}
	// Flags after the action are accepted.
	if code := te.run("config", "check", "--config", os.Getenv("PACE_CONFIG")); code != 0 {
		t.Fatalf("check --config: %d %s", code, te.err.String())
	}
	// An over-long socket path fails the check.
	t.Setenv("PACE_SOCK", "/tmp/"+strings.Repeat("s", 120))
	if code := te.run("config", "check"); code != exitUsage || !strings.Contains(te.err.String(), "103") {
		t.Fatalf("long socket: %d %q", code, te.err.String())
	}
}

func TestReplayCommand(t *testing.T) {
	te := newTestEnv(t)
	if code := te.run("replay", "/nonexistent/decisions.jsonl"); code != exitUsage {
		t.Fatalf("missing file: %d", code)
	}
	f := filepath.Join(t.TempDir(), "decisions.jsonl")
	os.WriteFile(f, nil, 0o644)
	code := te.run("replay", f, "--config", os.Getenv("PACE_CONFIG"))
	if code != 0 && code != exitFail {
		t.Fatalf("replay: %d %s", code, te.err.String())
	}
	if !strings.Contains(te.out.String(), "replay: records=") {
		t.Fatalf("summary: %q", te.out.String())
	}
}

func TestDoctorRuns(t *testing.T) {
	te := newTestEnv(t)
	code := te.run("doctor")
	if code != exitFail { // daemon is down in tests -> FAIL
		t.Fatalf("doctor exit %d, want 1 with no daemon:\n%s", code, te.out.String())
	}
	o := te.out.String()
	for _, check := range []string{"binary", "config", "daemon", "service", "shims", "claude", "logs", "platform", "failed"} {
		if !strings.Contains(o, check) {
			t.Errorf("doctor output missing %q:\n%s", check, o)
		}
	}
	if !strings.Contains(o, "FAIL  daemon") {
		t.Errorf("daemon down not reported as FAIL:\n%s", o)
	}
	startDaemon(t, te.sock, richStatus(), nil)
	te.run("doctor")
	if !strings.Contains(te.out.String(), "ok    daemon    pid "+strconv.Itoa(os.Getpid())) {
		t.Errorf("daemon pid not found via peer credentials:\n%s", te.out.String())
	}
	if strings.Contains(te.out.String(), "overrides") {
		t.Errorf("daemon and config agree on enforce, yet doctor reports an override:\n%s", te.out.String())
	}
}

// TestDoctorModeOverride: 'pace install --mode observe' sets PACE_MODE in the
// service, so the daemon runs observe while the file (or the defaults) says
// enforce; doctor says which one wins instead of printing both bare.
func TestDoctorModeOverride(t *testing.T) {
	te := newTestEnv(t)
	st := richStatus()
	st.Mode = "observe"
	startDaemon(t, te.sock, st, nil)
	te.run("doctor")
	o := te.out.String()
	if !strings.Contains(o, "WARN  mode      observe: enforcement is off (the daemon's environment overrides the config's mode=enforce)") {
		t.Errorf("override not explained:\n%s", o)
	}
}

func TestEXITLineGrammar(t *testing.T) {
	te := newTestEnv(t)
	te.c.paceExit(`weird "msg" with = and spaces`, "")
	lines := strings.Split(strings.TrimSpace(te.err.String()), "\n")
	verb, kv, ok := proto.ParseLine(lines[len(lines)-1])
	if !ok || verb != "EXIT" || kv["msg"] != `weird "msg" with = and spaces` {
		t.Fatalf("%v %v %v", verb, kv, ok)
	}
}

func TestIntegrateApplyRemove(t *testing.T) {
	te := newTestEnv(t)
	t.Setenv("HOME", te.home)
	for _, h := range []string{"claude", "codex"} {
		file := filepath.Join(te.home, "."+h, map[string]string{"claude": "settings.json", "codex": "hooks.json"}[h])
		os.MkdirAll(filepath.Dir(file), 0o700)
		orig := `{"keep":true,"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/other >/dev/null 2>&1"}]}]}}`
		os.WriteFile(file, []byte(orig), 0o600)
		if code := te.run("integrate", h, "--apply"); code != 0 {
			t.Fatalf("%s apply: %d %s", h, code, te.err.String())
		}
		if !strings.Contains(te.out.String(), "previous file saved as") {
			t.Fatalf("%s: %s", h, te.out.String())
		}
		found, err := integrate.HooksInstalled(file)
		if err != nil || len(found["SessionStart"]) != 1 || !strings.Contains(found["PreToolUse"][0], "/opt/pace/bin/pace hook "+h+"-pre-tool-use") {
			t.Fatalf("%s: %v %v", h, found, err)
		}
		b, _ := os.ReadFile(file)
		if !strings.Contains(string(b), `"keep": true`) || !strings.Contains(string(b), ">/dev/null 2>&1") {
			t.Fatalf("%s: lost other content:\n%s", h, b)
		}
		if code := te.run("integrate", h, "--apply"); code != 0 || !strings.Contains(te.out.String(), "nothing changed") {
			t.Fatalf("%s reapply: %d %s", h, code, te.out.String())
		}
		te.run("doctor")
		if !strings.Contains(te.out.String(), "ok    "+h) {
			t.Errorf("doctor does not see the %s hooks:\n%s", h, te.out.String())
		}
		if code := te.run("integrate", h, "--remove"); code != 0 {
			t.Fatalf("%s remove: %d %s", h, code, te.err.String())
		}
		b, _ = os.ReadFile(file)
		var got, want any
		json.Unmarshal(b, &got)
		json.Unmarshal([]byte(orig), &want)
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		if string(gb) != string(wb) {
			t.Fatalf("%s: remove did not restore:\n%s", h, b)
		}
	}
	if code := te.run("integrate", "codex", "--apply", "--remove"); code != exitUsage {
		t.Fatalf("exclusive flags: %d", code)
	}
	os.WriteFile(filepath.Join(te.home, ".codex", "hooks.json"), []byte("{bad"), 0o600)
	if code := te.run("integrate", "codex", "--apply"); code != exitFail || !strings.Contains(te.err.String(), "nothing changed") {
		t.Fatalf("bad file: %d %s", code, te.err.String())
	}
}

func TestCodexHookCommands(t *testing.T) {
	te := newTestEnv(t)
	te.c.stdin = strings.NewReader(`{"hook_event_name":"SessionStart","source":"startup"}`)
	if code := te.run("hook", "codex-session-start"); code != 0 || !strings.Contains(te.out.String(), "/opt/pace/bin/pace run [--class") {
		t.Fatalf("session start: %d %q", code, te.out.String())
	}
	// Daemon down, heavy command outside pace: a reminder, exit 0.
	te.c.stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"cd x && go test ./..."}}`)
	if code := te.run("hook", "codex-pre-tool-use"); code != 0 {
		t.Fatalf("pre-tool-use: %d", code)
	}
	var o integrate.PreToolUseOutput
	if err := json.Unmarshal(te.out.Bytes(), &o); err != nil || !strings.Contains(o.HookSpecificOutput.AdditionalContext, "/opt/pace/bin/pace run --class test -- <command>") {
		t.Fatalf("output %q %v", te.out.String(), err)
	}
}
