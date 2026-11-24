package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/units"
)

// MaxSocketPath is the longest unix socket path the kernel accepts
// (sun_path is 104 bytes on macOS including the NUL; measured).
const MaxSocketPath = 103

// Compiled-in defaults (strings are parsed like file values).
const (
	defMode           = policy.ModeEnforce
	defReserve        = "2G,12.5%"
	defSampleInterval = "1s"
	defFastInterval   = "250ms"

	defWarnAvailPct = 15.0
	defCritAvailPct = 7.0
	defPSIWarnPct   = 10.0
	defPSICritPct   = 5.0

	defWarnDeadline  = "20s"
	defGrace         = "5s"
	defCriticalGrace = "1s"
	defStep          = "2s"
	defCriticalStep  = "500ms"
	defRecoverHold   = "5s"
	defOrphanGrace   = "10s"
	defBudgetWarnPct = 90.0
	defMinVictim     = "64M"

	defUnmanagedPriority = 2
	defQueueTimeout      = "30m"

	defCPUWarnPerCore = 1.5
	defCPUOKPerCore   = 1.0

	defDiskWarnFree = "10G"
	defDiskCritFree = "3G"

	defHistorySize = 20
	defMinSamples  = 3
	defMaxKeys     = policy.DefaultHistoryMaxKeys

	defFailOpenSlots = 2
	defFailOpenWait  = "2s"

	defLogMaxSize   = "64M"
	defLogKeep      = 8
	defLogHeartbeat = "60s"

	// DefaultClass is the class used when a job declares none (or an unknown one).
	DefaultClass = "default"
)

// defTolerancePct is the budget tolerance: 10 % on every platform. macOS
// enforcement trails the sample (no kernel cap), and so does Linux without a
// delegated cgroup v2 tree (the common case for a per-user service: a login
// session scope is root-owned), where job usage is moreover a per-process sum
// that counts shared memory once per process. With job containers the
// kernel caps a job at exactly this line (enforce.hard_cap).
func defTolerancePct(bool) float64 { return 10 }

// defMarginPct is the learning margin: 10 % on macOS, 5 % on Linux.
func defMarginPct(darwin bool) float64 {
	if darwin {
		return 10
	}
	return 5
}

func intp(v int) *int { return &v }

// Default resource budgets and limits for each workload class.
var defaultClasses = map[string]ClassConfig{
	"build":   {Estimate: "2G,15%", Budget: "4G,30%", Slots: "per:16G", WallLimit: "60m", NoProgress: "10m", Priority: intp(5)},
	"test":    {Estimate: "1G,10%", Budget: "2G,20%", Slots: "per:8G", WallLimit: "30m", NoProgress: "10m", Priority: intp(5)},
	"browser": {Estimate: "1.5G", Budget: "3G,10%", Slots: "per:8G", WallLimit: "30m", NoProgress: "5m", Priority: intp(4)},
	"model":   {Estimate: "4G,40%", Budget: "60%", Slots: "1", WallLimit: "4h", NoProgress: "15m", Priority: intp(6)},
	"default": {Estimate: "1G,5%", Budget: "2G,10%", Slots: "0", WallLimit: "30m", NoProgress: "10m", Priority: intp(5)},
}

var classNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// errList collects every validation problem so one run reports them all.
type errList struct{ list []error }

func (e *errList) add(key, format string, args ...any) {
	e.list = append(e.list, fmt.Errorf("config: %s: %s", key, fmt.Sprintf(format, args...)))
}

func (e *errList) err() error { return errors.Join(e.list...) }

// ParseSize parses a size expression against capacity: "4G", "512M", "30%"
// (of capacity) or a comma list meaning the maximum of the parts ("4G,30%").
func ParseSize(s string, capacity int64) (int64, error) {
	return parseSizeExpr(s, capacity, true)
}

