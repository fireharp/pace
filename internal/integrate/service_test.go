package integrate

import (
	"bytes"
	"encoding/xml"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update)", p, err)
	}
	if string(want) != got {
		t.Fatalf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func spec() ServiceSpec {
	return ServiceSpec{
		PaceBin: "/Users/me/bin/pace",
		Mode:    "observe",
		LogDir:  "/Users/me/Library/Logs/pace",
		Env:     map[string]string{"PACE_SOCK": "/Users/me/g.sock", "PACE_HOME": ""},
	}
}

func TestLaunchdPlistGolden(t *testing.T) {
	golden(t, "com.pace.daemon.plist", LaunchdPlist(spec()))
	// No mode: PACE_MODE absent.
	s := spec()
	s.Mode = ""
	s.Env = nil
	golden(t, "com.pace.daemon.nomode.plist", LaunchdPlist(s))
}

func TestLaunchdPlistWellFormedAndEscaped(t *testing.T) {
	s := spec()
	s.PaceBin = "/Users/me/A & B <x>/pace"
	p := LaunchdPlist(s)
	dec := xml.NewDecoder(strings.NewReader(p))
	dec.Strict = true
	// The DOCTYPE is fine for encoding/xml; walk all tokens.
	var texts []string
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("plist not well-formed: %v", err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			texts = append(texts, string(cd))
		}
	}
	found := false
	for _, x := range texts {
		if x == s.PaceBin {
			found = true
		}
	}
	if !found {
		t.Fatalf("escaped path did not round-trip: %v", texts)
	}
	for _, want := range []string{"<key>KeepAlive</key>\n\t<true/>", "<key>ThrottleInterval</key>\n\t<integer>1</integer>",
		"<string>Interactive</string>", "<integer>-5</integer>", "<key>ExitTimeOut</key>\n\t<integer>5</integer>",
		"<key>GOMEMLIMIT</key>\n\t\t<string>32MiB</string>", "<key>PACE_MODE</key>\n\t\t<string>observe</string>",
		"/Users/me/Library/Logs/pace/daemon.log"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist missing %q", want)
		}
	}
}

func TestSystemdUnitGolden(t *testing.T) {
	golden(t, "pace.service", SystemdUnit(spec()))
	s := spec()
	s.PaceBin = "/home/me/my bin/pace%1$x"
	s.Mode = ""
	s.GoMemLimit = "16MiB"
	golden(t, "pace.quoted.service", SystemdUnit(s))
}

func TestSdQuote(t *testing.T) {
	cases := map[string]string{
		"/bin/pace":    "/bin/pace",
		"/my bin/pace": `"/my bin/pace"`,
		"a%b":          "a%%b",
		"$HOME":        "$$HOME",
		`a"b`:          `"a\"b"`,
		`a\b`:          `"a\\b"`,
		"K=V":          "K=V",
		"K=with space": `"K=with space"`,
		"":             `""`,
	}
	for in, want := range cases {
		if got := sdQuote(in); got != want {
			t.Errorf("sdQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

// fakeRunner records commands and answers them from a script.
type fakeRunner struct {
	calls  []string
	answer func(cmd string) ([]byte, error)
}

func (f *fakeRunner) Run(name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, cmd)
	if f.answer == nil {
		return nil, nil
	}
	return f.answer(cmd)
}

func (f *fakeRunner) mutating() []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "launchctl print") || strings.Contains(c, "is-active") || strings.Contains(c, " show ") {
			continue
		}
		out = append(out, c)
	}
	return out
}

var errExit = errors.New("exit status 113")

func newInstaller(t *testing.T, goos string, r Runner) (*Installer, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	home := t.TempDir()
	return &Installer{GOOS: goos, UID: 501, Home: home, Env: MapEnv(map[string]string{"HOME": home}), Runner: r, Out: &out, Sleep: func(time.Duration) {}}, &out
}

func equalCalls(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestInstallLaunchdFresh(t *testing.T) {
	r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "launchctl print") {
			return []byte("Could not find service"), errExit
		}
		return nil, nil
	}}
	inst, _ := newInstaller(t, "darwin", r)
	healthy := 0
	inst.Health = func() error { healthy++; return nil }
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "Library", "Logs", "pace")
	if err := inst.Install(s); err != nil {
		t.Fatal(err)
	}
	plist := PlistPath(inst.Home)
	equalCalls(t, r.mutating(), []string{"launchctl bootstrap gui/501 " + plist})
	b, err := os.ReadFile(plist)
	if err != nil || string(b) != LaunchdPlist(s) {
		t.Fatalf("plist not written: %v", err)
	}
	fi, _ := os.Stat(plist)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("plist mode %v", fi.Mode())
	}
	if _, err := os.Stat(s.LogDir); err != nil {
		t.Fatal("log dir not created")
	}
	if healthy != 1 {
		t.Fatalf("health polled %d times", healthy)
	}
}

