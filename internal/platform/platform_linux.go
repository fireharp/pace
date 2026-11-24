//go:build linux

package platform

// Linux backend: procfs + PSI sensing, kill(2) enforcement, reversible
// deprioritisation, and an optional cgroup v2 JobContainer (cgroup_linux.go).
// Pure Go, CGO_ENABLED=0. Parsing lives in procparse.go (no build tag).
//
// Measured (scripts/docker-linux-test.sh MEASURE=1: Docker Desktop VM,
// kernel 6.12 arm64, native binary, 303 processes of the user):
//
//	ListProcs median   anon 1.9 ms (6.4 us/proc)   rss 0.9 ms (2.9 us)
//	                   rss_swap 2.3 ms (7.5 us)     pss 6.0 ms (19.9 us)
//	                   pss_anon 5.1 ms (16.8 us)    cold (exe readlink) 2.3 ms
//	one read           stat 1.7 us, statm 1.4 us, status 2.6 us, environ 1.3 us,
//	                   smaps_rollup 8.6 us (small) / 13.3 us (200 MiB process)
//	SampleMachine 17 us, QuickPressure 30 us, JobTag 1.4 us
//	200 MiB anon alloc: every method within +1..+2 %; an idle Go helper reads
//	                   anon 2.3-4.3 MiB, rss 4.6-6.7 MiB, pss 3.8-5.8 MiB
//	cgroup Usage of a 100 MiB job 101.2 MiB vs anon footprint 102.3 MiB
//
// Decisions:
//
//   - Footprint default is "anon" = RssAnon + RssShmem + VmSwap from
//     /proc/pid/status: the memory a kill would actually free, matching the
//     macOS physical footprint semantics (clean file pages excluded), at
//     2x the cost of stat alone. smaps_rollup (pss, pss_anon) is the only
//     method that does not double count pages shared between a job's
//     processes, but it costs 3x and walks page tables under the target's
//     mmap lock (cost grows with process size; it can block behind a writer
//     under memory pressure, which the Backend contract forbids), so it is
//     opt-in via PACE_LINUX_FOOTPRINT. Wrapped jobs inside a cgroup container
//     get exact per-job numbers from the cgroup anyway.
//   - KernelLevel is always 0 (unknown) in SampleMachine: Linux has no kernel
//     pressure level, and the policy folds PSI itself with the configured
//     thresholds (Params.PSIWarn10/PSICrit10). QuickPressure, which runs
//     without config, derives a level from PSI and MemAvailable with the
//     default thresholds (see linuxQuickPressure).
//   - Total/Avail are capped by the cgroup-namespace root's memory.max when
//     it is finite (a container limit); the daemon's own cgroup chain is NOT
//     consulted, because a small MemoryMax on pace.service would otherwise
//     look like a tiny machine. PSI is the max of /proc/pressure/memory and
//     that cgroup's memory.pressure.
//   - NCPU = runtime.NumCPU() (the sched affinity mask). Load1Milli is NOT
//     /proc/loadavg: that is host-wide and counts D-state tasks, so divided
//     by a cpuset container's NCPU it showed idle CPUs as overloaded. It is
//     load-per-CPU x NCPU, the load per CPU being a 1-minute average of
//     runnable tasks only (see LoadMethod; PACE_LINUX_LOAD=loadavg for host
//     load1 per online CPU).
//   - SetBackground (measured as uid 1000 without capabilities: SCHED_IDLE
//     set ok, back to SCHED_OTHER EPERM; SCHED_BATCH set and back ok; ioprio
//     IDLE set and back ok; nice 0->10 ok, 10->0 EACCES; as root with
//     CAP_SYS_NICE all of them reversible). So: I/O always goes to
//     IOPRIO_CLASS_IDLE (level 1-5, our mark of what was changed, see
//     bgMarkValue); CPU goes to SCHED_IDLE only
//     when the revert is known to be permitted (CAP_SYS_NICE in the initial
//     user namespace and a probe of our own thread, or the target's
//     RLIMIT_NICE >= 20-nice), otherwise to SCHED_BATCH plus nice
//     LinuxOptions.ThrottleNice (BATCH alone keeps the full CFS weight:
//     measured 50 % -> 50 % share; with nice 10 50 % -> 10.5 %). That nice is
//     one-way unprivileged: restore puts the policy and ioprio back and
//     leaves the raised nice (best effort, as ARCHITECTURE.md allows), which
//     ThrottleNote reports in `pace status` and docs/CONTRACT.md documents.
//     Applied per thread (sched and ioprio are per-task on Linux); only
//     fair-class threads are changed, and each is restored to its own
//     saved policy/nice/ioprio.
//   - Those measurements are of threads sharing one CPU. Under a container
//     CPU quota (cpu.max on the cgroup-namespace root, fewer CPUs than the
//     affinity mask) they do not carry over: nice and SCHED_IDLE weigh only
//     the threads of one runqueue, while CFS bandwidth hands the quota's
//     runtime to every CPU's runqueue alike, so a throttled thread alone on
//     a CPU keeps its full share. Measured in a cpu.max 2-of-8-CPUs
//     container (kernel 5.15, uid 1001, nice 10): a throttled burner ran at
//     0.18-1.26x the per-thread rate of an unthrottled one beside it.
//     Nothing unprivileged and fail-open fixes that per thread, so the
//     throttle is unchanged there and ThrottleNote / ThrottleCaveat say it
//     is limited (a job container's own cpu.max still holds).
//   - Signal/SignalGroup refuse pid/pgid <= 1 (kill(-1) would hit every
//     process) and the caller's own pid/pgid.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/envcompat"
	"golang.org/x/sys/unix"
)