func parseSizeExpr(s string, capacity int64, allowPct bool) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	var best int64 = -1
	for _, part := range strings.Split(t, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			return 0, fmt.Errorf("empty part in size %q", s)
		}
		var v int64
		if strings.HasSuffix(p, "%") {
			if !allowPct {
				return 0, fmt.Errorf("percentage not allowed in %q (absolute size required)", s)
			}
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(p, "%")), 64)
			if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
				return 0, fmt.Errorf("invalid percentage %q", p)
			}
			if f <= 0 || f > 100 {
				return 0, fmt.Errorf("percentage %q must be in (0, 100]", p)
			}
			v = int64(float64(capacity) * f / 100)
		} else {
			if strings.Contains(p, ",") {
				return 0, fmt.Errorf("invalid size %q", p)
			}
			b, err := units.ParseBytes(p)
			if err != nil {
				return 0, err
			}
			v = b
		}
		if v > best {
			best = v
		}
	}
	return best, nil
}

// ParseSlots parses "N" (0 = unlimited) or "per:SIZE" = max(1, capacity/SIZE).
func ParseSlots(s string, capacity int64) (int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty slots")
	}
	if rest, ok := strings.CutPrefix(t, "per:"); ok {
		per, err := parseSizeExpr(rest, capacity, false)
		if err != nil {
			return 0, fmt.Errorf("invalid slots %q: %v", s, err)
		}
		if per <= 0 {
			return 0, fmt.Errorf("invalid slots %q: size must be > 0", s)
		}
		n := capacity / per
		if n < 1 {
			n = 1
		}
		if n > math.MaxInt32 {
			n = math.MaxInt32
		}
		return int(n), nil
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, fmt.Errorf("invalid slots %q: want N, 0 (unlimited) or per:SIZE", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid slots %q: must be >= 0", s)
	}
	return n, nil
}

// durMS parses a positive duration to ms (s empty = def).
func durMS(e *errList, key, s, def string) int64 {
	if strings.TrimSpace(s) == "" {
		s = def
	}
	d, err := units.ParseDuration(s)
	if err != nil {
		e.add(key, "invalid duration %q: %v", s, err)
		return 0
	}
	ms := d.Milliseconds()
	if ms <= 0 {
		e.add(key, "duration %q must be positive (>= 1ms)", s)
		return 0
	}
	return ms
}

// limitMS parses a class limit: "0"/"none"/"off" disable it (0), else positive.
func limitMS(e *errList, key, s string) int64 {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "none", "off":
		return 0
	}
	return durMS(e, key, s, s)
}

// pct validates a percentage field (0 = def) and returns it.
func pct(e *errList, key string, v, def float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		e.add(key, "must be a finite number")
		return def
	}
	if v == 0 {
		return def
	}
	if v < 0 || v > 100 {
		e.add(key, "%v must be between 0 and 100", v)
		return def
	}
	return v
}

func permille(p float64) int64 { return int64(math.Round(p * 10)) }

func milli(v float64) int64 { return int64(math.Round(v * 1000)) }

// size parses a size expression (s empty = def).
func size(e *errList, key, s, def string, capacity int64, allowPct bool) int64 {
	if strings.TrimSpace(s) == "" {
		s = def
	}
	v, err := parseSizeExpr(s, capacity, allowPct)
	if err != nil {
		e.add(key, "%v", err)
		return 0
	}
	return v
}

func posInt(e *errList, key string, v, def int) int {
	if v < 0 {
		e.add(key, "%d must not be negative", v)
		return def
	}
	if v == 0 {
		return def
	}
	return v
}

func priority(e *errList, key string, p *int, def int) int {
	if p == nil {
		return def
	}
	if *p < 0 || *p > 9 {
		e.add(key, "%d must be between 0 and 9", *p)
		return def
	}
	return *p
}

// absPath expands "~/" and requires an absolute path.
func absPath(e *errList, key, p, home string) string {
	p = strings.TrimSpace(p)
	if p == "~" {
		p = home
	} else if rest, ok := strings.CutPrefix(p, "~/"); ok {
		p = filepath.Join(home, rest)
	}
	if !filepath.IsAbs(p) {
		e.add(key, "path %q must be absolute", p)
		return p
	}
	return filepath.Clean(p)
}

