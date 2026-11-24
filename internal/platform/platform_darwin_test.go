package platform

import (
	"encoding/binary"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// pure helpers

func TestNormalizeLevel(t *testing.T) {
	for _, c := range []struct {
		in   uint32
		want int
	}{{0, 0}, {1, 1}, {2, 2}, {3, 0}, {4, 4}, {5, 0}, {8, 0}, {math.MaxUint32, 0}} {
		if got := normalizeLevel(c.in); got != c.want {
			t.Errorf("normalizeLevel(%d)=%d want %d", c.in, got, c.want)
		}
	}
}

func TestAvailFromLevel(t *testing.T) {
	const mem = int64(38654705664)
	for _, c := range []struct {
		total int64
		pct   uint32
		want  int64
	}{
		{100, 50, 50},
		{mem, 0, 0},
		{mem, 100, mem},
		{mem, 57, mem * 57 / 100},
		{mem, 150, mem}, // clamped
		{101, 50, 50},   // floor
		{199, 99, 197},  // 199*99/100 = 197.01
		{0, 50, 0},
		{-5, 50, 0},
		{1 << 62, 99, -1}, // -1: exact floor(total*pct/100) via big.Int
		{math.MaxInt64, 100, math.MaxInt64},
		{math.MaxInt64, 37, -1},
	} {
		if c.want == -1 {
			p := uint32(c.pct)
			if p > 100 {
				p = 100
			}
			v := new(big.Int).Mul(big.NewInt(c.total), big.NewInt(int64(p)))
			c.want = v.Div(v, big.NewInt(100)).Int64()
		}
		if got := availFromLevel(c.total, c.pct); got != c.want {
			t.Errorf("availFromLevel(%d,%d)=%d want %d", c.total, c.pct, got, c.want)
		}
	}
}

func TestTicksToMS(t *testing.T) {
	const tb = 24_000_000
	for _, c := range []struct {
		ticks, freq uint64
		want        int64
	}{
		{0, tb, 0},
		{1, tb, 0},
		{23_999, tb, 0},
		{24_000, tb, 1},
		{tb, tb, 1000},
		{23_895_157, tb, 995}, // measured 1 s burn
		{tb * 3600, tb, 3_600_000},
		{1_000_000_000, 1_000_000_000, 1000}, // Intel: ticks are ns
		{1_500_000, 1_000_000_000, 1},
		{5, 0, 0}, // unknown frequency
	} {
		if got := ticksToMS(c.ticks, c.freq); got != c.want {
			t.Errorf("ticksToMS(%d,%d)=%d want %d", c.ticks, c.freq, got, c.want)
		}
	}
	// no overflow at the extremes
	if got := ticksToMS(math.MaxUint64, tb); got <= 0 {
		t.Errorf("ticksToMS(max) overflowed: %d", got)
	}
	if got := ticksToMS(math.MaxUint64, 1); got != 1<<62 {
		t.Errorf("ticksToMS(max,1) = %d, want clamp 1<<62", got)
	}
}

func TestMapStat(t *testing.T) {
	for _, c := range []struct {
		in   int8
		want byte
	}{{pStatIdl, 'S'}, {pStatRun, 'R'}, {pStatSleep, 'S'}, {pStatStop, 'T'}, {pStatZomb, 'Z'}, {0, 'S'}, {9, 'S'}, {-1, 'S'}} {
		if got := mapStat(c.in); got != c.want {
			t.Errorf("mapStat(%d)=%c want %c", c.in, got, c.want)
		}
	}
}

func TestParseLoadavg(t *testing.T) {
	mk := func(ld0 uint32, fscale uint64, n int) []byte {
		b := make([]byte, n)
		if n >= 4 {
			binary.LittleEndian.PutUint32(b, ld0)
		}
		if n >= 24 {
			binary.LittleEndian.PutUint64(b[16:], fscale)
		}
		return b
	}
	for _, c := range []struct {
		name    string
		buf     []byte
		want    int64
		wantErr bool
	}{
		{"one", mk(2048, 2048, 24), 1000, false},
		{"fraction", mk(3*2048+512, 2048, 24), 3250, false},
		{"zero", mk(0, 2048, 24), 0, false},
		{"big", mk(math.MaxUint32, 2048, 24), int64(math.MaxUint32) * 1000 / 2048, false},
		{"short", mk(2048, 2048, 16), 0, true},
		{"fscale0", mk(2048, 0, 24), 0, true},
		{"fscaleNeg", mk(2048, math.MaxUint64, 24), 0, true},
	} {
		got, err := parseLoadavg(c.buf)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("%s: parseLoadavg=%d,%v want %d,err=%v", c.name, got, err, c.want, c.wantErr)
		}
	}
}

