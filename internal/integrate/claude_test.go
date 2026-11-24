package integrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

const sessionStartPayload = `{"session_id":"abc","transcript_path":"/x.jsonl","cwd":"/repo","hook_event_name":"SessionStart","source":"startup"}`

func bashPayload(cmd string) string {
	b, _ := json.Marshal(map[string]any{
		"session_id": "abc", "hook_event_name": "PreToolUse", "tool_name": "Bash",
		"tool_input": map[string]any{"command": cmd, "description": "x"}, "cwd": "/repo",
	})
	return string(b)
}

func statusFn(s proto.Status, err error) func(context.Context) (proto.Status, error) {
	return func(context.Context) (proto.Status, error) { return s, err }
}

func TestSessionStartWritesEnvFile(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "my shims")
	if _, err := InstallShims(ShimOptions{Dir: shimDir, PaceBin: "/x/pace", Tools: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(root, "env.sh")
	os.WriteFile(envFile, []byte("export FOO=1"), 0o600) // no trailing newline
	var out bytes.Buffer
	d := HookDeps{
		Stdin: strings.NewReader(sessionStartPayload), Stdout: &out,
		Env: MapEnv(map[string]string{"CLAUDE_ENV_FILE": envFile}), ShimDir: shimDir,
		Status: statusFn(sampleStatus(), nil),
	}
	if err := SessionStart(d); err != nil {
		t.Fatal(err)
	}
	// Twice: idempotent.
	d.Stdin = strings.NewReader(sessionStartPayload)
	if err := SessionStart(d); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(envFile)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) != 2 || lines[0] != "export FOO=1" || lines[1] != envFileLine(shimDir) {
		t.Fatalf("env file:\n%s", b)
	}
	// Sourcing it (repeatedly) puts the shim dir first exactly once.
	script := ". " + ShellQuote(envFile) + "; . " + ShellQuote(envFile) + `; printf %s "$PATH"`
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	got, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != shimDir+":/usr/bin:/bin" {
		t.Fatalf("PATH after sourcing = %q", got)
	}
	o := out.String()
	if !strings.Contains(o, "routed through 'pace run'") || !strings.Contains(o, "pressure=normal") || !strings.Contains(o, "headroom=13.0G") {
		t.Fatalf("context: %s", o)
	}
}

func TestSessionStartNoShimsNoEnvFile(t *testing.T) {
	root := t.TempDir()
	envFile := filepath.Join(root, "env.sh")
	var out bytes.Buffer
	d := HookDeps{
		Stdin: strings.NewReader("not json"), Stdout: &out,
		Env:     MapEnv(map[string]string{"CLAUDE_ENV_FILE": envFile}),
		ShimDir: filepath.Join(root, "none"),
		Status:  statusFn(proto.Status{}, errors.New("down")),
	}
	if err := SessionStart(d); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatal("env file written without shims")
	}
	o := out.String()
	if !strings.Contains(o, "pace run [--class") || !strings.Contains(o, "daemon unreachable") {
		t.Fatalf("context: %s", o)
	}
	// Quiet prints nothing.
	out.Reset()
	d.Quiet = true
	d.Stdin = nil
	if err := SessionStart(d); err != nil || out.Len() != 0 {
		t.Fatalf("quiet: %v %q", err, out.String())
	}
}

func TestSessionStartEnvFileError(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "s")
	InstallShims(ShimOptions{Dir: shimDir, PaceBin: "/x/pace", Tools: []string{"go"}})
	d := HookDeps{
		Env: MapEnv(map[string]string{"CLAUDE_ENV_FILE": filepath.Join(root, "nodir", "env")}), ShimDir: shimDir,
		Stdout: io.Discard,
	}
	if err := SessionStart(d); err == nil {
		t.Fatal("want error for unwritable env file")
	}
}

func decodePre(t *testing.T, s string) PreToolUseOutput {
	t.Helper()
	var o PreToolUseOutput
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		t.Fatalf("output not JSON: %q", s)
	}
	return o
}