func init() {
	newNative = func() (Backend, error) {
		o, err := LinuxOptionsFromEnv(envcompat.Getenv)
		if err != nil {
			return nil, err
		}
		return newLinux(o)
	}
	quickPressure = linuxQuickPressure
	newContainersAt = func(base string) (JobContainer, error) {
		o, err := LinuxOptionsFromEnv(envcompat.Getenv)
		if err != nil {
			return nil, err
		}
		if o.NoContainers {
			return nil, errors.New("cgroup: disabled by PACE_LINUX_CGROUP=0")
		}
		c, err := openCgroupContainer(o, base)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// threadSaved is one thread's settings before SetBackground(pid, true).
type threadSaved struct {
	policy uint32 // original policy (always a fair one when sched is set)
	nice   int32  // original nice (restored when niced)
	ioprio int
	sched  bool // we changed policy and/or nice
	niced  bool // we raised nice
	io     bool // we set our ioprio mark
}

// bgSaved is what SetBackground(pid, true) replaced, restored per thread by
// SetBackground(pid, false).
type bgSaved struct {
	cpuSet uint32 // policy we applied to fair threads (SCHED_IDLE or SCHED_BATCH)
	nice   int32  // nice we raised fair threads to (0 = nice untouched)
	// Threads created after background inherit our settings and mark; the
	// mark gives their creator's original policy (and nice when it was 0),
	// their ioprio goes back to the main thread's non-idle one (else none).
	newIoprio int
	threads   map[int]threadSaved
}

// markedThread is the record of a thread we have none for that carries our
// mark: its creator's original settings, as far as the mark tells them.
func markedThread(io int, curNice int32, ioprio int) threadSaved {
	sched, batch, from0, _ := parseBgMark(io)
	r := threadSaved{policy: unix.SCHED_NORMAL, nice: curNice, ioprio: ioprio, sched: sched, io: true}
	if batch {
		r.policy = unix.SCHED_BATCH
	}
	if from0 {
		r.nice, r.niced = 0, true
	}
	return r
}

// mark is the ioprio mark recording what was changed on a thread.
func (r threadSaved) mark() int {
	return bgMarkValue(r.sched, r.policy == unix.SCHED_BATCH, r.niced && r.nice == 0)
}

func isOurMark(io int, ioErr error) bool {
	if ioErr != nil {
		return false
	}
	_, _, _, ours := parseBgMark(io)
	return ours
}

type linuxBackend struct {
	mu   sync.Mutex
	opts LinuxOptions
	scan *procScanner
	buf  []byte
	// ssBuf holds /proc/schedstat while /proc/stat is read into buf.
	ssBuf []byte
	// cgRoot is the cgroup2 mount point (the cgroup-namespace root), "" if
	// cgroup2 is not mounted.
	cgRoot string
	// capSysNice: CAP_SYS_NICE effective against the initial user namespace
	// (see newLinux); cleared if a revert from SCHED_IDLE is ever refused.
	capSysNice bool
	bg         map[int]bgSaved
	load       loadEstimator
	// schedstat is /proc/schedstat when the kernel has it (CONFIG_SCHED_INFO).
	schedstat bool
	// quota is the cgroup-namespace root's cpu.max against the CPUs of our
	// affinity mask, read once at start (see cpuQuota.binds).
	quota cpuQuota

	contOnce   sync.Once
	cont       *cgroupContainer
	contReason string

	ioOnce sync.Once
	ioWeak bool // no disk honours the idle I/O class (no BFQ)
}

func newLinux(o LinuxOptions) (*linuxBackend, error) {
	if o.Footprint == "" {
		o.Footprint = FootprintAnon
	}
	if o.Load == "" {
		o.Load = LoadAuto
	}
	b := &linuxBackend{
		opts: o,
		scan: newProcScanner(procRoot, o.Footprint),
		bg:   map[int]bgSaved{},
	}
	b.scan.btimeFile = o.BtimeFile
	if _, err := b.scan.bootMS(); err != nil {
		return nil, err
	}
	if raw, err := readPath(procRoot+"/self/mountinfo", &b.buf, maxFileBuf); err == nil {
		if mp, _, ok := parseMountinfoCgroup2(raw); ok {
			b.cgRoot = mp
		}
	}
	b.capSysNice = selfHasCap(&b.buf, 23) && b.initialUserNS() && probeIdleRevert()
	b.load.method = o.Load
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err == nil && set.Count() > 0 {
		b.load.inSet = set.IsSet
	}
	b.quota = readCPUQuota(b.cgRoot, &b.buf, set.Count())
	_, err := readPath(procRoot+"/schedstat", &b.buf, maxFileBuf)
	b.schedstat = err == nil
	return b, nil
}

// readCPUQuota is the cpu.max of the cgroup2 mount root (in a container,
// the cgroup-namespace root, where docker --cpus writes its limit) against
// ncpu CPUs; the zero value when there is none or it cannot be read.
func readCPUQuota(root string, buf *[]byte, ncpu int) cpuQuota {
	if root == "" {
		return cpuQuota{}
	}
	raw, err := readPath(root+"/cpu.max", buf, 256)
	if err != nil {
		return cpuQuota{}
	}
	q, p, ok := parseCPUMax(raw)
	if !ok {
		return cpuQuota{}
	}
	return cpuQuota{quota: q, period: p, ncpu: ncpu}
}

// initialUserNS reports whether we run in the initial user namespace, where
// CapEff means what the scheduler checks (capable() uses init_user_ns).
// Inside rootless podman/docker, LXD or `unshare -r`, CapEff is full but
// leaving SCHED_IDLE or lowering nice still fails with EPERM. No uid_map
// (CONFIG_USER_NS=n) means there is only the initial namespace;
// probeIdleRevert still checks that the revert is permitted.
func (b *linuxBackend) initialUserNS() bool {
	raw, err := readPath(procRoot+"/self/uid_map", &b.buf, 4096)
	return initialUserNSFrom(raw, err)
}

// probeIdleRevert moves one of our own threads to SCHED_IDLE and back, and
// reports whether the revert was permitted. The goroutine never unlocks its
// OS thread, so the Go runtime discards that thread, whatever the result.
func probeIdleRevert() bool {
	res := make(chan bool, 1)
	go func() {
		runtime.LockOSThread()
		tid := unix.Gettid()
		a, err := unix.SchedGetAttr(tid, 0)
		if err != nil || !fairPolicy(a.Policy) {
			res <- false
			return
		}
		orig := a.Policy
		a.Flags &= schedResetOnFork
		a.Priority = 0
		a.Policy = unix.SCHED_IDLE
		if unix.SchedSetAttr(tid, a, 0) != nil {
			res <- false
			return
		}
		a.Policy = orig
		res <- unix.SchedSetAttr(tid, a, 0) == nil
	}()
	return <-res
}

func (b *linuxBackend) Name() string { return "linux" }

// ---------------------------------------------------------------------------
// Sensing.

// memSample is the memory part of a machine sample.
type memSample struct {
	total, avail, swapTotal, swapUsed int64
	some10, full10                    int64
}

// sampleMem reads meminfo and PSI, capping by the container limit at cgRoot
// when useCg. Shared by SampleMachine and QuickPressure.
func sampleMem(buf *[]byte, root, cgRoot string, useCg bool) (memSample, error) {
	raw, err := readPath(root+"/meminfo", buf, maxFileBuf)
	if err != nil {
		return memSample{}, fmt.Errorf("platform: read meminfo: %w", err)
	}
	mi, err := parseMeminfo(raw)
	if err != nil {
		return memSample{}, err
	}
	s := memSample{total: mi.Total, avail: mi.Avail, swapTotal: mi.SwapTotal,
		swapUsed: mi.SwapTotal - mi.SwapFree, some10: -1, full10: -1}
	if raw, err := readPath(root+"/pressure/memory", buf, 4096); err == nil {
		s.some10, s.full10 = parsePSI(raw)
	}
	if useCg && cgRoot != "" {
		applyCgroupLimit(buf, cgRoot, &s)
	}
	return s, nil
}

// applyCgroupLimit caps total/avail by dir/memory.max when finite, and folds
// in dir/memory.pressure (the container's own stall). Avail inside the limit
// is limit - memory.current + inactive_file (the kubelet "working set" rule:
// inactive page cache is reclaimable).
func applyCgroupLimit(buf *[]byte, dir string, s *memSample) {
	raw, err := readPath(dir+"/memory.max", buf, 256)
	if err != nil {
		return // real root cgroup has no memory.max: not in a container
	}
	limit, ok := parseCgroupMax(raw)
	if !ok || limit <= 0 {
		return
	}
	if limit < s.total {
		s.total = limit
	}
	raw, err = readPath(dir+"/memory.current", buf, 256)
	if err != nil {
		return
	}
	cur, ok := parseCgroupMax(raw)
	if !ok || cur < 0 {
		return
	}
	var inactive int64
	if raw, err := readPath(dir+"/memory.stat", buf, 64<<10); err == nil {
		inactive, _ = parseCgroupStatKey(raw, "inactive_file")
	}
	av := limit - cur + inactive
	av = max(0, min(av, limit))
	if av < s.avail {
		s.avail = av
	}
	if s.avail > s.total {
		s.avail = s.total
	}
	if raw, err := readPath(dir+"/memory.pressure", buf, 4096); err == nil {
		so, fu := parsePSI(raw)
		s.some10 = max(s.some10, so)
		s.full10 = max(s.full10, fu)
	}
}

func (b *linuxBackend) SampleMachine(diskPaths []string) (Machine, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ms, err := sampleMem(&b.buf, b.scan.root, b.cgRoot, b.opts.CgroupMemLimit)
	if err != nil {
		return Machine{}, err
	}
	m := Machine{
		NowMS:         time.Now().UnixMilli(),
		TotalBytes:    ms.total,
		AvailBytes:    ms.avail,
		KernelLevel:   0,
		SwapTotal:     ms.swapTotal,
		SwapUsed:      ms.swapUsed,
		PSIMemSome10:  ms.some10,
		PSIMemFull10:  ms.full10,
		NCPU:          runtime.NumCPU(),
		DiskFreeBytes: -1,
	}
	m.Load1Milli = b.sampleLoad(m.NCPU)
	m.DiskFreeBytes = diskFree(diskPaths)
	return m, nil
}

// sampleLoad returns Load1Milli for ncpu CPUs: the estimator's load per CPU
// (runnable tasks only, over our CPUs or per online CPU, see LoadMethod)
// times ncpu, so the policy's Load1Milli/NCPU is that per-CPU load. Falls
// back to the raw /proc/loadavg when /proc/stat cannot be parsed.
func (b *linuxBackend) sampleLoad(ncpu int) int64 {
	load1 := int64(-1)
	if raw, err := readPath(b.scan.root+"/loadavg", &b.buf, 256); err == nil {
		if l, err := parseProcLoadavg(raw); err == nil {
			load1 = l
		}
	}
	var ss []byte
	if b.schedstat && b.opts.Load != LoadAvg {
		raw, err := readPath(b.scan.root+"/schedstat", &b.buf, maxFileBuf)
		if err == nil {
			// stat is read into the same buffer below: keep a copy.
			b.ssBuf = append(b.ssBuf[:0], raw...)
			ss = b.ssBuf
		}
	}
	stat, err := readPath(b.scan.root+"/stat", &b.buf, maxFileBuf)
	if err != nil {
		return max(load1, 0)
	}
	perCPU, ok := b.load.update(time.Now(), stat, ss, load1)
	if !ok {
		return max(load1, 0)
	}
	return perCPU * int64(ncpu)
}

// diskFree returns the minimum free bytes (available to unprivileged users)
// over paths; -1 if none could be measured.
func diskFree(paths []string) int64 {
	free := int64(-1)
	for _, p := range paths {
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			continue
		}
		f := int64(st.Bavail) * int64(st.Bsize)
		if free < 0 || f < free {
			free = f
		}
	}
	return free
}

// Default thresholds used by QuickPressure (the policy uses configured ones).
const (
	quickPSIWarn10   = 100 // some avg10 >= 10 %
	quickPSICrit10   = 50  // full avg10 >= 5 %
	quickWarnPermill = 150 // avail < 15 % of total
	quickCritPermill = 70  // avail < 7 %
)

// linuxQuickPressure: 4 when full avg10 >= 5 % or avail < 7 %, 2 when some
// avg10 >= 10 % or avail < 15 %, else 1; 0 when meminfo is unreadable.
func linuxQuickPressure() int {
	var buf []byte
	cgRoot := ""
	if raw, err := readPath(procRoot+"/self/mountinfo", &buf, maxFileBuf); err == nil {
		if mp, _, ok := parseMountinfoCgroup2(raw); ok {
			cgRoot = mp
		}
	}
	o, _ := LinuxOptionsFromEnv(envcompat.Getenv)
	ms, err := sampleMem(&buf, procRoot, cgRoot, o.CgroupMemLimit)
	if err != nil {
		return 0
	}
	return quickLevel(ms)
}

func quickLevel(ms memSample) int {
	lvl := 1
	if ms.total > 0 {
		perm := ms.avail * 1000 / ms.total
		switch {
		case perm < quickCritPermill:
			lvl = 4
		case perm < quickWarnPermill:
			lvl = 2
		}
	}
	if ms.full10 >= quickPSICrit10 {
		lvl = 4
	} else if ms.some10 >= quickPSIWarn10 && lvl < 2 {
		lvl = 2
	}
	return lvl
}

func (b *linuxBackend) ListProcs(uid int) ([]Proc, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ps, err := b.scan.list(uid)
	if err != nil {
		return nil, err
	}
	// Forget deprioritisation records of processes that are gone.
	for pid := range b.bg {
		i := sort.Search(len(ps), func(i int) bool { return ps[i].PID >= pid })
		if i == len(ps) || ps[i].PID != pid {
			delete(b.bg, pid)
		}
	}
	return ps, nil
}

// JobTag implements TagReader.
func (b *linuxBackend) JobTag(pid int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.scan.jobTag(pid)
}

// ---------------------------------------------------------------------------
// Signals.

func (b *linuxBackend) Signal(pid int, sig syscall.Signal) error {
	if pid <= 1 {
		return fmt.Errorf("platform: refusing to signal pid %d", pid)
	}
	if pid == os.Getpid() {
		return fmt.Errorf("platform: refusing to signal self (pid %d)", pid)
	}
	return ignoreESRCH(unix.Kill(pid, sig))
}

func (b *linuxBackend) SignalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 1 {
		return fmt.Errorf("platform: refusing to signal process group %d", pgid)
	}
	if pgid == unix.Getpgrp() {
		return fmt.Errorf("platform: refusing to signal own process group %d", pgid)
	}
	return ignoreESRCH(unix.Kill(-pgid, sig))
}