func TestParseSwap(t *testing.T) {
	mk := func(total, avail, used uint64, n int) []byte {
		b := make([]byte, 32)
		binary.LittleEndian.PutUint64(b[0:], total)
		binary.LittleEndian.PutUint64(b[8:], avail)
		binary.LittleEndian.PutUint64(b[16:], used)
		return b[:n]
	}
	tot, used, err := parseSwap(mk(9216<<20, 1267<<20, 7949<<20, 32))
	if err != nil || tot != 9216<<20 || used != 7949<<20 {
		t.Errorf("parseSwap = %d,%d,%v", tot, used, err)
	}
	if _, _, err := parseSwap(mk(0, 0, 0, 32)); err != nil {
		t.Errorf("no swap: %v", err)
	}
	if _, _, err := parseSwap(mk(1, 1, 1, 20)); err == nil {
		t.Error("short buffer accepted")
	}
	if _, _, err := parseSwap(mk(math.MaxUint64, 0, 0, 32)); err == nil {
		t.Error("implausible total accepted")
	}
}

func TestHasSystemPrefix(t *testing.T) {
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder", true},
		{"/usr/libexec/trustd", true},
		{"/usr/sbin/cfprefsd", true},
		{"/sbin/launchd", true},
		{"/Library/Apple/System/Library/foo", true},
		{"/bin/zsh", false},
		{"/usr/bin/ssh", false},
		{"/bin/sleep", false},
		{"/usr/local/bin/node", false},
		{"/Library/Application Support/x", false},
		{"/Applications/Safari.app/Contents/MacOS/Safari", false},
		{"/Systemx/evil", false},
		{"/sbin", false},
		{"/system/Library/x", false}, // case matters (sealed volume paths are exact)
		{"", false},
		{"System/Library/x", false},
	} {
		if got := hasSystemPrefix(c.path); got != c.want {
			t.Errorf("hasSystemPrefix(%q)=%v want %v", c.path, got, c.want)
		}
	}
}

func TestClassifySystem(t *testing.T) {
	const fin = "/System/Library/CoreServices/Finder.app/Contents/MacOS/Finder"
	for _, c := range []struct {
		name string
		cs   uint32
		exe  string
		kerr error
		want bool
	}{
		{"platform+prefix", csPlatformBinary, fin, nil, true},
		{"platform+prefix+otherflags", csPlatformBinary | 0x1 | 0x300, "/usr/libexec/x", nil, true},
		{"platform outside prefix (zsh)", csPlatformBinary, "/bin/zsh", nil, false},
		{"prefix but not platform (spoof)", 0x1, fin, nil, false},
		{"neither", 0, "/usr/local/bin/node", nil, false},
		{"EPERM wins", 0, "/usr/local/bin/x", unix.EPERM, true},
		{"EPERM wrapped", 0, "", errors.Join(errors.New("x"), unix.EPERM), true},
		{"ESRCH not system", 0, "", unix.ESRCH, false},
		{"empty exe platform", csPlatformBinary, "", nil, false},
	} {
		if got := classifySystem(c.cs, c.exe, c.kerr); got != c.want {
			t.Errorf("%s: classifySystem=%v want %v", c.name, got, c.want)
		}
	}
}

// procargs builds a KERN_PROCARGS2 buffer.
func procargs(argc int32, exe string, pad int, argv, env, apple []string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(argc))
	b = append(b, exe...)
	b = append(b, make([]byte, 1+pad)...)
	for _, s := range argv {
		b = append(append(b, s...), 0)
	}
	for _, s := range env {
		b = append(append(b, s...), 0)
	}
	b = append(b, 0)
	for _, s := range apple {
		b = append(append(b, s...), 0)
	}
	return b
}

