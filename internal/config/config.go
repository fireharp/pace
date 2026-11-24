// Package config loads, validates and resolves the TOML configuration.
//
// Location: $PACE_CONFIG, else $XDG_CONFIG_HOME/pace/config.toml, else
// ~/.config/pace/config.toml (both OSes). The file is optional; compiled-in
// defaults apply. Unknown keys are an error.
//
// Environment overrides (applied after the file, used by tests):
//
//	PACE_HOME      base dir for socket/state/logs (overrides platform defaults
//	              and the file's home/socket/log_dir)
//	PACE_SOCK      socket path
//	PACE_CAPACITY  capacity override, e.g. "8G"
//	PACE_MODE      observe|warn-only|enforce
//	PACE_CONFIG    config file path (read by Load)
//
// Size fields accept "4G", "512M", a percentage of capacity "30%", or a
// comma list meaning the maximum of the parts: "4G,30%".
//
// Precedence for paths: platform default < file (home, socket, log_dir) <
// PACE_HOME < PACE_SOCK. Setting home (file or PACE_HOME) moves the socket to
// <home>/run/pace.sock and the logs to <home>/logs unless they are set
// explicitly (the file's socket/log_dir are ignored under PACE_HOME).
package config

import (
	"github.com/fireharp/pace/internal/policy"
)

// Config mirrors the TOML file. Empty/zero fields mean "default".
type Config struct {
	Mode           string `toml:"mode"`             // observe|warn-only|enforce (default enforce)
	Capacity       string `toml:"capacity"`         // override machine memory (tests)
	Reserve        string `toml:"reserve"`          // default "2G,12.5%"
	AdmitBelowWarn bool   `toml:"admit_below_warn"` // admission may go below the warn line (default false)
	UseKernelLevel *bool  `toml:"use_kernel_level"`
	SampleInterval string `toml:"sample_interval"` // default 1s
	FastInterval   string `toml:"fast_interval"`   // default 250ms

	Home   string `toml:"home"`    // base dir (state, history); platform default
	Socket string `toml:"socket"`  // default <home>/run/pace.sock
	LogDir string `toml:"log_dir"` // default platform logs dir

	Pressure  PressureConfig         `toml:"pressure"`
	Enforce   EnforceConfig          `toml:"enforce"`
	Unmanaged UnmanagedConfig        `toml:"unmanaged"`
	Queue     QueueConfig            `toml:"queue"`
	CPU       CPUConfig              `toml:"cpu"`
	Disk      DiskConfig             `toml:"disk"`
	Learn     LearnConfig            `toml:"learn"`
	FailOpen  FailOpenConfig         `toml:"failopen"`
	Notify    NotifyConfig           `toml:"notify"`
	Log       LogConfig              `toml:"log"`
	Classes   map[string]ClassConfig `toml:"classes"`
}

type PressureConfig struct {
	WarnAvailPct float64 `toml:"warn_avail_pct"` // default 15
	CritAvailPct float64 `toml:"crit_avail_pct"` // default 7
	PSIWarnPct   float64 `toml:"psi_warn_pct"`   // Linux some avg10, default 10
	PSICritPct   float64 `toml:"psi_crit_pct"`   // Linux full avg10, default 5
}

type EnforceConfig struct {
	WarnDeadline  string  `toml:"warn_deadline"`   // default 20s
	Grace         string  `toml:"grace"`           // default 5s
	CriticalGrace string  `toml:"critical_grace"`  // default 1s
	Step          string  `toml:"step"`            // default 2s
	CriticalStep  string  `toml:"critical_step"`   // default 500ms
	RecoverHold   string  `toml:"recover_hold"`    // default 5s
	OrphanGrace   string  `toml:"orphan_grace"`    // default 10s
	BudgetWarnPct float64 `toml:"budget_warn_pct"` // default 90
	TolerancePct  float64 `toml:"tolerance_pct"`   // default 10 (both platforms)
	MinVictim     string  `toml:"min_victim"`      // default 64M
	// HardCap: on Linux with job containers, the kernel caps each job at
	// budget*(1+tolerance) in enforce mode (memory.max). Default true.
	HardCap *bool `toml:"hard_cap"`
}

