package platform

// macOS backend: pure Go over golang.org/x/sys/unix, CGO_ENABLED=0.
//
// Sources (measured on an M3 Pro / macOS 14.8):
//   - machine: sysctl hw.memsize, hw.ncpu, kern.memorystatus_level (percent
//     available), kern.memorystatus_vm_pressure_level (1/2/4), vm.swapusage,
//     vm.loadavg; statfs for disk space.
//   - processes: sysctl kern.proc.uid read raw into a reused buffer;
//     per process proc_info(PIDRUSAGE, RUSAGE_INFO_V4) for ri_phys_footprint
//     and CPU (mach ticks converted with hw.tbfrequency).
//   - classification (Exe, System, JobTag) is done once per process identity
//     (pid, start time, p_comm) and cached; exec changes p_comm and so
//     invalidates the entry.
//
// The private calls (proc_info 336, csops 169) are made with raw syscalls,
// exactly what libproc's proc_pid_rusage / proc_pidpath wrap.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	newNative = func() (Backend, error) { return newDarwin() }
	quickPressure = darwinQuickPressure
}

const (
	ctlKern       = 1  // CTL_KERN
	kernProc      = 14 // KERN_PROC
	kernProcUID   = 5  // KERN_PROC_UID (effective uid)
	kernProcargs2 = 49 // KERN_PROCARGS2

	procInfoCallPidinfo = 2    // PROC_INFO_CALL_PIDINFO
	procInfoCallRusage  = 9    // PROC_INFO_CALL_PIDRUSAGE
	procPidTaskInfo     = 4    // PROC_PIDTASKINFO (struct proc_taskinfo, 96 bytes)
	eprocSLeader        = 0x02 // EPROC_SLEADER in kinfo_proc.kp_eproc.e_flag (sys/sysctl.h)
	noDev               = -1   // NODEV: kp_eproc.e_tdev of a process without a controlling terminal
	procPidPathInfo     = 11   // PROC_PIDPATHINFO
	procPidPathMax      = 4096

	rusageInfoV4    = 4
	rusageV4Size    = 296 // sizeof(struct rusage_info_v4)
	riUserTime      = 16
	riSystemTime    = 24
	riResidentSize  = 64
	riPhysFootprint = 72

	csOpsStatus      = 0
	csPlatformBinary = 0x04000000

	// p_stat values (sys/proc.h).
	pStatIdl   = 1
	pStatRun   = 2
	pStatSleep = 3
	pStatStop  = 4
	pStatZomb  = 5

	prioDarwinProcess = 4      // PRIO_DARWIN_PROCESS
	prioDarwinBG      = 0x1000 // PRIO_DARWIN_BG

	// jobTagEnv is the environment variable the wrapper sets on every job.
	jobTagEnv = "PACE_JOB_ID"
	// maxTagLen bounds a tag read from a foreign environment.
	maxTagLen = 256
	// argBufMax caps the reused KERN_PROCARGS2 buffer (kern.argmax is 1 MiB).
	argBufMax = 4 << 20
)

// systemPrefixes are OS-owned locations (sealed system volume / Apple-only).
// /bin and /usr/bin are deliberately absent: sh, zsh, make, git and sleep are
// platform binaries, and they are exactly what agent jobs run.
var systemPrefixes = [...]string{"/System/", "/usr/libexec/", "/usr/sbin/", "/sbin/", "/Library/Apple/"}

// procMeta is the per-identity classification cache entry.
type procMeta struct {
	startUS int64
	comm    [17]byte
	commStr string
	exe     string
	system  bool
	tag     string
	gen     uint64
}

