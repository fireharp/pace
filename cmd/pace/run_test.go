package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/wrapper"
)

// captureRun replaces wrapper.Run for the test and returns the options seen.
func captureRun(t *testing.T, code int) *[]wrapper.Options {
	t.Helper()
	var seen []wrapper.Options
	old := wrapperRun
	wrapperRun = func(o wrapper.Options) int { seen = append(seen, o); return code }
	t.Cleanup(func() { wrapperRun = old })
	return &seen
}

func TestRunParsesFlagsIntoOptions(t *testing.T) {
	te := newTestEnv(t)
	seen := captureRun(t, 42)
	code := te.run("run", "--class", "build", "--mem", "1.5G", "--budget", "6G", "--priority", "7",
		"--timeout", "30m", "--no-wait", "--queue-timeout", "90", "--status-file", "/tmp/s.json", "-q",
		"--", "go", "test", "-v", "--", "x")
	if code != 42 {
		t.Fatalf("exit %d, want the wrapper's 42", code)
	}
	if len(*seen) != 1 {
		t.Fatalf("wrapper called %d times", len(*seen))
	}
	o := (*seen)[0]
	if o.Class != "build" || o.EstimateBytes != 1536<<20 || o.BudgetBytes != 6<<30 || o.Priority == nil || *o.Priority != 7 ||
		o.WallLimitMS != 30*60*1000 || !o.NoWait || o.QueueTimeoutMS != 90_000 || o.StatusFile != "/tmp/s.json" || !o.Quiet {
		t.Fatalf("options %+v", o)
	}
	if strings.Join(o.Argv, " ") != "go test -v -- x" {
		t.Fatalf("argv %q", o.Argv)
	}
	if o.Config.Socket != te.sock {
		t.Fatalf("socket %q want %q", o.Config.Socket, te.sock)
	}
	if o.Stdout != te.c.stdout || o.Stderr != te.c.stderr || o.Stdin != te.c.stdin {
		t.Fatal("streams not passed through")
	}
}

func TestRunDefaults(t *testing.T) {
	te := newTestEnv(t)
	seen := captureRun(t, 0)
	t.Setenv("PACE_STATUS_FILE", "/tmp/env-status.json")
	// Flags of the command after the first non-flag are the command's.
	if code := te.run("run", "npm", "test", "--mem", "9G"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	o := (*seen)[0]
	if o.Class != "" || o.EstimateBytes != 0 || o.Priority != nil || o.NoWait || o.StatusFile != "/tmp/env-status.json" {
		t.Fatalf("defaults %+v", o)
	}
	if strings.Join(o.Argv, " ") != "npm test --mem 9G" {
		t.Fatalf("argv %q", o.Argv)
	}
	// --priority 0 is a real value.
	te.run("run", "--priority", "0", "true")
	if p := (*seen)[1].Priority; p == nil || *p != 0 {
		t.Fatalf("priority 0 lost: %v", p)
	}
}

func TestRunUsageErrors(t *testing.T) {
	te := newTestEnv(t)
	seen := captureRun(t, 0)
	cases := [][]string{
		{"run"},
		{"run", "--"},
		{"run", "--mem", "lots", "--", "true"},
		{"run", "--mem", "-1G", "true"},
		{"run", "--budget", "", "true"},
		{"run", "--priority", "10", "true"},
		{"run", "--priority", "-1", "true"},
		{"run", "--priority", "x", "true"},
		{"run", "--timeout", "soon", "true"},
		{"run", "--queue-timeout", "-5s", "true"},
		{"run", "--bogus", "true"},
		{"run", "--class"},
		{"run", "--", ""},
	}
	for _, args := range cases {
		code := te.run(args...)
		if code != proto.ExitPaceError {
			t.Errorf("%v: exit %d, want 202", args, code)
			continue
		}
		lines := strings.Split(strings.TrimRight(te.err.String(), "\n"), "\n")
		last := lines[len(lines)-1]
		verb, kv, ok := proto.ParseLine(last)
		if !ok || verb != proto.VerbExit || kv["code"] != "202" || kv["source"] != "pace" || kv["msg"] == "" {
			t.Errorf("%v: last stderr line %q is not a valid EXIT line", args, last)
		}
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "pace run: ") {
			t.Errorf("%v: missing human error line: %q", args, te.err.String())
		}
		if te.out.Len() != 0 {
			t.Errorf("%v: wrote to stdout: %q", args, te.out.String())
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("wrapper ran on a usage error: %+v", *seen)
	}
	if code := te.run("run", "-h"); code != 0 || !strings.Contains(te.out.String(), "Usage: pace run") || !strings.Contains(te.out.String(), "-mem SIZE") {
		t.Fatalf("run -h: %d %q", code, te.out.String())
	}
}

func TestShimExec(t *testing.T) {
	te := newTestEnv(t)
	seen := captureRun(t, 5)
	var execs [][]string
	old := execRealHook
	execRealHook = func(path string, argv []string) int { execs = append(execs, append([]string{path}, argv...)); return 9 }
	t.Cleanup(func() { execRealHook = old })

	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "real")
	t.Setenv("PACE_SHIM_DIR", shimDir)
	if _, err := integrate.InstallShims(integrate.ShimOptions{Dir: shimDir, PaceBin: te.c.exe, Tools: []string{"go", "npm"}}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"go", "npm"} {
		os.MkdirAll(realDir, 0o755)
		os.WriteFile(filepath.Join(realDir, n), []byte("#!/bin/sh\n"), 0o755)
	}
	t.Setenv("PATH", shimDir+":"+realDir)
	realGo := filepath.Join(realDir, "go")

	// Light command: exec the real binary with argv[0] = name.
	if code := te.run("shim-exec", "go", "version"); code != 9 {
		t.Fatalf("light: exit %d", code)
	}
	if strings.Join(execs[0], " ") != realGo+" go version" {
		t.Fatalf("light exec %q", execs[0])
	}
	// Heavy command: pace run --class test -- <real> args.
	if code := te.run("shim-exec", "go", "test", "./..."); code != 5 {
		t.Fatalf("heavy: exit %d", code)
	}
	o := (*seen)[0]
	if o.Class != "test" || strings.Join(o.Argv, " ") != realGo+" test ./..." {
		t.Fatalf("heavy options %+v", o)
	}
	// Inside a governed job: always pass through.
	t.Setenv("PACE_JOB_ID", "j1")
	te.run("shim-exec", "go", "build")
	t.Setenv("PACE_JOB_ID", "")
	t.Setenv("PACE_SHIM_DISABLE", "1")
	te.run("shim-exec", "npm", "install")
	t.Setenv("PACE_SHIM_DISABLE", "")
	if len(execs) != 3 || len(*seen) != 1 {
		t.Fatalf("pass-through: execs=%v runs=%d", execs, len(*seen))
	}
	// Only shims on PATH: not found -> 127.
	t.Setenv("PATH", shimDir)
	if code := te.run("shim-exec", "go", "build"); code != 127 || !strings.Contains(te.err.String(), "command not found") {
		t.Fatalf("not found: %d %q", code, te.err.String())
	}
	if code := te.run("shim-exec"); code != exitUsage {
		t.Fatalf("no name: %d", code)
	}
}
