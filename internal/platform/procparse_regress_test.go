package platform

// Portable regression tests for the Linux review findings 41, 43 and 44
// (fixture procfs trees; they run on macOS too).

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Finding 44: a restarted daemon (new scanner) after a wall-clock step must
// see the same StartMS, so persisted unit keys "pg:<pgid>:<leaderStartMS>"
// still match and frozen units are found again.
func TestScannerBtimeAnchorSurvivesRestart(t *testing.T) {
	root := fixtureRoot(t)
	bootID := filepath.Join(root, "sys", "kernel", "random", "boot_id")
	if err := os.MkdirAll(filepath.Dir(bootID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootID, []byte("11111111-2222-3333-4444-555555555555\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	anchor := filepath.Join(t.TempDir(), "run", "btime")
	scan := func() int64 {
		t.Helper()
		s := fixtureScanner(root, FootprintAnon)
		s.btimeFile = anchor
		got, err := s.list(1000)
		if err != nil {
			t.Fatal(err)
		}
		p := findProcIn(got, 100)
		if p == nil {
			t.Fatal("pid 100 missing")
		}
		return p.StartMS
	}
	first := scan()
	if first != fixtureBootMS+123450 {
		t.Fatalf("first StartMS = %d", first)
	}
	// The clock steps by +1 s and the daemon restarts.
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1700000001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scan(); got != first {
		t.Errorf("after restart + clock step StartMS = %d, want %d", got, first)
	}
	// Without the anchor the step shows (what every restart did before).
	got, _ := fixtureScanner(root, FootprintAnon).list(1000)
	if p := findProcIn(got, 100); p == nil || p.StartMS != first+1000 {
		t.Errorf("unanchored StartMS = %+v", p)
	}
	// A new boot (different boot_id) replaces the stale anchor.
	if err := os.WriteFile(bootID, []byte("99999999-2222-3333-4444-555555555555\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scan(); got != first+1000 {
		t.Errorf("new boot StartMS = %d, want %d", got, first+1000)
	}
	b, _ := os.ReadFile(anchor)
	if id, ms, ok := parseBtimeAnchor(b); !ok || id != "99999999-2222-3333-4444-555555555555" || ms != 1700000001000 {
		t.Errorf("anchor = %q", b)
	}
	if st, err := os.Stat(anchor); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("anchor mode: %v %v", st, err)
	}
	// Garbage is replaced.
	if err := os.WriteFile(anchor, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scan(); got != first+1000 {
		t.Errorf("garbage anchor StartMS = %d", got)
	}
	// No boot_id: no anchoring, btime as read.
	if err := os.Remove(bootID); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1700000005\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := scan(); got != first+5000 {
		t.Errorf("no boot_id StartMS = %d", got)
	}
}

func TestParseBtimeAnchor(t *testing.T) {
	b := formatBtimeAnchor("abc", 1700000000000)
	if id, ms, ok := parseBtimeAnchor(b); !ok || id != "abc" || ms != 1700000000000 {
		t.Errorf("round trip %q: %q %d %v", b, id, ms, ok)
	}
	for _, bad := range []string{"", "pace-btime 1 abc", "pace-btime 2 abc 5", "pace-btime 1 abc -5", "pace-btime 1 abc x", "x y z w"} {
		if _, _, ok := parseBtimeAnchor([]byte(bad)); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Finding 41: CapEff is relative to our user namespace; only the identity
// map of the initial namespace makes it meaningful to the scheduler.
func TestParseUIDMapIdentity(t *testing.T) {
	for in, want := range map[string]bool{
		"         0          0 4294967295\n": true,
		"0 0 4294967295":                     true,
		"         0          0          1\n": false, // unshare -r
		"         0       1000          1\n": false, // rootless container
		"0 0 4294967295\n1 1 1\n":            false,
		"":                                   false,
	} {
		if got := parseUIDMapIdentity([]byte(in)); got != want {
			t.Errorf("parseUIDMapIdentity(%q) = %v", in, got)
		}
	}
}

// procStatFixture builds /proc/stat with per-CPU busy/idle ticks.
func procStatFixture(running, blocked int, busy, idle []uint64) []byte {
	var sb strings.Builder
	sb.WriteString("cpu  1 2 3 4 5 6 7 8 0 0\n")
	for i := range busy {
		fmt.Fprintf(&sb, "cpu%d %d 0 0 %d 0 0 0 0 0 0\n", i, busy[i], idle[i])
	}
	fmt.Fprintf(&sb, "intr 1 2 3\nctxt 5\nbtime 1700000000\nprocesses 9\nprocs_running %d\nprocs_blocked %d\n", running, blocked)
	return []byte(sb.String())
}

func TestParseProcStatLoad(t *testing.T) {
	in := []byte("cpu  10 1 2 30 4 5 6 7 0 0\ncpu0 10 1 2 30 4 5 6 7 0 0\ncpu3 1 1 1 1 1 1 1 1 0 0\ncpuX 1\nprocs_running 7\nprocs_blocked 9\n")
	got, running, online := parseProcStatLoad(in, nil)
	if running != 7 || online != 2 || len(got) != 4 {
		t.Fatalf("running %d online %d len %d", running, online, len(got))
	}
	if got[0] != (cpuTicks{busy: 10 + 1 + 2 + 5 + 6 + 7, total: 10 + 1 + 2 + 5 + 6 + 7 + 30 + 4, ok: true}) || got[1].ok || !got[3].ok {
		t.Errorf("ticks = %+v", got)
	}
	// Reuse clears stale entries.
	got, running, online = parseProcStatLoad([]byte("cpu1 1 0 0 1 0 0 0 0\n"), got)
	if running != -1 || online != 1 || got[0].ok || !got[1].ok || got[3].ok {
		t.Errorf("reuse: %+v %d %d", got, running, online)
	}
	if n := testing.AllocsPerRun(50, func() { got, _, _ = parseProcStatLoad(in, got) }); n != 0 {
		t.Errorf("parseProcStatLoad allocates %.1f times", n)
	}
}

func TestParseSchedstat(t *testing.T) {
	in := []byte("version 15\ntimestamp 4295\ncpu0 0 0 100 50 60 30 9000000 2000000 77\ndomain0 3 1 2 3\ncpu2 0 0 1 1 1 1 5 7 1\n")
	d, ok := parseSchedstat(in, nil)
	if !ok || len(d) != 3 || d[0] != 2000000 || d[1] != -1 || d[2] != 7 {
		t.Errorf("schedstat = %v %v", d, ok)
	}
	for _, bad := range []string{"version 14\ncpu0 0 0 1 1 1 1 5 7 1\n", "cpu0 0 0 1 1 1 1 5 7 1\n", "version 15\ncpu0 1 2\n"} {
		if _, ok := parseSchedstat([]byte(bad), nil); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Finding 43: in a --cpuset-cpus=0-1 container on a 16-CPU host whose load
// is 6, the old per-core load was 6/NCPU(2) = 3.0 (CPU "high" at 1.5) though
// our CPUs were idle. Host-wide counts must be per online CPU, and tasks in
// uninterruptible sleep (I/O waits) must not count.
func TestLoadEstimatorHostWideAndDState(t *testing.T) {
	busy, idle := make([]uint64, 16), make([]uint64, 16)
	t0 := time.Unix(1000, 0)
	e := loadEstimator{method: LoadAuto}
	got, ok := e.update(t0, procStatFixture(7, 0, busy, idle), nil, 6000)
	if !ok || got != 375 {
		t.Errorf("seed per CPU = %d,%v want 375 (6.0 / 16 online)", got, ok)
	}
	// I/O-bound: loadavg 8 from 8 D-state tasks, only us running.
	e = loadEstimator{method: LoadAuto}
	_, _ = e.update(t0, procStatFixture(1, 8, busy, idle), nil, 8000)
	var v int64
	for i := 1; i <= 600; i++ { // 10 minutes at 1 s
		v, _ = e.update(t0.Add(time.Duration(i)*time.Second), procStatFixture(1, 8, busy, idle), nil, 8000)
	}
	if v > 5 || e.source != "procs_running" {
		t.Errorf("D-state load per CPU after 10 min = %d (%s), want ~0", v, e.source)
	}
	// Converges like load1: one time constant covers 1-1/e of a step.
	e = loadEstimator{method: LoadAuto}
	_, _ = e.update(t0, procStatFixture(1, 0, busy, idle), nil, 0)
	for i := 1; i <= 60; i++ {
		for c := range busy {
			busy[c] += 100
		}
		v, _ = e.update(t0.Add(time.Duration(i)*time.Second), procStatFixture(33, 0, busy, idle), nil, 0)
	}
	if want := int64(2000 * (1 - math.Exp(-1))); v < want-20 || v > want+20 {
		t.Errorf("after 60 s of 32 runnable on 16 CPUs: %d want ~%d", v, want)
	}
	// LoadAvg method: host load per online CPU, no smoothing.
	e = loadEstimator{method: LoadAvg}
	if v, ok := e.update(t0, procStatFixture(1, 0, busy, idle), nil, 6000); !ok || v != 375 || e.source != "loadavg" {
		t.Errorf("loadavg per CPU = %d,%v", v, ok)
	}
	// Nothing readable: no estimate.
	e = loadEstimator{method: LoadAuto}
	if _, ok := e.update(t0, nil, nil, -1); ok {
		t.Errorf("estimate from nothing")
	}
}

// With /proc/schedstat the load is the runqueue occupancy of OUR CPUs only.
func TestLoadEstimatorSchedstatAffinity(t *testing.T) {
	const n = 16
	busy, idle := make([]uint64, n), make([]uint64, n)
	delay := make([]int64, n)
	ss := func() []byte {
		var sb strings.Builder
		sb.WriteString("version 15\ntimestamp 1\n")
		for i := 0; i < n; i++ {
			fmt.Fprintf(&sb, "cpu%d 0 0 1 1 1 1 0 %d 1\ndomain0 ffff 1 2 3\n", i, delay[i])
		}
		return []byte(sb.String())
	}
	e := loadEstimator{method: LoadAuto, inSet: func(id int) bool { return id < 2 }} // --cpuset-cpus=0-1
	t0 := time.Unix(1000, 0)
	if v, ok := e.update(t0, procStatFixture(13, 0, busy, idle), ss(), 6000); !ok || v != 375 {
		t.Fatalf("seed = %d,%v", v, ok)
	}
	var v int64
	for s := 1; s <= 600; s++ {
		// CPUs 0-1 idle; CPUs 4-9 each run one busy task with one waiting.
		for i := 0; i < n; i++ {
			if i >= 4 && i <= 9 {
				busy[i] += 100
				delay[i] += int64(time.Second)
			} else {
				idle[i] += 100
			}
		}
		v, _ = e.update(t0.Add(time.Duration(s)*time.Second), procStatFixture(13, 0, busy, idle), ss(), 6000)
	}
	if v > 5 || e.source != "schedstat" {
		t.Errorf("our idle CPUs: load per CPU %d (%s), want ~0", v, e.source)
	}
	// Now our two CPUs are saturated: 1 running + 2 waiting on each.
	for s := 601; s <= 1200; s++ {
		for i := 0; i < 2; i++ {
			busy[i] += 100
			delay[i] += 2 * int64(time.Second)
		}
		v, _ = e.update(t0.Add(time.Duration(s)*time.Second), procStatFixture(13, 0, busy, idle), ss(), 6000)
	}
	if v < 2950 || v > 3000 {
		t.Errorf("our saturated CPUs: load per CPU %d, want ~3000", v)
	}
}

// Review of finding 43: without /proc/schedstat (CONFIG_SCHEDSTATS off, the
// Docker Desktop kernel) a saturated --cpuset-cpus=0-1 container on an
// otherwise idle 16-CPU host read procs_running per online CPU, 6/16 = 375,
// below the 1500 warn level. Our CPUs' busy ticks plus their share of the
// waiting tasks must give ~3.0 per CPU; busy CPUs outside our mask still 0.
func TestLoadEstimatorProcsRunningAffinity(t *testing.T) {
	const n = 16
	run := func(ours func(int) bool, hot func(int) bool) (int64, string) {
		busy, idle := make([]uint64, n), make([]uint64, n)
		e := loadEstimator{method: LoadAuto, inSet: ours}
		t0 := time.Unix(1000, 0)
		var v int64
		for s := 0; s <= 600; s++ {
			for i := 0; i < n; i++ {
				if hot(i) {
					busy[i] += 100
				} else {
					idle[i] += 100
				}
			}
			v, _ = e.update(t0.Add(time.Duration(s)*time.Second), procStatFixture(7, 0, busy, idle), nil, 6000)
		}
		return v, e.source
	}
	first2 := func(id int) bool { return id < 2 }
	if v, src := run(first2, first2); v < 2950 || v > 3000 || src != "procs_running" {
		t.Errorf("saturated cpuset: load per CPU %d (%s), want ~3000", v, src)
	}
	// The 6 runnable tasks run on CPUs 4-9 (one each, none waiting).
	if v, _ := run(first2, func(id int) bool { return id >= 4 && id <= 9 }); v > 5 {
		t.Errorf("our idle CPUs: load per CPU %d, want ~0", v)
	}
	// No affinity restriction: 6 tasks on 16 CPUs, 2 busy, 4 waiting.
	if v, _ := run(nil, first2); v < 360 || v > 380 {
		t.Errorf("whole host: load per CPU %d, want ~375", v)
	}
}

// Review of finding 41: a kernel without user namespaces has no uid_map;
// that is the initial namespace, not "untrusted".
func TestInitialUserNSFrom(t *testing.T) {
	if !initialUserNSFrom(nil, syscall.ENOENT) {
		t.Error("missing uid_map (CONFIG_USER_NS=n) not treated as the initial namespace")
	}
	if initialUserNSFrom(nil, syscall.EACCES) {
		t.Error("unreadable uid_map trusted")
	}
	if !initialUserNSFrom([]byte("         0          0 4294967295\n"), nil) {
		t.Error("identity map not initial")
	}
	if initialUserNSFrom([]byte("         0       1000          1\n"), nil) {
		t.Error("rootless map treated as initial")
	}
}

// Review of finding 45 case 3: the ioprio mark records whether the policy
// was changed, the original policy and whether nice was raised from 0, so a
// restore after a daemon restart can leave user BATCH/IDLE threads alone.
func TestBgMarkRoundTrip(t *testing.T) {
	seen := map[int]bool{}
	for _, c := range []struct{ sched, batch, from0 bool }{
		{false, false, false}, {true, false, false}, {true, true, false}, {true, false, true}, {true, true, true},
	} {
		v := bgMarkValue(c.sched, c.batch, c.from0)
		if v>>13 != 3 || v&0x1fff == 0 || v&0x1fff > 7 || seen[v] {
			t.Errorf("mark %+v = %#x: not a distinct idle-class level 1-7", c, v)
		}
		seen[v] = true
		s, b, z, ours := parseBgMark(v)
		if !ours || s != c.sched || b != c.batch || z != c.from0 {
			t.Errorf("parseBgMark(bgMarkValue(%+v)) = %v %v %v %v", c, s, b, z, ours)
		}
	}
	// ionice -c3 (level 0), best-effort and none are not ours; level 7 (the
	// earlier mark) is ours with the policy changed from NORMAL.
	for _, v := range []int{3 << 13, 2<<13 | 4, 0, 3<<13 | 1<<3} {
		if _, _, _, ours := parseBgMark(v); ours {
			t.Errorf("%#x taken for our mark", v)
		}
	}
	if s, b, z, ours := parseBgMark(3<<13 | 7); !ours || !s || b || z {
		t.Errorf("legacy mark 7 = %v %v %v %v", s, b, z, ours)
	}
}
