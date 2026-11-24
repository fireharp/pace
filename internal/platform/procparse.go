package platform

// procfs and cgroupfs parsing for the Linux backend.
//
// This file has no build tag on purpose: it uses only portable syscalls
// (openat, fstat, readlinkat via x/sys/unix), so the fixture tests in
// procparse_test.go run on macOS too, pointed at testdata/linux/proc.
// The Linux-only parts (sched, ioprio, kill, statfs, cgroup writes) live in
// platform_linux.go and cgroup_linux.go.
//
// Design notes (measured numbers: see the header of platform_linux.go):
//   - Every per-process file is read through a dirfd of /proc/<pid>, so all
//     reads of one scan refer to the same process even if the pid is reused
//     mid-scan (openat on a stale dirfd fails with ESRCH).
//   - The uid filter is fstat(dirfd).Uid: the owner of /proc/<pid> is the
//     process' effective uid (root when the process is non-dumpable). It is one
//     cheap syscall and skips other users' processes before any file is read.
//     setuid programs and non-dumpable processes of the user therefore look
//     like root's and are never listed (never signalled), which is the safe
//     direction.
//   - btime is read once per scanner: the kernel recomputes it from the wall
//     clock on every read, so a clock step would otherwise shift every StartMS
//     and break (pid, start) identity. Across restarts the first btime of the
//     boot is persisted (LinuxOptions.BtimeFile, keyed by boot_id).

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// procRoot is the procfs mount point. A package variable so tests can point
// the parsers at a fake tree.
var procRoot = "/proc"

const (
	clkTck     = 100 // USER_HZ: fixed by the Linux ABI on every architecture
	msPerTick  = 1000 / clkTck
	pfKthread  = 0x00200000 // PF_KTHREAD in /proc/pid/stat flags
	maxFileBuf = 1 << 20    // cap for one procfs read (environ can be large)
)

// FootprintMethod selects how the Linux backend measures Proc.Footprint.
type FootprintMethod string

const (
	// FootprintAnon (default) = RssAnon + RssShmem + VmSwap from
	// /proc/pid/status: memory only this process' exit can free (anonymous,
	// shared-memory and swapped pages), the closest analogue of the macOS
	// physical footprint. Clean file-backed pages (binaries, libraries, page
	// cache) are excluded because the kernel can drop them without a kill.
	// Cost: stat + status.
	FootprintAnon FootprintMethod = "anon"
	// FootprintRSS = rss field of /proc/pid/stat only. Cheapest (one read),
	// counts shared libraries in every process and ignores swap.
	FootprintRSS FootprintMethod = "rss"
	// FootprintRSSSwap = statm resident + status VmSwap (the classic
	// "RSS+swap"). Cost: stat + statm + status.
	FootprintRSSSwap FootprintMethod = "rss_swap"
	// FootprintPSS = Pss + SwapPss from /proc/pid/smaps_rollup: shared pages
	// split between the sharers, so sums over a tree do not double count.
	// Walks the page tables under the target's mmap lock: slower, and it can
	// block behind a process that holds the lock for writing.
	FootprintPSS FootprintMethod = "pss"
	// FootprintPSSAnon = Pss_Anon + Pss_Shmem + SwapPss (kernel >= 5.9;
	// falls back to Pss + SwapPss). Same cost as FootprintPSS.
	FootprintPSSAnon FootprintMethod = "pss_anon"
)

// FootprintMethods lists every accepted method (stable order).
var FootprintMethods = []FootprintMethod{FootprintAnon, FootprintRSS, FootprintRSSSwap, FootprintPSS, FootprintPSSAnon}

// ParseFootprintMethod validates s ("" means the default, FootprintAnon).
func ParseFootprintMethod(s string) (FootprintMethod, error) {
	if s == "" {
		return FootprintAnon, nil
	}
	for _, m := range FootprintMethods {
		if string(m) == s {
			return m, nil
		}
	}
	return "", fmt.Errorf("platform: unknown footprint method %q (want one of anon, rss, rss_swap, pss, pss_anon)", s)
}

// LinuxOptions tune the Linux backend. New() reads them from the environment
// (LinuxOptionsFromEnv); they are not part of the TOML config because they
// only matter on Linux and only for unusual deployments.
type LinuxOptions struct {
	// Footprint method for Proc.Footprint ($PACE_LINUX_FOOTPRINT, default anon).
	Footprint FootprintMethod
	// CgroupMemLimit caps TotalBytes/AvailBytes by the memory.max of the
	// cgroup-namespace root (a container's limit) when it is finite
	// ($PACE_LINUX_CGROUP_LIMIT=0 disables; default on).
	CgroupMemLimit bool
	// CgroupBase pins the pace root under which each daemon's scope of
	// per-job cgroups is created ($PACE_CGROUP_BASE, absolute cgroupfs
	// path). Empty = auto-detect <delegation root>/pace. Only the daemon
	// reads it: wrappers get the job's path on ADMITTED.
	CgroupBase string
	// NoContainers disables the cgroup JobContainer ($PACE_LINUX_CGROUP=0).
	NoContainers bool
	// CgroupHighPct sets a capped job's memory.high as a percentage of its
	// memory.max ($PACE_LINUX_CGROUP_HIGH_PCT, 1-100). 0 (default) leaves
	// memory.high at "max": measured, a no-swap job throttled at 90 % of its
	// cap crawls in reclaim penalty sleeps for minutes instead of being
	// OOM-killed, which looks stuck rather than giving the agent a fast,
	// explicit KILLED. The policy's own warn step plays the "high" role.
	CgroupHighPct int
	// ThrottleNice is the nice value SetBackground raises fair threads to
	// when SCHED_IDLE could not be reverted, i.e. when it falls back to
	// SCHED_BATCH, which alone keeps the full CFS weight
	// ($PACE_LINUX_THROTTLE_NICE, 0-19; env default 10; 0 = off, BATCH only).
	// Without CAP_SYS_NICE or RLIMIT_NICE the restore cannot lower nice
	// again, so this part of the throttle is one-way (best effort restore,
	// as docs/ARCHITECTURE.md allows). Measured on two busy loops sharing
	// one CPU: nice 5 gets 24.6 % of it, 10 gets 9.5 %, 19 gets 1.5 %.
	ThrottleNice int
	// ThrottleCPUPct caps a throttled job container's CPU at this percentage
	// of one CPU (cpu.max; $PACE_LINUX_THROTTLE_CPU_PCT, 0-10000; env default
	// 25; 0 = no cap), where the cpu controller is delegated to the job
	// containers. Measured in Docker with the cpu controller on: the job
	// cgroup is a scheduling entity of its own, so neither SCHED_IDLE nor
	// nice 10 on its threads, nor cpu.weight=1 on it, lowers its share
	// against a process outside pace (50 % -> 50 %); cpu.weight=1 only works
	// against the other pace jobs (50 % -> 1 %). Only a quota holds against
	// everything: cpu.max 25 % took it to 25 % against the outside process
	// (two busy loops pinned to one CPU, 4 s).
	ThrottleCPUPct int
	// Load selects the Load1Milli estimate ($PACE_LINUX_LOAD): LoadAuto
	// (default) or LoadAvg. See LoadMethod.
	Load LoadMethod
	// BtimeFile persists the boot time first seen on this boot (keyed by
	// /proc/sys/kernel/random/boot_id), so StartMS, and the unit keys built
	// from it, survive a daemon restart after a wall-clock step or NTP drift
	// moved /proc/stat btime ($PACE_LINUX_BTIME_FILE: a path, or "off";
	// env default $PACE_HOME/run/btime, else $XDG_RUNTIME_DIR/pace/btime,
	// else $XDG_STATE_HOME/pace/run/btime, else ~/.local/state/pace/run/btime).
	// "" = do not persist (btime is still read only once per backend).
	BtimeFile string
}

// LoadMethod selects how the Linux backend estimates Machine.Load1Milli.
// /proc/loadavg is host-wide (not namespaced) and counts tasks in
// uninterruptible sleep (disk/NFS waits), while NCPU is the affinity mask, so
// load1/NCPU overstates CPU contention in a cpuset container and under I/O.
type LoadMethod string

