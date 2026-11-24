package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/units"
)

const G = units.GiB

func envOf(m map[string]string) getenvFunc {
	return func(k string) string { return m[k] }
}

func baseEnv() map[string]string { return map[string]string{"HOME": "/Users/tester"} }

func mustParse(t *testing.T, text string) Config {
	t.Helper()
	c, err := Parse(text)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return c
}

func mustResolve(t *testing.T, c Config, total int64, plat string, env map[string]string) Resolved {
	t.Helper()
	r, err := c.resolve(total, plat, envOf(env))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return r
}

func pctOf(c int64, p float64) int64 { return int64(float64(c) * p / 100) }

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func class(t *testing.T, r Resolved, name string) policy.ClassParams {
	t.Helper()
	c, ok := r.ClassByName(name)
	if !ok {
		t.Fatalf("class %q missing", name)
	}
	return c
}

func TestDefaultsDarwin(t *testing.T) {
	C := 36 * G
	r := mustResolve(t, Default(), C, "darwin", baseEnv())
	p := r.Params

	if r.Home != "/Users/tester/Library/Application Support/pace" {
		t.Errorf("home %q", r.Home)
	}
	if r.Socket != "/Users/tester/Library/Application Support/pace/run/pace.sock" {
		t.Errorf("socket %q", r.Socket)
	}
	if r.LogDir != "/Users/tester/Library/Logs/pace" {
		t.Errorf("logs %q", r.LogDir)
	}
	checks := []struct {
		name      string
		got, want int64
	}{
		{"capacity override", p.CapacityBytes, 0},
		{"reserve", p.ReserveBytes, max64(2*G, pctOf(C, 12.5))},
		{"warn avail", p.WarnAvailPermille, 150},
		{"crit avail", p.CritAvailPermille, 70},
		{"psi warn", p.PSIWarn10, 100},
		{"psi crit", p.PSICrit10, 50},
		{"budget warn", p.BudgetWarnPermille, 900},
		{"tolerance", p.TolerancePermille, 100},
		{"margin", p.MarginPermille, 100},
		{"warn deadline", p.WarnDeadlineMS, 20000},
		{"grace", p.GraceMS, 5000},
		{"critical grace", p.CriticalGraceMS, 1000},
		{"step", p.StepMS, 2000},
		{"critical step", p.CriticalStepMS, 500},
		{"recover hold", p.RecoverHoldMS, 5000},
		{"orphan grace", p.OrphanGraceMS, 10000},
		{"min victim", p.MinVictimBytes, 64 * units.MiB},
		{"unmanaged prio", int64(p.UnmanagedPriority), 2},
		{"unmanaged budget", p.UnmanagedBudgetBytes, 0},
		{"queue timeout", p.DefaultQueueTimeoutMS, 30 * 60 * 1000},
		{"cpu warn", p.CPUWarnPerCoreMilli, 1500},
		{"cpu ok", p.CPUOKPerCoreMilli, 1000},
		{"disk warn", p.DiskWarnFreeBytes, 10 * G},
		{"disk crit", p.DiskCritFreeBytes, 3 * G},
		{"history", int64(p.HistorySize), 20},
		{"min samples", int64(p.HistoryMinSamples), 3},
		{"sample", r.SampleMS, 1000},
		{"fast", r.FastMS, 250},
		{"heartbeat", r.HeartbeatMS, 60000},
		{"failopen slots", int64(r.FailOpenSlots), 2},
		{"failopen wait", r.FailOpenWaitMS, 2000},
		{"log max", r.LogMaxBytes, 64 * units.MiB},
		{"log keep", int64(r.LogKeep), 8},
		{"capacity", r.CapacityBytes, C},
		{"machine", r.MachineBytes, C},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
	if p.Mode != policy.ModeEnforce || !p.UseKernelLevel || p.DefaultClass != "default" {
		t.Errorf("mode %q kernel %v default %q", p.Mode, p.UseKernelLevel, p.DefaultClass)
	}
	if r.ScopeRootPID != 0 || r.NotifyMacOS {
		t.Errorf("scope %d notify %v", r.ScopeRootPID, r.NotifyMacOS)
	}
	if strings.Join(r.DiskPaths, ",") != "/,/Users/tester" {
		t.Errorf("disk paths %v", r.DiskPaths)
	}
	if r.Hash == "" || !strings.HasPrefix(r.Hash, "sha256:") || len(r.Hash) != 7+64 {
		t.Errorf("hash %q", r.Hash)
	}

	// Classes sorted by name with the documented defaults.
	var names []string
	for _, c := range p.Classes {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "browser,build,default,model,test" {
		t.Fatalf("classes %v", names)
	}
	min := func(a, b int64) int64 {
		if a < b {
			return a
		}
		return b
	}
	maxB := C - p.ReserveBytes
	want := map[string]policy.ClassParams{
		"build":   {Name: "build", EstimateBytes: max64(2*G, pctOf(C, 15)), BudgetBytes: min(max64(4*G, pctOf(C, 30)), maxB), Slots: 2, WallLimitMS: 3600000, NoProgressMS: 600000, Priority: 5},
		"test":    {Name: "test", EstimateBytes: max64(1*G, pctOf(C, 10)), BudgetBytes: min(max64(2*G, pctOf(C, 20)), maxB), Slots: 4, WallLimitMS: 1800000, NoProgressMS: 600000, Priority: 5},
		"browser": {Name: "browser", EstimateBytes: 3 * G / 2, BudgetBytes: min(max64(3*G, pctOf(C, 10)), maxB), Slots: 4, WallLimitMS: 1800000, NoProgressMS: 300000, Priority: 4},
		"model":   {Name: "model", EstimateBytes: max64(4*G, pctOf(C, 40)), BudgetBytes: min(pctOf(C, 60), maxB), Slots: 1, WallLimitMS: 4 * 3600000, NoProgressMS: 900000, Priority: 6},
		"default": {Name: "default", EstimateBytes: max64(1*G, pctOf(C, 5)), BudgetBytes: min(max64(2*G, pctOf(C, 10)), maxB), Slots: 0, WallLimitMS: 1800000, NoProgressMS: 600000, Priority: 5},
	}
	for name, w := range want {
		if got := class(t, r, name); got != w {
			t.Errorf("class %s = %+v, want %+v", name, got, w)
		}
	}
	// Sanity against hand-computed values (36G machine).
	if b := class(t, r, "build").BudgetBytes; units.FormatBytes(b) != "10.8G" {
		t.Errorf("build budget %s", units.FormatBytes(b))
	}
	if units.FormatBytes(p.ReserveBytes) != "4.5G" {
		t.Errorf("reserve %s", units.FormatBytes(p.ReserveBytes))
	}
	// Unknown class falls back to default.
	if c, ok := r.ClassByName("nope"); ok || c.Name != "default" {
		t.Errorf("ClassByName(nope) = %+v, %v", c, ok)
	}
}

func TestDefaultsLinux(t *testing.T) {
	tests := []struct {
		name                   string
		env                    map[string]string
		home, sock, logs       string
		wantTol, wantMarginPML int64
	}{
		{
			name: "no xdg",
			env:  map[string]string{"HOME": "/home/u"},
			home: "/home/u/.local/state/pace", sock: "/home/u/.local/state/pace/run/pace.sock", logs: "/home/u/.local/state/pace/logs",
		},
		{
			name: "xdg state and runtime",
			env:  map[string]string{"HOME": "/home/u", "XDG_STATE_HOME": "/st", "XDG_RUNTIME_DIR": "/run/user/1000"},
			home: "/st/pace", sock: "/run/user/1000/pace/pace.sock", logs: "/st/pace/logs",
		},
		{
			name: "relative xdg ignored",
			env:  map[string]string{"HOME": "/home/u", "XDG_STATE_HOME": "rel", "XDG_RUNTIME_DIR": "rel2"},
			home: "/home/u/.local/state/pace", sock: "/home/u/.local/state/pace/run/pace.sock", logs: "/home/u/.local/state/pace/logs",
		},
		{
			name: "PACE_HOME beats xdg runtime",
			env:  map[string]string{"HOME": "/home/u", "XDG_RUNTIME_DIR": "/run/user/1000", "PACE_HOME": "/tmp/g"},
			home: "/tmp/g", sock: "/tmp/g/run/pace.sock", logs: "/tmp/g/logs",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := mustResolve(t, Default(), 16*G, "linux", tc.env)
			if r.Home != tc.home || r.Socket != tc.sock || r.LogDir != tc.logs {
				t.Errorf("paths = %q %q %q, want %q %q %q", r.Home, r.Socket, r.LogDir, tc.home, tc.sock, tc.logs)
			}
			if r.Params.TolerancePermille != 100 || r.Params.MarginPermille != 50 {
				t.Errorf("tolerance %d margin %d, want 100 and 50", r.Params.TolerancePermille, r.Params.MarginPermille)
			}
			if r.Platform != "linux" {
				t.Errorf("platform %q", r.Platform)
			}
		})
	}
	// Unknown platforms get the Linux conventions.
	r := mustResolve(t, Default(), 16*G, "freebsd", map[string]string{"HOME": "/home/u"})
	if r.Home != "/home/u/.local/state/pace" || r.Params.TolerancePermille != 100 || !r.HardCap {
		t.Errorf("freebsd: %q %d", r.Home, r.Params.TolerancePermille)
	}
}

func TestEnvOverrides(t *testing.T) {
	env := baseEnv()
	env["PACE_HOME"] = "/tmp/gh"
	env["PACE_CAPACITY"] = "8G"
	env["PACE_MODE"] = "observe"
	r := mustResolve(t, mustParse(t, `
home = "/elsewhere"
socket = "/elsewhere/s.sock"
log_dir = "/elsewhere/logs"
capacity = "4G"
mode = "warn-only"
`), 36*G, "darwin", env)
	if r.Home != "/tmp/gh" || r.Socket != "/tmp/gh/run/pace.sock" || r.LogDir != "/tmp/gh/logs" {
		t.Errorf("paths %q %q %q", r.Home, r.Socket, r.LogDir)
	}
	if r.Params.CapacityBytes != 8*G || r.CapacityBytes != 8*G || r.MachineBytes != 36*G {
		t.Errorf("capacity %d / %d", r.Params.CapacityBytes, r.CapacityBytes)
	}
	if r.Params.Mode != policy.ModeObserve {
		t.Errorf("mode %q", r.Params.Mode)
	}
	if r.Params.UseKernelLevel {
		t.Error("use_kernel_level should default to false under a capacity override")
	}
	// Percentages resolve against the override: reserve max(2G, 1G) = 2G.
	if r.Params.ReserveBytes != 2*G {
		t.Errorf("reserve %d", r.Params.ReserveBytes)
	}
	if b := class(t, r, "model").BudgetBytes; b != min64(pctOf(8*G, 60), 6*G) {
		t.Errorf("model budget %d", b)
	}

	env["PACE_SOCK"] = "/tmp/x.sock"
	r = mustResolve(t, Default(), 36*G, "darwin", env)
	if r.Socket != "/tmp/x.sock" || r.Home != "/tmp/gh" {
		t.Errorf("PACE_SOCK: %q %q", r.Socket, r.Home)
	}

	// Explicit use_kernel_level wins over the capacity rule.
	r = mustResolve(t, mustParse(t, "use_kernel_level = true"), 36*G, "darwin", env)
	if !r.Params.UseKernelLevel {
		t.Error("explicit use_kernel_level=true ignored")
	}
	r = mustResolve(t, mustParse(t, "use_kernel_level = false"), 36*G, "darwin", baseEnv())
	if r.Params.UseKernelLevel {
		t.Error("explicit use_kernel_level=false ignored")
	}

	// File capacity without env.
	r = mustResolve(t, mustParse(t, `capacity = "4G"`), 36*G, "darwin", baseEnv())
	if r.Params.CapacityBytes != 4*G || r.Params.UseKernelLevel {
		t.Errorf("file capacity: %d kernel %v", r.Params.CapacityBytes, r.Params.UseKernelLevel)
	}
	// File home moves socket and logs; explicit socket wins.
	r = mustResolve(t, mustParse(t, "home = \"~/g\"\nsocket = \"/tmp/s.sock\""), 36*G, "darwin", baseEnv())
	if r.Home != "/Users/tester/g" || r.Socket != "/tmp/s.sock" || r.LogDir != "/Users/tester/g/logs" {
		t.Errorf("file home: %q %q %q", r.Home, r.Socket, r.LogDir)
	}
	// totalBytes unknown but capacity given.
	env2 := baseEnv()
	env2["PACE_CAPACITY"] = "2G"
	if _, err := Default().resolve(0, "darwin", envOf(env2)); err == nil || !strings.Contains(err.Error(), "reserve") {
		// 2G capacity with a 2G reserve leaves nothing: must be rejected.
		t.Errorf("2G capacity: err %v", err)
	}
	env2["PACE_CAPACITY"] = "6G"
	r = mustResolve(t, Default(), 0, "darwin", env2)
	if r.CapacityBytes != 6*G {
		t.Errorf("capacity %d", r.CapacityBytes)
	}
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Finding 38: the Linux kernel hard cap is on by default and can be turned
// off; the Linux tolerance is no longer the 5 % that assumed containers.
func TestHardCapKnob(t *testing.T) {
	r := mustResolve(t, Default(), 16*G, "linux", baseEnv())
	if !r.HardCap || r.Params.TolerancePermille != 100 {
		t.Errorf("default hard cap %v tolerance %d", r.HardCap, r.Params.TolerancePermille)
	}
	if !strings.Contains(r.Describe(), "hard cap") {
		t.Errorf("describe lacks the hard cap:\n%s", r.Describe())
	}
	r = mustResolve(t, mustParse(t, "[enforce]\nhard_cap = false\n"), 16*G, "linux", baseEnv())
	if r.HardCap {
		t.Error("hard_cap = false not applied")
	}
	if d := mustResolve(t, Default(), 16*G, "darwin", baseEnv()); strings.Contains(d.Describe(), "hard cap") {
		t.Error("darwin describes a linux-only knob")
	}
}

func TestUnknownKeys(t *testing.T) {
	_, err := Parse(`
bogus = 1
[classes.build]
solts = "2"
[pressure]
warn_avail = 3
`)
	if err == nil {
		t.Fatal("want error")
	}
	for _, k := range []string{"bogus", "classes.build.solts", "pressure.warn_avail"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("error %q does not name %q", err, k)
		}
	}
	if _, err := Parse(`mode = 3`); err == nil {
		t.Error("type mismatch accepted")
	}
	if _, err := Parse(`[[[`); err == nil {
		t.Error("syntax error accepted")
	}
}

func TestInvalidValues(t *testing.T) {
	tests := []struct {
		name, toml string
		env        map[string]string
		want       string // substring of the error
	}{
		{"mode", `mode = "loud"`, nil, `config: mode: "loud" is not one of observe, warn-only, enforce`},
		{"PACE_MODE", ``, map[string]string{"PACE_MODE": "x"}, `config: PACE_MODE: "x"`},
		{"capacity garbage", `capacity = "lots"`, nil, "config: capacity: invalid size"},
		{"capacity pct", `capacity = "50%"`, nil, "config: capacity: percentage not allowed"},
		{"capacity zero", `capacity = "0"`, nil, "config: capacity: \"0\" must be > 0"},
		{"capacity too big", `capacity = "64G"`, nil, "config: capacity: 64.0G exceeds physical memory 36.0G"},
		{"PACE_CAPACITY", ``, map[string]string{"PACE_CAPACITY": "x"}, "config: PACE_CAPACITY: invalid size"},
		{"reserve all", `reserve = "100%"`, nil, "config: reserve: 36.0G leaves nothing"},
		{"reserve zero", `reserve = "0"`, nil, "config: reserve: \"0\" must be > 0"},
		{"reserve pct range", `reserve = "120%"`, nil, "config: reserve: percentage \"120%\" must be in (0, 100]"},
		{"reserve empty part", `reserve = "2G,"`, nil, "config: reserve: empty part"},
		{"sample interval", `sample_interval = "fast"`, nil, "config: sample_interval: invalid duration"},
		{"sample zero", `sample_interval = "0"`, nil, "config: sample_interval: duration \"0\" must be positive"},
		{"sample negative", `sample_interval = "-1s"`, nil, "config: sample_interval: invalid duration"},
		{"fast > sample", `fast_interval = "2s"`, nil, "config: fast_interval: 2s must not exceed sample_interval 1s"},
		{"relative home", `home = "rel/dir"`, nil, `config: home: path "rel/dir" must be absolute`},
		{"warn pct range", "[pressure]\nwarn_avail_pct = 150.0", nil, "config: pressure.warn_avail_pct: 150 must be between 0 and 100"},
		{"crit negative", "[pressure]\ncrit_avail_pct = -1.0", nil, "config: pressure.crit_avail_pct: -1 must be between 0 and 100"},
		{"warn <= crit", "[pressure]\nwarn_avail_pct = 5.0", nil, "config: pressure.warn_avail_pct: 5 must be greater than crit_avail_pct 7"},
		{"nan", "[pressure]\npsi_warn_pct = nan", nil, "config: pressure.psi_warn_pct: must be a finite number"},
		{"grace", "[enforce]\ngrace = \"soon\"", nil, "config: enforce.grace: invalid duration"},
		{"critical grace > grace", "[enforce]\ncritical_grace = \"10s\"", nil, "config: enforce.critical_grace: 10s must not exceed grace 5s"},
		{"critical step > step", "[enforce]\ncritical_step = \"3s\"", nil, "config: enforce.critical_step: 3s must not exceed step 2s"},
		{"budget warn pct", "[enforce]\nbudget_warn_pct = 101.0", nil, "config: enforce.budget_warn_pct"},
		{"tolerance pct", "[enforce]\ntolerance_pct = -3.0", nil, "config: enforce.tolerance_pct"},
		{"min victim", "[enforce]\nmin_victim = \"big\"", nil, "config: enforce.min_victim: invalid size"},
		{"unmanaged priority", "[unmanaged]\npriority = 10", nil, "config: unmanaged.priority: 10 must be between 0 and 9"},
		{"unmanaged budget", "[unmanaged]\nbudget = \"x\"", nil, "config: unmanaged.budget: invalid size"},
		{"unmanaged protect path", "[unmanaged]\nprotect = [\"/usr/bin/x\"]", nil, "config: unmanaged.protect: \"/usr/bin/x\": give a process or executable name, not a path"},
		{"unmanaged protect empty", "[unmanaged]\nprotect = [\"\"]", nil, "config: unmanaged.protect"},
		{"scope root", "[unmanaged]\nscope_root_pid = -4", nil, "config: unmanaged.scope_root_pid: -4 must not be negative"},
		{"queue timeout", "[queue]\ntimeout = \"0s\"", nil, "config: queue.timeout: duration \"0s\" must be positive"},
		{"cpu order", "[cpu]\nwarn_load_per_core = 0.5", nil, "config: cpu.warn_load_per_core: 0.5 must be greater than ok_load_per_core 1"},
		{"cpu negative", "[cpu]\nok_load_per_core = -1.0", nil, "config: cpu.ok_load_per_core: -1 must be between 0 and 1000"},
		{"disk pct", "[disk]\nwarn_free = \"10%\"", nil, "config: disk.warn_free: percentage not allowed"},
		{"disk order", "[disk]\ncrit_free = \"20G\"", nil, "config: disk.warn_free: 10.0G must be greater than crit_free 20.0G"},
		{"disk path", "[disk]\npaths = [\"/\", \"rel\"]", nil, `config: disk.paths[1]: path "rel" must be absolute`},
		{"history negative", "[learn]\nhistory_size = -1", nil, "config: learn.history_size: -1 must not be negative"},
		{"min samples > history", "[learn]\nhistory_size = 2", nil, "config: learn.min_samples: 3 must not exceed history_size 2"},
		{"failopen slots", "[failopen]\nslots = -2", nil, "config: failopen.slots: -2 must not be negative"},
		{"failopen wait", "[failopen]\nwait = \"x\"", nil, "config: failopen.wait: invalid duration"},
		{"log size small", "[log]\nmax_size = \"10K\"", nil, "config: log.max_size: 10K must be at least 1M"},
		{"log keep", "[log]\nkeep = -1", nil, "config: log.keep: -1 must not be negative"},
		{"heartbeat", "[log]\nheartbeat = \"-\"", nil, "config: log.heartbeat: invalid duration"},
		{"class name", "[classes.Build]\nslots = \"1\"", nil, "config: classes.Build: invalid class name"},
		{"class estimate", "[classes.build]\nestimate = \"huge\"", nil, "config: classes.build.estimate: invalid size"},
		{"class estimate zero", "[classes.build]\nestimate = \"0\"", nil, "config: classes.build.estimate: \"0\" must be > 0"},
		{"class budget", "[classes.test]\nbudget = \"10%,\"", nil, "config: classes.test.budget: empty part"},
		{"class est > budget", "[classes.x]\nestimate = \"8G\"\nbudget = \"4G\"", nil, "config: classes.x.estimate: 8.0G exceeds the class budget 4.0G"},
		{"class slots", "[classes.build]\nslots = \"many\"", nil, "config: classes.build.slots: invalid slots \"many\""},
		{"class slots negative", "[classes.build]\nslots = \"-1\"", nil, "config: classes.build.slots: invalid slots \"-1\": must be >= 0"},
		{"class slots per pct", "[classes.build]\nslots = \"per:10%\"", nil, "config: classes.build.slots: invalid slots \"per:10%\""},
		{"class slots per zero", "[classes.build]\nslots = \"per:0\"", nil, "size must be > 0"},
		{"class wall", "[classes.build]\nwall_limit = \"forever\"", nil, "config: classes.build.wall_limit: invalid duration"},
		{"class noprog", "[classes.build]\nno_progress = \"-5m\"", nil, "config: classes.build.no_progress"},
		{"class priority", "[classes.build]\npriority = -1", nil, "config: classes.build.priority: -1 must be between 0 and 9"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParse(t, tc.toml)
			env := baseEnv()
			for k, v := range tc.env {
				env[k] = v
			}
			_, err := c.resolve(36*G, "darwin", envOf(env))
			if err == nil {
				t.Fatalf("want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q\n does not contain %q", err, tc.want)
			}
		})
	}
}

