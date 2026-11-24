package integrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

func codexPayload(toolInput string) string {
	return `{"session_id":"s","hook_event_name":"PreToolUse","tool_name":"Bash","turn_id":"t","tool_use_id":"u","tool_input":` + toolInput + `}`
}

func TestCommandFromToolInput(t *testing.T) {
	cases := map[string]string{
		`{"command":"go test ./..."}`:                      "go test ./...",
		`{"cmd":"npm ci"}`:                                 "npm ci",
		`{"command":["bash","-lc","cd x && cargo build"]}`: "cd x && cargo build",
		`{"command":["zsh","-l","-c","pytest -x"]}`:        "pytest -x",
		`{"command":["make","-j8"]}`:                       "make -j8",
		`{"command":["bash","-c","go test ./...","sh"]}`:   "go test ./...",
		`{"command":["bash","-e","-c","go test"]}`:         "go test",
		`{"command":["bash","--norc","-c","npm ci"]}`:      "npm ci",
		`{"command":["/bin/zsh","-lc","pytest"]}`:          "pytest",
		`{"other":1}`: "",
		`"str"`:       "",
	}
	for in, want := range cases {
		if got := CommandFromToolInput(json.RawMessage(in)); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestClassifyShell(t *testing.T) {
	cases := []struct {
		line          string
		class         string
		heavy, routed bool
	}{
		{"ls -la", "", false, false},
		{"go test ./...", "test", true, false},
		{"cd svc && go test ./...", "test", true, false},
		{"FOO=1 CGO_ENABLED=0 go build ./...", "build", true, false},
		{"time npm ci", "build", true, false},
		{"pace run -- go test ./...", "test", true, true},
		{"/Users/x/.local/bin/pace run --class build -- cargo build", "build", true, true},
		{"pace run -- go test ./... && go build ./...", "test", true, false},
		{"git status; go version", "", false, false},
		{"rg foo | head", "", false, false},
		{"(cd a; pytest -q)", "test", true, false},
		// review findings: quotes, heredocs, redirections, comments
		{"pace run --class test -- bash -c 'go test ./... && go build ./...'", "test", true, true},
		{"pace run -- sh -c 'cd x; cargo build'", "build", true, true},
		{`git commit -m "fix flaky test; go test now passes"`, "", false, false},
		{"git commit -m 'npm ci && make'", "", false, false},
		{"cat <<EOF > f.sh\ngo test ./...\nEOF\nls", "", false, false},
		{"cat <<-'END' >x\n\tnpm ci\n\tEND\ngo build ./...", "build", true, false},
		{"go test ./... 2>&1 | tee log", "test", true, false},
		{"npm test &> out.log", "test", true, false},
		{"# go test ./...\nls", "", false, false},
		{`echo "a" && pytest`, "test", true, false},
		{"echo a\\\n && go vet ./...", "build", true, false},
		// second review: here-strings, glued redirections, arithmetic, $'..', sh -c arg0
		{`grep -q foo <<< "some text" && npm test`, "test", true, false},
		{"jq . <<< \"$json\"\nnpm ci", "build", true, false},
		{"cat <<< hello\nnpm test", "test", true, false},
		{"npm ci&>/dev/null", "build", true, false},
		{"pytest&>log.txt", "test", true, false},
		{"npm ci>/dev/null 2>&1", "build", true, false},
		{"go test ./... >>log 2>&1", "test", true, false},
		{"echo $((1<<20))\ncargo build", "build", true, false},
		{`echo $'it\'s' && npm test`, "test", true, false},
		{`echo $'a\' && npm test'`, "", false, false},
		{"pace run -- bash -c 'npm test' bash", "test", true, true},
		{"pace run --mem 2G -- bash -lc 'npm test' extra", "test", true, true},
	}
	for _, c := range cases {
		class, heavy, routed := ClassifyShell(c.line)
		if class != c.class || heavy != c.heavy || routed != c.routed {
			t.Errorf("%q: got (%q,%v,%v) want (%q,%v,%v)", c.line, class, heavy, routed, c.class, c.heavy, c.routed)
		}
	}
}

func TestCodexPreToolUse(t *testing.T) {
	warn := sampleStatus()
	warn.Pressure = proto.LevelWarn
	cases := []struct {
		name    string
		stdin   string
		status  proto.Status
		err     error
		contain []string
		absent  []string
	}{
		{"normal light", codexPayload(`{"command":"ls"}`), sampleStatus(), nil, nil, nil},
		{"normal heavy unrouted", codexPayload(`{"command":"cd a && go test ./..."}`), sampleStatus(), nil,
			[]string{"class=test", "runs outside pace", "'/opt/my pace/pace' run --class test -- <command>"}, []string{"pressure="}},
		{"normal heavy routed", codexPayload(`{"command":"pace run -- go test ./..."}`), sampleStatus(), nil, nil, nil},
		{"daemon down heavy", codexPayload(`{"command":["bash","-lc","npm ci"]}`), proto.Status{}, &UnreachableError{Err: errors.New("x")},
			[]string{"class=build", "runs outside pace"}, nil},
		{"warn heavy unrouted", codexPayload(`{"command":"cargo build"}`), warn, nil,
			[]string{"pressure=warn", "one at a time", "runs outside pace"}, []string{"will wait for headroom"}},
		{"warn heavy routed", codexPayload(`{"command":"pace run -- cargo build"}`), warn, nil,
			[]string{"pressure=warn", "will wait for headroom"}, []string{"runs outside pace"}},
		{"not shell", `{"hook_event_name":"PreToolUse","tool_name":"apply_patch","tool_input":{"command":"go test"}}`, warn, nil, nil, nil},
		{"garbage", `{{`, warn, nil, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := CodexPreToolUse(HookDeps{Stdin: strings.NewReader(c.stdin), Stdout: &out, Status: statusFn(c.status, c.err), PaceBin: "/opt/my pace/pace"})
			if err != nil {
				t.Fatal(err)
			}
			if len(c.contain) == 0 {
				if out.Len() != 0 {
					t.Fatalf("expected no output, got %q", out.String())
				}
				return
			}
			if strings.Contains(out.String(), "permissionDecision") || strings.Contains(out.String(), "updatedInput") {
				t.Fatal("hook must never decide or rewrite")
			}
			o := decodePre(t, out.String())
			ctx := o.HookSpecificOutput.AdditionalContext
			for _, s := range c.contain {
				if !strings.Contains(ctx, s) {
					t.Errorf("missing %q:\n%s", s, ctx)
				}
			}
			for _, s := range c.absent {
				if strings.Contains(ctx, s) {
					t.Errorf("unexpected %q:\n%s", s, ctx)
				}
			}
		})
	}
}

// Claude keeps its contract: nothing at normal pressure, even for an
// unrouted heavy command (the shims route it).
func TestClaudePreToolUseNoNudge(t *testing.T) {
	var out bytes.Buffer
	if err := PreToolUse(HookDeps{Stdin: strings.NewReader(bashPayload("go test ./...")), Stdout: &out, Status: statusFn(sampleStatus(), nil)}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("claude hook nudged: %s", out.String())
	}
}

func TestCodexSessionStart(t *testing.T) {
	var out bytes.Buffer
	if err := CodexSessionStart(HookDeps{Stdin: strings.NewReader(`{"hook_event_name":"SessionStart","source":"startup"}`), Stdout: &out, Status: statusFn(sampleStatus(), nil), PaceBin: "/b/pace"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"/b/pace run [--class", "Now: mode=", "201 = killed"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q: %s", want, s)
		}
	}
	if strings.Count(s, "\n") != 1 {
		t.Errorf("want one line: %q", s)
	}
	out.Reset()
	CodexSessionStart(HookDeps{Stdin: strings.NewReader(""), Stdout: &out, Status: statusFn(proto.Status{}, errors.New("down"))})
	if !strings.Contains(out.String(), "daemon unreachable") || !strings.Contains(out.String(), "pace run [--class") {
		t.Errorf("down: %s", out.String())
	}
	out.Reset()
	CodexSessionStart(HookDeps{Stdin: strings.NewReader(""), Stdout: &out, Quiet: true})
	if out.Len() != 0 {
		t.Error("quiet printed")
	}
}

func TestHooksSnippetCodex(t *testing.T) {
	h := decodeHooks(t, []byte(HooksSnippet(HarnessCodex, "/a b/pace")))
	if len(h["SessionStart"]) != 1 || h["SessionStart"][0].Hooks[0].Command != "'/a b/pace' hook codex-session-start" {
		t.Fatalf("%+v", h)
	}
	if h["PreToolUse"][0].Matcher != "Bash" || h["PreToolUse"][0].Hooks[0].Command != "'/a b/pace' hook codex-pre-tool-use" {
		t.Fatalf("%+v", h)
	}
}

// The tokenizer never panics and stays fast on arbitrary input.
func TestClassifyShellRandom(t *testing.T) {
	alphabet := []rune("ab go test npm ci'\"\\$()<>&|;#\n\t=-")
	seed := uint64(1)
	for n := 0; n < 50000; n++ {
		seed = seed*6364136223846793005 + 1442695040888963407
		l := int(seed>>58) + 1
		b := make([]rune, l)
		x := seed
		for i := range b {
			x = x*6364136223846793005 + 1442695040888963407
			b[i] = alphabet[int(x>>33)%len(alphabet)]
		}
		ClassifyShell(string(b))
	}
	big := strings.Repeat("echo 'a && b' \"c;d\" $((1<<2)) <<< x && go test ./... | tee log\n", 1500) // ~100KB
	start := time.Now()
	ClassifyShell(big)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("100KB took %v", el)
	}
}