const (
	// LoadAuto (default): a 1-minute exponential average (the load1 time
	// constant) of runnable tasks only. With /proc/schedstat it is the
	// runqueue occupancy of the CPUs in our affinity mask ((d run_time +
	// d run_delay) / d t per CPU); otherwise, from /proc/stat, our CPUs'
	// busy fraction plus their share of the waiting tasks (procs_running
	// minus busy CPUs, attributed by busy time), scaled to NCPU. Seeded from
	// LoadAvg.
	LoadAuto LoadMethod = "auto"
	// LoadAvg: /proc/loadavg load1 per online CPU, scaled to NCPU (still
	// counts D-state tasks, but no longer divides host load by our CPUs).
	LoadAvg LoadMethod = "loadavg"
)

// LinuxOptionsFromEnv reads LinuxOptions; unknown values are an error so a
// typo does not silently change what gets killed.
func LinuxOptionsFromEnv(getenv func(string) string) (LinuxOptions, error) {
	o := LinuxOptions{CgroupMemLimit: true, ThrottleNice: defaultThrottleNice, ThrottleCPUPct: defaultThrottleCPUPct, Load: LoadAuto}
	if v := getenv("PACE_LINUX_THROTTLE_NICE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 19 {
			return o, fmt.Errorf("platform: PACE_LINUX_THROTTLE_NICE=%q: want 0-19", v)
		}
		o.ThrottleNice = n
	}
	if v := getenv("PACE_LINUX_THROTTLE_CPU_PCT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 10000 {
			return o, fmt.Errorf("platform: PACE_LINUX_THROTTLE_CPU_PCT=%q: want 0-10000", v)
		}
		o.ThrottleCPUPct = n
	}
	switch v := LoadMethod(getenv("PACE_LINUX_LOAD")); v {
	case "", LoadAuto:
	case LoadAvg:
		o.Load = LoadAvg
	default:
		return o, fmt.Errorf("platform: PACE_LINUX_LOAD=%q: want auto or loadavg", v)
	}
	bf, err := btimeFileFromEnv(getenv)
	if err != nil {
		return o, err
	}
	o.BtimeFile = bf
	m, err := ParseFootprintMethod(getenv("PACE_LINUX_FOOTPRINT"))
	if err != nil {
		return o, err
	}
	o.Footprint = m
	switch v := getenv("PACE_LINUX_CGROUP_LIMIT"); v {
	case "", "1", "true", "on":
	case "0", "false", "off":
		o.CgroupMemLimit = false
	default:
		return o, fmt.Errorf("platform: PACE_LINUX_CGROUP_LIMIT=%q: want 0 or 1", v)
	}
	switch v := getenv("PACE_LINUX_CGROUP"); v {
	case "", "1", "true", "on":
	case "0", "false", "off":
		o.NoContainers = true
	default:
		return o, fmt.Errorf("platform: PACE_LINUX_CGROUP=%q: want 0 or 1", v)
	}
	if v := getenv("PACE_LINUX_CGROUP_HIGH_PCT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 100 {
			return o, fmt.Errorf("platform: PACE_LINUX_CGROUP_HIGH_PCT=%q: want 0-100", v)
		}
		o.CgroupHighPct = n
	}
	if b := getenv("PACE_CGROUP_BASE"); b != "" {
		if !strings.HasPrefix(b, "/") {
			return o, fmt.Errorf("platform: PACE_CGROUP_BASE=%q must be absolute", b)
		}
		o.CgroupBase = strings.TrimRight(b, "/")
	}
	return o, nil
}

// defaultThrottleNice: nice 10 cuts a throttled thread to ~1/10 of a normal
// one's CPU share (measured 9.5 %) while, being one-way for unprivileged
// users, it does not leave an unthrottled job nearly starved as 19 would.
const defaultThrottleNice = 10

// defaultThrottleCPUPct: a quarter of one CPU. Between what nice 10 leaves a
// thread against one competitor (measured 9.5-10.5 %) and an unthrottled
// job, and, being a cap, it holds against any number of competitors. The
// throttle comes before a freeze (0 %): the job keeps making some progress.
const defaultThrottleCPUPct = 25

// cpuMaxQuota is the cpu.max value for pct % of one CPU over a 100 ms period
// ("max 100000" when pct <= 0).
func cpuMaxQuota(pct int) string {
	if pct <= 0 {
		return "max 100000"
	}
	return strconv.Itoa(pct*1000) + " 100000"
}

// cpuQuota is the cpu.max of the cgroup-namespace root (a container's CPU
// limit: docker --cpus 2 writes "200000 100000") against the number of
// CPUs in our affinity mask. The zero value is no quota.
type cpuQuota struct {
	quota, period int64 // microseconds; quota < 0 = "max"
	ncpu          int
}

// parseCPUMax parses cpu.max content: "QUOTA PERIOD" or "max PERIOD"
// (quota -1). A bare "QUOTA" takes the kernel's default period, 100 ms.
func parseCPUMax(b []byte) (quota, period int64, ok bool) {
	f := bytes.Fields(b)
	if len(f) == 0 || len(f) > 2 {
		return 0, 0, false
	}
	period = 100000
	if len(f) == 2 {
		n, ok := parseUint(f[1])
		if !ok || n == 0 || n >= 1<<40 {
			return 0, 0, false
		}
		period = int64(n)
	}
	if string(f[0]) == "max" {
		return -1, period, true
	}
	n, ok := parseUint(f[0])
	if !ok || n == 0 || n >= 1<<50 {
		return 0, 0, false
	}
	return int64(n), period, true
}

// binds: the quota gives fewer CPUs of runtime than there are CPUs to run
// on, so CPUs sit throttled while threads wait, and a per-thread weight
// (nice, SCHED_IDLE) no longer decides who gets the quota. On one CPU every
// thread shares one runqueue and the weight holds.
func (q cpuQuota) binds() bool {
	return q.quota > 0 && q.period > 0 && q.ncpu > 1 && q.quota < int64(q.ncpu)*q.period
}

// cpus is the quota in CPUs ("2", "1.5").
func (q cpuQuota) cpus() string {
	return strconv.FormatFloat(float64(q.quota)/float64(q.period), 'f', -1, 64)
}

// quotaThrottleNote is the status note for a binding namespace quota ("" if
// it does not bind). cpuMax: job containers cap a throttled job with their
// own cpu.max, which holds.
func quotaThrottleNote(q cpuQuota, cpuMax bool) string {
	if !q.binds() {
		return ""
	}
	s := fmt.Sprintf("linux CPU throttle under this container's CPU quota (cpu.max %s of %d CPUs) is limited: "+
		"nice and SCHED_IDLE weigh only threads that share a CPU, while the quota hands its runtime to every CPU alike, "+
		"so a throttled thread alone on a CPU keeps its full share and THROTTLED may not slow a job at all "+
		"(measured: 0.18-1.26x the per-thread CPU of an unthrottled job)", q.cpus(), q.ncpu)
	if cpuMax {
		s += "; a job in a job container is held by that container's cpu.max instead"
	}
	return s
}

// quotaThrottleCaveat is the THROTTLED msg part for a binding namespace
// quota ("" if it does not bind, or cpuMax: this job's container holds it).
func quotaThrottleCaveat(q cpuQuota, cpuMax bool) string {
	if !q.binds() || cpuMax {
		return ""
	}
	return "is limited by the container's CPU quota (nice acts per CPU only)"
}

