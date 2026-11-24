package integrate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeExe(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"go":            "go",
		"/usr/bin/go":   "/usr/bin/go",
		"":              "''",
		"a b":           "'a b'",
		"it's":          `'it'\''s'`,
		"$HOME":         "'$HOME'",
		"/A S/pace":     "'/A S/pace'",
		"x;rm -rf /":    "'x;rm -rf /'",
		"back`tick`":    "'back`tick`'",
		"quote\"dq":     `'quote"dq'`,
		"semi:colon@1%": "semi:colon@1%",
	}
	for in, want := range cases {
		if got := ShellQuote(in); got != want {
			t.Errorf("ShellQuote(%q) = %s, want %s", in, got, want)
		}
		// Round trip through sh.
		out, err := exec.Command("/bin/sh", "-c", "printf %s "+ShellQuote(in)).Output()
		if err != nil {
			t.Fatalf("sh: %v", err)
		}
		if string(out) != in {
			t.Errorf("sh round trip of %q gave %q", in, out)
		}
	}
}

func TestValidToolName(t *testing.T) {
	for _, ok := range []string{"go", "python3", "python3.12", "g++", "x_y-z"} {
		if !ValidToolName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", "a b", "a'b", "$x", strings.Repeat("a", 65)} {
		if ValidToolName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}

func TestShimScriptExecsPace(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims dir") // a space, to test quoting
	paceBin := filepath.Join(root, "bin with space", "pace")
	// Fake pace prints its argv, one per line.
	writeExe(t, paceBin, "#!/bin/sh\nfor a in \"$@\"; do printf '[%s]\\n' \"$a\"; done\n")
	shim := filepath.Join(shimDir, "go")
	writeExe(t, shim, ShimScript(paceBin, "go", shimDir))
	if !IsShim(shim) {
		t.Fatal("generated script not recognised as shim")
	}
	cmd := exec.Command(shim, "build", "a b", "it's", "")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shim: %v", err)
	}
	want := "[shim-exec]\n[go]\n[build]\n[a b]\n[it's]\n[]\n"
	if string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
}

func TestShimScriptFallbackWhenPaceMissing(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "real")
	writeExe(t, filepath.Join(realDir, "go"), "#!/bin/sh\necho \"real $*\"\necho \"PATH=$PATH\"\n")
	shim := filepath.Join(shimDir, "go")
	writeExe(t, shim, ShimScript(filepath.Join(root, "missing", "pace"), "go", shimDir))
	cmd := exec.Command(shim, "version")
	cmd.Env = []string{"PATH=" + shimDir + ":" + shimDir + "/:" + realDir + ":/usr/bin:/bin"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shim fallback: %v (stderr %s)", err, stderr.String())
	}
	s := string(out)
	if !strings.Contains(s, "real version") {
		t.Fatalf("real tool not run: %q", s)
	}
	if strings.Contains(s, shimDir) {
		t.Fatalf("shim dir still in PATH: %q", s)
	}
	if !strings.Contains(s, "PATH="+realDir+":/usr/bin:/bin") {
		t.Fatalf("PATH not preserved: %q", s)
	}
	if !strings.HasPrefix(stderr.String(), "pace: DEGRADED reason=pace_missing ") {
		t.Fatalf("missing DEGRADED line: %q", stderr.String())
	}
}

// Found by scripts/accept/m4.sh: with the shim directory on PATH spelled
// non-canonically (TMPDIR ending in "/" gives ".../T//x/shims"), the
// pace-missing fallback did not drop it and exec'd itself forever.
func TestShimScriptFallbackNonCanonicalPathAndLoopGuard(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "real")
	writeExe(t, filepath.Join(realDir, "go"), "#!/bin/sh\necho \"real $*\"\n")
	shim := filepath.Join(shimDir, "go")
	writeExe(t, shim, ShimScript(filepath.Join(root, "missing", "pace"), "go", shimDir))
	odd := strings.Replace(shimDir, root, root+"//./", 1)

	run := func(path string) (string, string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, shim, "version")
		cmd.Env = []string{"PATH=" + path}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if ctx.Err() != nil {
			t.Fatalf("shim fallback did not terminate (PATH=%s): %s", path, stderr.String())
		}
		return string(out), stderr.String(), err
	}
	out, _, err := run(odd + ":" + realDir + ":/usr/bin:/bin")
	if err != nil || !strings.Contains(out, "real version") {
		t.Fatalf("non-canonical PATH entry: out %q err %v", out, err)
	}
	// No real tool anywhere: 127 after one extra hop at most, not a loop.
	// (A second spelling that cd cannot resolve is not dropped; the guard stops it.)
	writeExe(t, filepath.Join(root, "shims2", "go"), ShimScript(filepath.Join(root, "missing", "pace"), "go", filepath.Join(root, "elsewhere")))
	_, stderr, err := run(odd + ":" + filepath.Join(root, "shims2") + ":/usr/bin:/bin")
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 127 || !strings.Contains(stderr, "real tool not found") {
		t.Fatalf("want exit 127 with a reason, got %v: %s", err, stderr)
	}
}

