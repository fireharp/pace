package envcompat

import "testing"

func TestWrapFallsBackToLegacyName(t *testing.T) {
	m := map[string]string{"GOV_HOME": "/old", "PACE_SOCK": "/new.sock", "GOV_SOCK": "/old.sock", "HOME": "/h"}
	get := Wrap(func(k string) string { return m[k] })
	for key, want := range map[string]string{
		"PACE_HOME": "/old",      // only the old name is set
		"PACE_SOCK": "/new.sock", // the new name wins
		"PACE_MODE": "",          // neither
		"HOME":      "/h",        // not a pace variable
		"GOV_HOME":  "/old",      // asked by its old name directly
	} {
		if got := get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLegacy(t *testing.T) {
	for key, want := range map[string]string{"PACE_CONFIG": "GOV_CONFIG", "PACE_JOB_ID": "", "PACE_": "", "HOME": "", "GOV_HOME": ""} {
		if got := Legacy(key); got != want {
			t.Errorf("Legacy(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestLookupList(t *testing.T) {
	env := []string{"GOV_MODE=a", "PATH=/bin", "GOV_MODE=b", "GOV_JOB_ID=j"}
	if got := Lookup(env, "PACE_MODE"); got != "b" {
		t.Errorf("legacy mode = %q, want b", got)
	}
	env = append(env, "PACE_MODE=c")
	if got := Lookup(env, "PACE_MODE"); got != "c" {
		t.Errorf("mode = %q, want c", got)
	}
	// A job id is per job, never inherited from an old gov job.
	if got := Lookup(env, "PACE_JOB_ID"); got != "" {
		t.Errorf("job id = %q, want none", got)
	}
}