func btimeFileFromEnv(getenv func(string) string) (string, error) {
	switch v := getenv("PACE_LINUX_BTIME_FILE"); {
	case v == "0" || v == "off" || v == "false":
		return "", nil
	case v != "":
		if !strings.HasPrefix(v, "/") {
			return "", fmt.Errorf("platform: PACE_LINUX_BTIME_FILE=%q must be absolute or off", v)
		}
		return v, nil
	}
	if h := getenv("PACE_HOME"); strings.HasPrefix(h, "/") {
		return h + "/run/btime", nil
	}
	if rt := getenv("XDG_RUNTIME_DIR"); strings.HasPrefix(rt, "/") {
		return rt + "/pace/btime", nil
	}
	if st := getenv("XDG_STATE_HOME"); strings.HasPrefix(st, "/") {
		return st + "/pace/run/btime", nil
	}
	if h := getenv("HOME"); strings.HasPrefix(h, "/") {
		return h + "/.local/state/pace/run/btime", nil
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// Small allocation-free parsing helpers.

// parseUint parses a decimal unsigned integer; ok=false on anything else.
func parseUint(b []byte) (uint64, bool) {
	if len(b) == 0 || len(b) > 20 {
		return 0, false
	}
	var n uint64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		d := uint64(c - '0')
		if n > (^uint64(0)-d)/10 {
			return 0, false
		}
		n = n*10 + d
	}
	return n, true
}

// parseInt parses an optionally negative decimal integer.
func parseInt(b []byte) (int64, bool) {
	neg := false
	if len(b) > 0 && b[0] == '-' {
		neg, b = true, b[1:]
	}
	u, ok := parseUint(b)
	if !ok || u > 1<<63-1 {
		return 0, false
	}
	if neg {
		return -int64(u), true
	}
	return int64(u), true
}

// parseFixed parses a non-negative decimal like "12.34" into an integer with
// the given number of decimals, truncating extra digits: ("12.34", 1) = 123,
// ("0.52", 3) = 520, ("7", 1) = 70.
func parseFixed(b []byte, decimals int) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	var n int64
	seenDot, frac, digits := false, 0, 0
	for _, c := range b {
		switch {
		case c == '.' && !seenDot:
			seenDot = true
		case c >= '0' && c <= '9':
			if seenDot {
				if frac >= decimals {
					continue
				}
				frac++
			}
			if n > (1<<62)/10 {
				return 0, false
			}
			n = n*10 + int64(c-'0')
			digits++
		default:
			return 0, false
		}
	}
	if digits == 0 {
		return 0, false
	}
	for ; frac < decimals; frac++ {
		n *= 10
	}
	return n, true
}

// nextLine returns the line starting at b[i:] (without '\n') and the index
// after it.
func nextLine(b []byte, i int) ([]byte, int) {
	j := bytes.IndexByte(b[i:], '\n')
	if j < 0 {
		return b[i:], len(b)
	}
	return b[i : i+j], i + j + 1
}

// fields splits on ASCII spaces/tabs into dst (reused) without allocating
// when dst has capacity.
func fields(dst [][]byte, b []byte) [][]byte {
	dst = dst[:0]
	i := 0
	for i < len(b) {
		for i < len(b) && (b[i] == ' ' || b[i] == '\t') {
			i++
		}
		if i >= len(b) {
			break
		}
		j := i
		for j < len(b) && b[j] != ' ' && b[j] != '\t' {
			j++
		}
		dst = append(dst, b[i:j])
		i = j
	}
	return dst
}

// kbLine parses "Key:   1234 kB" (value in kB) into bytes for the given key
// prefix; ok=false when the line is not that key.
func kbValue(line []byte) (int64, bool) {
	c := bytes.IndexByte(line, ':')
	if c < 0 {
		return 0, false
	}
	v := bytes.TrimSpace(line[c+1:])
	unit := int64(1)
	if bytes.HasSuffix(v, []byte(" kB")) {
		v = bytes.TrimSpace(v[:len(v)-3])
		unit = 1024
	}
	n, ok := parseUint(v)
	if !ok || n > (1<<62)/uint64(unit) {
		return 0, false
	}
	return int64(n) * unit, true
}

func lineKey(line []byte) []byte {
	c := bytes.IndexByte(line, ':')
	if c < 0 {
		return nil
	}
	return line[:c]
}

// ---------------------------------------------------------------------------
// Machine-level files.

// memInfo is the subset of /proc/meminfo the backend uses (bytes).
type memInfo struct {
	Total, Avail, SwapTotal, SwapFree int64
}

// parseMeminfo parses /proc/meminfo. MemTotal is required. Without
// MemAvailable (kernel < 3.14) Avail falls back to MemFree + Buffers + Cached.
func parseMeminfo(b []byte) (memInfo, error) {
	var m memInfo
	var free, buffers, cached int64
	haveTotal, haveAvail := false, false
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		k := lineKey(line)
		if k == nil {
			continue
		}
		var dst *int64
		switch string(k) { // no allocation: compiler optimises string(b) in switch
		case "MemTotal":
			dst, haveTotal = &m.Total, true
		case "MemAvailable":
			dst, haveAvail = &m.Avail, true
		case "SwapTotal":
			dst = &m.SwapTotal
		case "SwapFree":
			dst = &m.SwapFree
		case "MemFree":
			dst = &free
		case "Buffers":
			dst = &buffers
		case "Cached":
			dst = &cached
		default:
			continue
		}
		v, ok := kbValue(line)
		if !ok {
			return memInfo{}, fmt.Errorf("platform: meminfo: bad line %q", line)
		}
		*dst = v
	}
	if !haveTotal || m.Total <= 0 {
		return memInfo{}, errors.New("platform: meminfo: no MemTotal")
	}
	if !haveAvail {
		m.Avail = free + buffers + cached
	}
	if m.Avail > m.Total {
		m.Avail = m.Total
	}
	if m.SwapFree > m.SwapTotal {
		m.SwapFree = m.SwapTotal
	}
	return m, nil
}

// parsePSI parses a pressure file ("some avg10=1.23 ..." / "full avg10=...")
// into avg10 permille (x10 of percent, truncated). A missing line or value is
// -1.
func parsePSI(b []byte) (some10, full10 int64) {
	some10, full10 = -1, -1
	var arr [8][]byte
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		fs := fields(arr[:0], line)
		if len(fs) < 2 {
			continue
		}
		var dst *int64
		switch string(fs[0]) {
		case "some":
			dst = &some10
		case "full":
			dst = &full10
		default:
			continue
		}
		for _, f := range fs[1:] {
			if v, ok := bytes.CutPrefix(f, []byte("avg10=")); ok {
				if n, ok := parseFixed(v, 1); ok && n <= 1000 {
					*dst = n
				}
				break
			}
		}
	}
	return some10, full10
}

// parseProcLoadavg returns the 1-minute load average x1000.
func parseProcLoadavg(b []byte) (int64, error) {
	end := bytes.IndexAny(b, " \t\n")
	if end < 0 {
		end = len(b)
	}
	v, ok := parseFixed(b[:end], 3)
	if !ok {
		return 0, fmt.Errorf("platform: loadavg: bad %q", b)
	}
	return v, nil
}

// parseBtime returns the "btime" (boot time, unix seconds) of /proc/stat.
func parseBtime(b []byte) (int64, error) {
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		if v, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			n, ok := parseUint(bytes.TrimSpace(v))
			if !ok || n == 0 {
				return 0, fmt.Errorf("platform: /proc/stat: bad btime %q", line)
			}
			return int64(n), nil
		}
	}
	return 0, errors.New("platform: /proc/stat: no btime")
}

// cpuTicks is one "cpuN" line of /proc/stat: busy (user+nice+system+irq+
// softirq+steal) and total (busy+idle+iowait) ticks; ok=false if absent.
type cpuTicks struct {
	busy, total uint64
	ok          bool
}