func (c Config) resolve(totalBytes int64, platform string, env getenvFunc) (Resolved, error) {
	var e errList
	if platform == "" {
		platform = runtime.GOOS
	}
	darwin := platform == "darwin"
	r := Resolved{Platform: platform, MachineBytes: totalBytes}

	userH, err := userHome(env)
	if err != nil {
		return Resolved{}, err
	}

	// Mode.
	mode := c.Mode
	if m := env("PACE_MODE"); m != "" {
		mode = m
	}
	modeKey := "mode"
	if env("PACE_MODE") != "" {
		modeKey = "PACE_MODE"
	}
	switch mode {
	case "":
		mode = defMode
	case policy.ModeObserve, policy.ModeWarnOnly, policy.ModeEnforce:
	default:
		e.add(modeKey, "%q is not one of observe, warn-only, enforce", mode)
	}

	// Capacity.
	capStr, capKey := c.Capacity, "capacity"
	if v := env("PACE_CAPACITY"); v != "" {
		capStr, capKey = v, "PACE_CAPACITY"
	}
	var override int64
	if strings.TrimSpace(capStr) != "" {
		v, err := parseSizeExpr(capStr, 0, false)
		switch {
		case err != nil:
			e.add(capKey, "%v", err)
		case v <= 0:
			e.add(capKey, "%q must be > 0", capStr)
		case totalBytes > 0 && v > totalBytes:
			e.add(capKey, "%s exceeds physical memory %s (a capacity override may only simulate a smaller machine)",
				units.FormatBytes(v), units.FormatBytes(totalBytes))
		default:
			override = v
		}
	}
	capacity := totalBytes
	if override > 0 {
		capacity = override
	}
	if capacity <= 0 {
		e.add("capacity", "machine memory unknown (%d bytes) and no capacity override set", totalBytes)
		return Resolved{}, e.err()
	}
	r.CapacityBytes = capacity

	useKernel := override == 0
	if c.UseKernelLevel != nil {
		useKernel = *c.UseKernelLevel
	}

	nerr := len(e.list)
	reserve := size(&e, "reserve", c.Reserve, defReserve, capacity, true)
	if len(e.list) > nerr {
		// already reported
	} else if reserve <= 0 {
		e.add("reserve", "%q must be > 0", c.Reserve)
	} else if reserve >= capacity {
		e.add("reserve", "%s leaves nothing of capacity %s", units.FormatBytes(reserve), units.FormatBytes(capacity))
	}

	r.SampleMS = durMS(&e, "sample_interval", c.SampleInterval, defSampleInterval)
	r.FastMS = durMS(&e, "fast_interval", c.FastInterval, defFastInterval)
	if r.SampleMS > 0 && r.FastMS > r.SampleMS {
		e.add("fast_interval", "%s must not exceed sample_interval %s", units.FormatMS(r.FastMS), units.FormatMS(r.SampleMS))
	}

	// Paths.
	var home, sock, logs string
	if darwin {
		home = filepath.Join(userH, "Library", "Application Support", "pace")
		logs = filepath.Join(userH, "Library", "Logs", "pace")
		sock = filepath.Join(home, "run", "pace.sock")
	} else {
		state := filepath.Join(userH, ".local", "state")
		if x := env("XDG_STATE_HOME"); x != "" && filepath.IsAbs(x) {
			state = x
		}
		home = filepath.Join(state, "pace")
		logs = filepath.Join(home, "logs")
		sock = filepath.Join(home, "run", "pace.sock")
		if rt := env("XDG_RUNTIME_DIR"); rt != "" && filepath.IsAbs(rt) {
			sock = filepath.Join(rt, "pace", "pace.sock")
		}
	}
	if c.Home != "" {
		home = absPath(&e, "home", c.Home, userH)
		sock = filepath.Join(home, "run", "pace.sock")
		logs = filepath.Join(home, "logs")
	}
	if c.Socket != "" {
		sock = absPath(&e, "socket", c.Socket, userH)
	}
	if c.LogDir != "" {
		logs = absPath(&e, "log_dir", c.LogDir, userH)
	}
	sockKey := "socket"
	if gh := env("PACE_HOME"); gh != "" {
		home = absPath(&e, "PACE_HOME", gh, userH)
		sock = filepath.Join(home, "run", "pace.sock")
		logs = filepath.Join(home, "logs")
		sockKey = "PACE_HOME"
	}
	if gs := env("PACE_SOCK"); gs != "" {
		sock = absPath(&e, "PACE_SOCK", gs, userH)
		sockKey = "PACE_SOCK"
	}
	if len(sock) > MaxSocketPath {
		e.add(sockKey, "socket path %q is %d bytes; unix sockets allow at most %d: set PACE_SOCK (or socket in the config) to a shorter path",
			sock, len(sock), MaxSocketPath)
	}
	r.Home, r.Socket, r.LogDir = home, sock, logs

	// Pressure.
	warnAvail := pct(&e, "pressure.warn_avail_pct", c.Pressure.WarnAvailPct, defWarnAvailPct)
	critAvail := pct(&e, "pressure.crit_avail_pct", c.Pressure.CritAvailPct, defCritAvailPct)
	if warnAvail <= critAvail {
		e.add("pressure.warn_avail_pct", "%v must be greater than crit_avail_pct %v", warnAvail, critAvail)
	}
	psiWarn := pct(&e, "pressure.psi_warn_pct", c.Pressure.PSIWarnPct, defPSIWarnPct)
	psiCrit := pct(&e, "pressure.psi_crit_pct", c.Pressure.PSICritPct, defPSICritPct)

	// Enforcement.
	en := c.Enforce
	p := policy.Params{
		Mode:              mode,
		CapacityBytes:     override,
		UseKernelLevel:    useKernel,
		ReserveBytes:      reserve,
		AdmitBelowWarn:    c.AdmitBelowWarn,
		WarnAvailPermille: permille(warnAvail),
		CritAvailPermille: permille(critAvail),
		PSIWarn10:         permille(psiWarn),
		PSICrit10:         permille(psiCrit),
		WarnDeadlineMS:    durMS(&e, "enforce.warn_deadline", en.WarnDeadline, defWarnDeadline),
		GraceMS:           durMS(&e, "enforce.grace", en.Grace, defGrace),
		CriticalGraceMS:   durMS(&e, "enforce.critical_grace", en.CriticalGrace, defCriticalGrace),
		StepMS:            durMS(&e, "enforce.step", en.Step, defStep),
		CriticalStepMS:    durMS(&e, "enforce.critical_step", en.CriticalStep, defCriticalStep),
		RecoverHoldMS:     durMS(&e, "enforce.recover_hold", en.RecoverHold, defRecoverHold),
		OrphanGraceMS:     durMS(&e, "enforce.orphan_grace", en.OrphanGrace, defOrphanGrace),
		DefaultClass:      DefaultClass,
	}
	if p.GraceMS > 0 && p.CriticalGraceMS > p.GraceMS {
		e.add("enforce.critical_grace", "%s must not exceed grace %s", units.FormatMS(p.CriticalGraceMS), units.FormatMS(p.GraceMS))
	}
	if p.StepMS > 0 && p.CriticalStepMS > p.StepMS {
		e.add("enforce.critical_step", "%s must not exceed step %s", units.FormatMS(p.CriticalStepMS), units.FormatMS(p.StepMS))
	}
	p.BudgetWarnPermille = permille(pct(&e, "enforce.budget_warn_pct", en.BudgetWarnPct, defBudgetWarnPct))
	p.TolerancePermille = permille(pct(&e, "enforce.tolerance_pct", en.TolerancePct, defTolerancePct(darwin)))
	p.MinVictimBytes = size(&e, "enforce.min_victim", en.MinVictim, defMinVictim, capacity, true)
	r.HardCap = en.HardCap == nil || *en.HardCap

	// Unmanaged.
	p.UnmanagedPriority = priority(&e, "unmanaged.priority", c.Unmanaged.Priority, defUnmanagedPriority)
	switch strings.ToLower(strings.TrimSpace(c.Unmanaged.Budget)) {
	case "", "none", "0", "off":
	default:
		p.UnmanagedBudgetBytes = size(&e, "unmanaged.budget", c.Unmanaged.Budget, "", capacity, true)
		if p.UnmanagedBudgetBytes <= 0 {
			e.add("unmanaged.budget", "%q must be > 0 (or \"none\")", c.Unmanaged.Budget)
		}
	}
	if len(c.Unmanaged.Protect) > 64 {
		e.add("unmanaged.protect", "at most 64 names (got %d)", len(c.Unmanaged.Protect))
	}
	for _, n := range c.Unmanaged.Protect {
		if n == "" || strings.ContainsAny(n, "/\x00") || len(n) > 255 {
			e.add("unmanaged.protect", "%q: give a process or executable name, not a path", n)
			continue
		}
		r.ProtectNames = append(r.ProtectNames, n)
	}
	r.NotifyStderr = c.Unmanaged.NotifyStderr == nil || *c.Unmanaged.NotifyStderr
	if c.Unmanaged.ScopeRootPID < 0 {
		e.add("unmanaged.scope_root_pid", "%d must not be negative", c.Unmanaged.ScopeRootPID)
	} else {
		r.ScopeRootPID = c.Unmanaged.ScopeRootPID
	}

	p.DefaultQueueTimeoutMS = durMS(&e, "queue.timeout", c.Queue.Timeout, defQueueTimeout)

	// CPU.
	cpuWarn, cpuOK := c.CPU.WarnLoadPerCore, c.CPU.OKLoadPerCore
	for _, f := range []struct {
		key string
		v   *float64
		def float64
	}{{"cpu.warn_load_per_core", &cpuWarn, defCPUWarnPerCore}, {"cpu.ok_load_per_core", &cpuOK, defCPUOKPerCore}} {
		switch {
		case math.IsNaN(*f.v) || math.IsInf(*f.v, 0):
			e.add(f.key, "must be a finite number")
			*f.v = f.def
		case *f.v == 0:
			*f.v = f.def
		case *f.v < 0 || *f.v > 1000:
			e.add(f.key, "%v must be between 0 and 1000", *f.v)
			*f.v = f.def
		}
	}
	if cpuWarn <= cpuOK {
		e.add("cpu.warn_load_per_core", "%v must be greater than ok_load_per_core %v", cpuWarn, cpuOK)
	}
	p.CPUWarnPerCoreMilli, p.CPUOKPerCoreMilli = milli(cpuWarn), milli(cpuOK)

	// Disk.
	p.DiskWarnFreeBytes = size(&e, "disk.warn_free", c.Disk.WarnFree, defDiskWarnFree, capacity, false)
	p.DiskCritFreeBytes = size(&e, "disk.crit_free", c.Disk.CritFree, defDiskCritFree, capacity, false)
	if p.DiskWarnFreeBytes <= p.DiskCritFreeBytes {
		e.add("disk.warn_free", "%s must be greater than crit_free %s",
			units.FormatBytes(p.DiskWarnFreeBytes), units.FormatBytes(p.DiskCritFreeBytes))
	}
	paths := c.Disk.Paths
	if len(paths) == 0 {
		paths = []string{"/", userH}
	}
	seen := map[string]bool{}
	for i, dp := range paths {
		a := absPath(&e, fmt.Sprintf("disk.paths[%d]", i), dp, userH)
		if !seen[a] {
			seen[a] = true
			r.DiskPaths = append(r.DiskPaths, a)
		}
	}

	// Learning.
	p.MarginPermille = permille(pct(&e, "learn.margin_pct", c.Learn.MarginPct, defMarginPct(darwin)))
	p.HistorySize = posInt(&e, "learn.history_size", c.Learn.HistorySize, defHistorySize)
	p.HistoryMinSamples = posInt(&e, "learn.min_samples", c.Learn.MinSamples, defMinSamples)
	p.HistoryMaxKeys = posInt(&e, "learn.max_keys", c.Learn.MaxKeys, defMaxKeys)
	if p.HistoryMinSamples > p.HistorySize {
		e.add("learn.min_samples", "%d must not exceed history_size %d", p.HistoryMinSamples, p.HistorySize)
	}

	// Fail-open.
	r.FailOpenSlots = posInt(&e, "failopen.slots", c.FailOpen.Slots, defFailOpenSlots)
	r.FailOpenWaitMS = durMS(&e, "failopen.wait", c.FailOpen.Wait, defFailOpenWait)
	r.NotifyMacOS = c.Notify.MacOS

	// Log.
	r.LogMaxBytes = size(&e, "log.max_size", c.Log.MaxSize, defLogMaxSize, capacity, false)
	if r.LogMaxBytes > 0 && r.LogMaxBytes < units.MiB {
		e.add("log.max_size", "%s must be at least 1M", units.FormatBytes(r.LogMaxBytes))
	}
	r.LogKeep = posInt(&e, "log.keep", c.Log.Keep, defLogKeep)
	r.HeartbeatMS = durMS(&e, "log.heartbeat", c.Log.Heartbeat, defLogHeartbeat)

	// Classes.
	p.Classes = resolveClasses(&e, c.Classes, capacity, capacity-reserve)
	r.Params = p

	if err := e.err(); err != nil {
		return Resolved{}, err
	}
	r.Hash = r.computeHash()
	return r, nil
}