type UnmanagedConfig struct {
	Priority     *int   `toml:"priority"`       // default 2
	Budget       string `toml:"budget"`         // per process group; default none ("none"/"0" = none)
	ScopeRootPID int    `toml:"scope_root_pid"` // tests: only descendants of this pid
	// Protect names processes (comm or executable name) whose process group
	// is never acted on: the main process of a service that must survive,
	// such as an agent orchestrator. Its whole group is protected, because
	// pace signals groups.
	Protect []string `toml:"protect"`
	// NotifyStderr: on Linux, a unit's WARN and KILLED lines (with their
	// hints) are also written to its processes' own stderr, right before
	// the signal, since no pace run prints them. Default true.
	NotifyStderr *bool `toml:"notify_stderr"`
}

type QueueConfig struct {
	Timeout string `toml:"timeout"` // default 30m
}

type CPUConfig struct {
	WarnLoadPerCore float64 `toml:"warn_load_per_core"` // default 1.5
	OKLoadPerCore   float64 `toml:"ok_load_per_core"`   // default 1.0
}

type DiskConfig struct {
	Paths    []string `toml:"paths"`     // default ["/"] (+ home dir volume)
	WarnFree string   `toml:"warn_free"` // default 10G
	CritFree string   `toml:"crit_free"` // default 3G
}

type LearnConfig struct {
	MarginPct   float64 `toml:"margin_pct"`   // default 10 macOS / 5 linux
	HistorySize int     `toml:"history_size"` // default 20
	MinSamples  int     `toml:"min_samples"`  // default 3
	MaxKeys     int     `toml:"max_keys"`     // command keys remembered, default 256
}

type FailOpenConfig struct {
	Slots int    `toml:"slots"` // concurrent fail-open jobs, default 2
	Wait  string `toml:"wait"`  // reconnect wait before failing open, default 2s
}

type NotifyConfig struct {
	MacOS bool `toml:"macos"` // osascript notification on KILLED (default false)
}

type LogConfig struct {
	MaxSize   string `toml:"max_size"`  // default 64M
	Keep      int    `toml:"keep"`      // default 8
	Heartbeat string `toml:"heartbeat"` // default 60s
}

// ClassConfig overrides one class. Sizes accept "4G", "30%", "4G,30%".
// wall_limit / no_progress accept "0", "none" or "off" to disable the limit.
type ClassConfig struct {
	Estimate   string `toml:"estimate"`
	Budget     string `toml:"budget"`
	Slots      string `toml:"slots"` // "3", "0" (unlimited), or "per:16G" = max(1, capacity/16G)
	WallLimit  string `toml:"wall_limit"`
	NoProgress string `toml:"no_progress"`
	Priority   *int   `toml:"priority"`
}

// Resolved is the fully validated runtime configuration.
type Resolved struct {
	Params         policy.Params
	SampleMS       int64
	FastMS         int64
	HeartbeatMS    int64
	Home           string
	Socket         string
	LogDir         string
	ScopeRootPID   int
	ProtectNames   []string
	DiskPaths      []string
	FailOpenSlots  int
	FailOpenWaitMS int64
	NotifyMacOS    bool
	// HardCap: job containers get memory.max = budget*(1+tolerance) in
	// enforce mode (enforce.hard_cap).
	HardCap     bool
	LogMaxBytes int64
	LogKeep     int
	// NotifyStderr (unmanaged.notify_stderr): a unit's WARN and KILLED
	// lines also go to its processes' own stderr (Linux).
	NotifyStderr bool
	// Platform the configuration was resolved for ("darwin" or "linux").
	Platform string
	// MachineBytes is the physical memory passed to Resolve; CapacityBytes is
	// what percentages were resolved against (the override, else MachineBytes).
	MachineBytes  int64
	CapacityBytes int64
	Hash          string // "sha256:<hex>" of the canonical effective config
	Source        string // file path loaded, or "defaults"
}

// Default returns the compiled-in defaults (as a Config with empty fields
// resolved by Resolve).
func Default() Config { return Config{} }

// Load reads path ("" = $PACE_CONFIG or the default location; a missing file
// at the default location is not an error, a missing explicit file is).
func Load(path string) (Config, string, error) { return load(path, osEnv) }

// Resolve applies env overrides and defaults for a machine with totalBytes of
// memory on platform ("darwin"|"linux"; "" = the running OS; any other value
// gets the Linux/XDG conventions), validates, and computes Params.
func (c Config) Resolve(totalBytes int64, platform string) (Resolved, error) {
	return c.resolve(totalBytes, platform, osEnv)
}

// LoadResolved is Load + Resolve.
func LoadResolved(path string, totalBytes int64, platform string) (Resolved, error) {
	c, src, err := Load(path)
	if err != nil {
		return Resolved{}, err
	}
	r, err := c.Resolve(totalBytes, platform)
	r.Source = src
	return r, err
}