// parseProcStatLoad reads the per-CPU tick lines and procs_running of
// /proc/stat into dst (indexed by CPU id, grown as needed). online counts the
// cpuN lines (the kernel lists online CPUs only); running is -1 if absent.
func parseProcStatLoad(b []byte, dst []cpuTicks) (out []cpuTicks, running int64, online int) {
	for i := range dst {
		dst[i] = cpuTicks{}
	}
	running = -1
	var fb [16][]byte
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		if v, ok := bytes.CutPrefix(line, []byte("procs_running ")); ok {
			if n, ok := parseUint(bytes.TrimSpace(v)); ok {
				running = int64(n)
			}
			continue
		}
		if len(line) < 4 || line[0] != 'c' || line[1] != 'p' || line[2] != 'u' || line[3] < '0' || line[3] > '9' {
			continue
		}
		f := fields(fb[:0], line)
		if len(f) < 9 {
			continue
		}
		id, ok := parseUint(f[0][3:])
		if !ok || id > 1<<16 {
			continue
		}
		var v [8]uint64
		good := true
		for k := range v {
			if v[k], ok = parseUint(f[k+1]); !ok {
				good = false
			}
		}
		if !good {
			continue
		}
		for int(id) >= len(dst) {
			dst = append(dst, cpuTicks{})
		}
		busy := v[0] + v[1] + v[2] + v[5] + v[6] + v[7]
		dst[id] = cpuTicks{busy: busy, total: busy + v[3] + v[4], ok: true}
		online++
	}
	return dst, running, online
}

// parseSchedstat reads run_delay (ns tasks spent waiting on the runqueue,
// cumulative) per CPU from /proc/schedstat (version >= 15) into dst, indexed
// by CPU id; absent CPUs are -1. ok=false on an unknown format.
func parseSchedstat(b []byte, dst []int64) ([]int64, bool) {
	for i := range dst {
		dst[i] = -1
	}
	version := false
	var fb [16][]byte
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		if v, ok := bytes.CutPrefix(line, []byte("version ")); ok {
			n, ok := parseUint(bytes.TrimSpace(v))
			if !ok || n < 15 {
				return dst, false
			}
			version = true
			continue
		}
		if !version || len(line) < 4 || !bytes.HasPrefix(line, []byte("cpu")) || line[3] < '0' || line[3] > '9' {
			continue
		}
		// cpuN yld_count 0 sched_count sched_goidle ttwu_count ttwu_local
		//      rq_cpu_time run_delay pcount
		f := fields(fb[:0], line)
		if len(f) < 10 {
			return dst, false
		}
		id, ok := parseUint(f[0][3:])
		if !ok || id > 1<<16 {
			continue
		}
		d, ok := parseUint(f[8])
		if !ok {
			return dst, false
		}
		for int(id) >= len(dst) {
			dst = append(dst, -1)
		}
		dst[id] = int64(d)
	}
	return dst, version
}

// loadTau is the load1 time constant.
const loadTau = 60 * time.Second

// loadEstimator turns CPU counters into a load-per-CPU average with the
// load1 time constant, counting runnable tasks only (see LoadMethod).
type loadEstimator struct {
	method LoadMethod
	// inSet reports whether CPU id is in our affinity mask (nil = all).
	inSet func(int) bool

	seeded        bool
	perCPU        float64 // EWMA of runnable tasks per CPU
	last          time.Time
	prevT, curT   []cpuTicks
	prevD, curD   []int64
	havePrev      bool
	prevSchedstat bool
	// source is what the last update used: "loadavg", "schedstat" or
	// "procs_running" (tests, diagnostics).
	source string
}

// update folds one sample. stat is /proc/stat, schedstat /proc/schedstat
// (nil when unavailable), load1Milli the /proc/loadavg load1 x1000 (-1 if
// unreadable). It returns the load per CPU x1000, or ok=false when nothing
// could be estimated.
func (e *loadEstimator) update(now time.Time, stat, schedstat []byte, load1Milli int64) (int64, bool) {
	var running int64
	var online int
	e.curT, running, online = parseProcStatLoad(stat, e.curT)
	seed, haveSeed := 0.0, false
	if load1Milli >= 0 && online > 0 {
		seed, haveSeed = float64(load1Milli)/1000/float64(online), true
	}
	if e.method == LoadAvg {
		e.source = "loadavg"
		return int64(seed*1000 + 0.5), haveSeed
	}
	useSS := false
	if schedstat != nil {
		e.curD, useSS = parseSchedstat(schedstat, e.curD)
	}
	// x: runnable tasks per CPU over the interval since the last sample.
	x, haveX := 0.0, false
	dt := now.Sub(e.last)
	switch {
	case useSS:
		e.source = "schedstat"
		if e.havePrev && e.prevSchedstat && dt > 0 {
			sum, n := 0.0, 0
			for id := range e.curT {
				if e.inSet != nil && !e.inSet(id) {
					continue
				}
				if id >= len(e.prevT) || id >= len(e.curD) || id >= len(e.prevD) {
					continue
				}
				c, p := e.curT[id], e.prevT[id]
				if !c.ok || !p.ok || c.total <= p.total || c.busy < p.busy || e.curD[id] < e.prevD[id] || e.prevD[id] < 0 {
					continue
				}
				busy := float64(c.busy-p.busy) / float64(c.total-p.total)
				wait := float64(e.curD[id]-e.prevD[id]) / float64(dt.Nanoseconds())
				sum += busy + wait
				n++
			}
			if n > 0 {
				x, haveX = sum/float64(n), true
			}
		}
	case running >= 0 && online > 0:
		e.source = "procs_running"
		x, haveX = e.runningPerCPU(running, online, dt)
	}
	// The counters just read are the baseline of the next interval.
	e.prevT, e.curT = e.curT, e.prevT
	e.prevD, e.curD = e.curD, e.prevD
	e.havePrev, e.prevSchedstat = true, useSS
	switch {
	case !e.seeded:
		switch {
		case haveSeed:
			e.perCPU = seed
		case haveX:
			e.perCPU = x
		default:
			return 0, false
		}
		e.seeded = true
	case haveX && dt > 0:
		f := math.Exp(-float64(dt) / float64(loadTau))
		e.perCPU = e.perCPU*f + x*(1-f)
	}
	e.last = now
	return int64(e.perCPU*1000 + 0.5), true
}

// runningPerCPU estimates runnable tasks per CPU of our affinity mask from
// /proc/stat alone (no /proc/schedstat): our CPUs' busy fraction over the
// interval plus our share of the tasks waiting for a CPU. procs_running is
// host-wide; waiting = procs_running - 1 (us) - busy CPUs. A task waits only
// on a runqueue whose CPU is busy, so the waiting tasks are attributed to
// CPUs in proportion to their busy time. A --cpuset-cpus=0-1 container with
// 6 runnable tasks on an otherwise idle 16-CPU host reads 3.0 per CPU (not
// 6/16), and busy CPUs outside our mask read 0. Without a previous sample
// (or tick deltas) it is procs_running per online CPU.
func (e *loadEstimator) runningPerCPU(running int64, online int, dt time.Duration) (float64, bool) {
	tasks := float64(max(running-1, 0)) // procs_running includes the thread reading /proc/stat
	if e.havePrev && dt > 0 {
		busyAll, busySet, nAll, nSet := 0.0, 0.0, 0, 0
		for id := range e.curT {
			if id >= len(e.prevT) {
				break
			}
			c, p := e.curT[id], e.prevT[id]
			if !c.ok || !p.ok || c.total <= p.total || c.busy < p.busy {
				continue
			}
			f := min(float64(c.busy-p.busy)/float64(c.total-p.total), 1)
			busyAll += f
			nAll++
			if e.inSet == nil || e.inSet(id) {
				busySet += f
				nSet++
			}
		}
		if nSet > 0 {
			waiting := max(tasks-busyAll, 0)
			share := float64(nSet) / float64(nAll)
			if busyAll > 0 {
				share = busySet / busyAll
			}
			return (busySet + waiting*share) / float64(nSet), true
		}
	}
	return tasks / float64(online), true
}

// initialUserNSFrom reports whether the reading (raw, err) of
// /proc/self/uid_map shows the initial user namespace: the identity map, or
// no such file (a kernel without CONFIG_USER_NS has only the initial one).
func initialUserNSFrom(raw []byte, err error) bool {
	if errors.Is(err, os.ErrNotExist) {
		return true
	}
	return err == nil && parseUIDMapIdentity(raw)
}