func TestProcArgsEnv(t *testing.T) {
	long := strings.Repeat("x", maxTagLen+1)
	ok := strings.Repeat("y", maxTagLen)
	for _, c := range []struct {
		name  string
		buf   []byte
		want  string
		found bool
	}{
		{"found", procargs(2, "/x/y", 3, []string{"y", "a"}, []string{"FOO=1", "PACE_JOB_ID=j-42", "BAR=2"}, []string{"ptr_munge="}), "j-42", true},
		{"first env", procargs(1, "/x", 0, []string{"x"}, []string{"PACE_JOB_ID=first"}, nil), "first", true},
		{"no padding", procargs(1, "/x", 0, []string{"x"}, []string{"A=1", "PACE_JOB_ID=np"}, nil), "np", true},
		{"absent", procargs(1, "/x", 2, []string{"x"}, []string{"FOO=1"}, nil), "", false},
		{"empty env", procargs(1, "/x", 2, []string{"x"}, nil, []string{"PACE_JOB_ID=apple"}), "", false},
		{"only in argv", procargs(2, "/x", 1, []string{"x", "PACE_JOB_ID=evil"}, []string{"FOO=1"}, nil), "", false},
		{"only in apple strings", procargs(1, "/x", 1, []string{"x"}, []string{"FOO=1"}, []string{"PACE_JOB_ID=apple"}), "", false},
		{"prefix key", procargs(1, "/x", 1, []string{"x"}, []string{"PACE_JOB_IDX=1", "XPACE_JOB_ID=2"}, nil), "", false},
		{"key without =", procargs(1, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID"}, nil), "", false},
		{"empty value", procargs(1, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID="}, nil), "", true},
		{"value with =", procargs(1, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID=a=b"}, nil), "a=b", true},
		{"empty argv element", procargs(3, "/x", 1, []string{"x", "", "z"}, []string{"PACE_JOB_ID=e"}, nil), "e", true},
		{"argc 0", procargs(0, "/x", 1, nil, []string{"PACE_JOB_ID=z"}, nil), "z", true},
		{"max length", procargs(1, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID=" + ok}, nil), ok, true},
		{"too long", procargs(1, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID=" + long}, nil), "", false},
		{"unterminated last", append(procargs(1, "/x", 1, []string{"x"}, nil, nil)[:len(procargs(1, "/x", 1, []string{"x"}, nil, nil))-1], "PACE_JOB_ID=u"...), "u", true},
		{"argc beyond data", procargs(5, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID=q"}, nil), "", false},
		{"argc huge", procargs(1<<30, "/x", 1, []string{"x"}, nil, nil), "", false},
		{"argc negative", procargs(-1, "/x", 1, []string{"x"}, []string{"PACE_JOB_ID=n"}, nil), "", false},
		{"no NUL after exe", append(binary.LittleEndian.AppendUint32(nil, 1), "/x/never/ends"...), "", false},
		{"only argc", binary.LittleEndian.AppendUint32(nil, 1), "", false},
		{"short", []byte{1, 0}, "", false},
		{"nil", nil, "", false},
	} {
		got, found := procArgsEnv(c.buf, jobTagEnv)
		if got != c.want || found != c.found {
			t.Errorf("%s: procArgsEnv=%q,%v want %q,%v", c.name, got, found, c.want, c.found)
		}
	}
	if _, found := procArgsEnv(procargs(1, "/x", 1, []string{"x"}, []string{"=v"}, nil), ""); found {
		t.Error("empty key matched")
	}
}

func TestCstr(t *testing.T) {
	for _, c := range []struct {
		in   []byte
		want string
	}{{[]byte("abc\x00def"), "abc"}, {[]byte("abc"), "abc"}, {[]byte{0}, ""}, {nil, ""}} {
		if got := string(cstr(c.in)); got != c.want {
			t.Errorf("cstr(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// real machine

func newTestBackend(t testing.TB) *darwinBackend {
	t.Helper()
	b, err := newDarwin()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNewRegistersDarwin(t *testing.T) {
	be, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if be.Name() != "darwin" {
		t.Fatalf("Name=%q", be.Name())
	}
	if _, ok := be.(TagReader); !ok {
		t.Error("darwin backend does not implement TagReader")
	}
	if _, ok := be.(ContainerProvider); ok {
		t.Error("darwin backend must not claim JobContainer support")
	}
	if lvl := QuickPressure(); lvl != 1 && lvl != 2 && lvl != 4 {
		t.Errorf("QuickPressure=%d, want 1/2/4", lvl)
	}
}

func TestNameToMIB(t *testing.T) {
	mib, err := nameToMIB("kern.proc")
	if err != nil || len(mib) != 2 || mib[0] != ctlKern || mib[1] != kernProc {
		t.Errorf("kern.proc -> %v, %v", mib, err)
	}
	mib, err = nameToMIB("kern.procargs2")
	if err != nil || len(mib) != 2 || mib[1] != kernProcargs2 {
		t.Errorf("kern.procargs2 -> %v, %v", mib, err)
	}
	if _, err := nameToMIB("no.such.sysctl.pace"); err == nil {
		t.Error("unknown name resolved")
	}
	if _, err := nameToMIB(""); err == nil {
		t.Error("empty name resolved")
	}
}

func TestSampleMachine(t *testing.T) {
	b := newTestBackend(t)
	mem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		t.Fatal(err)
	}
	ncpu, _ := unix.SysctlUint32("hw.ncpu")
	before := time.Now().UnixMilli()
	m, err := b.SampleMachine([]string{t.TempDir(), "/"})
	if err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixMilli()
	t.Logf("machine: %+v", m)
	if m.TotalBytes != int64(mem) {
		t.Errorf("TotalBytes=%d want hw.memsize %d", m.TotalBytes, mem)
	}
	if m.AvailBytes <= 0 || m.AvailBytes > m.TotalBytes {
		t.Errorf("AvailBytes=%d out of (0, %d]", m.AvailBytes, m.TotalBytes)
	}
	if pct, _ := unix.SysctlUint32("kern.memorystatus_level"); pct > 0 {
		want := m.TotalBytes * int64(pct) / 100
		if d := m.AvailBytes - want; d < -m.TotalBytes/50 || d > m.TotalBytes/50 {
			t.Errorf("AvailBytes=%d, level now %d%% -> %d", m.AvailBytes, pct, want)
		}
	}
	if m.KernelLevel != 1 && m.KernelLevel != 2 && m.KernelLevel != 4 {
		t.Errorf("KernelLevel=%d", m.KernelLevel)
	}
	if m.NCPU <= 0 || m.NCPU != int(ncpu) {
		t.Errorf("NCPU=%d want %d", m.NCPU, ncpu)
	}
	if m.SwapTotal < 0 || m.SwapUsed < 0 || m.SwapUsed > m.SwapTotal {
		t.Errorf("swap total=%d used=%d", m.SwapTotal, m.SwapUsed)
	}
	if m.Load1Milli < 0 || m.Load1Milli > int64(m.NCPU)*1000*100 {
		t.Errorf("Load1Milli=%d", m.Load1Milli)
	}
	if m.PSIMemSome10 != -1 || m.PSIMemFull10 != -1 {
		t.Errorf("PSI fields must be -1 on darwin: %d %d", m.PSIMemSome10, m.PSIMemFull10)
	}
	if m.DiskFreeBytes <= 0 {
		t.Errorf("DiskFreeBytes=%d", m.DiskFreeBytes)
	}
	if m.NowMS < before || m.NowMS > after {
		t.Errorf("NowMS=%d not in [%d,%d]", m.NowMS, before, after)
	}
	// DiskFreeBytes is the minimum over the paths.
	var st unix.Statfs_t
	if err := unix.Statfs("/", &st); err == nil {
		root := int64(st.Bavail) * int64(st.Bsize)
		if m.DiskFreeBytes > root+(1<<30) {
			t.Errorf("DiskFreeBytes %d > root free %d", m.DiskFreeBytes, root)
		}
	}
}

func TestSampleMachineDiskPaths(t *testing.T) {
	b := newTestBackend(t)
	for _, c := range []struct {
		name  string
		paths []string
		none  bool
	}{
		{"nil", nil, true},
		{"empty", []string{}, true},
		{"empty string", []string{""}, true},
		{"missing", []string{"/no/such/dir/pace-test"}, true},
		{"missing+root", []string{"/no/such/dir/pace-test", "/"}, false},
		{"tempdir", []string{t.TempDir()}, false},
	} {
		m, err := b.SampleMachine(c.paths)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.none && m.DiskFreeBytes != -1 {
			t.Errorf("%s: DiskFreeBytes=%d want -1", c.name, m.DiskFreeBytes)
		}
		if !c.none && m.DiskFreeBytes <= 0 {
			t.Errorf("%s: DiskFreeBytes=%d want >0", c.name, m.DiskFreeBytes)
		}
	}
}

// findProc returns the proc with pid from a fresh listing.
func findProc(t *testing.T, b *darwinBackend, pid int) (Proc, bool) {
	t.Helper()
	ps, err := b.ListProcs(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.PID == pid {
			return p, true
		}
	}
	return Proc{}, false
}

// waitProc polls until cond holds for pid (present) or the timeout passes.
func waitProc(t *testing.T, b *darwinBackend, pid int, what string, cond func(Proc, bool) bool) Proc {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		p, ok := findProc(t, b, pid)
		if cond(p, ok) {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d: timeout waiting for %s (last %+v present=%v)", pid, what, p, ok)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestListProcsSelf(t *testing.T) {
	b := newTestBackend(t)
	ps, err := b.ListProcs(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) < 3 {
		t.Fatalf("only %d processes listed", len(ps))
	}
	var self *Proc
	seen := map[int]bool{}
	for i := range ps {
		p := &ps[i]
		if seen[p.PID] {
			t.Errorf("pid %d listed twice", p.PID)
		}
		seen[p.PID] = true
		if p.UID != os.Getuid() {
			t.Errorf("pid %d uid %d", p.PID, p.UID)
		}
		if p.PID <= 0 || p.StartMS <= 0 || p.Comm == "" {
			t.Errorf("bad entry %+v", *p)
		}
		if p.Footprint < 0 || p.CPUMS < 0 {
			t.Errorf("negative usage %+v", *p)
		}
		switch p.State {
		case 'R', 'S', 'T', 'Z':
		default:
			t.Errorf("pid %d state %q", p.PID, p.State)
		}
		if p.State == 'Z' && p.Footprint != 0 {
			t.Errorf("zombie %d footprint %d", p.PID, p.Footprint)
		}
		if p.PID == os.Getpid() {
			self = p
		}
	}
	if self == nil {
		t.Fatal("own process not listed")
	}
	if self.System {
		t.Error("test process classified as System")
	}
	if self.PPID != os.Getppid() || self.PGID != unix.Getpgrp() {
		t.Errorf("self ppid/pgid %d/%d want %d/%d", self.PPID, self.PGID, os.Getppid(), unix.Getpgrp())
	}
	exe, _ := os.Executable()
	exe, _ = filepath.EvalSymlinks(exe)
	got, _ := filepath.EvalSymlinks(self.Exe)
	if got != exe {
		t.Errorf("self Exe=%q want %q", self.Exe, exe)
	}
	if self.Footprint < 1<<20 {
		t.Errorf("self footprint %d implausibly small", self.Footprint)
	}
	if self.JobTag != os.Getenv(jobTagEnv) {
		t.Errorf("self JobTag=%q env=%q", self.JobTag, os.Getenv(jobTagEnv))
	}

	// Compare with an independent x/sys snapshot.
	ref, err := unix.SysctlKinfoProcSlice("kern.proc.uid", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	common := 0
	for i := range ref {
		if seen[int(ref[i].Proc.P_pid)] {
			common++
		}
	}
	if common < len(ref)*9/10 {
		t.Errorf("only %d of %d reference pids listed", common, len(ref))
	}
	nsys := 0
	for _, p := range ps {
		if p.System {
			nsys++
		}
	}
	t.Logf("%d procs (%d System), reference %d", len(ps), nsys, len(ref))
}

func TestListProcsOtherUID(t *testing.T) {
	b := newTestBackend(t)
	if _, err := b.ListProcs(-1); err == nil {
		t.Error("negative uid accepted")
	}
	// A uid with no processes yields an empty list, not an error.
	ps, err := b.ListProcs(77777)
	if err != nil || len(ps) != 0 {
		t.Errorf("uid 77777: %d procs, %v", len(ps), err)
	}
	// Listing root works (no root needed); usage may be unreadable but must not fail.
	ps, err = b.ListProcs(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.UID != 0 {
			t.Errorf("root listing has uid %d (pid %d)", p.UID, p.PID)
		}
	}
	// Then our own again: the buffer and cache must not be confused.
	if _, ok := findProc(t, b, os.Getpid()); !ok {
		t.Error("self missing after listing another uid")
	}
}

func TestFootprintAllocation(t *testing.T) {
	b := newTestBackend(t)
	base := spawn(t, spawnOpts{mode: "sleep"})
	big := spawn(t, spawnOpts{mode: "alloc:300M"})
	base.expect("ready")
	big.expect("ready")
	time.Sleep(100 * time.Millisecond)
	bp, ok1 := findProc(t, b, base.pid)
	ap, ok2 := findProc(t, b, big.pid)
	if !ok1 || !ok2 {
		t.Fatalf("helpers not listed: base=%v alloc=%v", ok1, ok2)
	}
	const want = 300 << 20
	delta := ap.Footprint - bp.Footprint
	t.Logf("alloc footprint=%d (%.1f MiB) baseline=%d (%.1f MiB) delta=%.1f MiB",
		ap.Footprint, float64(ap.Footprint)/(1<<20), bp.Footprint, float64(bp.Footprint)/(1<<20), float64(delta)/(1<<20))
	if ap.Footprint < want*9/10 || ap.Footprint > want*11/10 {
		t.Errorf("footprint %d not within 10%% of %d", ap.Footprint, want)
	}
	if delta < want*9/10 || delta > want*11/10 {
		t.Errorf("footprint delta %d not within 10%% of %d", delta, want)
	}
	// RSS sanity: footprint counts compressed/swapped pages too, so it is
	// never meaningfully below resident size for anonymous memory.
	b.mu.Lock()
	fp, _, rss, err := b.rusage(big.pid)
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("direct rusage: footprint=%.1f MiB resident=%.1f MiB", float64(fp)/(1<<20), float64(rss)/(1<<20))
	if rss == 0 || fp < rss*9/10 {
		t.Errorf("footprint %d vs resident %d implausible", fp, rss)
	}
	if d := int64(fp) - ap.Footprint; d < -(32<<20) || d > 32<<20 {
		t.Errorf("listing footprint %d vs direct %d", ap.Footprint, fp)
	}
}

func TestCPUTime(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "burn:1000"})
	h.expect("ready")
	p0, ok := findProc(t, b, h.pid)
	if !ok {
		t.Fatal("helper not listed")
	}
	h.send("go")
	h.expect("done")
	p1, ok := findProc(t, b, h.pid)
	if !ok {
		t.Fatal("helper vanished")
	}
	d := p1.CPUMS - p0.CPUMS
	t.Logf("CPUMS %d -> %d (delta %d ms for a 1000 ms burn)", p0.CPUMS, p1.CPUMS, d)
	if d < 700 || d > 1300 {
		t.Errorf("CPU delta %d ms, want 1000 +-30%%", d)
	}
}

func TestStartPGIDAndState(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "sleep", attr: &syscall.SysProcAttr{Setpgid: true}})
	plain := spawn(t, spawnOpts{mode: "sleep"})
	h.expect("ready")
	plain.expect("ready")
	p, ok := findProc(t, b, h.pid)
	if !ok {
		t.Fatal("helper not listed")
	}
	if p.PGID != h.pid {
		t.Errorf("Setpgid child PGID=%d want %d", p.PGID, h.pid)
	}
	if p.PPID != os.Getpid() {
		t.Errorf("PPID=%d want %d", p.PPID, os.Getpid())
	}
	if d := p.StartMS - h.spawned.UnixMilli(); d < -2000 || d > 2000 {
		t.Errorf("StartMS=%d spawned=%d (delta %d ms)", p.StartMS, h.spawned.UnixMilli(), d)
	}
	if p.State == 'T' || p.State == 'Z' {
		t.Errorf("state %c", p.State)
	}
	q, ok := findProc(t, b, plain.pid)
	if !ok || q.PGID != unix.Getpgrp() {
		t.Errorf("plain child PGID=%d want ours %d", q.PGID, unix.Getpgrp())
	}
	// StartMS is stable across ticks (identity).
	p2, _ := findProc(t, b, h.pid)
	if p2.StartMS != p.StartMS {
		t.Errorf("StartMS changed %d -> %d", p.StartMS, p2.StartMS)
	}
}

func TestSetsidGrandchildListed(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "setsid", attr: &syscall.SysProcAttr{Setpgid: true}})
	f := h.expect("ready")
	if len(f) != 2 {
		t.Fatalf("bad ready line %v", f)
	}
	gpid, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatal(err)
	}
	g := waitProc(t, b, gpid, "grandchild listed", func(_ Proc, ok bool) bool { return ok })
	t.Cleanup(func() { killIfSame(gpid, g.StartMS) })
	if g.PPID != h.pid {
		t.Errorf("grandchild PPID=%d want %d", g.PPID, h.pid)
	}
	if g.PGID != gpid || g.PGID == h.pid {
		t.Errorf("setsid grandchild PGID=%d want own %d", g.PGID, gpid)
	}
	if sid, err := unix.Getsid(gpid); err != nil || sid != gpid {
		t.Errorf("Getsid=%d,%v", sid, err)
	}
	if g.StartMS < h.spawned.UnixMilli()-1000 {
		t.Errorf("grandchild StartMS %d before parent spawn %d", g.StartMS, h.spawned.UnixMilli())
	}
	if g.System {
		t.Error("grandchild classified System")
	}
}

func TestSignalGroup(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "group", attr: &syscall.SysProcAttr{Setpgid: true}})
	f := h.expect("ready")
	member, err := strconv.Atoi(f[1])
	if err != nil {
		t.Fatal(err)
	}
	pgid := h.pid
	m := waitProc(t, b, member, "member listed", func(_ Proc, ok bool) bool { return ok })
	t.Cleanup(func() { killIfSame(member, m.StartMS) })
	if m.PGID != pgid {
		t.Fatalf("member PGID=%d want %d", m.PGID, pgid)
	}
	if !groupHasLive(pgid) {
		t.Error("groupHasLive false for a live group")
	}
	if groupHasLive(1 << 30) {
		t.Error("groupHasLive true for a nonexistent group")
	}

	if err := b.SignalGroup(pgid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{h.pid, member} {
		waitProc(t, b, pid, "stopped", func(p Proc, ok bool) bool { return ok && p.State == 'T' })
	}
	if err := b.SignalGroup(pgid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{h.pid, member} {
		waitProc(t, b, pid, "continued", func(p Proc, ok bool) bool { return ok && p.State != 'T' })
	}
	if err := b.SignalGroup(pgid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// The helper is our child and stays a zombie until reaped, so its pid
	// (and therefore the pgid) cannot be recycled while we probe it.
	waitProc(t, b, h.pid, "zombie", func(p Proc, ok bool) bool { return !ok || p.State == 'Z' })
	waitProc(t, b, member, "member gone", func(p Proc, ok bool) bool { return !ok || p.State == 'Z' })
	t.Logf("raw killpg on zombie-only group: %v; groupHasLive=%v", unix.Kill(-pgid, 0), groupHasLive(pgid))
	if groupHasLive(pgid) {
		t.Error("groupHasLive true for a zombie-only group")
	}
	if err := b.SignalGroup(pgid, syscall.SIGKILL); err != nil {
		t.Errorf("SignalGroup on dead group: %v", err)
	}
	if err := b.Signal(h.pid, syscall.SIGTERM); err != nil {
		t.Errorf("Signal on zombie: %v", err)
	}
	h.stop()
	// pids above PID_MAX (99999) cannot exist: guaranteed ESRCH.
	if err := b.SignalGroup(1<<30, syscall.SIGKILL); err != nil {
		t.Errorf("SignalGroup ESRCH: %v", err)
	}
	if err := b.Signal(1<<30, syscall.SIGKILL); err != nil {
		t.Errorf("Signal ESRCH: %v", err)
	}
}

func TestSignalSingle(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "sleep", attr: &syscall.SysProcAttr{Setpgid: true}})
	h.expect("ready")
	if err := b.Signal(h.pid, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	waitProc(t, b, h.pid, "stopped", func(p Proc, ok bool) bool { return ok && p.State == 'T' })
	if err := b.Signal(h.pid, syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	waitProc(t, b, h.pid, "running", func(p Proc, ok bool) bool { return ok && p.State != 'T' })
	if err := b.Signal(h.pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitProc(t, b, h.pid, "terminated", func(p Proc, ok bool) bool { return !ok || p.State == 'Z' })
}

func TestSignalRefusals(t *testing.T) {
	b := newTestBackend(t)
	// Signal 0 is the null signal: harmless even if a guard were broken.
	const null = syscall.Signal(0)
	for _, pid := range []int{0, -1, -12345, os.Getpid()} {
		if err := b.Signal(pid, null); err == nil {
			t.Errorf("Signal(%d) accepted", pid)
		}
		if err := b.SetBackground(pid, false); err == nil {
			t.Errorf("SetBackground(%d) accepted", pid)
		}
	}
	for _, pgid := range []int{0, 1, -1, -7, unix.Getpgrp()} {
		if err := b.SignalGroup(pgid, null); err == nil {
			t.Errorf("SignalGroup(%d) accepted", pgid)
		}
	}
	// EPERM is an error (launchd belongs to root).
	if err := b.Signal(1, null); err == nil || !errors.Is(err, unix.EPERM) {
		t.Errorf("Signal(1,0) = %v, want EPERM", err)
	}
}

func TestSetBackground(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "sleep"})
	h.expect("ready")
	p0, err := taskPriority(h.pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	p1, _ := taskPriority(h.pid)
	bg, err := unix.Getpriority(prioDarwinProcess, h.pid)
	t.Logf("pti_priority normal=%d background=%d getpriority(PRIO_DARWIN_PROCESS)=%#x err=%v", p0, p1, bg, err)
	if p1 >= p0 {
		t.Errorf("background priority %d not below normal %d", p1, p0)
	}
	// Idempotent.
	if err := b.SetBackground(h.pid, true); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	p2, _ := taskPriority(h.pid)
	t.Logf("pti_priority restored=%d", p2)
	if p2 != p0 {
		t.Errorf("restored priority %d want %d", p2, p0)
	}
	if err := b.SetBackground(h.pid, false); err != nil {
		t.Fatal(err)
	}
	if err := b.SetBackground(1<<30, true); err != nil {
		t.Errorf("SetBackground on missing pid: %v", err)
	}
}

func TestSystemClassification(t *testing.T) {
	b := newTestBackend(t)
	sl := spawn(t, spawnOpts{path: "/bin/sleep", args: []string{"60"}})
	p := waitProc(t, b, sl.pid, "/bin/sleep listed", func(p Proc, ok bool) bool { return ok && p.Comm == "sleep" })
	cs, err := csFlags(sl.pid)
	t.Logf("/bin/sleep csflags=%#x err=%v exe=%q", cs, err, p.Exe)
	if cs&csPlatformBinary == 0 {
		t.Log("note: /bin/sleep is not flagged as a platform binary here")
	}
	if p.System {
		t.Error("/bin/sleep (platform binary outside OS prefixes) classified System")
	}
	if p.Exe != "/bin/sleep" {
		t.Errorf("Exe=%q", p.Exe)
	}

	ps, err := b.ListProcs(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, q := range ps {
		switch {
		case q.PID == os.Getpid():
			if q.System {
				t.Error("own test process is System")
			}
		case (q.Comm == "Finder" || q.Comm == "Dock") && strings.HasPrefix(q.Exe, "/System/"):
			found++
			if !q.System {
				t.Errorf("%s (pid %d, %s) not System", q.Comm, q.PID, q.Exe)
			}
		case q.Comm == "loginwindow":
			if !q.System {
				t.Errorf("loginwindow (pid %d) not System", q.PID)
			}
		}
		if q.System && q.JobTag != "" {
			t.Errorf("System pid %d carries a job tag", q.PID)
		}
	}
	if found == 0 {
		t.Log("no Finder/Dock in this session; that half of the check was skipped")
	} else {
		t.Logf("verified %d Finder/Dock process(es) are System", found)
	}
}

func TestExecInvalidatesCache(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "exec"})
	h.expect("ready")
	p0, ok := findProc(t, b, h.pid)
	if !ok {
		t.Fatal("helper not listed")
	}
	exe, _ := os.Executable()
	if filepath.Base(p0.Exe) != filepath.Base(exe) {
		t.Errorf("pre-exec Exe=%q", p0.Exe)
	}
	h.send("go")
	p1 := waitProc(t, b, h.pid, "exec'd sleep", func(p Proc, ok bool) bool { return ok && p.Comm == "sleep" })
	if p1.Exe != "/bin/sleep" {
		t.Errorf("post-exec Exe=%q want /bin/sleep (stale cache?)", p1.Exe)
	}
	if p1.StartMS != p0.StartMS {
		t.Errorf("exec changed StartMS %d -> %d", p0.StartMS, p1.StartMS)
	}
	if p1.System {
		t.Error("/bin/sleep after exec classified System")
	}
}

func TestCacheEviction(t *testing.T) {
	b := newTestBackend(t)
	h := spawn(t, spawnOpts{mode: "sleep"})
	h.expect("ready")
	if _, ok := findProc(t, b, h.pid); !ok {
		t.Fatal("helper not listed")
	}
	b.mu.Lock()
	_, cached := b.cache[h.pid]
	b.mu.Unlock()
	if !cached {
		t.Fatal("helper not cached")
	}
	h.stop()
	waitProc(t, b, h.pid, "gone", func(_ Proc, ok bool) bool { return !ok })
	b.mu.Lock()
	_, cached = b.cache[h.pid]
	n := len(b.cache)
	b.mu.Unlock()
	if cached {
		t.Error("dead process still cached")
	}
	ps, _ := b.ListProcs(os.Getuid())
	if n > len(ps)+64 {
		t.Errorf("cache has %d entries for %d processes", n, len(ps))
	}
}

func TestJobTag(t *testing.T) {
	b := newTestBackend(t)
	tag := "pace-test-" + strconv.Itoa(os.Getpid())
	tagged := spawn(t, spawnOpts{mode: "sleep", env: []string{jobTagEnv + "=" + tag}})
	untagged := spawn(t, spawnOpts{mode: "sleep"})
	tagged.expect("ready")
	untagged.expect("ready")
	if got := b.JobTag(tagged.pid); got != tag {
		t.Errorf("JobTag(tagged)=%q want %q", got, tag)
	}
	if got := b.JobTag(untagged.pid); got != "" {
		t.Errorf("JobTag(untagged)=%q", got)
	}
	for _, pid := range []int{0, -1, 1 << 30} {
		if got := b.JobTag(pid); got != "" {
			t.Errorf("JobTag(%d)=%q", pid, got)
		}
	}
	if got := b.JobTag(1); got != "" { // launchd: not ours, unreadable
		t.Errorf("JobTag(1)=%q", got)
	}
	// ListProcs fills the tag on first sight.
	p, ok := findProc(t, b, tagged.pid)
	if !ok || p.JobTag != tag {
		t.Errorf("listed JobTag=%q want %q", p.JobTag, tag)
	}
	q, _ := findProc(t, b, untagged.pid)
	if q.JobTag != "" {
		t.Errorf("listed untagged JobTag=%q", q.JobTag)
	}
	// A platform binary hides its environment (documented); just report it.
	sl := spawn(t, spawnOpts{path: "/bin/sleep", args: []string{"60"}, env: []string{jobTagEnv + "=" + tag}})
	waitProc(t, b, sl.pid, "sleep listed", func(p Proc, ok bool) bool { return ok && p.Comm == "sleep" })
	t.Logf("JobTag(/bin/sleep with tag)=%q (platform binaries usually hide env)", b.JobTag(sl.pid))
}

func TestListProcsBufferReuse(t *testing.T) {
	b := newTestBackend(t)
	uid := os.Getuid()
	if _, err := b.ListProcs(uid); err != nil {
		t.Fatal(err)
	}
	first := &b.kbuf[0]
	for i := 0; i < 5; i++ {
		if _, err := b.ListProcs(uid); err != nil {
			t.Fatal(err)
		}
	}
	if &b.kbuf[0] != first {
		t.Log("kinfo buffer was regrown (process count jumped); not a failure")
	}
	allocs := testing.AllocsPerRun(10, func() {
		if _, err := b.ListProcs(uid); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("steady-state ListProcs allocs/op = %.1f (kbuf %d entries)", allocs, len(b.kbuf))
	// One allocation is the returned slice; process churn on the machine adds
	// a few per new process.
	if allocs > 40 {
		t.Errorf("ListProcs allocs/op %.1f, want steady-state reuse", allocs)
	}
	sm := testing.AllocsPerRun(20, func() { _, _ = b.SampleMachine(nil) })
	t.Logf("SampleMachine(nil) allocs/op = %.1f", sm)
	if sm > 0 {
		t.Errorf("SampleMachine(nil) allocates %.1f/op", sm)
	}
}

func TestKinfoBufferGrowth(t *testing.T) {
	b := newTestBackend(t)
	b.kbuf = make([]unix.KinfoProc, 1) // force ENOMEM then regrow
	ps, err := b.ListProcs(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if len(b.kbuf) < len(ps) || len(ps) < 3 {
		t.Errorf("after growth kbuf=%d procs=%d", len(b.kbuf), len(ps))
	}
}

func TestConcurrentUse(t *testing.T) {
	b := newTestBackend(t)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				if _, err := b.ListProcs(os.Getuid()); err != nil {
					errs <- err
				}
				if _, err := b.SampleMachine([]string{"/"}); err != nil {
					errs <- err
				}
				_ = b.JobTag(os.Getpid())
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestDeterministicIdentity(t *testing.T) {
	// Two backends see the same identity fields for the same live processes.
	b1, b2 := newTestBackend(t), newTestBackend(t)
	p1, ok1 := findProc(t, b1, os.Getpid())
	p2, ok2 := findProc(t, b2, os.Getpid())
	if !ok1 || !ok2 {
		t.Fatal("self missing")
	}
	if p1.PID != p2.PID || p1.StartMS != p2.StartMS || p1.PGID != p2.PGID || p1.Exe != p2.Exe ||
		p1.System != p2.System || p1.Comm != p2.Comm || p1.JobTag != p2.JobTag {
		t.Errorf("identity differs:\n%+v\n%+v", p1, p2)
	}
}