func ignoreESRCH(err error) error {
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// Deprioritisation.

// SetBackground gives every thread it changes our ioprio mark (class IDLE,
// level 1-5: what was changed, see bgMarkValue), so threads created
// afterwards (they inherit it) and restores after a daemon restart lost our
// records touch only our threads and know their original policy.
const (
	ioprioWhoProcess = 1
	schedResetOnFork = 0x01 // SCHED_FLAG_RESET_ON_FORK
)

func ioprioGet(tid int) (int, error) {
	r, _, e := unix.Syscall(unix.SYS_IOPRIO_GET, ioprioWhoProcess, uintptr(tid), 0)
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func ioprioSet(tid, v int) error {
	_, _, e := unix.Syscall(unix.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(tid), uintptr(v))
	if e != 0 {
		return e
	}
	return nil
}

func ioprioClass(v int) int { return v >> ioprioClassShift }

// threads returns the thread ids of pid.
func (b *linuxBackend) threads(pid int) ([]int, error) {
	d, err := os.Open(b.scan.root + "/" + strconv.Itoa(pid) + "/task")
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	tids := make([]int, 0, len(names))
	for _, n := range names {
		if v, err := strconv.Atoi(n); err == nil && v > 0 {
			tids = append(tids, v)
		}
	}
	sort.Ints(tids)
	return tids, nil
}

// canRevertIdle reports whether a thread of pid at nice could be moved back
// from SCHED_IDLE to SCHED_OTHER by us: CAP_SYS_NICE, or the target's
// RLIMIT_NICE admits its nice (rlimit >= 20 - nice; see sched(7)).
func (b *linuxBackend) canRevertIdle(pid int, nice int32) bool {
	if b.capSysNice {
		return true
	}
	var rl unix.Rlimit
	if err := unix.Prlimit(pid, unix.RLIMIT_NICE, nil, &rl); err != nil {
		return false
	}
	if rl.Cur == unix.RLIM_INFINITY || rl.Cur >= 40 {
		return true
	}
	return int64(rl.Cur) >= int64(20-nice)
}

func fairPolicy(p uint32) bool { return p == unix.SCHED_NORMAL || p == unix.SCHED_BATCH }

func (b *linuxBackend) SetBackground(pid int, on bool) error {
	if pid <= 1 {
		return fmt.Errorf("platform: refusing to deprioritise pid %d", pid)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	tids, err := b.threads(pid)
	if err != nil {
		if isVanished(err) {
			delete(b.bg, pid)
			return nil
		}
		return err
	}
	if on {
		return b.background(pid, tids)
	}
	return b.restore(pid, tids)
}

func (b *linuxBackend) background(pid int, tids []int) error {
	saved, have := b.bg[pid]
	if !have {
		main, err := unix.SchedGetAttr(pid, 0)
		if err != nil {
			return ignoreESRCH(err)
		}
		mainIO, err := ioprioGet(pid)
		if err != nil {
			return ignoreESRCH(err)
		}
		saved = bgSaved{cpuSet: unix.SCHED_BATCH, threads: map[int]threadSaved{}}
		if ioprioClass(mainIO) != ioprioClassIdle { // (our mark: inherited, original unknown)
			saved.newIoprio = mainIO
		}
		switch {
		case b.canRevertIdle(pid, main.Nice):
			saved.cpuSet = unix.SCHED_IDLE
		case b.opts.ThrottleNice > 0:
			saved.nice = int32(b.opts.ThrottleNice)
		}
	}
	applied := 0
	var firstErr error
	note := func(err error) bool {
		if err == nil {
			applied++
			return true
		}
		if firstErr == nil && !errors.Is(err, unix.ESRCH) {
			firstErr = err
		}
		return false
	}
	for _, tid := range tids {
		a, err := unix.SchedGetAttr(tid, 0)
		if err != nil {
			continue // thread exited
		}
		io, ioErr := ioprioGet(tid)
		marked := isOurMark(io, ioErr)
		rec, known := saved.threads[tid]
		if !known && marked {
			// Created after background (or forked by a thread we changed, or
			// marked by a previous daemon): it inherited our settings, the
			// mark tells its creator's originals.
			rec = markedThread(io, a.Nice, saved.newIoprio)
		}
		// Only fair-class threads are touched: RT/deadline/IDLE threads keep
		// their policy (an RT policy could not be restored unprivileged).
		if fairPolicy(a.Policy) || (rec.sched && a.Policy == saved.cpuSet) {
			if !rec.sched {
				rec.policy, rec.nice = a.Policy, a.Nice
			}
			want := *a
			want.Policy = saved.cpuSet
			want.Flags &= schedResetOnFork
			want.Priority = 0
			want.Nice = max(a.Nice, saved.nice)
			if want.Policy != a.Policy || want.Nice != a.Nice {
				if note(unix.SchedSetAttr(tid, &want, 0)) {
					rec.sched = true
					if want.Nice != a.Nice {
						rec.niced = true
					}
				}
			}
		}
		// A thread already in the idle I/O class (ionice -c3) whose policy we
		// left alone needs nothing: no mark, nothing to restore.
		if ioErr == nil && (rec.sched || marked || ioprioClass(io) != ioprioClassIdle) {
			if !rec.io && !marked {
				rec.ioprio = io
			}
			if m := rec.mark(); io == m || note(ioprioSet(tid, m)) {
				rec.io = true
			}
		}
		saved.threads[tid] = rec
	}
	for tid := range saved.threads { // forget exited threads
		if i := sort.SearchInts(tids, tid); i == len(tids) || tids[i] != tid {
			delete(saved.threads, tid)
		}
	}
	b.bg[pid] = saved
	if applied == 0 && firstErr != nil {
		return fmt.Errorf("platform: deprioritise %d: %w", pid, firstErr)
	}
	return nil
}

// restore puts every thread we changed back to its own saved policy, nice and
// ioprio. Without a record (a daemon restart, or a child that inherited our
// settings) only threads carrying our mark are touched, as far as the mark
// says: the policy (if we changed it) goes back to SCHED_NORMAL or
// SCHED_BATCH, nice to 0 if it was raised from 0 (else it is left alone),
// and the ioprio to none.
func (b *linuxBackend) restore(pid int, tids []int) error {
	saved, have := b.bg[pid]
	if !have {
		saved = bgSaved{}
	}
	var firstErr error
	fail := func(err error) {
		if err != nil && !errors.Is(err, unix.ESRCH) && firstErr == nil {
			firstErr = err
		}
	}
	for _, tid := range tids {
		a, err := unix.SchedGetAttr(tid, 0)
		if err != nil {
			continue // thread exited
		}
		io, ioErr := ioprioGet(tid)
		marked := isOurMark(io, ioErr)
		rec, known := saved.threads[tid]
		if !known {
			if !marked {
				continue // not ours
			}
			rec = markedThread(io, a.Nice, saved.newIoprio)
		}
		ours := a.Policy == saved.cpuSet
		if !have {
			ours = a.Policy == unix.SCHED_IDLE || a.Policy == unix.SCHED_BATCH
		}
		if rec.sched && ours {
			want := *a
			want.Policy = rec.policy
			want.Flags &= schedResetOnFork
			want.Priority = 0
			if rec.niced {
				want.Nice = rec.nice
			}
			if want.Policy != a.Policy || want.Nice != a.Nice {
				err := unix.SchedSetAttr(tid, &want, 0)
				if err != nil && want.Nice < a.Nice && (errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES)) {
					// Lowering nice needs CAP_SYS_NICE or RLIMIT_NICE: the
					// nice part of the throttle is one-way (best effort).
					want.Nice = a.Nice
					err = nil
					if want.Policy != a.Policy {
						err = unix.SchedSetAttr(tid, &want, 0)
					}
				}
				if err != nil && a.Policy == unix.SCHED_IDLE && errors.Is(err, unix.EPERM) {
					b.capSysNice = false // CapEff lied (user namespace): use BATCH from now on
				}
				fail(err)
			}
		}
		if rec.io && marked {
			fail(ioprioSet(tid, rec.ioprio))
		}
	}
	if firstErr != nil {
		return fmt.Errorf("platform: restore priority of %d: %w", pid, firstErr)
	}
	delete(b.bg, pid)
	return nil
}

// ThrottleNote implements ThrottleNoter. The BATCH + nice throttle is used
// exactly when the job's RLIMIT_NICE does not admit leaving SCHED_IDLE at its
// nice, and that is the same check as lowering nice back to it, so there the
// raised nice is one-way.
//
// It also reports a weak throttle: SCHED_BATCH alone (ThrottleNice 0,
// unprivileged) keeps the job's full CPU share, and the idle I/O class does
// nothing unless a disk uses the BFQ scheduler.
func (b *linuxBackend) ThrottleNote(cpuMax bool) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var parts []string
	switch n := b.opts.ThrottleNice; {
	case b.capSysNice:
	case n > 0:
		parts = append(parts, fmt.Sprintf("linux throttle without CAP_SYS_NICE: SCHED_BATCH + nice %d + idle I/O; "+
			"UNTHROTTLED restores the policy and I/O class but the job, and every child it started while throttled, "+
			"keeps nice %d (PACE_LINUX_THROTTLE_NICE=0: fully reversible, much weaker)", n, n))
	case cpuMax:
		parts = append(parts, "linux throttle without CAP_SYS_NICE and with PACE_LINUX_THROTTLE_NICE=0: SCHED_BATCH + idle I/O only; "+
			"SCHED_BATCH keeps the full CPU share, so THROTTLED barely slows a job without a job container or an unmanaged process group "+
			"(measured: 50% of a contended CPU before and after); a job in a container is held by its cpu.max quota")
	default:
		parts = append(parts, "linux throttle without CAP_SYS_NICE and with PACE_LINUX_THROTTLE_NICE=0: SCHED_BATCH + idle I/O only; "+
			"SCHED_BATCH keeps the job's full CPU share, so THROTTLED barely slows it (measured: 50% of a contended CPU before and after)")
	}
	if n := quotaThrottleNote(b.quota, cpuMax); n != "" {
		parts = append(parts, n)
	}
	if b.ioWeakLocked() {
		parts = append(parts, "the idle I/O class of the throttle has no effect here: no disk uses the BFQ I/O scheduler, the only one that honours it")
	}
	return strings.Join(parts, "; ")
}

// ThrottleCaveat implements ThrottleCaveater: the short form of
// ThrottleNote, for THROTTLED lines.
func (b *linuxBackend) ThrottleCaveat(cpuMax bool) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var parts []string
	switch n := b.opts.ThrottleNice; {
	case b.capSysNice:
	case n > 0:
		parts = append(parts, fmt.Sprintf("leaves nice %d after UNTHROTTLED", n))
	case cpuMax: // the container's cpu.max quota holds
	default:
		parts = append(parts, "is weak (SCHED_BATCH keeps the CPU share)")
	}
	if c := quotaThrottleCaveat(b.quota, cpuMax); c != "" {
		parts = append(parts, c)
	}
	if b.ioWeakLocked() {
		parts = append(parts, "does not slow I/O (no BFQ disk)")
	}
	if len(parts) == 0 {
		return ""
	}
	return "throttle on this machine " + strings.Join(parts, " and ") + "; see pace status notes"
}