// parseUIDMapIdentity reports whether /proc/self/uid_map is the initial user
// namespace's identity map "0 0 4294967295". Capabilities in CapEff are
// relative to our own user namespace, while the scheduler checks
// CAP_SYS_NICE against the initial one.
func parseUIDMapIdentity(b []byte) bool {
	var fb [4][]byte
	lines := 0
	ok := false
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		f := fields(fb[:0], line)
		if len(f) == 0 {
			continue
		}
		lines++
		ok = len(f) == 3 && string(f[0]) == "0" && string(f[1]) == "0" && string(f[2]) == "4294967295"
	}
	return lines == 1 && ok
}

// ---------------------------------------------------------------------------
// Per-process files.

// procStat is the subset of /proc/pid/stat the backend uses.
type procStat struct {
	PID       int
	Comm      []byte // aliases the input buffer
	State     byte
	PPID      int
	PGRP      int
	Session   int
	TTYNr     int64 // controlling terminal device; 0 = none
	Flags     uint64
	UTime     uint64 // clock ticks
	STime     uint64
	StartTime uint64 // clock ticks since boot
	RSSPages  int64
}

// parseStat parses /proc/pid/stat. comm is everything between the first '('
// and the LAST ')': it may itself contain spaces, parentheses and ") (".
func parseStat(b []byte) (procStat, error) {
	var s procStat
	b = bytes.TrimRight(b, "\n")
	open := bytes.IndexByte(b, '(')
	closeP := bytes.LastIndexByte(b, ')')
	if open < 1 || closeP < open {
		return s, errors.New("platform: stat: no comm")
	}
	pid, ok := parseUint(bytes.TrimSpace(b[:open]))
	if !ok || pid == 0 || pid > 1<<31 {
		return s, errors.New("platform: stat: bad pid")
	}
	s.PID = int(pid)
	s.Comm = b[open+1 : closeP]
	// Fields after comm, numbered as in proc(5): 3=state ... 24=rss.
	rest := b[closeP+1:]
	field := 3
	var vals [25][]byte
	for i := 0; i < len(rest) && field <= 24; {
		for i < len(rest) && rest[i] == ' ' {
			i++
		}
		if i >= len(rest) {
			break
		}
		j := i
		for j < len(rest) && rest[j] != ' ' {
			j++
		}
		vals[field] = rest[i:j]
		field++
		i = j
	}
	if field <= 24 {
		return s, fmt.Errorf("platform: stat: only %d fields", field-1)
	}
	if len(vals[3]) != 1 {
		return s, errors.New("platform: stat: bad state")
	}
	s.State = vals[3][0]
	num := func(i int) uint64 {
		v, good := parseUint(vals[i])
		if !good {
			ok = false
		}
		return v
	}
	ok = true
	ppid := num(4)
	pgrp := num(5)
	sess := num(6)
	tty, goodTTY := parseInt(vals[7])
	s.Flags = num(9)
	s.UTime = num(14)
	s.STime = num(15)
	s.StartTime = num(22)
	rss, good := parseInt(vals[24])
	if !ok || !good || !goodTTY || ppid > 1<<31 || pgrp > 1<<31 || sess > 1<<31 {
		return s, errors.New("platform: stat: bad number")
	}
	s.PPID, s.PGRP, s.Session, s.TTYNr = int(ppid), int(pgrp), int(sess), tty
	if rss < 0 {
		rss = 0
	}
	s.RSSPages = rss
	return s, nil
}

// normState maps Linux task states onto the contract's letters: R, S, D, T,
// Z are kept; t (traced) -> T; I/P/K/W (idle, parked, wakekill, waking) ->
// S; X/x (dead) -> X (callers drop those).
func normState(c byte) byte {
	switch c {
	case 'R', 'S', 'D', 'T', 'Z':
		return c
	case 't':
		return 'T'
	case 'X', 'x':
		return 'X'
	default:
		return 'S'
	}
}

// procStatus is the subset of /proc/pid/status the backend uses (bytes).
type procStatus struct {
	UID                              int // real uid, -1 if absent
	VmRSS, RssAnon, RssShmem, VmSwap int64
	HaveRssAnon                      bool
}

func parseStatus(b []byte) procStatus {
	st := procStatus{UID: -1}
	var arr [6][]byte
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		k := lineKey(line)
		var dst *int64
		switch string(k) {
		case "Uid":
			fs := fields(arr[:0], line[len(k)+1:])
			if len(fs) > 0 {
				if u, ok := parseUint(fs[0]); ok && u < 1<<31 {
					st.UID = int(u)
				}
			}
			continue
		case "VmRSS":
			dst = &st.VmRSS
		case "RssAnon":
			dst = &st.RssAnon
			st.HaveRssAnon = true
		case "RssShmem":
			dst = &st.RssShmem
		case "VmSwap":
			dst = &st.VmSwap
		default:
			continue
		}
		if v, ok := kbValue(line); ok {
			*dst = v
		}
	}
	return st
}

// anonFootprint is the FootprintAnon value from a parsed status.
func (s procStatus) anonFootprint() int64 {
	if s.HaveRssAnon {
		return s.RssAnon + s.RssShmem + s.VmSwap
	}
	return s.VmRSS + s.VmSwap // kernel < 4.5
}

var (
	errStatmShort = errors.New("platform: statm: short")
	errStatmBad   = errors.New("platform: statm: bad number")
)

// parseStatm returns resident and shared page counts of /proc/pid/statm.
func parseStatm(b []byte) (resident, shared int64, err error) {
	var fs [8][]byte
	n := 0
	for i := 0; i < len(b) && n < len(fs); {
		for i < len(b) && (b[i] == ' ' || b[i] == '\n') {
			i++
		}
		j := i
		for j < len(b) && b[j] != ' ' && b[j] != '\n' {
			j++
		}
		if j > i {
			fs[n] = b[i:j]
			n++
		}
		i = j
	}
	if n < 3 {
		return 0, 0, errStatmShort
	}
	r, ok1 := parseUint(fs[1])
	s, ok2 := parseUint(fs[2])
	if !ok1 || !ok2 {
		return 0, 0, errStatmBad
	}
	return int64(r), int64(s), nil
}

// smapsRollup is the subset of /proc/pid/smaps_rollup the backend uses.
type smapsRollup struct {
	Rss, Pss, PssAnon, PssShmem, Swap, SwapPss int64
	HavePss, HavePssAnon                       bool
}

func parseSmapsRollup(b []byte) smapsRollup {
	var r smapsRollup
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		var dst *int64
		switch string(lineKey(line)) {
		case "Rss":
			dst = &r.Rss
		case "Pss":
			dst, r.HavePss = &r.Pss, true
		case "Pss_Anon":
			dst, r.HavePssAnon = &r.PssAnon, true
		case "Pss_Shmem":
			dst = &r.PssShmem
		case "Swap":
			dst = &r.Swap
		case "SwapPss":
			dst = &r.SwapPss
		default:
			continue
		}
		if v, ok := kbValue(line); ok {
			*dst = v
		}
	}
	return r
}