func TestInstallLaunchdUpgradeSameContentKickstarts(t *testing.T) {
	r := &fakeRunner{} // everything succeeds: service is loaded
	inst, _ := newInstaller(t, "darwin", r)
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "logs")
	plist := PlistPath(inst.Home)
	writeExe(t, plist, LaunchdPlist(s))
	if err := inst.Install(s); err != nil {
		t.Fatal(err)
	}
	equalCalls(t, r.mutating(), []string{"launchctl kickstart -k gui/501/com.pace.daemon"})
}

func TestInstallLaunchdChangedContentReloads(t *testing.T) {
	printed := 0
	r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "launchctl print") {
			printed++
			if printed <= 2 { // loaded at first check and on the first poll after bootout
				return []byte("\tstate = running\n"), nil
			}
			return nil, errExit
		}
		return nil, nil
	}}
	inst, _ := newInstaller(t, "darwin", r)
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "logs")
	plist := PlistPath(inst.Home)
	writeExe(t, plist, "old content")
	if err := inst.Install(s); err != nil {
		t.Fatal(err)
	}
	equalCalls(t, r.mutating(), []string{
		"launchctl bootout gui/501/com.pace.daemon",
		"launchctl bootstrap gui/501 " + plist,
	})
	if b, _ := os.ReadFile(plist); string(b) != LaunchdPlist(s) {
		t.Fatal("plist not rewritten")
	}
}

func TestInstallLaunchdBootstrapRetry(t *testing.T) {
	boots := 0
	r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		switch {
		case strings.HasPrefix(cmd, "launchctl print"):
			return nil, errExit
		case strings.HasPrefix(cmd, "launchctl bootstrap"):
			boots++
			if boots == 1 {
				return []byte("Bootstrap failed: 5: Input/output error"), errExit
			}
		}
		return nil, nil
	}}
	inst, _ := newInstaller(t, "darwin", r)
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "logs")
	if err := inst.Install(s); err != nil {
		t.Fatal(err)
	}
	if boots != 2 {
		t.Fatalf("bootstrap attempts %d", boots)
	}
	// Both attempts fail -> error mentions the output.
	r2 := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "launchctl print") || strings.HasPrefix(cmd, "launchctl bootstrap") {
			return []byte("Bootstrap failed: 5"), errExit
		}
		return nil, nil
	}}
	inst2, _ := newInstaller(t, "darwin", r2)
	if err := inst2.Install(s); err == nil || !strings.Contains(err.Error(), "Bootstrap failed") {
		t.Fatalf("want bootstrap error, got %v", err)
	}
}

func TestInstallLaunchdBootoutNeverUnloads(t *testing.T) {
	r := &fakeRunner{} // print always succeeds: never unloads
	inst, _ := newInstaller(t, "darwin", r)
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "logs")
	writeExe(t, PlistPath(inst.Home), "old")
	if err := inst.Install(s); err == nil || !strings.Contains(err.Error(), "still loaded") {
		t.Fatalf("want still-loaded error, got %v", err)
	}
}

func TestInstallHealthTimeout(t *testing.T) {
	r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		if strings.HasPrefix(cmd, "launchctl print") {
			return nil, errExit
		}
		return nil, nil
	}}
	inst, _ := newInstaller(t, "darwin", r)
	inst.HealthWait = 300 * time.Millisecond
	polls := 0
	inst.Health = func() error { polls++; return errors.New("connection refused") }
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "logs")
	err := inst.Install(s)
	if err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("want health error, got %v", err)
	}
	if polls < 3 {
		t.Fatalf("polled %d times", polls)
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
				if strings.HasPrefix(cmd, "launchctl print") || strings.Contains(cmd, "is-active") {
					return nil, errExit
				}
				return nil, nil
			}}
			inst, out := newInstaller(t, goos, r)
			inst.DryRun = true
			inst.Health = func() error { t.Fatal("health polled on dry run"); return nil }
			s := spec()
			s.LogDir = filepath.Join(inst.Home, "logs")
			if err := inst.Install(s); err != nil {
				t.Fatal(err)
			}
			if len(r.mutating()) != 0 {
				t.Fatalf("dry run executed %v", r.mutating())
			}
			ents, _ := os.ReadDir(inst.Home)
			if len(ents) != 0 {
				t.Fatalf("dry run wrote files: %v", ents)
			}
			o := out.String()
			if !strings.Contains(o, "would write") || !strings.Contains(o, "would run:") {
				t.Fatalf("dry run output: %s", o)
			}
			if err := inst.Uninstall(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInstallValidation(t *testing.T) {
	inst, _ := newInstaller(t, "darwin", &fakeRunner{})
	s := spec()
	s.Mode = "bogus"
	if err := inst.Install(s); err == nil {
		t.Error("bad mode accepted")
	}
	s = spec()
	s.PaceBin = "pace"
	if err := inst.Install(s); err == nil {
		t.Error("relative binary accepted")
	}
	inst.GOOS = "plan9"
	if err := inst.Install(spec()); err == nil {
		t.Error("unsupported OS accepted")
	}
	if err := inst.Uninstall(); err == nil {
		t.Error("unsupported OS uninstall accepted")
	}
	if _, err := inst.Status(); err == nil {
		t.Error("unsupported OS status accepted")
	}
}

func TestUninstallLaunchd(t *testing.T) {
	r := &fakeRunner{}
	inst, out := newInstaller(t, "darwin", r)
	plist := PlistPath(inst.Home)
	writeExe(t, plist, "x")
	if err := inst.Uninstall(); err != nil {
		t.Fatal(err)
	}
	equalCalls(t, r.mutating(), []string{"launchctl bootout gui/501/com.pace.daemon"})
	if fileExists(plist) {
		t.Fatal("plist not removed")
	}
	// Not installed at all.
	r2 := &fakeRunner{answer: func(string) ([]byte, error) { return nil, errExit }}
	inst2, out2 := newInstaller(t, "darwin", r2)
	if err := inst2.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out2.String(), "not installed") || len(r2.mutating()) != 0 {
		t.Fatalf("out %q calls %v", out2.String(), r2.calls)
	}
	_ = out
}