// ioWeakLocked: no disk honours the idle I/O class (checked once).
func (b *linuxBackend) ioWeakLocked() bool {
	b.ioOnce.Do(func() {
		honoured, known := ioIdleHonoured(sysBlockRoot)
		b.ioWeak = known && !honoured
	})
	return b.ioWeak
}

// sysBlockRoot is where block devices are listed (tests use a fixture).
var sysBlockRoot = "/sys/block"

// selfHasCap reports whether capability bit is in our effective set.
func selfHasCap(buf *[]byte, bit uint) bool {
	raw, err := readPath(procRoot+"/self/status", buf, 64<<10)
	if err != nil {
		return false
	}
	for i := 0; i < len(raw); {
		var line []byte
		line, i = nextLine(raw, i)
		if v, ok := bytes.CutPrefix(line, []byte("CapEff:")); ok {
			n, err := strconv.ParseUint(string(bytes.TrimSpace(v)), 16, 64)
			return err == nil && n&(1<<bit) != 0
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Containers.

// Containers implements ContainerProvider. Detection runs once (with the
// first scope asked for); when cgroup v2 delegation is not usable it returns
// ok=false without logging.
func (b *linuxBackend) Containers(scope string) (JobContainer, bool) {
	b.contOnce.Do(func() {
		if b.opts.NoContainers {
			b.contReason = "disabled by PACE_LINUX_CGROUP=0"
			return
		}
		c, err := detectCgroupContainer(b.opts, true, scope)
		if err != nil {
			b.contReason = err.Error()
			return
		}
		b.cont = c
	})
	if b.cont == nil {
		return nil, false
	}
	return b.cont, true
}

// ContainersReason explains why Containers() returned ok=false ("" if ok or
// not yet probed). For `pace status`/diagnostics; not part of Backend.
func (b *linuxBackend) ContainersReason() string { return b.contReason }