type darwinBackend struct {
	mu sync.Mutex

	totalBytes int64
	ncpu       int
	tbFreq     uint64
	selfPID    int
	selfPGRP   int

	// Resolved MIBs (resolved once; kept in the struct so syscalls do not
	// make them escape on every call).
	mibLevel    []int32
	mibPressure []int32
	mibSwap     []int32
	mibLoad     []int32
	mibProcUID  [4]int32
	mibArgs     [3]int32

	kbuf    []unix.KinfoProc // reused kern.proc.uid buffer
	ruBuf   [512]byte        // reused rusage buffer (v4 is 296 bytes)
	pathBuf [procPidPathMax]byte
	argBuf  []byte // reused KERN_PROCARGS2 buffer
	small   [64]byte

	cache map[int]*procMeta
	gen   uint64

	// tagsInList makes ListProcs fill Proc.JobTag on first sight of a
	// non-system process (cached per identity). Default true.
	tagsInList bool
}

var _ Backend = (*darwinBackend)(nil)
var _ TagReader = (*darwinBackend)(nil)

func newDarwin() (*darwinBackend, error) {
	if unsafe.Sizeof(unix.KinfoProc{}) != unix.SizeofKinfoProc {
		return nil, fmt.Errorf("platform/darwin: KinfoProc size %d != %d", unsafe.Sizeof(unix.KinfoProc{}), unix.SizeofKinfoProc)
	}
	b := &darwinBackend{
		selfPID:    unix.Getpid(),
		selfPGRP:   unix.Getpgrp(),
		cache:      make(map[int]*procMeta, 1024),
		tagsInList: true,
	}
	mem, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return nil, fmt.Errorf("platform/darwin: hw.memsize: %w", err)
	}
	if mem == 0 || mem > 1<<62 {
		return nil, fmt.Errorf("platform/darwin: implausible hw.memsize %d", mem)
	}
	b.totalBytes = int64(mem)
	ncpu, err := unix.SysctlUint32("hw.ncpu")
	if err != nil {
		return nil, fmt.Errorf("platform/darwin: hw.ncpu: %w", err)
	}
	b.ncpu = int(ncpu)
	tb, err := unix.SysctlUint64("hw.tbfrequency")
	if err != nil || tb == 0 {
		return nil, fmt.Errorf("platform/darwin: hw.tbfrequency=%d: %v", tb, err)
	}
	b.tbFreq = tb
	for _, r := range []struct {
		name string
		dst  *[]int32
	}{
		{"kern.memorystatus_level", &b.mibLevel},
		{"kern.memorystatus_vm_pressure_level", &b.mibPressure},
		{"vm.swapusage", &b.mibSwap},
		{"vm.loadavg", &b.mibLoad},
	} {
		mib, err := nameToMIB(r.name)
		if err != nil {
			return nil, fmt.Errorf("platform/darwin: resolve %s: %w", r.name, err)
		}
		*r.dst = mib
	}
	b.mibProcUID = [4]int32{ctlKern, kernProc, kernProcUID, 0}
	b.mibArgs = [3]int32{ctlKern, kernProcargs2, 0}
	return b, nil
}

func (b *darwinBackend) Name() string { return "darwin" }

// ---------------------------------------------------------------------------
// raw syscalls

// sysctlRaw is __sysctl(2) without allocation. mib must be non-empty.
func sysctlRaw(mib []int32, old unsafe.Pointer, oldlen *uintptr, newp unsafe.Pointer, newlen uintptr) error {
	_, _, e := unix.Syscall6(unix.SYS_SYSCTL,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(old), uintptr(unsafe.Pointer(oldlen)),
		uintptr(newp), newlen)
	if e != 0 {
		return e
	}
	return nil
}

// nameToMIB resolves a sysctl name with the {0,3} name2oid interface.
func nameToMIB(name string) ([]int32, error) {
	if name == "" {
		return nil, unix.EINVAL
	}
	var buf [unix.CTL_MAXNAME + 2]int32
	n := uintptr(len(buf)) * 4
	nb := []byte(name)
	q := []int32{0, 3}
	if err := sysctlRaw(q, unsafe.Pointer(&buf[0]), &n, unsafe.Pointer(&nb[0]), uintptr(len(nb))); err != nil {
		return nil, err
	}
	cnt := int(n / 4)
	if cnt <= 0 || cnt > len(buf) {
		return nil, unix.EINVAL
	}
	return append([]int32(nil), buf[:cnt]...), nil
}