func TestInstallSystemd(t *testing.T) {
	active := false
	r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		if strings.Contains(cmd, "is-active") && !active {
			return nil, errExit
		}
		return nil, nil
	}}
	inst, _ := newInstaller(t, "linux", r)
	inst.Env = MapEnv(map[string]string{"HOME": inst.Home, "XDG_CONFIG_HOME": filepath.Join(inst.Home, "cfg")})
	s := spec()
	s.LogDir = filepath.Join(inst.Home, "logs")
	if err := inst.Install(s); err != nil {
		t.Fatal(err)
	}
	unit := filepath.Join(inst.Home, "cfg", "systemd", "user", "pace.service")
	if b, err := os.ReadFile(unit); err != nil || string(b) != SystemdUnit(s) {
		t.Fatalf("unit not written: %v", err)
	}
	equalCalls(t, r.mutating(), []string{"systemctl --user daemon-reload", "systemctl --user enable --now pace.service"})

	// Upgrade while active: restart.
	active = true
	r.calls = nil
	if err := inst.Install(s); err != nil {
		t.Fatal(err)
	}
	equalCalls(t, r.mutating(), []string{"systemctl --user daemon-reload", "systemctl --user enable --now pace.service", "systemctl --user restart pace.service"})

	// Uninstall.
	r.calls = nil
	if err := inst.Uninstall(); err != nil {
		t.Fatal(err)
	}
	equalCalls(t, r.mutating(), []string{"systemctl --user disable --now pace.service", "systemctl --user daemon-reload"})
	if fileExists(unit) {
		t.Fatal("unit not removed")
	}
	r.calls = nil
	if err := inst.Uninstall(); err != nil || len(r.calls) != 0 {
		t.Fatalf("second uninstall: %v %v", err, r.calls)
	}
}

func TestStatus(t *testing.T) {
	r := &fakeRunner{answer: func(cmd string) ([]byte, error) {
		return []byte("gui/501/com.pace.daemon = {\n\tactive count = 1\n\tstate = running\n\tpid = 4242\n\tendpoints = {\n\t\tstate = idle\n\t}\n}\n"), nil
	}}
	inst, _ := newInstaller(t, "darwin", r)
	st, err := inst.Status()
	if err != nil || !st.Loaded || !st.Running || st.PID != 4242 || st.Installed {
		t.Fatalf("status %+v %v", st, err)
	}
	r.answer = func(string) ([]byte, error) { return nil, errExit }
	st, _ = inst.Status()
	if st.Loaded || st.Running {
		t.Fatalf("unloaded status %+v", st)
	}

	rl := &fakeRunner{answer: func(string) ([]byte, error) {
		return []byte("LoadState=loaded\nActiveState=active\nMainPID=77\n"), nil
	}}
	il, _ := newInstaller(t, "linux", rl)
	st, err = il.Status()
	if err != nil || !st.Loaded || !st.Running || st.PID != 77 {
		t.Fatalf("linux status %+v %v", st, err)
	}
}

func TestParseLaunchctlPrint(t *testing.T) {
	running, pid, state := parseLaunchctlPrint("x = {\n\tstate = not running\n\tlast exit code = 1\n}\n")
	if running || pid != 0 || state != "not running" {
		t.Fatalf("%v %d %q", running, pid, state)
	}
}

func TestCheckBinaryPath(t *testing.T) {
	if _, err := CheckBinaryPath("pace"); err == nil {
		t.Error("relative accepted")
	}
	if _, err := CheckBinaryPath("/private/var/folders/x/go-build123/b001/exe/pace"); err == nil {
		t.Error("go run binary accepted")
	}
	if w, err := CheckBinaryPath("/tmp/pace"); err != nil || len(w) != 1 {
		t.Errorf("tmp: %v %v", w, err)
	}
	if w, err := CheckBinaryPath("/Users/me/bin/pace"); err != nil || len(w) != 0 {
		t.Errorf("normal: %v %v", w, err)
	}
}