// envTag returns the value of the first PACE_JOB_ID= entry of a NUL-separated
// environment block ("" if none). Values are capped at 256 bytes and must be
// printable ASCII, so a corrupt block cannot produce a junk job id.
func envTag(b []byte) string {
	const key = "PACE_JOB_ID="
	for len(b) > 0 {
		var e []byte
		if i := bytes.IndexByte(b, 0); i >= 0 {
			e, b = b[:i], b[i+1:]
		} else {
			e, b = b, nil
		}
		v, ok := bytes.CutPrefix(e, []byte(key))
		if !ok {
			continue
		}
		if len(v) == 0 || len(v) > 256 {
			return ""
		}
		for _, c := range v {
			if c < 0x21 || c > 0x7e {
				return ""
			}
		}
		return string(v)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Process classification.

var systemExePrefixes = []string{
	"/usr/lib/systemd/", "/lib/systemd/", "/usr/sbin/", "/sbin/",
}

// systemExeBases are same-user session infrastructure protected wherever
// installed (distros and Nix put them in different places).
var systemExeBases = map[string]bool{
	"systemd": true, "dbus-daemon": true, "dbus-broker": true, "dbus-broker-launch": true,
	// The container runtime: killing it takes every container down. dockerd
	// and containerd live in /usr/bin on Debian/Ubuntu, so the prefixes
	// above do not cover them. The containers' own processes stay ordinary.
	"dockerd": true, "docker-proxy": true, "containerd": true, "containerd-shim": true,
	"containerd-shim-runc-v1": true, "containerd-shim-runc-v2": true, "runc": true, "crun": true,
	"crio": true, // not conmon: rootless podman's container monitor is the user's own work
	// Rootless Docker: rootlesskit (its child's comm is "exe", its exe still
	// rootlesskit) is the service's main process and shares dockerd's group;
	// killing it, or its network or filesystem helpers, stops every container.
	"rootlesskit": true, "slirp4netns": true, "vpnkit": true, "fuse-overlayfs": true,
}

// libexecDir holds per-session helper daemons as direct children (gvfsd,
// gsd-*, at-spi-bus-launcher, xdg-desktop-portal, goa-daemon, ...). Its
// subdirectories hold toolchains and plumbing users run as ordinary work
// (gcc's cc1/cc1plus in /usr/libexec/gcc/<triple>/<ver>/, git-core, podman,
// docker cli-plugins), so only depth 1 counts, minus libexecUserExes.
const libexecDir = "/usr/libexec/"

// libexecUserExes are heavy user workloads that distros install directly in
// /usr/libexec (RHEL/Fedora qemu-kvm, run by libvirt user sessions).
var libexecUserExes = map[string]bool{"qemu-kvm": true}

// isSystemProc decides Proc.System on Linux: init (pid 1), kernel threads
// (PF_KTHREAD only: inside a PID namespace pid 2 and its children are
// ordinary user processes), a process whose exe cannot be read for
// permission reasons (we cannot tell what it is), exes under the OS
// prefixes, session helpers directly in /usr/libexec, and exes named like
// the session bus / user manager.
func isSystemProc(pid int, flags uint64, exe string, exeDenied bool) bool {
	if pid == 1 || flags&pfKthread != 0 || exeDenied {
		return true
	}
	if exe == "" {
		return false
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	for _, p := range systemExePrefixes {
		if strings.HasPrefix(exe, p) {
			return true
		}
	}
	base := exe
	if i := strings.LastIndexByte(exe, '/'); i >= 0 {
		base = exe[i+1:]
	}
	if rest, ok := strings.CutPrefix(exe, libexecDir); ok && rest != "" &&
		!strings.Contains(rest, "/") && !libexecUserExes[rest] {
		return true
	}
	return systemExeBases[base]
}

// ---------------------------------------------------------------------------
// cgroup v2 discovery parsing.

// parseSelfCgroup returns the cgroup v2 path ("0::/x/y") of a /proc/pid/cgroup
// file; ok=false on a v1-only host.
func parseSelfCgroup(b []byte) (string, bool) {
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		if p, ok := bytes.CutPrefix(line, []byte("0::")); ok {
			s := strings.TrimSuffix(string(p), " (deleted)")
			if strings.HasPrefix(s, "/") {
				return s, true
			}
		}
	}
	return "", false
}

// parseMountinfoCgroup2 returns the mount point and mount root of the first
// cgroup2 mount in a mountinfo file.
func parseMountinfoCgroup2(b []byte) (mountPoint, root string, ok bool) {
	var fs [][]byte
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		fs = fields(fs[:0], line)
		// id parent maj:min root mountpoint opts [optional...] - fstype source superopts
		sep := -1
		for k := 6; k < len(fs); k++ {
			if len(fs[k]) == 1 && fs[k][0] == '-' {
				sep = k
				break
			}
		}
		if sep < 0 || sep+1 >= len(fs) || len(fs) < 5 {
			continue
		}
		if string(fs[sep+1]) == "cgroup2" {
			return unescapeMount(fs[4]), unescapeMount(fs[3]), true
		}
	}
	return "", "", false
}

// unescapeMount decodes the \ooo octal escapes mountinfo uses for spaces etc.
func unescapeMount(b []byte) string {
	if bytes.IndexByte(b, '\\') < 0 {
		return string(b)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		if b[i] == '\\' && i+3 < len(b) {
			if v, err := strconv.ParseUint(string(b[i+1:i+4]), 8, 8); err == nil {
				out = append(out, byte(v))
				i += 3
				continue
			}
		}
		out = append(out, b[i])
	}
	return string(out)
}

// cgroupDir joins a cgroup2 mount and a /proc/self/cgroup path, honouring a
// mount whose root is a subtree (bind mounts): the path must lie under the
// mount root. ok=false when it does not (the cgroup is not visible here).
func cgroupDir(mountPoint, mountRoot, cgPath string) (string, bool) {
	mountRoot = strings.TrimRight(mountRoot, "/")
	if mountRoot != "" {
		if cgPath != mountRoot && !strings.HasPrefix(cgPath, mountRoot+"/") {
			return "", false
		}
		cgPath = cgPath[len(mountRoot):]
	}
	if strings.Contains("/"+cgPath+"/", "/../") {
		return "", false
	}
	d := strings.TrimRight(mountPoint, "/") + strings.TrimRight(cgPath, "/")
	if d == "" {
		d = "/"
	}
	return d, true
}

// cgroupOwnMemory is memory.stat anon + shmem (see cgroupContainer.Usage).
func cgroupOwnMemory(stat []byte) (int64, bool) {
	anon, ok := parseCgroupStatKey(stat, "anon")
	if !ok {
		return 0, false
	}
	shmem, _ := parseCgroupStatKey(stat, "shmem")
	return anon + shmem, true
}

// ownOOMKills reads memory.events: the processes OOM-killed for the cgroup's
// own limit. oom_kill (or oom_group_kill) counts only when "oom" (the cgroup
// reached its memory.max) is non-zero: alone it also counts kills by the
// machine-wide OOM killer or for an ancestor's limit.
func ownOOMKills(events []byte) int64 {
	if oom, _ := parseCgroupStatKey(events, "oom"); oom <= 0 {
		return 0
	}
	n, _ := parseCgroupStatKey(events, "oom_kill")
	if g, _ := parseCgroupStatKey(events, "oom_group_kill"); g > n {
		n = g
	}
	return n
}

// parseCgroupStatKey returns one "key value" entry of memory.stat (bytes).
func parseCgroupStatKey(b []byte, key string) (int64, bool) {
	for i := 0; i < len(b); {
		var line []byte
		line, i = nextLine(b, i)
		if v, ok := bytes.CutPrefix(line, []byte(key)); ok && len(v) > 1 && v[0] == ' ' {
			n, ok := parseUint(bytes.TrimSpace(v))
			if ok && n < 1<<62 {
				return int64(n), true
			}
			return 0, false
		}
	}
	return 0, false
}

// parseCgroupMax parses memory.max-style content: "max" -> -1 (unlimited).
func parseCgroupMax(b []byte) (int64, bool) {
	b = bytes.TrimSpace(b)
	if string(b) == "max" {
		return -1, true
	}
	n, ok := parseUint(b)
	if !ok || n >= 1<<62 {
		return 0, false
	}
	return int64(n), true
}

// ---------------------------------------------------------------------------
// File reading through fds with a reused buffer.

// readFd reads all of fd into *buf (grown up to limit) and returns the bytes.
// A file larger than limit is truncated to limit bytes (no error).
func readFd(fd int, buf *[]byte, limit int) ([]byte, error) {
	b := *buf
	if cap(b) == 0 {
		b = make([]byte, 0, 4096)
	}
	b = b[:0]
	for {
		if len(b) == cap(b) {
			if cap(b) >= limit {
				break
			}
			nb := make([]byte, len(b), min(2*cap(b), limit))
			copy(nb, b)
			b = nb
		}
		n, err := unix.Read(fd, b[len(b):cap(b)])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			*buf = b
			return nil, err
		}
		if n == 0 {
			break
		}
		b = b[:len(b)+n]
	}
	*buf = b
	return b, nil
}