// resolveClasses merges file classes over the defaults. A file class with a
// built-in name overrides only the fields it sets; a new class starts from
// the "default" class. Budgets are capped at maxBudget (capacity - reserve);
// estimates are capped at the budget, except that an explicitly configured
// estimate above the (capped) budget is an error.
func resolveClasses(e *errList, user map[string]ClassConfig, capacity, maxBudget int64) []policy.ClassParams {
	names := make([]string, 0, len(defaultClasses)+len(user))
	for n := range defaultClasses {
		names = append(names, n)
	}
	for n := range user {
		if _, ok := defaultClasses[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]policy.ClassParams, 0, len(names))
	for _, name := range names {
		key := "classes." + name
		if !classNameRE.MatchString(name) {
			e.add(key, "invalid class name (want lowercase letters, digits, '_' or '-', starting with a letter, at most 32 chars)")
			continue
		}
		base, builtin := defaultClasses[name]
		if !builtin {
			base = defaultClasses[DefaultClass]
		}
		u, hasUser := user[name]
		estSet := hasUser && strings.TrimSpace(u.Estimate) != ""
		if hasUser {
			if strings.TrimSpace(u.Estimate) != "" {
				base.Estimate = u.Estimate
			}
			if strings.TrimSpace(u.Budget) != "" {
				base.Budget = u.Budget
			}
			if strings.TrimSpace(u.Slots) != "" {
				base.Slots = u.Slots
			}
			if strings.TrimSpace(u.WallLimit) != "" {
				base.WallLimit = u.WallLimit
			}
			if strings.TrimSpace(u.NoProgress) != "" {
				base.NoProgress = u.NoProgress
			}
			if u.Priority != nil {
				base.Priority = u.Priority
			}
		}
		cp := policy.ClassParams{Name: name}
		est, eerr := parseSizeExpr(base.Estimate, capacity, true)
		if eerr != nil {
			e.add(key+".estimate", "%v", eerr)
		} else if est <= 0 {
			e.add(key+".estimate", "%q must be > 0", base.Estimate)
		}
		bud, berr := parseSizeExpr(base.Budget, capacity, true)
		if berr != nil {
			e.add(key+".budget", "%v", berr)
		} else if bud <= 0 {
			e.add(key+".budget", "%q must be > 0", base.Budget)
		}
		if maxBudget > 0 && bud > maxBudget {
			bud = maxBudget
		}
		if eerr == nil && berr == nil && est > bud {
			if estSet {
				e.add(key+".estimate", "%s exceeds the class budget %s", units.FormatBytes(est), units.FormatBytes(bud))
			}
			est = bud
		}
		cp.EstimateBytes, cp.BudgetBytes = est, bud
		slots, serr := ParseSlots(base.Slots, capacity)
		if serr != nil {
			e.add(key+".slots", "%v", serr)
		}
		cp.Slots = slots
		cp.WallLimitMS = limitMS(e, key+".wall_limit", base.WallLimit)
		cp.NoProgressMS = limitMS(e, key+".no_progress", base.NoProgress)
		cp.Priority = priority(e, key+".priority", base.Priority, 5)
		out = append(out, cp)
	}
	return out
}

// computeHash is sha256 over the canonical JSON of the effective config,
// excluding Source and Hash. encoding/json emits struct fields in
// declaration order and the config contains no maps, so the encoding is
// canonical: file comments, key order and equivalent spellings ("4G" vs
// "4096M") do not change it.
func (r Resolved) computeHash() string {
	r.Hash, r.Source = "", ""
	b, err := json.Marshal(r)
	if err != nil {
		// Resolved has no unmarshalable types; keep the contract anyway.
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ClassByName returns the resolved class, or the default class when name is
// unknown or empty (ok=false then).
func (r Resolved) ClassByName(name string) (policy.ClassParams, bool) {
	var def policy.ClassParams
	for _, c := range r.Params.Classes {
		if c.Name == name {
			return c, true
		}
		if c.Name == r.Params.DefaultClass {
			def = c
		}
	}
	return def, false
}