func procInfo(call, pid, flavor int, arg uint64, buf unsafe.Pointer, size int) (int, error) {
	r, _, e := unix.Syscall6(unix.SYS_PROC_INFO, uintptr(call), uintptr(pid), uintptr(flavor),
		uintptr(arg), uintptr(buf), uintptr(size))
	if e != 0 {
		return 0, e
	}
	return int(r), nil
}

func csFlags(pid int) (uint32, error) {
	var f uint32
	_, _, e := unix.Syscall6(unix.SYS_CSOPS, uintptr(pid), csOpsStatus, uintptr(unsafe.Pointer(&f)), 4, 0, 0)
	if e != 0 {
		return 0, e
	}
	return f, nil
}

// readSmall reads a fixed-size sysctl into b.small (caller holds mu).
func (b *darwinBackend) readSmall(mib []int32, want int) ([]byte, error) {
	n := uintptr(len(b.small))
	if err := sysctlRaw(mib, unsafe.Pointer(&b.small[0]), &n, nil, 0); err != nil {
		return nil, err
	}
	if int(n) < want {
		return nil, fmt.Errorf("short sysctl read: %d < %d", n, want)
	}
	return b.small[:n], nil
}

// ---------------------------------------------------------------------------
// pure helpers (unit tested)

// normalizeLevel maps kern.memorystatus_vm_pressure_level to 0/1/2/4.
func normalizeLevel(v uint32) int {
	switch v {
	case 1, 2, 4:
		return int(v)
	}
	return 0
}

// availFromLevel converts kern.memorystatus_level (percent) to bytes.
func availFromLevel(total int64, pct uint32) int64 {
	if pct > 100 {
		pct = 100
	}
	if total <= 0 {
		return 0
	}
	return total/100*int64(pct) + total%100*int64(pct)/100
}

// ticksToMS converts mach absolute-time ticks at freq Hz to milliseconds
// without overflow.
func ticksToMS(ticks, freq uint64) int64 {
	if freq == 0 {
		return 0
	}
	ms := ticks/freq*1000 + (ticks%freq)*1000/freq
	if ms > 1<<62 {
		return 1 << 62
	}
	return int64(ms)
}

// mapStat maps p_stat to the contract's state byte.
func mapStat(s int8) byte {
	switch s {
	case pStatRun:
		return 'R'
	case pStatStop:
		return 'T'
	case pStatZomb:
		return 'Z'
	case pStatIdl, pStatSleep:
		return 'S'
	}
	return 'S'
}

// parseLoadavg decodes struct loadavg {fixpt_t ldavg[3]; long fscale;}
// (24 bytes on 64-bit) into the 1-minute load x1000.
func parseLoadavg(buf []byte) (int64, error) {
	if len(buf) < 24 {
		return 0, fmt.Errorf("vm.loadavg: short read %d", len(buf))
	}
	ld := int64(binary.LittleEndian.Uint32(buf[0:]))
	fscale := int64(binary.LittleEndian.Uint64(buf[16:]))
	if fscale <= 0 {
		return 0, fmt.Errorf("vm.loadavg: bad fscale %d", fscale)
	}
	return ld * 1000 / fscale, nil
}

// parseSwap decodes struct xsw_usage {u64 total, avail, used; u32 pagesize; u32 encrypted}.
func parseSwap(buf []byte) (total, used int64, err error) {
	if len(buf) < 24 {
		return 0, 0, fmt.Errorf("vm.swapusage: short read %d", len(buf))
	}
	t := binary.LittleEndian.Uint64(buf[0:])
	u := binary.LittleEndian.Uint64(buf[16:])
	if t > 1<<62 || u > 1<<62 {
		return 0, 0, fmt.Errorf("vm.swapusage: implausible total=%d used=%d", t, u)
	}
	return int64(t), int64(u), nil
}