func TestInstallShims(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shims")
	pace := filepath.Join(root, "pace")
	writeExe(t, pace, "#!/bin/sh\n")

	// Dry run writes nothing.
	var buf bytes.Buffer
	paths, err := InstallShims(ShimOptions{Dir: dir, PaceBin: pace, Tools: []string{"go", "npm"}, DryRun: true, Out: &buf})
	if err != nil || len(paths) != 2 {
		t.Fatalf("dry run: %v %v", paths, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created the directory")
	}
	if !strings.Contains(buf.String(), "would write") {
		t.Fatalf("dry run output: %q", buf.String())
	}

	// Default tool list.
	paths, err = InstallShims(ShimOptions{Dir: dir, PaceBin: pace})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(HeavyTools()) {
		t.Fatalf("wrote %d shims, want %d", len(paths), len(HeavyTools()))
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o755 {
			t.Errorf("%s mode %v", p, fi.Mode())
		}
		if !IsShim(p) {
			t.Errorf("%s not a shim", p)
		}
	}
	// No temp files left behind.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}

	// Re-install replaces our shims; a foreign file is left alone and reported.
	foreign := filepath.Join(dir, "make")
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	writeExe(t, foreign, "#!/bin/sh\necho mine\n")
	_, err = InstallShims(ShimOptions{Dir: dir, PaceBin: pace, Tools: []string{"go", "make"}})
	if err == nil || !strings.Contains(err.Error(), "not a pace shim") {
		t.Fatalf("want foreign-file error, got %v", err)
	}
	if b, _ := os.ReadFile(foreign); string(b) != "#!/bin/sh\necho mine\n" {
		t.Fatal("foreign file was modified")
	}

	// Invalid inputs.
	if _, err := InstallShims(ShimOptions{Dir: dir, PaceBin: pace, Tools: []string{"a/b"}}); err == nil {
		t.Error("invalid tool accepted")
	}
	if _, err := InstallShims(ShimOptions{Dir: dir, PaceBin: pace, Tools: []string{"pace"}}); err == nil {
		t.Error("shimming pace accepted")
	}
	if _, err := InstallShims(ShimOptions{Dir: dir, PaceBin: "pace"}); err == nil {
		t.Error("relative pace path accepted")
	}
	if _, err := InstallShims(ShimOptions{PaceBin: pace}); err == nil {
		t.Error("empty dir accepted")
	}
}

func TestUninstallShims(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shims")
	pace := filepath.Join(root, "pace")
	if _, err := InstallShims(ShimOptions{Dir: dir, PaceBin: pace, Tools: []string{"go", "npm"}}); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "mine")
	writeExe(t, foreign, "#!/bin/sh\n")

	removed, err := UninstallShims(dir, true, nil)
	if err != nil || len(removed) != 2 {
		t.Fatalf("dry run: %v %v", removed, err)
	}
	if !IsShim(filepath.Join(dir, "go")) {
		t.Fatal("dry run removed a shim")
	}
	removed, err = UninstallShims(dir, false, nil)
	if err != nil || len(removed) != 2 {
		t.Fatalf("uninstall: %v %v", removed, err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("foreign file removed")
	}
	// Dir kept because a foreign file remains; remove it and uninstall again.
	os.Remove(foreign)
	if _, err := UninstallShims(dir, false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty shim dir not removed")
	}
	// Missing dir is fine.
	if r, err := UninstallShims(filepath.Join(root, "nope"), false, nil); err != nil || r != nil {
		t.Fatalf("missing dir: %v %v", r, err)
	}
}

func TestLookPathExcluding(t *testing.T) {
	root := t.TempDir()
	shims := filepath.Join(root, "shims")
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	c := filepath.Join(root, "c")
	writeExe(t, filepath.Join(shims, "go"), ShimScript("/x/pace", "go", shims))
	writeExe(t, filepath.Join(a, "go"), ShimScript("/x/pace", "go", a)) // a second shim dir
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "go"), []byte("not exec"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(c, "go"), "#!/bin/sh\n")
	if err := os.Mkdir(filepath.Join(c, "dirname"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shimlink")
	if err := os.Symlink(shims, link); err != nil {
		t.Fatal(err)
	}

	path := strings.Join([]string{"", ".", "rel", link, shims, a, b, c}, string(filepath.ListSeparator))
	got, err := LookPathExcluding("go", path, []string{shims}, "")
	if err != nil || got != filepath.Join(c, "go") {
		t.Fatalf("got %q %v, want c/go", got, err)
	}
	// self is skipped.
	if _, err := LookPathExcluding("go", c, nil, filepath.Join(c, "go")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("self not skipped: %v", err)
	}
	// Directories are not executables.
	if _, err := LookPathExcluding("dirname", c, nil, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dir matched: %v", err)
	}
	for _, bad := range []string{"", "a/go"} {
		if _, err := LookPathExcluding(bad, c, nil, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := LookPathExcluding("nope", path, nil, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestListShimsAndPathIndex(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shims")
	real := filepath.Join(root, "real")
	writeExe(t, filepath.Join(real, "go"), "#!/bin/sh\n")
	if _, err := InstallShims(ShimOptions{Dir: dir, PaceBin: "/x/pace", Tools: []string{"npm", "go"}}); err != nil {
		t.Fatal(err)
	}
	writeExe(t, filepath.Join(dir, "other"), "#!/bin/sh\n")
	path := dir + ":" + real
	list, err := ListShims(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "go" || list[1].Name != "npm" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Real != filepath.Join(real, "go") || list[1].Real != "" {
		t.Fatalf("real = %+v", list)
	}
	if l, err := ListShims(filepath.Join(root, "none"), path); err != nil || l != nil {
		t.Fatalf("missing dir: %v %v", l, err)
	}
	if PathIndex(path, dir+"/") != 0 || PathIndex(path, real) != 1 || PathIndex(path, "/x") != -1 {
		t.Fatal("PathIndex")
	}
}

func TestDefaultPaths(t *testing.T) {
	env := MapEnv(map[string]string{"HOME": "/h"})
	if got := DefaultShimDir(env); got != "/h/.pace/shims" {
		t.Errorf("shim dir %q", got)
	}
	if got := DefaultShimDir(MapEnv(map[string]string{"HOME": "/h", "PACE_HOME": "/g"})); got != "/g/shims" {
		t.Errorf("PACE_HOME shim dir %q", got)
	}
	if got := DefaultShimDir(MapEnv(map[string]string{"PACE_SHIM_DIR": "/s/", "PACE_HOME": "/g"})); got != "/s" {
		t.Errorf("PACE_SHIM_DIR %q", got)
	}
	if got := DefaultSocket(env, "darwin"); got != "/h/Library/Application Support/pace/run/pace.sock" {
		t.Errorf("darwin socket %q", got)
	}
	if got := DefaultSocket(MapEnv(map[string]string{"HOME": "/h", "XDG_RUNTIME_DIR": "/run/user/1"}), "linux"); got != "/run/user/1/pace/pace.sock" {
		t.Errorf("linux socket %q", got)
	}
	if got := DefaultSocket(env, "linux"); got != "/h/.local/state/pace/run/pace.sock" {
		t.Errorf("linux fallback socket %q", got)
	}
	if got := DefaultSocket(MapEnv(map[string]string{"PACE_SOCK": "/s.sock", "PACE_HOME": "/g"}), "darwin"); got != "/s.sock" {
		t.Errorf("PACE_SOCK %q", got)
	}
	if got := DefaultSocket(MapEnv(map[string]string{"PACE_HOME": "/g"}), "darwin"); got != "/g/run/pace.sock" {
		t.Errorf("PACE_HOME socket %q", got)
	}
	if got := SocketPath("/c.sock", env); got != "/c.sock" {
		t.Errorf("configured socket %q", got)
	}
	if got := DefaultLogDir(env, "darwin"); got != "/h/Library/Logs/pace" {
		t.Errorf("log dir %q", got)
	}
	if got := DefaultLogDir(env, "linux"); got != "/h/.local/state/pace" {
		t.Errorf("linux log dir %q", got)
	}
}
