package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/integrate"
)

// oldGovInstall lays out what gov left in HOME: its service file (mode
// observe), config.toml and fleet.toml, a shim and Claude Code hooks.
func oldGovInstall(t *testing.T, te *testEnv) (service, settings string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	write := func(p, s string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	service = integrate.LegacyServiceFile(runtime.GOOS, te.home, nil)
	if runtime.GOOS == "darwin" {
		write(service, `<plist><dict><key>Label</key><string>com.gov.daemon</string>
<key>EnvironmentVariables</key><dict><key>GOV_MODE</key>
		<string>observe</string></dict></dict></plist>`, 0o644)
	} else {
		write(service, "[Service]\nExecStart=/usr/local/bin/gov daemon\nEnvironment=GOV_MODE=observe\n", 0o644)
	}
	cfgDir := filepath.Join(te.home, ".config", "gov")
	write(filepath.Join(cfgDir, "config.toml"), "# my gov config\n", 0o600)
	write(filepath.Join(cfgDir, "fleet.toml"), "# my fleet\n", 0o644)
	write(filepath.Join(te.home, ".gov", "shims", "go"), "#!/bin/sh\n"+integrate.LegacyShimMarker+": routes heavy 'go' commands through 'gov run'.\nexec /usr/local/bin/gov shim-exec go \"$@\"\n", 0o755)
	settings = filepath.Join(te.home, ".claude", "settings.json")
	write(settings, `{
  "model": "opus",
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "/usr/local/bin/gov hook claude-session-start --quiet"}]}],
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/gov hook claude-pre-tool-use"}]},
      {"matcher": "Edit", "hooks": [{"type": "command", "command": "my-own-hook"}]}
    ]
  }
}
`, 0o644)
	return service, settings
}

func TestMigratePlanChangesNothing(t *testing.T) {
	te := newTestEnv(t)
	service, settings := oldGovInstall(t, te)
	before, _ := os.ReadFile(settings)
	if code := te.run("migrate"); code != 0 {
		t.Fatalf("migrate: %d\n%s%s", code, te.out.String(), te.err.String())
	}
	o := te.out.String()
	for _, want := range []string{"plan only", "mode observe", "would copy", "would remove", "would replace the hooks", "--mode", "would write"} {
		if !strings.Contains(o, want) && !(want == "--mode" && strings.Contains(o, "PACE_MODE")) {
			t.Errorf("plan missing %q:\n%s", want, o)
		}
	}
	if _, err := os.Stat(service); err != nil {
		t.Errorf("plan removed the old service file: %v", err)
	}
	if after, _ := os.ReadFile(settings); string(after) != string(before) {
		t.Errorf("plan changed %s", settings)
	}
	if _, err := os.Stat(filepath.Join(te.home, ".config", "pace")); err == nil {
		t.Errorf("plan created ~/.config/pace")
	}
}

func TestMigrateApply(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("service install supported on darwin/linux only")
	}
	te := newTestEnv(t)
	service, settings := oldGovInstall(t, te)
	rr := &recRunner{}
	newRunner = func() integrate.Runner { return rr }
	startDaemon(t, te.sock, richStatus(), nil) // the new service's health check
	if code := te.run("migrate", "--apply"); code != 0 {
		t.Fatalf("migrate --apply: %d\n%s%s", code, te.out.String(), te.err.String())
	}
	if _, err := os.Stat(service); !os.IsNotExist(err) {
		t.Errorf("old service file still there: %v", err)
	}
	if runtime.GOOS == "linux" && !strings.Contains(strings.Join(rr.calls, "\n"), "disable --now gov.service") {
		t.Errorf("gov.service not disabled: %v", rr.calls)
	}
	// config copied with its permissions
	fi, err := os.Stat(filepath.Join(te.home, ".config", "pace", "config.toml"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config.toml not copied (0600): %v %v", fi, err)
	}
	if b, _ := os.ReadFile(filepath.Join(te.home, ".config", "pace", "fleet.toml")); string(b) != "# my fleet\n" {
		t.Errorf("fleet.toml = %q", b)
	}
	// shims moved: gov's gone, pace's for the same tool written
	if _, err := os.Stat(filepath.Join(te.home, ".gov")); !os.IsNotExist(err) {
		t.Errorf("~/.gov left behind: %v", err)
	}
	shim, err := os.ReadFile(filepath.Join(te.paceHome, "shims", "go"))
	if err != nil || !strings.Contains(string(shim), integrate.ShimMarker) || !strings.Contains(string(shim), "/opt/pace/bin/pace") {
		t.Errorf("pace shim for go: %v\n%s", err, shim)
	}
	// hooks replaced, the user's own hook kept
	b, _ := os.ReadFile(settings)
	s := string(b)
	if strings.Contains(s, "/usr/local/bin/gov") || !strings.Contains(s, "/opt/pace/bin/pace hook claude-session-start") ||
		!strings.Contains(s, "my-own-hook") || !strings.Contains(s, `"model": "opus"`) {
		t.Errorf("settings after migrate:\n%s", s)
	}
	// new service installed with the old mode
	var file string
	if runtime.GOOS == "darwin" {
		file = integrate.PlistPath(te.home)
	} else {
		file = integrate.UnitPath(te.home, nil)
	}
	if b, err := os.ReadFile(file); err != nil || !strings.Contains(string(b), "observe") || !strings.Contains(string(b), "/opt/pace/bin/pace") {
		t.Errorf("new service file: %v\n%s", err, b)
	}
	// running it again is harmless
	if code := te.run("migrate", "--apply"); code != 0 {
		t.Fatalf("second migrate --apply: %d\n%s%s", code, te.out.String(), te.err.String())
	}
	if !strings.Contains(te.out.String(), "no gov service file") {
		t.Errorf("second run output:\n%s", te.out.String())
	}
}

func TestServiceFileEnv(t *testing.T) {
	dir := t.TempDir()
	unit := filepath.Join(dir, "gov.service")
	os.WriteFile(unit, []byte("[Service]\nEnvironment=GOV_MODE=enforce\nEnvironment=\"GOV_CONFIG=/a b/c.toml\"\nEnvironment=GOMEMLIMIT=32MiB\n"), 0o644)
	got := integrate.ServiceFileEnv(unit)
	if got["MODE"] != "enforce" || got["CONFIG"] != "/a b/c.toml" || len(got) != 2 {
		t.Errorf("unit env = %v", got)
	}
	plist := filepath.Join(dir, "com.gov.daemon.plist")
	os.WriteFile(plist, []byte("<key>GOV_SOCK</key>\n\t\t<string>/x/R&amp;D/g.sock</string>"), 0o644)
	if got := integrate.ServiceFileEnv(plist); got["SOCK"] != "/x/R&D/g.sock" {
		t.Errorf("plist env = %v", got)
	}
	if got := integrate.ServiceFileEnv(filepath.Join(dir, "missing")); len(got) != 0 {
		t.Errorf("missing file env = %v", got)
	}
}