func TestMultipleErrorsReported(t *testing.T) {
	c := mustParse(t, "mode = \"x\"\n[enforce]\ngrace = \"y\"\n[classes.build]\npriority = 12")
	_, err := c.resolve(36*G, "darwin", envOf(baseEnv()))
	if err == nil {
		t.Fatal("want error")
	}
	for _, k := range []string{"config: mode:", "config: enforce.grace:", "config: classes.build.priority:"} {
		if !strings.Contains(err.Error(), k) {
			t.Errorf("missing %q in %q", k, err)
		}
	}
}

func TestParseSize(t *testing.T) {
	C := 32 * G
	tests := []struct {
		in   string
		want int64
		err  bool
	}{
		{"4G", 4 * G, false},
		{"512M", 512 * units.MiB, false},
		{"1.5G", 3 * G / 2, false},
		{"30%", pctOf(C, 30), false},
		{"12.5%", 4 * G, false},
		{"4G,30%", pctOf(C, 30), false},
		{"30%,4G", pctOf(C, 30), false},
		{"16G,30%", 16 * G, false},
		{" 4G , 10% ", 4 * G, false},
		{"1G,2G,3G", 3 * G, false},
		{"100%", C, false},
		{"0", 0, false},
		{"", 0, true},
		{"4G,", 0, true},
		{",", 0, true},
		{"0%", 0, true},
		{"101%", 0, true},
		{"-5%", 0, true},
		{"abc%", 0, true},
		{"4X", 0, true},
		{"-4G", 0, true},
	}
	for _, tc := range tests {
		got, err := ParseSize(tc.in, C)
		if (err != nil) != tc.err {
			t.Errorf("ParseSize(%q) err = %v, want err %v", tc.in, err, tc.err)
			continue
		}
		if !tc.err && got != tc.want {
			t.Errorf("ParseSize(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseSlots(t *testing.T) {
	tests := []struct {
		in   string
		C    int64
		want int
		err  bool
	}{
		{"3", 36 * G, 3, false},
		{"0", 36 * G, 0, false},
		{"per:16G", 36 * G, 2, false},
		{"per:16G", 8 * G, 1, false},
		{"per:8G", 36 * G, 4, false},
		{"per:8G", 64 * G, 8, false},
		{"per:1.5G", 3 * G, 2, false},
		{" 2 ", 36 * G, 2, false},
		{"", 36 * G, 0, true},
		{"-1", 36 * G, 0, true},
		{"x", 36 * G, 0, true},
		{"per:", 36 * G, 0, true},
		{"per:0", 36 * G, 0, true},
		{"per:10%", 36 * G, 0, true},
		{"1.5", 36 * G, 0, true},
	}
	for _, tc := range tests {
		got, err := ParseSlots(tc.in, tc.C)
		if (err != nil) != tc.err {
			t.Errorf("ParseSlots(%q) err = %v, want err %v", tc.in, err, tc.err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseSlots(%q, %d) = %d, want %d", tc.in, tc.C, got, tc.want)
		}
	}
}

func TestClassMerge(t *testing.T) {
	c := mustParse(t, `
[classes.build]
budget = "20G"
priority = 7

[classes.lint]
estimate = "512M"
slots = "4"
wall_limit = "none"
no_progress = "off"

[classes.model]
budget = "100%"
`)
	C := 36 * G
	r := mustResolve(t, c, C, "darwin", baseEnv())
	b := class(t, r, "build")
	if b.BudgetBytes != 20*G || b.Priority != 7 || b.Slots != 2 || b.EstimateBytes != max64(2*G, pctOf(C, 15)) || b.WallLimitMS != 3600000 {
		t.Errorf("build %+v", b)
	}
	l := class(t, r, "lint")
	def := class(t, r, "default")
	if l.EstimateBytes != 512*units.MiB || l.Slots != 4 || l.WallLimitMS != 0 || l.NoProgressMS != 0 ||
		l.BudgetBytes != def.BudgetBytes || l.Priority != def.Priority {
		t.Errorf("lint %+v (default %+v)", l, def)
	}
	// Budget capped at capacity - reserve.
	m := class(t, r, "model")
	if m.BudgetBytes != C-r.Params.ReserveBytes {
		t.Errorf("model budget %d, want cap %d", m.BudgetBytes, C-r.Params.ReserveBytes)
	}
	// Sorted, with the new class inserted.
	for i := 1; i < len(r.Params.Classes); i++ {
		if r.Params.Classes[i-1].Name >= r.Params.Classes[i].Name {
			t.Fatalf("classes not sorted: %v", r.Params.Classes)
		}
	}
	if len(r.Params.Classes) != 6 {
		t.Errorf("want 6 classes, got %d", len(r.Params.Classes))
	}

	// Small machine: default estimates exceeding the capped budget are capped
	// (not an error, since the user did not set them).
	env := baseEnv()
	env["PACE_CAPACITY"] = "4G"
	r = mustResolve(t, Default(), 36*G, "darwin", env)
	for _, cp := range r.Params.Classes {
		if cp.BudgetBytes > 2*G || cp.EstimateBytes > cp.BudgetBytes || cp.EstimateBytes <= 0 {
			t.Errorf("small machine class %+v", cp)
		}
	}
}

func TestHashStability(t *testing.T) {
	a := `
# a comment
mode = "enforce"
reserve = "4G,10%"
[enforce]
grace = "5s"
[classes.build]
budget = "8G"
slots = "2"
[classes.lint]
estimate = "1G"
`
	b := `
[classes.lint]
estimate = "1024M"   # same size, different spelling
[classes.build]
slots = "2"
budget = "8192M"
[enforce]
grace = "5000ms"
`
	b = "reserve = \"10%,4G\"\n" + b // top-level keys must precede tables
	env := baseEnv()
	ra := mustResolve(t, mustParse(t, a), 36*G, "darwin", env)
	rb := mustResolve(t, mustParse(t, b), 36*G, "darwin", env)
	if ra.Hash != rb.Hash {
		t.Errorf("equivalent configs hash differently:\n%s\n%s", ra.Hash, rb.Hash)
	}
	// Repeated resolution is identical.
	for i := 0; i < 20; i++ {
		if r := mustResolve(t, mustParse(t, a), 36*G, "darwin", env); r.Hash != ra.Hash {
			t.Fatal("hash not deterministic")
		}
	}
	// Source is excluded.
	rs := ra
	rs.Source = "/some/file"
	if rs.computeHash() != ra.Hash {
		t.Error("Source changes hash")
	}
	// A real change changes the hash.
	rc := mustResolve(t, mustParse(t, strings.Replace(a, `grace = "5s"`, `grace = "4s"`, 1)), 36*G, "darwin", env)
	if rc.Hash == ra.Hash {
		t.Error("changed grace did not change hash")
	}
	// Machine size changes the effective config, hence the hash.
	rm := mustResolve(t, mustParse(t, a), 32*G, "darwin", env)
	if rm.Hash == ra.Hash {
		t.Error("different machine, same hash")
	}
}

func TestSocketLength(t *testing.T) {
	long := "/" + strings.Repeat("d", 120)
	env := baseEnv()
	env["PACE_HOME"] = long
	_, err := Default().resolve(36*G, "darwin", envOf(env))
	if err == nil || !strings.Contains(err.Error(), "config: PACE_HOME: socket path") || !strings.Contains(err.Error(), "at most 103") {
		t.Errorf("PACE_HOME long: %v", err)
	}
	env = baseEnv()
	env["PACE_SOCK"] = "/" + strings.Repeat("s", 103) // 104 bytes
	_, err = Default().resolve(36*G, "darwin", envOf(env))
	if err == nil || !strings.Contains(err.Error(), "config: PACE_SOCK: socket path") || !strings.Contains(err.Error(), "is 104 bytes") {
		t.Errorf("PACE_SOCK long: %v", err)
	}
	env["PACE_SOCK"] = "/" + strings.Repeat("s", 102) // exactly 103: ok
	if _, err := Default().resolve(36*G, "darwin", envOf(env)); err != nil {
		t.Errorf("103-byte socket rejected: %v", err)
	}
	// A long HOME on macOS breaks the default socket.
	env = map[string]string{"HOME": "/Users/" + strings.Repeat("h", 80)}
	_, err = Default().resolve(36*G, "darwin", envOf(env))
	if err == nil || !strings.Contains(err.Error(), "config: socket: socket path") {
		t.Errorf("long HOME: %v", err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	// Missing default file: defaults, no error.
	env := map[string]string{"HOME": dir}
	c, src, err := load("", envOf(env))
	if err != nil || src != "defaults" || c.Mode != "" {
		t.Fatalf("missing default: %+v %q %v", c, src, err)
	}
	// XDG_CONFIG_HOME location.
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "pace"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(xdg, "pace", "config.toml")
	if err := os.WriteFile(p, []byte("mode = \"observe\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env["XDG_CONFIG_HOME"] = xdg
	c, src, err = load("", envOf(env))
	if err != nil || src != p || c.Mode != "observe" {
		t.Fatalf("xdg: %+v %q %v", c, src, err)
	}
	// ~/.config fallback.
	delete(env, "XDG_CONFIG_HOME")
	home := filepath.Join(dir, ".config", "pace")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	hp := filepath.Join(home, "config.toml")
	if err := os.WriteFile(hp, []byte("mode = \"warn-only\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, src, err = load("", envOf(env))
	if err != nil || src != hp || c.Mode != "warn-only" {
		t.Fatalf("home: %+v %q %v", c, src, err)
	}
	// PACE_CONFIG explicit and missing: error.
	env["PACE_CONFIG"] = filepath.Join(dir, "nope.toml")
	if _, _, err := load("", envOf(env)); err == nil {
		t.Error("missing PACE_CONFIG file accepted")
	}
	// Explicit path wins over PACE_CONFIG.
	c, src, err = load(p, envOf(env))
	if err != nil || src != p || c.Mode != "observe" {
		t.Fatalf("explicit: %+v %q %v", c, src, err)
	}
	// Unknown key error names the file and key.
	bad := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(bad, []byte("[log]\nkep = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = load(bad, envOf(env))
	if err == nil || !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "log.kep") {
		t.Errorf("bad file: %v", err)
	}
}

func TestLoadResolved(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(p, []byte("mode = \"observe\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PACE_HOME", "/tmp/pace-cfgtest")
	t.Setenv("PACE_SOCK", "")
	t.Setenv("PACE_MODE", "")
	t.Setenv("PACE_CAPACITY", "")
	r, err := LoadResolved(p, 16*G, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	if r.Source != p || r.Params.Mode != "observe" || r.Home != "/tmp/pace-cfgtest" {
		t.Errorf("%q %q %q", r.Source, r.Params.Mode, r.Home)
	}
	if _, err := LoadResolved(filepath.Join(dir, "missing.toml"), 16*G, "darwin"); err == nil {
		t.Error("missing explicit file accepted")
	}
}

// uncommentExample turns every "# " setting line into a live line.
func uncommentExample(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "# ") {
			l = strings.TrimPrefix(l, "# ")
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

func TestExampleTOML(t *testing.T) {
	ex := ExampleTOML()
	for _, plat := range []string{"darwin", "linux"} {
		def := mustResolve(t, Default(), 36*G, plat, baseEnv())
		// As shipped (all comments) it parses to the defaults.
		r := mustResolve(t, mustParse(t, ex), 36*G, plat, baseEnv())
		if r.Hash != def.Hash {
			t.Errorf("%s: commented example differs from defaults", plat)
		}
		// Uncommenting every setting line reproduces the defaults exactly.
		un := uncommentExample(ex)
		c, err := Parse(un)
		if err != nil {
			t.Fatalf("uncommented example does not parse: %v\n%s", err, un)
		}
		r = mustResolve(t, c, 36*G, plat, baseEnv())
		if r.Hash != def.Hash {
			t.Errorf("%s: uncommented example differs from defaults:\n%s\nvs\n%s", plat, r.Describe(), def.Describe())
		}
		if len(c.Classes) != 5 {
			t.Errorf("example lists %d classes", len(c.Classes))
		}
	}
	// The "##" example class block is valid TOML when uncommented.
	idx := strings.Index(ex, "## [classes.lint]")
	if idx < 0 {
		t.Fatal("example class missing")
	}
	block := strings.ReplaceAll(ex[idx:], "## ", "")
	c, err := Parse(block)
	if err != nil {
		t.Fatalf("example class block: %v", err)
	}
	if _, ok := c.Classes["lint"]; !ok {
		t.Error("lint class not parsed")
	}
}

func TestDescribe(t *testing.T) {
	env := baseEnv()
	env["PACE_CAPACITY"] = "8G"
	r := mustResolve(t, mustParse(t, "[unmanaged]\nscope_root_pid = 42\nbudget = \"1G\""), 36*G, "darwin", env)
	r.Source = "/x/config.toml"
	d := r.Describe()
	for _, want := range []string{
		"source: /x/config.toml", "platform: darwin", "mode", "enforce",
		"8.0G (override; machine 36.0G)", "kernel level", "off",
		"descendants of pid 42", "budget 1.0G", "NAME", "build", "browser", "model", "unlimited",
		"4h", "hash sha256:", "warn when available < 15%", "critical < 7%", "kill above budget +10%",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("Describe missing %q:\n%s", want, d)
		}
	}
	if d != r.Describe() {
		t.Error("Describe not deterministic")
	}
}