func TestPreToolUse(t *testing.T) {
	warn := sampleStatus()
	warn.Pressure = proto.LevelWarn
	warn.HeadroomBytes = 1200 << 20
	warn.SuggestedConcurrency = 1
	warn.Admitting = false
	warn.NotAdmittingReasons = []string{"pressure"}
	crit := warn
	crit.Pressure = proto.LevelCritical

	cases := []struct {
		name    string
		stdin   string
		status  proto.Status
		err     error
		want    bool
		contain []string
	}{
		{"normal", bashPayload("go test ./..."), sampleStatus(), nil, false, nil},
		{"warn heavy", bashPayload("go test ./..."), warn, nil, true, []string{"pace: mode=enforce pressure=warn headroom=1.2G suggested_concurrency=1 admitting=false reasons=pressure", "class=test", "one at a time"}},
		{"warn light", bashPayload("ls"), warn, nil, true, []string{"pressure=warn"}},
		{"critical", bashPayload("npm ci"), crit, nil, true, []string{"pressure=critical", "do not start heavy commands", "class=build"}},
		{"daemon down", bashPayload("go build"), proto.Status{}, &UnreachableError{Err: errors.New("x")}, false, nil},
		{"not bash", `{"hook_event_name":"PreToolUse","tool_name":"Edit","tool_input":{}}`, warn, nil, false, nil},
		{"garbage", `{{{`, warn, nil, false, nil},
		{"empty", ``, warn, nil, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := PreToolUse(HookDeps{Stdin: strings.NewReader(c.stdin), Stdout: &out, Status: statusFn(c.status, c.err)})
			if err != nil {
				t.Fatal(err)
			}
			if !c.want {
				if out.Len() != 0 {
					t.Fatalf("expected no output, got %q", out.String())
				}
				return
			}
			o := decodePre(t, out.String())
			if o.HookSpecificOutput.HookEventName != "PreToolUse" {
				t.Fatalf("event name %q", o.HookSpecificOutput.HookEventName)
			}
			// Never a decision or rewrite.
			if strings.Contains(out.String(), "permissionDecision") || strings.Contains(out.String(), "updatedInput") {
				t.Fatal("hook must never decide or rewrite")
			}
			for _, s := range c.contain {
				if !strings.Contains(o.HookSpecificOutput.AdditionalContext, s) {
					t.Errorf("context missing %q:\n%s", s, o.HookSpecificOutput.AdditionalContext)
				}
			}
		})
	}
}