// readAt reads dirfd/name into *buf.
func readAt(dirfd int, name string, buf *[]byte, limit int) ([]byte, error) {
	fd, err := openatRetry(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	return readFd(fd, buf, limit)
}

// readPath reads an absolute path into *buf.
func readPath(path string, buf *[]byte, limit int) ([]byte, error) {
	return readAt(unix.AT_FDCWD, path, buf, limit)
}

func openatRetry(dirfd int, name string, flags int) (int, error) {
	for {
		fd, err := unix.Openat(dirfd, name, flags, 0)
		if err == unix.EINTR {
			continue
		}
		return fd, err
	}
}

// isVanished reports errors meaning "the process went away" (skip silently).
func isVanished(err error) bool {
	return errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH)
}

func isDenied(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// ---------------------------------------------------------------------------
// Process scanner.

type procCacheEntry struct {
	start  int64
	comm   string
	exe    string
	system bool
	gen    uint32
}

// procScanner lists one user's processes from a procfs tree. Not safe for
// concurrent use (the Linux backend serialises calls).
type procScanner struct {
	root     string
	method   FootprintMethod
	pageSize int64
	btimeMS  int64 // read once; see package comment
	// btimeFile persists the first btime of this boot across restarts
	// ("" = off); see LinuxOptions.BtimeFile.
	btimeFile string
	buf       []byte
	linkBuf   []byte
	cache     map[int]*procCacheEntry
	gen       uint32
	lastN     int
	// ownerOf returns the uid owning /proc/<pid> (fstat result); tests
	// override it because fixture files are all owned by the test user.
	ownerOf func(pid int, st *unix.Stat_t) int
	// stats of the last scan, for measurements and tests
	lastSkippedVanished int
}

func newProcScanner(root string, method FootprintMethod) *procScanner {
	if method == "" {
		method = FootprintAnon
	}
	return &procScanner{
		root:     root,
		method:   method,
		pageSize: int64(os.Getpagesize()),
		cache:    make(map[int]*procCacheEntry),
		linkBuf:  make([]byte, 4096),
		ownerOf:  func(_ int, st *unix.Stat_t) int { return int(st.Uid) },
	}
}

// bootMS returns boot time in unix ms, read once.
func (s *procScanner) bootMS() (int64, error) {
	if s.btimeMS != 0 {
		return s.btimeMS, nil
	}
	b, err := readPath(s.root+"/stat", &s.buf, maxFileBuf)
	if err != nil {
		return 0, fmt.Errorf("platform: read %s/stat: %w", s.root, err)
	}
	bt, err := parseBtime(b)
	if err != nil {
		return 0, err
	}
	s.btimeMS = bt * 1000
	if s.btimeFile != "" {
		s.btimeMS = s.anchorBtime(s.btimeMS)
	}
	return s.btimeMS, nil
}

// btimeMagic heads the btime anchor file: "pace-btime 1 <boot_id> <ms>\n".
const btimeMagic = "pace-btime 1"

func formatBtimeAnchor(bootID string, ms int64) []byte {
	return []byte(btimeMagic + " " + bootID + " " + strconv.FormatInt(ms, 10) + "\n")
}

func parseBtimeAnchor(b []byte) (bootID string, ms int64, ok bool) {
	f := strings.Fields(string(b))
	if len(f) != 4 || f[0]+" "+f[1] != btimeMagic || f[2] == "" {
		return "", 0, false
	}
	v, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil || v <= 0 {
		return "", 0, false
	}
	return f[2], v, true
}

// anchorBtime returns the boot time recorded in s.btimeFile for this boot
// (same boot_id), recording cur when there is none. The kernel recomputes
// btime from the wall clock on every read, so a clock step (or NTP slew
// accumulated over days) between two daemon runs would otherwise move every
// StartMS and orphan the persisted unit keys. Any failure returns cur.
func (s *procScanner) anchorBtime(cur int64) int64 {
	raw, err := readPath(s.root+"/sys/kernel/random/boot_id", &s.buf, 256)
	if err != nil {
		return cur
	}
	id := strings.TrimSpace(string(raw))
	if id == "" || strings.ContainsAny(id, " \t\n") {
		return cur
	}
	stored := func() (int64, bool) {
		b, err := os.ReadFile(s.btimeFile)
		if err != nil {
			return 0, false
		}
		bid, ms, ok := parseBtimeAnchor(b)
		return ms, ok && bid == id
	}
	if ms, ok := stored(); ok {
		return ms
	}
	dir := s.btimeFile[:max(strings.LastIndexByte(s.btimeFile, '/'), 0)]
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return cur
		}
	}
	data := formatBtimeAnchor(id, cur)
	// First writer wins when the file is absent (O_EXCL), so a wrapper and a
	// daemon starting together agree; a stale file (older boot) is replaced.
	if f, err := os.OpenFile(s.btimeFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil && cerr == nil {
			return cur
		}
		_ = os.Remove(s.btimeFile)
		return cur
	} else if !errors.Is(err, os.ErrExist) {
		return cur
	}
	tmp := s.btimeFile + ".tmp" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		_ = os.Remove(tmp)
		return cur
	}
	if err := os.Rename(tmp, s.btimeFile); err != nil {
		_ = os.Remove(tmp)
		return cur
	}
	if ms, ok := stored(); ok {
		return ms
	}
	return cur
}

// list returns uid's processes sorted by pid. Processes that vanish or are
// unreadable mid-scan are skipped.
func (s *procScanner) list(uid int) ([]Proc, error) {
	if uid < 0 {
		return nil, fmt.Errorf("platform: bad uid %d", uid)
	}
	boot, err := s.bootMS()
	if err != nil {
		return nil, err
	}
	d, err := os.Open(s.root)
	if err != nil {
		return nil, fmt.Errorf("platform: open %s: %w", s.root, err)
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, fmt.Errorf("platform: list %s: %w", s.root, err)
	}
	rootfd := int(d.Fd())
	pids := make([]int, 0, len(names))
	for _, n := range names {
		if v, ok := parseUint([]byte(n)); ok && v > 0 && v < 1<<31 && n[0] != '0' {
			pids = append(pids, int(v))
		}
	}
	sort.Ints(pids)

	s.gen++
	s.lastSkippedVanished = 0
	out := make([]Proc, 0, max(s.lastN, 16))
	var st unix.Stat_t
	var nameBuf [16]byte
	for _, pid := range pids {
		name := strconv.AppendInt(nameBuf[:0], int64(pid), 10)
		dfd, err := openatRetry(rootfd, string(name), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC)
		if err != nil {
			s.lastSkippedVanished++
			continue
		}
		p, ok := s.readProc(dfd, pid, uid, boot, &st)
		unix.Close(dfd)
		if ok {
			out = append(out, p)
		}
	}
	// Forget processes not seen this scan.
	for pid, e := range s.cache {
		if e.gen != s.gen {
			delete(s.cache, pid)
		}
	}
	s.lastN = len(out)
	return out, nil
}