// hasSystemPrefix reports whether path lies in an OS-owned location.
func hasSystemPrefix(path string) bool {
	for _, p := range systemPrefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// classifySystem is the protected-process rule: a platform binary under an
// OS-owned prefix (both required: the flag alone would protect sh/make/git,
// a path alone can be spoofed), or a process we may not signal at all.
func classifySystem(csflags uint32, exe string, killErr error) bool {
	if errors.Is(killErr, unix.EPERM) {
		return true
	}
	return csflags&csPlatformBinary != 0 && hasSystemPrefix(exe)
}

// cstr returns the NUL-terminated prefix of b.
func cstr(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// procArgsEnv finds key in a KERN_PROCARGS2 buffer:
//
//	int argc | exec_path\0 | \0 padding | argv[0]\0 ... argv[argc-1]\0 | env\0 ... | \0 | apple strings
//
// It returns the value and true when found. Malformed buffers yield "", false.
func procArgsEnv(buf []byte, key string) (string, bool) {
	if len(buf) < 4 || key == "" {
		return "", false
	}
	argc := int(int32(binary.LittleEndian.Uint32(buf)))
	if argc < 0 || argc > len(buf) {
		return "", false
	}
	b := buf[4:]
	// exec path
	i := indexByte(b, 0)
	if i < 0 {
		return "", false
	}
	b = b[i:]
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	// argv (each may be empty)
	for n := 0; n < argc; n++ {
		i := indexByte(b, 0)
		if i < 0 {
			return "", false
		}
		b = b[i+1:]
	}
	// env until the first empty string
	for len(b) > 0 {
		i := indexByte(b, 0)
		if i < 0 {
			i = len(b) // unterminated last string: still a candidate
		}
		if i == 0 {
			break
		}
		s := b[:i]
		if len(s) > len(key) && s[len(key)] == '=' && string(s[:len(key)]) == key {
			v := s[len(key)+1:]
			if len(v) > maxTagLen {
				return "", false
			}
			return string(v), true
		}
		if i >= len(b) {
			break
		}
		b = b[i+1:]
	}
	return "", false
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// SampleMachine

func (b *darwinBackend) SampleMachine(diskPaths []string) (Machine, error) {
	m := Machine{
		NowMS:         time.Now().UnixMilli(),
		TotalBytes:    b.totalBytes,
		NCPU:          b.ncpu,
		PSIMemSome10:  -1,
		PSIMemFull10:  -1,
		DiskFreeBytes: -1,
	}
	b.mu.Lock()
	buf, err := b.readSmall(b.mibLevel, 4)
	if err != nil {
		b.mu.Unlock()
		return Machine{}, fmt.Errorf("platform/darwin: kern.memorystatus_level: %w", err)
	}
	m.AvailBytes = availFromLevel(b.totalBytes, binary.LittleEndian.Uint32(buf))

	buf, err = b.readSmall(b.mibPressure, 4)
	if err != nil {
		b.mu.Unlock()
		return Machine{}, fmt.Errorf("platform/darwin: kern.memorystatus_vm_pressure_level: %w", err)
	}
	m.KernelLevel = normalizeLevel(binary.LittleEndian.Uint32(buf))

	buf, err = b.readSmall(b.mibSwap, 24)
	if err == nil {
		m.SwapTotal, m.SwapUsed, err = parseSwap(buf)
	}
	if err != nil {
		b.mu.Unlock()
		return Machine{}, fmt.Errorf("platform/darwin: vm.swapusage: %w", err)
	}

	buf, err = b.readSmall(b.mibLoad, 24)
	if err == nil {
		m.Load1Milli, err = parseLoadavg(buf)
	}
	b.mu.Unlock()
	if err != nil {
		return Machine{}, fmt.Errorf("platform/darwin: vm.loadavg: %w", err)
	}

	// statfs on local volumes is served from the mount table; a path that
	// fails (missing, unmounted) is skipped. Paths on network mounts could
	// block and should not be configured.
	for _, p := range diskPaths {
		if p == "" {
			continue
		}
		var st unix.Statfs_t
		if err := unix.Statfs(p, &st); err != nil {
			continue
		}
		free := int64(st.Bavail) * int64(st.Bsize)
		if free < 0 {
			continue
		}
		if m.DiskFreeBytes < 0 || free < m.DiskFreeBytes {
			m.DiskFreeBytes = free
		}
	}
	return m, nil
}

func darwinQuickPressure() int {
	v, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return 0
	}
	return normalizeLevel(v)
}

// ---------------------------------------------------------------------------
// ListProcs

// kinfoByUID reads kern.proc.uid into the reused buffer (caller holds mu).
func (b *darwinBackend) kinfoByUID(uid int) ([]unix.KinfoProc, error) {
	const sz = unix.SizeofKinfoProc
	b.mibProcUID[3] = int32(uid)
	mib := b.mibProcUID[:]
	for attempt := 0; attempt < 8; attempt++ {
		if len(b.kbuf) > 0 {
			n := uintptr(len(b.kbuf)) * sz
			err := sysctlRaw(mib, unsafe.Pointer(&b.kbuf[0]), &n, nil, 0)
			if err == nil {
				if n%sz != 0 {
					return nil, fmt.Errorf("kern.proc.uid: size %d not a multiple of %d", n, sz)
				}
				return b.kbuf[:n/sz], nil
			}
			if !errors.Is(err, unix.ENOMEM) {
				return nil, fmt.Errorf("kern.proc.uid: %w", err)
			}
		}
		var need uintptr
		if err := sysctlRaw(mib, nil, &need, nil, 0); err != nil {
			return nil, fmt.Errorf("kern.proc.uid size: %w", err)
		}
		cnt := int(need / sz)
		newLen := cnt + cnt/4 + 32
		if newLen <= len(b.kbuf) {
			newLen = 2 * len(b.kbuf)
		}
		b.kbuf = make([]unix.KinfoProc, newLen)
	}
	return nil, errors.New("kern.proc.uid: process table kept growing")
}

// rusage reads RUSAGE_INFO_V4 for pid into b.ruBuf (caller holds mu).
func (b *darwinBackend) rusage(pid int) (footprint, cpuTicks, resident uint64, err error) {
	if _, err = procInfo(procInfoCallRusage, pid, rusageInfoV4, 0, unsafe.Pointer(&b.ruBuf[0]), len(b.ruBuf)); err != nil {
		return 0, 0, 0, err
	}
	le := binary.LittleEndian
	return le.Uint64(b.ruBuf[riPhysFootprint:]),
		le.Uint64(b.ruBuf[riUserTime:]) + le.Uint64(b.ruBuf[riSystemTime:]),
		le.Uint64(b.ruBuf[riResidentSize:]), nil
}

// exePath is proc_pidpath (caller holds mu).
func (b *darwinBackend) exePath(pid int) (string, error) {
	// The call returns 0, not the length, on success (measured): clear the
	// reused buffer so a short copy can never expose a previous process' path.
	clear(b.pathBuf[:])
	if _, err := procInfo(procInfoCallPidinfo, pid, procPidPathInfo, 0, unsafe.Pointer(&b.pathBuf[0]), len(b.pathBuf)); err != nil {
		return "", err
	}
	return string(cstr(b.pathBuf[:len(b.pathBuf)-1])), nil
}

// meta returns the cached classification for one kinfo entry, computing it on
// first sight of the identity. It returns nil if the process vanished.
func (b *darwinBackend) meta(pid int, startUS int64, comm *[17]byte, zombie bool) *procMeta {
	m := b.cache[pid]
	if m != nil && m.startUS == startUS && m.comm == *comm {
		m.gen = b.gen
		return m
	}
	if m == nil {
		m = &procMeta{}
		b.cache[pid] = m
	}
	*m = procMeta{startUS: startUS, comm: *comm, gen: b.gen}
	m.commStr = string(cstr(comm[:]))
	if zombie {
		return m
	}
	exe, err := b.exePath(pid)
	if errors.Is(err, unix.ESRCH) {
		delete(b.cache, pid)
		return nil
	}
	m.exe = exe
	cs, _ := csFlags(pid)
	m.system = classifySystem(cs, exe, unix.Kill(pid, 0))
	if b.tagsInList && !m.system {
		m.tag = b.jobTagLocked(pid)
	}
	return m
}

// termShell reports whether a process is a terminal's shell (Proc.TermLeader):
// it has a controlling terminal (e_tdev is NODEV without one) and either
// leads the session, or leads its own process group as the child of the
// session's leader when that leader is not the user's. The second is the
// login shell of Terminal.app and iTerm2: they start it through
// /usr/bin/login, which runs as root and leads the session, so the user's
// -zsh is only a group leader there. A job of a user's shell (its own group,
// the same terminal) is not one: its parent, the shell, is listed for the
// user.
func termShell(pid, ppid, pgid int, sessionLeader, hasTTY, parentListed bool, getsid func(int) (int, error)) bool {
	if !hasTTY {
		return false
	}
	if sessionLeader {
		return true
	}
	if pid != pgid || ppid <= 1 || parentListed {
		return false
	}
	sid, err := getsid(pid)
	return err == nil && sid == ppid
}

func (b *darwinBackend) ListProcs(uid int) ([]Proc, error) {
	if uid < 0 {
		return nil, fmt.Errorf("platform/darwin: invalid uid %d", uid)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	kps, err := b.kinfoByUID(uid)
	if err != nil {
		return nil, fmt.Errorf("platform/darwin: %w", err)
	}
	b.gen++
	out := make([]Proc, 0, len(kps))
	listed := make(map[int32]bool, len(kps))
	for i := range kps {
		listed[kps[i].Proc.P_pid] = true
	}
	for i := range kps {
		k := &kps[i]
		pid := int(k.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		startUS := k.Proc.P_starttime.Sec*1e6 + int64(k.Proc.P_starttime.Usec)
		p := Proc{
			PID:     pid,
			PPID:    int(k.Eproc.Ppid),
			PGID:    int(k.Eproc.Pgid),
			UID:     int(k.Eproc.Ucred.Uid),
			StartMS: startUS / 1000,
			State:   mapStat(k.Proc.P_stat),
			TermLeader: termShell(pid, int(k.Eproc.Ppid), int(k.Eproc.Pgid), k.Eproc.Flag&eprocSLeader != 0,
				k.Eproc.Tdev != noDev, listed[k.Eproc.Ppid], unix.Getsid),
		}
		zombie := p.State == 'Z'
		if !zombie {
			fp, ticks, _, err := b.rusage(pid)
			switch {
			case err == nil:
				if fp > 1<<62 {
					fp = 1 << 62
				}
				p.Footprint = int64(fp)
				p.CPUMS = ticksToMS(ticks, b.tbFreq)
			case errors.Is(err, unix.ESRCH):
				continue // vanished mid-scan
			default:
				// EPERM (e.g. setuid process of another ruid): listed with zero
				// usage; classification below marks it System.
			}
		}
		m := b.meta(pid, startUS, &k.Proc.P_comm, zombie)
		if m == nil {
			continue
		}
		p.Comm, p.Exe, p.System, p.JobTag = m.commStr, m.exe, m.system, m.tag
		out = append(out, p)
	}
	// Evict entries not seen this tick (O(1) check in steady state).
	if len(b.cache) > len(out) {
		for pid, m := range b.cache {
			if m.gen != b.gen {
				delete(b.cache, pid)
			}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// TagReader

// JobTag returns PACE_JOB_ID from pid's environment, "" when unreadable
// (platform binaries and argv-rewriting processes hide it).
func (b *darwinBackend) JobTag(pid int) string {
	if pid <= 0 {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.jobTagLocked(pid)
}

func (b *darwinBackend) jobTagLocked(pid int) string {
	b.mibArgs[2] = int32(pid)
	mib := b.mibArgs[:]
	var need uintptr
	if err := sysctlRaw(mib, nil, &need, nil, 0); err != nil || need == 0 {
		return ""
	}
	if need > argBufMax {
		need = argBufMax
	}
	if uintptr(len(b.argBuf)) < need {
		b.argBuf = make([]byte, need)
	}
	n := uintptr(len(b.argBuf))
	if err := sysctlRaw(mib, unsafe.Pointer(&b.argBuf[0]), &n, nil, 0); err != nil {
		return ""
	}
	if n > uintptr(len(b.argBuf)) {
		n = uintptr(len(b.argBuf))
	}
	v, _ := procArgsEnv(b.argBuf[:n], jobTagEnv)
	return v
}

// ---------------------------------------------------------------------------
// enforcement

func (b *darwinBackend) Signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("platform/darwin: refusing to signal pid %d", pid)
	}
	if pid == b.selfPID {
		return fmt.Errorf("platform/darwin: refusing to signal the governor itself (pid %d)", pid)
	}
	if err := unix.Kill(pid, sig); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("platform/darwin: kill(%d, %v): %w", pid, sig, err)
	}
	return nil
}

func (b *darwinBackend) SignalGroup(pgid int, sig syscall.Signal) error {
	// pgid 0 is our own group and -1 is broadcast; pgid 1 is launchd's.
	if pgid <= 1 {
		return fmt.Errorf("platform/darwin: refusing to signal process group %d", pgid)
	}
	if pgid == b.selfPGRP {
		return fmt.Errorf("platform/darwin: refusing to signal the governor's own process group %d", pgid)
	}
	err := unix.Kill(-pgid, sig)
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	// xnu answers EPERM for a group whose remaining members are all zombies
	// (measured): that is a dead group, not a permission problem.
	if errors.Is(err, unix.EPERM) && !groupHasLive(pgid) {
		return nil
	}
	return fmt.Errorf("platform/darwin: killpg(%d, %v): %w", pgid, sig, err)
}

// groupHasLive reports whether process group pgid has a non-zombie member.
// It answers true when the group cannot be read, so errors are not hidden.
func groupHasLive(pgid int) bool {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return true
	}
	for i := range kps {
		if kps[i].Proc.P_stat != pStatZomb && int(kps[i].Eproc.Pgid) == pgid {
			return true
		}
	}
	return false
}

// SetBackground uses PRIO_DARWIN_BG, which (unlike nice) is reversible
// without privileges and also throttles I/O.
func (b *darwinBackend) SetBackground(pid int, on bool) error {
	if pid <= 0 {
		return fmt.Errorf("platform/darwin: refusing to set priority of pid %d", pid)
	}
	if pid == b.selfPID {
		return fmt.Errorf("platform/darwin: refusing to background the governor itself (pid %d)", pid)
	}
	prio := 0
	if on {
		prio = prioDarwinBG
	}
	if err := unix.Setpriority(prioDarwinProcess, pid, prio); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("platform/darwin: setpriority(PRIO_DARWIN_PROCESS, %d, %#x): %w", pid, prio, err)
	}
	return nil
}

// taskPriority returns proc_taskinfo.pti_priority (test/diagnostic helper).
func taskPriority(pid int) (int32, error) {
	var buf [96]byte
	n, err := procInfo(procInfoCallPidinfo, pid, procPidTaskInfo, 0, unsafe.Pointer(&buf[0]), len(buf))
	if err != nil {
		return 0, err
	}
	if n < len(buf) {
		return 0, fmt.Errorf("proc_taskinfo: short read %d", n)
	}
	return int32(binary.LittleEndian.Uint32(buf[92:])), nil
}
