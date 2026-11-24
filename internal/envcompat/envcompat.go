// Package envcompat reads pace's environment variables. pace was called gov
// before; every PACE_* variable falls back to its old GOV_* name, so setups
// written for gov keep working after the rename.
package envcompat

import (
	"os"
	"strings"
)

const (
	// Prefix starts every pace environment variable.
	Prefix = "PACE_"
	// LegacyPrefix is the prefix the same variables had under the old name.
	LegacyPrefix = "GOV_"
)

// perJob are variables pace sets for the jobs it runs, not settings. They
// have no fallback: an old gov job's GOV_JOB_ID must not make pace think it
// runs inside one of its own jobs.
var perJob = map[string]bool{"PACE_JOB_ID": true, "PACE_STATUS_FILE": true, "PACE_SHIM_FALLBACK": true}

// Legacy returns the pre-rename name of key (PACE_HOME -> GOV_HOME), or ""
// if key is not a pace setting.
func Legacy(key string) string {
	if perJob[key] {
		return ""
	}
	if rest, ok := strings.CutPrefix(key, Prefix); ok && rest != "" {
		return LegacyPrefix + rest
	}
	return ""
}

// Wrap returns a getenv that answers PACE_* lookups from the legacy GOV_*
// name when the new one is empty. Other keys pass through unchanged.
func Wrap(get func(string) string) func(string) string {
	return func(key string) string {
		if v := get(key); v != "" {
			return v
		}
		if old := Legacy(key); old != "" {
			return get(old)
		}
		return ""
	}
}

// Getenv is os.Getenv with the legacy fallback.
var Getenv = Wrap(os.Getenv)

// Lookup returns the value of key from an environment list (KEY=VALUE
// entries, last one wins), with the legacy fallback.
func Lookup(env []string, key string) string {
	return Wrap(func(k string) string { return lookupList(env, k) })(key)
}

func lookupList(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}
