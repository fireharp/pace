package integrate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A hooks.json shaped like a real one: other tools' hooks, a shell
// redirection that must not come back HTML-escaped, key order to keep.
const otherHooks = `{
  "zeta": 1,
  "hooks": {
    "PostToolUse": [
      {"hooks": [{"command": "\"/opt/tool/bin/hookline\" hook codex", "timeout": 30, "type": "command"}], "matcher": "Bash|Edit"}
    ],
    "PreToolUse": [
      {"hooks": [{"type": "command", "command": "if [ -x /x/hook.sh ]; then /bin/sh /x/hook.sh; else cat >/dev/null 2>&1 || :; fi", "timeout": 10}]}
    ],
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/x/start.sh", "timeout": 10}]}
    ]
  },
  "alpha": {"nested": [1, 2, {"b": "<&>"}]}
}
`

func decodeHooks(t *testing.T, b []byte) map[string][]HookGroup {
	t.Helper()
	var doc struct {
		Hooks map[string][]HookGroup `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, b)
	}
	return doc.Hooks
}

func countPace(groups map[string][]HookGroup, ev string) (pace, other int) {
	for _, g := range groups[ev] {
		for _, h := range g.Hooks {
			if IsPaceHook(h.Command) {
				pace++
			} else {
				other++
			}
		}
	}
	return
}

func TestApplyHooksMergesAndKeepsOthers(t *testing.T) {
	out, changed, err := ApplyHooks([]byte(otherHooks), HarnessCodex, "/Users/me/.local/bin/pace", false)
	if err != nil || !changed {
		t.Fatalf("apply: changed=%v err=%v", changed, err)
	}
	s := string(out)
	if !strings.Contains(s, `>/dev/null 2>&1`) || strings.Contains(s, `\u003e`) || !strings.Contains(s, `"<&>"`) {
		t.Fatalf("other content was escaped or lost:\n%s", s)
	}
	// Key order: zeta, hooks, alpha; events keep theirs.
	if !(strings.Index(s, `"zeta"`) < strings.Index(s, `"hooks"`) && strings.Index(s, `"hooks"`) < strings.Index(s, `"alpha"`)) {
		t.Fatalf("top-level key order changed:\n%s", s)
	}
	if !(strings.Index(s, `"PostToolUse"`) < strings.Index(s, `"PreToolUse"`) && strings.Index(s, `"PreToolUse"`) < strings.Index(s, `"SessionStart"`)) {
		t.Fatalf("event order changed:\n%s", s)
	}
	h := decodeHooks(t, out)
	for ev, want := range map[string][2]int{"SessionStart": {1, 1}, "PreToolUse": {1, 1}, "PostToolUse": {0, 1}} {
		g, o := countPace(h, ev)
		if g != want[0] || o != want[1] {
			t.Errorf("%s: pace=%d other=%d, want %v", ev, g, o, want)
		}
	}
	if !strings.Contains(s, `/Users/me/.local/bin/pace hook codex-session-start`) || !strings.Contains(s, `/Users/me/.local/bin/pace hook codex-pre-tool-use`) {
		t.Fatalf("pace hooks missing:\n%s", s)
	}
	for _, g := range h["PreToolUse"] {
		for _, c := range g.Hooks {
			if IsPaceHook(c.Command) && g.Matcher != "Bash" {
				t.Errorf("pace PreToolUse matcher %q, want Bash", g.Matcher)
			}
		}
	}

	// Idempotent: a second apply changes nothing, even with a new path.
	again, changed, err := ApplyHooks(out, HarnessCodex, "/Users/me/.local/bin/pace", false)
	if err != nil || changed || !bytes.Equal(again, out) {
		t.Fatalf("second apply changed it: %v %v", changed, err)
	}
	moved, changed, err := ApplyHooks(out, HarnessCodex, "/opt/pace/bin/pace", false)
	if err != nil || !changed {
		t.Fatalf("new path: %v %v", changed, err)
	}
	if strings.Contains(string(moved), "/Users/me/.local/bin/pace") {
		t.Fatalf("old pace path kept:\n%s", moved)
	}
	if g, _ := countPace(decodeHooks(t, moved), "SessionStart"); g != 1 {
		t.Fatalf("pace SessionStart hooks after move: %d", g)
	}

	// Remove restores the other tools' content exactly (semantically).
	back, changed, err := ApplyHooks(moved, HarnessCodex, "", true)
	if err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	if !jsonEqual(back, []byte(otherHooks)) {
		t.Fatalf("remove did not restore the original:\n%s", back)
	}
}

func TestApplyHooksMixedGroupKeepsForeignHooks(t *testing.T) {
	in := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/x/other"},{"type":"command","command":"'/old path/pace' hook claude-pre-tool-use","timeout":2}]}]}}`
	out, changed, err := ApplyHooks([]byte(in), HarnessClaude, "/new/pace", true)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	h := decodeHooks(t, out)
	if g, o := countPace(h, "PreToolUse"); g != 0 || o != 1 {
		t.Fatalf("pace=%d other=%d:\n%s", g, o, out)
	}
}

func TestApplyHooksEmptyAndInvalid(t *testing.T) {
	out, changed, err := ApplyHooks(nil, HarnessClaude, "/b/pace", false)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	if g, _ := countPace(decodeHooks(t, out), "SessionStart"); g != 1 {
		t.Fatalf("no SessionStart hook:\n%s", out)
	}
	// Removing everything from a file pace created leaves no "hooks" key.
	back, _, _ := ApplyHooks(out, HarnessClaude, "", true)
	if strings.Contains(string(back), "hooks") {
		t.Fatalf("empty hooks key kept: %s", back)
	}
	for _, bad := range []string{`[1,2]`, `{"hooks": 3}`, `{"hooks": {"SessionStart": 1}}`, `{`, `{} {}`, `{"hooks": {}}}`, `{"a":1}]`, `{"a":1} x`} {
		if _, _, err := ApplyHooks([]byte(bad), HarnessClaude, "/b/pace", false); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
	// Nothing to remove: unchanged, byte-identical.
	in := []byte(`{"a":1}`)
	if out, changed, err := ApplyHooks(in, HarnessClaude, "", true); err != nil || changed || !bytes.Equal(out, in) {
		t.Fatalf("remove from file without pace hooks: %v %v %s", changed, err, out)
	}
}

func TestUpdateHooksFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "hooks.json")
	now := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	// Remove on a missing file: nothing happens.
	if r, err := UpdateHooksFile(path, HarnessCodex, "", true, now); err != nil || r.Changed {
		t.Fatalf("%+v %v", r, err)
	}
	r, err := UpdateHooksFile(path, HarnessCodex, "/b/pace", false, now)
	if err != nil || !r.Changed || r.Backup != "" {
		t.Fatalf("create: %+v %v", r, err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	os.Chmod(path, 0o640)
	before, _ := os.ReadFile(path)
	if r, err := UpdateHooksFile(path, HarnessCodex, "/b/pace", false, now); err != nil || r.Changed {
		t.Fatalf("second apply: %+v %v", r, err)
	}
	r, err = UpdateHooksFile(path, HarnessCodex, "", true, now)
	if err != nil || !r.Changed || r.Backup == "" {
		t.Fatalf("remove: %+v %v", r, err)
	}
	if b, _ := os.ReadFile(r.Backup); !bytes.Equal(b, before) {
		t.Fatal("backup differs from the previous content")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode not kept: %v", st.Mode())
	}
	found, err := HooksInstalled(path)
	if err != nil || len(found) != 0 {
		t.Fatalf("%v %v", found, err)
	}
	// An invalid file is left alone.
	os.WriteFile(path, []byte("{oops"), 0o600)
	if _, err := UpdateHooksFile(path, HarnessCodex, "/b/pace", false, now); err == nil {
		t.Fatal("want parse error")
	}
	if b, _ := os.ReadFile(path); string(b) != "{oops" {
		t.Fatal("invalid file was rewritten")
	}
	ents, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range ents {
		if strings.Contains(e.Name(), "pace-tmp") {
			t.Fatalf("temp file left: %s", e.Name())
		}
	}
}

func TestHooksFilePaths(t *testing.T) {
	env := MapEnv(map[string]string{"HOME": "/h"})
	if got := HooksFile(HarnessClaude, env); got != "/h/.claude/settings.json" {
		t.Error(got)
	}
	if got := HooksFile(HarnessCodex, env); got != "/h/.codex/hooks.json" {
		t.Error(got)
	}
	env = MapEnv(map[string]string{"HOME": "/h", "CODEX_HOME": "/c", "CLAUDE_CONFIG_DIR": "/cl"})
	if HooksFile(HarnessCodex, env) != "/c/hooks.json" || HooksFile(HarnessClaude, env) != "/cl/settings.json" {
		t.Error("env overrides ignored")
	}
}

func TestIsPaceHook(t *testing.T) {
	for _, c := range []string{"pace hook claude-session-start", "/a/b/pace hook codex-pre-tool-use", "'/a b/pace' hook claude-pre-tool-use --quiet",
		"/x/pace-dev hook claude-session-start", "/x/Pace hook codex-session-start", "/dl/pace-darwin-arm64 hook claude-pre-tool-use", `"/a b/pace2" hook codex-pre-tool-use`} {
		if !IsPaceHook(c) {
			t.Errorf("%q not recognised", c)
		}
	}
	for _, c := range []string{"hookline hook codex", "pace run -- x", "/x/govern hook claude-x", "echo pace hook codex-other",
		"pace hook claude-session-start && /my/own/thing.sh", "/x/pace hook claude-session-start; rm -rf /tmp/x", "if true; then pace hook codex-pre-tool-use; fi"} {
		if IsPaceHook(c) {
			t.Errorf("%q wrongly recognised", c)
		}
	}
}

// Review finding: a binary with another name (pace-dev, a cross-built copy)
// must still be recognised, so apply stays idempotent and remove works.
func TestApplyHooksOtherBinaryNames(t *testing.T) {
	for _, bin := range []string{"/x/pace-dev", "/x/Pace", "/dl/pace-darwin-arm64", "/a b/pace2", "/Users/me/John's tools/pace", `/x/a"b$c/pace`} {
		out, _, err := ApplyHooks([]byte(otherHooks), HarnessClaude, bin, false)
		if err != nil {
			t.Fatal(err)
		}
		again, changed, _ := ApplyHooks(out, HarnessClaude, bin, false)
		if changed || !bytes.Equal(again, out) {
			t.Errorf("%s: second apply changed the file", bin)
		}
		back, changed, _ := ApplyHooks(out, HarnessClaude, "", true)
		if !changed || !jsonEqual(back, []byte(otherHooks)) {
			t.Errorf("%s: remove did not restore:\n%s", bin, back)
		}
	}
}

// Review finding: a symlinked settings file is updated at its target and
// the link survives.
func TestUpdateHooksFileSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(target), 0o700)
	os.WriteFile(target, []byte(`{"theme":"dark"}`), 0o644)
	link := filepath.Join(dir, "claude", "settings.json")
	os.MkdirAll(filepath.Dir(link), 0o700)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r, err := UpdateHooksFile(link, HarnessClaude, "/b/pace", false, now)
	if err != nil || !r.Changed {
		t.Fatalf("%+v %v", r, err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a regular file")
	}
	b, _ := os.ReadFile(target)
	if !strings.Contains(string(b), "hook claude-session-start") || !strings.Contains(string(b), `"theme": "dark"`) {
		t.Fatalf("target not updated:\n%s", b)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o644 {
		t.Fatalf("target mode %v", st.Mode())
	}
	real, _ := filepath.EvalSymlinks(target)
	if !strings.HasPrefix(r.Backup, real+".pace-backup-") {
		t.Fatalf("backup %q not next to the target", r.Backup)
	}
	// Two changes within the same second keep both backups.
	r2, err := UpdateHooksFile(link, HarnessClaude, "", true, now)
	if err != nil || r2.Backup == r.Backup {
		t.Fatalf("backup collision: %q %q %v", r.Backup, r2.Backup, err)
	}
	if b, _ := os.ReadFile(r.Backup); string(b) != `{"theme":"dark"}` {
		t.Fatalf("first backup overwritten: %s", b)
	}
	// A dangling link is an error, nothing written.
	os.Remove(target)
	if _, err := UpdateHooksFile(link, HarnessClaude, "/b/pace", false, now); err == nil {
		t.Fatal("want error for a dangling link")
	}
}