func TestPreToolUseBudget(t *testing.T) {
	// A slow daemon and a stdin that never closes: still returns within budget.
	slow := func(ctx context.Context) (proto.Status, error) {
		<-ctx.Done()
		return proto.Status{}, ctx.Err()
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	start := time.Now()
	var out bytes.Buffer
	if err := PreToolUse(HookDeps{Stdin: pr, Stdout: &out, Status: slow, Budget: 80 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Fatalf("took %v", el)
	}
	if out.Len() != 0 {
		t.Fatalf("output %q", out.String())
	}

	// Slow status with a prompt stdin.
	start = time.Now()
	if err := PreToolUse(HookDeps{Stdin: strings.NewReader(bashPayload("go build")), Stdout: &out, Status: slow, Budget: 80 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Fatalf("took %v", el)
	}
	// Status func that ignores ctx entirely.
	stuck := func(context.Context) (proto.Status, error) { time.Sleep(time.Second); return proto.Status{}, nil }
	start = time.Now()
	PreToolUse(HookDeps{Stdin: strings.NewReader(bashPayload("go build")), Stdout: &out, Status: stuck, Budget: 50 * time.Millisecond})
	if el := time.Since(start); el > 150*time.Millisecond {
		t.Fatalf("ignored-ctx status took %v", el)
	}
}

func TestPreToolUseAgainstFakeDaemon(t *testing.T) {
	fd := startFakeDaemon(t, &fakeDaemon{})
	warn := sampleStatus()
	warn.Pressure = proto.LevelWarn
	fd.status.Store(warn)
	c := &Client{Socket: fd.sock, Timeout: time.Second}
	var out bytes.Buffer
	d := HookDeps{
		Stdin: strings.NewReader(bashPayload("pytest")), Stdout: &out,
		Status: func(ctx context.Context) (proto.Status, error) { s, _, err := c.Status(ctx); return s, err },
	}
	start := time.Now()
	if err := PreToolUse(d); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 200*time.Millisecond {
		t.Fatalf("hook took %v", el)
	}
	if !strings.Contains(decodePre(t, out.String()).HookSpecificOutput.AdditionalContext, "pressure=warn") {
		t.Fatalf("out %q", out.String())
	}
}

func TestClaudeSettingsSnippet(t *testing.T) {
	s := ClaudeSettingsSnippet("/Users/me/my bin/pace")
	var v struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("snippet not JSON: %v\n%s", err, s)
	}
	ss := v.Hooks["SessionStart"]
	pt := v.Hooks["PreToolUse"]
	if len(ss) != 1 || len(pt) != 1 || pt[0].Matcher != "Bash" {
		t.Fatalf("snippet shape: %s", s)
	}
	if ss[0].Hooks[0].Command != "'/Users/me/my bin/pace' hook claude-session-start" ||
		pt[0].Hooks[0].Command != "'/Users/me/my bin/pace' hook claude-pre-tool-use" {
		t.Fatalf("commands: %s", s)
	}
	if pt[0].Hooks[0].Timeout <= 0 || ss[0].Hooks[0].Type != "command" {
		t.Fatalf("timeouts/types: %s", s)
	}
}

func TestEnvFileLineQuoting(t *testing.T) {
	dir := "/a b/it's"
	line := envFileLine(dir)
	cmd := exec.Command("/bin/sh", "-c", line+"\n"+`printf %s "$PATH"`)
	cmd.Env = []string{"PATH=/bin"}
	got, err := cmd.Output()
	if err != nil || string(got) != dir+":/bin" {
		t.Fatalf("got %q %v", got, err)
	}
}

// The PreToolUse texts say only what the daemon's mode does: observe admits
// every job at once and enforces nothing, warn-only queues and refuses but
// never stops a running job, enforce keeps its texts.
func TestPreToolUseModeHonest(t *testing.T) {
	const (
		stopping = "pace is stopping jobs now"
		queue    = "heavy commands will queue or be refused"
		wait     = "will wait for headroom"
	)
	cases := []struct {
		mode, level string
		contain     []string
		absent      []string
	}{
		{"enforce", proto.LevelCritical, []string{stopping, wait, "do not start heavy commands"}, nil},
		{"enforce", proto.LevelWarn, []string{queue, wait, "one at a time"}, []string{stopping}},
		{"warn-only", proto.LevelCritical, []string{queue, wait, "stops no running job", "do not start heavy commands"}, []string{stopping}},
		{"warn-only", proto.LevelWarn, []string{queue, wait, "stops no running job"}, []string{stopping}},
		{"observe", proto.LevelCritical, []string{"only reports", "nothing is queued, refused or stopped", "starts it at once", "class=build"}, []string{stopping, queue, wait}},
		{"observe", proto.LevelWarn, []string{"only reports", "nothing is queued, refused or stopped", "starts it at once", "class=build"}, []string{stopping, queue, wait}},
		// An empty mode (an older daemon) or one this hook does not know
		// gets the enforce texts.
		{"", proto.LevelCritical, []string{stopping, wait, "do not start heavy commands"}, []string{"only reports", "stops no running job"}},
		{"", proto.LevelWarn, []string{queue, wait, "one at a time"}, []string{stopping, "only reports", "stops no running job"}},
		{"bogus", proto.LevelCritical, []string{stopping, wait, "do not start heavy commands"}, []string{"only reports", "stops no running job"}},
		{"bogus", proto.LevelWarn, []string{queue, wait, "one at a time"}, []string{stopping, "only reports", "stops no running job"}},
	}
	hooks := []struct {
		name string
		fn   func(HookDeps) error
		cmd  string
	}{
		{"claude", PreToolUse, "make build"},
		{"codex", CodexPreToolUse, "pace run -- make build"},
	}
	for _, h := range hooks {
		for _, c := range cases {
			t.Run(h.name+"/"+orDash(c.mode)+"/"+c.level, func(t *testing.T) {
				s := sampleStatus()
				s.Mode = c.mode
				s.Pressure = c.level
				s.Admitting = c.mode == policy.ModeObserve
				var out bytes.Buffer
				if err := h.fn(HookDeps{Stdin: strings.NewReader(bashPayload(h.cmd)), Stdout: &out, Status: statusFn(s, nil)}); err != nil {
					t.Fatal(err)
				}
				ctx := decodePre(t, out.String()).HookSpecificOutput.AdditionalContext
				if !strings.Contains(ctx, "pace: mode="+orDash(c.mode)+" pressure="+c.level) {
					t.Errorf("no summary line:\n%s", ctx)
				}
				for _, w := range c.contain {
					if !strings.Contains(ctx, w) {
						t.Errorf("missing %q:\n%s", w, ctx)
					}
				}
				for _, w := range c.absent {
					if strings.Contains(ctx, w) {
						t.Errorf("claims %q, which mode %s does not do:\n%s", w, c.mode, ctx)
					}
				}
			})
		}
	}
}

// SessionStart explains exit 201 for every killing policy (not only
// mem_budget) and says what observe and warn-only do not do.
func TestSessionStartModeAndExitCodes(t *testing.T) {
	starts := []struct {
		name string
		fn   func(HookDeps) error
	}{{"claude", SessionStart}, {"codex", CodexSessionStart}}
	for _, st := range starts {
		for _, mode := range []string{"enforce", "warn-only", "observe"} {
			t.Run(st.name+"/"+mode, func(t *testing.T) {
				s := sampleStatus()
				s.Mode = mode
				var out bytes.Buffer
				if err := st.fn(HookDeps{Stdin: strings.NewReader(sessionStartPayload), Stdout: &out, Status: statusFn(s, nil)}); err != nil {
					t.Fatal(err)
				}
				got := out.String()
				if strings.Count(got, "\n") != 1 {
					t.Errorf("want one line: %q", got)
				}
				for _, w := range []string{"201 = killed by policy", "policy=", "mem_budget: lower parallelism or ask for a larger --budget", "wall_clock: split the work or raise --timeout", "stuck: check why the command hangs", "pressure: retry when pace status shows headroom", "Now: mode=" + mode} {
					if !strings.Contains(got, w) {
						t.Errorf("missing %q: %s", w, got)
					}
				}
				if strings.Contains(got, "201 = killed by policy (lower parallelism") {
					t.Errorf("201 explained as a memory budget kill only: %s", got)
				}
				switch mode {
				case "observe":
					if !strings.Contains(got, "Mode observe: pace only reports and warns") {
						t.Errorf("no observe note: %s", got)
					}
				case "warn-only":
					if !strings.Contains(got, "never throttles, freezes or kills a running one") {
						t.Errorf("no warn-only note: %s", got)
					}
				default:
					if strings.Contains(got, "Mode ") {
						t.Errorf("enforce carries a mode note: %s", got)
					}
				}
			})
		}
	}
}

// The example agent note repeats the SessionStart exit-code
// sentence; the two copies must not drift apart (the note once kept the
// old, wrong 201 text).
func TestAgentNoteExitCodesMatchHook(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "agent-note.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimSuffix(exitCodesText, " ")
	if !strings.Contains(string(b), want) {
		t.Errorf("docs/agent-note.md does not contain the hook's exit-code text %q", want)
	}
}