func (s *procScanner) readProc(dfd, pid, uid int, boot int64, st *unix.Stat_t) (Proc, bool) {
	if err := unix.Fstat(dfd, st); err != nil {
		return Proc{}, false
	}
	if s.ownerOf(pid, st) != uid {
		return Proc{}, false
	}
	b, err := readAt(dfd, "stat", &s.buf, maxFileBuf)
	if err != nil {
		s.lastSkippedVanished++
		return Proc{}, false
	}
	ps, err := parseStat(b)
	if err != nil || ps.PID != pid {
		return Proc{}, false
	}
	state := normState(ps.State)
	if state == 'X' {
		return Proc{}, false
	}
	p := Proc{
		PID:     pid,
		PPID:    ps.PPID,
		PGID:    ps.PGRP,
		UID:     uid,
		StartMS: boot + int64(ps.StartTime)*msPerTick,
		CPUMS:   int64(ps.UTime+ps.STime) * msPerTick,
		State:   state,
		// A session leader with a controlling terminal: a terminal's shell.
		TermLeader: ps.Session == pid && ps.TTYNr != 0,
	}
	rssPages := ps.RSSPages
	flags := ps.Flags

	// comm/exe/system cache keyed by (pid, start, comm): exec changes comm,
	// so a changed comm re-reads exe and reclassifies.
	e := s.cache[pid]
	if e != nil && e.start == p.StartMS && e.comm == string(ps.Comm) {
		p.Comm, p.Exe, p.System = e.comm, e.exe, e.system
		e.gen = s.gen
	} else {
		comm := string(ps.Comm)
		exe, denied, cacheable := "", false, true
		if flags&pfKthread == 0 {
			exe, denied, cacheable = s.readExe(dfd)
		}
		p.Comm, p.Exe = comm, exe
		p.System = isSystemProc(pid, flags, exe, denied)
		if cacheable {
			if e == nil {
				e = &procCacheEntry{}
				s.cache[pid] = e
			}
			*e = procCacheEntry{start: p.StartMS, comm: comm, exe: exe, system: p.System, gen: s.gen}
		} else if e != nil {
			e.gen = s.gen - 1 // stale: drop at the end of this scan
		}
	}
	if flags&pfKthread != 0 {
		return p, true // kernel threads own no user memory
	}
	p.Footprint = s.footprint(dfd, rssPages)
	return p, true
}

// readExe resolves the exe link. denied=true when permission was refused;
// cacheable=false when the answer may change (process exiting, zombie).
func (s *procScanner) readExe(dfd int) (exe string, denied, cacheable bool) {
	for {
		n, err := unix.Readlinkat(dfd, "exe", s.linkBuf)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			if isDenied(err) {
				return "", true, true
			}
			return "", false, false
		}
		if n == len(s.linkBuf) && len(s.linkBuf) < 1<<16 {
			s.linkBuf = make([]byte, 2*len(s.linkBuf))
			continue
		}
		return string(s.linkBuf[:n]), false, true
	}
}

// footprint measures one process with the configured method. On a read
// failure it degrades to stat's rss (never to 0 for a live process).
func (s *procScanner) footprint(dfd int, rssPages int64) int64 {
	fallback := rssPages * s.pageSize
	switch s.method {
	case FootprintRSS:
		return fallback
	case FootprintRSSSwap:
		b, err := readAt(dfd, "statm", &s.buf, 4096)
		if err != nil {
			return fallback
		}
		res, _, err := parseStatm(b)
		if err != nil {
			return fallback
		}
		v := res * s.pageSize
		if b, err := readAt(dfd, "status", &s.buf, 64<<10); err == nil {
			v += parseStatus(b).VmSwap
		}
		return v
	case FootprintPSS, FootprintPSSAnon:
		b, err := readAt(dfd, "smaps_rollup", &s.buf, 64<<10)
		if err == nil {
			r := parseSmapsRollup(b)
			if r.HavePss {
				if s.method == FootprintPSSAnon && r.HavePssAnon {
					return r.PssAnon + r.PssShmem + r.SwapPss
				}
				return r.Pss + r.SwapPss
			}
		}
		fallthrough // smaps_rollup absent (kernel < 4.14) or unreadable
	default: // FootprintAnon
		b, err := readAt(dfd, "status", &s.buf, 64<<10)
		if err != nil {
			return fallback
		}
		return parseStatus(b).anonFootprint()
	}
}

// jobTag reads PACE_JOB_ID from root/<pid>/environ ("" when unreadable).
func (s *procScanner) jobTag(pid int) string {
	if pid <= 0 {
		return ""
	}
	b, err := readPath(s.root+"/"+strconv.Itoa(pid)+"/environ", &s.buf, maxFileBuf)
	if err != nil {
		return ""
	}
	return envTag(b)
}

// errnoOf unwraps a syscall.Errno (0 if none), for tests and logs.
func errnoOf(err error) syscall.Errno {
	var e syscall.Errno
	if errors.As(err, &e) {
		return e
	}
	return 0
}

// Linux SetBackground marks every thread it changes with IOPRIO_CLASS_IDLE
// and a non-zero level (`ionice -c3` sets level 0; the kernel ignores the
// level of the idle class but keeps and reports it, measured). The level
// records what a restore without our in-memory record (a daemon restart, or
// a thread created from a changed one, which inherits the mark) may undo:
//
//	1      only the ioprio was changed
//	2..5   policy and/or nice changed: 2 + 1 if the original policy was
//	       SCHED_BATCH (else SCHED_NORMAL) + 2 if nice was raised from 0
//	6, 7   policy changed, original SCHED_NORMAL, nice unknown (level 7 is
//	       the mark of earlier builds)
//
// So a thread whose policy we did not change (a user's chrt -i or chrt -b
// thread) keeps it, and a user BATCH thread goes back to BATCH, not NORMAL.
const (
	ioprioClassShift = 13
	ioprioClassIdle  = 3
	ioprioDataMask   = 1<<ioprioClassShift - 1
	ioprioIdleMark   = ioprioClassIdle << ioprioClassShift
	bgMarkIOOnly     = 1
	bgMarkSched      = 2
	bgMarkOrigBatch  = 1
	bgMarkNiceFrom0  = 2
)

// bgMarkValue is the ioprio value marking a thread with what we changed.
func bgMarkValue(sched, origBatch, niceFrom0 bool) int {
	if !sched {
		return ioprioIdleMark | bgMarkIOOnly
	}
	lvl := bgMarkSched
	if origBatch {
		lvl += bgMarkOrigBatch
	}
	if niceFrom0 {
		lvl += bgMarkNiceFrom0
	}
	return ioprioIdleMark | lvl
}

// activeScheduler returns the bracketed (active) name of a
// /sys/block/<dev>/queue/scheduler file ("[none] mq-deadline bfq" -> "none").
func activeScheduler(b []byte) string {
	i := bytes.IndexByte(b, '[')
	j := bytes.IndexByte(b, ']')
	if i < 0 || j <= i {
		return string(bytes.TrimSpace(b)) // a single scheduler is printed bare
	}
	return string(b[i+1 : j])
}

// ioIdleHonoured reports whether the idle I/O class (the I/O half of the
// Linux throttle) can act: only the BFQ scheduler honours it. It looks at
// the physical disks under sysBlock (/sys/block); virtual devices (loop,
// ram, zram, dm, md, nbd, optical) take their scheduler from the disks
// below them and are skipped. known=false when no disk could be read.
func ioIdleHonoured(sysBlock string) (honoured, known bool) {
	ents, err := os.ReadDir(sysBlock)
	if err != nil {
		return false, false
	}
	for _, e := range ents {
		n := e.Name()
		skip := false
		for _, p := range []string{"loop", "ram", "zram", "dm-", "md", "nbd", "sr"} {
			if strings.HasPrefix(n, p) {
				skip = true
			}
		}
		if skip {
			continue
		}
		b, err := os.ReadFile(sysBlock + "/" + n + "/queue/scheduler")
		if err != nil {
			continue
		}
		known = true
		if activeScheduler(b) == "bfq" {
			return true, true
		}
	}
	return false, known
}

// parseBgMark decodes an ioprio value; ours=false if it is not our mark.
func parseBgMark(v int) (sched, origBatch, niceFrom0, ours bool) {
	if v>>ioprioClassShift != ioprioClassIdle {
		return false, false, false, false
	}
	lvl := v & ioprioDataMask
	switch {
	case lvl == bgMarkIOOnly:
		return false, false, false, true
	case lvl >= bgMarkSched && lvl <= bgMarkSched+bgMarkOrigBatch+bgMarkNiceFrom0:
		d := lvl - bgMarkSched
		return true, d&bgMarkOrigBatch != 0, d&bgMarkNiceFrom0 != 0, true
	case lvl == 6 || lvl == 7:
		return true, false, false, true
	}
	return false, false, false, false
}
