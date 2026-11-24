package config

import (
	"strings"
	"testing"
)

func TestUnmanagedNotifyStderr(t *testing.T) {
	r := mustResolve(t, mustParse(t, ""), 36*G, "linux", baseEnv())
	if !r.NotifyStderr {
		t.Fatal("unmanaged.notify_stderr is off by default")
	}
	if !strings.Contains(r.Describe(), "notify_stderr on") {
		t.Fatalf("config print does not show notify_stderr:\n%s", r.Describe())
	}
	off := mustResolve(t, mustParse(t, "[unmanaged]\nnotify_stderr = false"), 36*G, "linux", baseEnv())
	if off.NotifyStderr {
		t.Fatal("unmanaged.notify_stderr = false is not honoured")
	}
	if off.Hash == r.Hash {
		t.Fatal("notify_stderr does not change the config hash")
	}
	if !strings.Contains(ExampleTOML(), "# notify_stderr = true") {
		t.Fatal("config example lacks notify_stderr")
	}
}
