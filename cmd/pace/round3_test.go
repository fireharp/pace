package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// Round 3: a usage or config error (exit 202) still writes the status file
// (--status-file when it was parsed, else $PACE_STATUS_FILE), with outcome
// error and exit_code 202, before the EXIT line.
func TestUsageErrorWritesStatusFile(t *testing.T) {
	te := newTestEnv(t)
	captureRun(t, 0)
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.toml")
	os.WriteFile(bad, []byte("nonsense = = 1\n"), 0o644)
	check := func(name, path string, args ...string) {
		t.Helper()
		os.Remove(path)
		if code := te.run(args...); code != proto.ExitPaceError {
			t.Fatalf("%s: exit %d", name, code)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: no status file: %v", name, err)
		}
		var sf map[string]any
		if err := json.Unmarshal(raw, &sf); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sf["outcome"] != "error" || sf["exit_code"] != float64(202) || sf["source"] != "pace" || sf["state"] != "done" || sf["error"] == "" {
			t.Fatalf("%s: status file %s", name, raw)
		}
	}
	a := filepath.Join(dir, "a.json")
	check("bad config", a, "run", "--status-file", a, "--config", bad, "--", "true")
	check("bad size", a, "run", "--status-file", a, "--mem", "lots", "--", "true")
	env := filepath.Join(dir, "env.json")
	t.Setenv("PACE_STATUS_FILE", env)
	check("env, bad size", env, "run", "--mem", "lots", "--", "true")
	t.Setenv("PACE_MODE", "bogus")
	check("env, bad mode", env, "run", "--", "true")
}
