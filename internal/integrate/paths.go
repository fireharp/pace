package integrate

import (
	"os"
	"path/filepath"
	"runtime"
)

// Label is the launchd label and the base name of the systemd unit.
const (
	Label    = "com.pace.daemon"
	UnitName = "pace.service"
)

// Env is a lookup function for environment variables (os.Getenv in
// production, a map in tests).
type Env func(string) string

func (e Env) get(k string) string {
	if e == nil {
		return os.Getenv(k)
	}
	return e(k)
}

// MapEnv returns an Env backed by m.
func MapEnv(m map[string]string) Env { return func(k string) string { return m[k] } }

func homeDir(env Env) string {
	if h := env.get("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// DefaultShimDir is $PACE_SHIM_DIR, else $PACE_HOME/shims, else ~/.pace/shims.
func DefaultShimDir(env Env) string {
	if d := env.get("PACE_SHIM_DIR"); d != "" {
		return filepath.Clean(d)
	}
	if h := env.get("PACE_HOME"); h != "" {
		return filepath.Join(h, "shims")
	}
	return filepath.Join(homeDir(env), ".pace", "shims")
}

// DefaultSocket returns the socket path used when the resolved config does
// not name one: $PACE_SOCK, else $PACE_HOME/run/pace.sock, else the platform
// default from docs/ARCHITECTURE.md.
func DefaultSocket(env Env, goos string) string {
	if s := env.get("PACE_SOCK"); s != "" {
		return s
	}
	if h := env.get("PACE_HOME"); h != "" {
		return filepath.Join(h, "run", "pace.sock")
	}
	home := homeDir(env)
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "pace", "run", "pace.sock")
	}
	if x := env.get("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "pace", "pace.sock")
	}
	return filepath.Join(home, ".local", "state", "pace", "run", "pace.sock")
}

// SocketPath picks the configured socket, falling back to DefaultSocket.
func SocketPath(configured string, env Env) string {
	if configured != "" {
		return configured
	}
	return DefaultSocket(env, runtime.GOOS)
}

// DefaultLogDir is the daemon log directory used by the service files when
// the resolved config does not name one.
func DefaultLogDir(env Env, goos string) string {
	if h := env.get("PACE_HOME"); h != "" {
		return filepath.Join(h, "logs")
	}
	home := homeDir(env)
	if goos == "darwin" {
		return filepath.Join(home, "Library", "Logs", "pace")
	}
	if x := env.get("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pace")
	}
	return filepath.Join(home, ".local", "state", "pace")
}
