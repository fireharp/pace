package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
)

var (
	buildOnce sync.Once
	paceBin   string
	buildErr  error
)

// builtPace compiles this command once per test run (CGO_ENABLED=0).
func builtPace(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds a binary")
	}
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pacebin-")
		if err != nil {
			buildErr = err
			return
		}
		paceBin = filepath.Join(dir, "pace")
		cmd := exec.Command("go", "build", "-o", paceBin, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = errors.New(string(out))
		}
	})
	if buildErr != nil {
		t.Fatalf("build: %v", buildErr)
	}
	return paceBin
}

func TestMain(m *testing.M) {
	code := m.Run()
	if paceBin != "" {
		os.RemoveAll(filepath.Dir(paceBin))
	}
	os.Exit(code)
}

func TestE2EUsageErrorExitCode(t *testing.T) {
	bin := builtPace(t)
	newTestEnv(t)
	cmd := exec.Command(bin, "run", "--mem", "lots", "--", "true")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != proto.ExitPaceError {
		t.Fatalf("want exit 202, got %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if v, kv, ok := proto.ParseLine(lines[len(lines)-1]); !ok || v != "EXIT" || kv["code"] != "202" || kv["source"] != "pace" {
		t.Fatalf("last line %q", lines[len(lines)-1])
	}
}

// A real shim script -> pace shim-exec -> syscall.Exec of the real tool for a
// light command, with argv[0] preserved and the shim dir skipped.
func TestE2EShimPassThrough(t *testing.T) {
	bin := builtPace(t)
	te := newTestEnv(t)
	_ = te
	root := t.TempDir()
	shimDir := filepath.Join(root, "shims")
	realDir := filepath.Join(root, "real")
	os.MkdirAll(realDir, 0o755)
	// Fake "go": prints $0 and args and whether PACE_JOB_ID is set.
	os.WriteFile(filepath.Join(realDir, "go"), []byte("#!/bin/sh\necho \"real0=$(basename \"$0\") args=$*\"\n"), 0o755)
	if _, err := integrate.InstallShims(integrate.ShimOptions{Dir: shimDir, PaceBin: bin, Tools: []string{"go"}}); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+shimDir+":"+realDir+":/usr/bin:/bin", "PACE_SHIM_DIR="+shimDir)
	start := time.Now()
	cmd := exec.Command(filepath.Join(shimDir, "go"), "version", "a b")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("shim: %v %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "real0=go args=version a b" {
		t.Fatalf("output %q", out)
	}
	t.Logf("light command through shim: %v", time.Since(start))

	// Exit codes of the real tool pass through.
	os.WriteFile(filepath.Join(realDir, "go"), []byte("#!/bin/sh\nexit 7\n"), 0o755)
	cmd = exec.Command(filepath.Join(shimDir, "go"), "env")
	cmd.Env = env
	err = cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Fatalf("want exit 7, got %v", err)
	}
}
