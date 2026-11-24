//go:build linux

package platform

// cgroup v2 JobContainer.
//
// Layout: <root>/<scope>/job-<id>/ with root = <delegation root>/pace unless
// PACE_CGROUP_BASE pins it, and scope the daemon's own subtree (the daemon
// derives it from its socket path), so that two daemons of one user (an
// installed service and a test daemon with its own PACE_HOME/PACE_SOCK) never
// list, release or remove each other's job containers. The delegation root
// is the topmost ancestor of our own cgroup that we may write (mkdir +
// cgroup.procs), stopping at the cgroup2 mount point:
//
//	root on a host / privileged container with --cgroupns=private -> mount root
//	systemd user manager (user@UID.service, Delegate=yes)          -> user@UID.service
//	an ssh/login session scope (root-owned)                         -> none: ok=false
//
// Only the daemon detects the base. It gives every wrapper the job's
// container path on ADMITTED, and the wrapper opens that base
// (ContainersAt) without detecting anything, so the two always agree. Moving
// a process into the job cgroup also requires write access to the common
// ancestor's cgroup.procs (cgroup v2 delegation containment), so a wrapper
// started from a root-owned session scope cannot place its child there: the
// spawn into the cgroup and Attach then fail and the wrapper says so with an
// INFO line and carries on without a container.
//
// The memory controller must be enabled in the delegation root's
// subtree_control. A non-root cgroup that contains processes cannot enable
// it (no-internal-processes rule): when we are the ONLY process there (a
// systemd Delegate=yes unit), we move ourselves into "<deleg>/pace-self" and
// retry; otherwise detection reports ok=false.

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type cgroupContainer struct {
	mu      sync.Mutex
	base    string
	highPct int // memory.high as % of memory.max; 0 = "max" (see LinuxOptions)
	cpuPct  int // cpu.max of a throttled job, % of one CPU; 0 = none (LinuxOptions.ThrottleCPUPct)
	buf     []byte
}

func cgWritable(dir string) bool {
	return unix.Access(dir, unix.W_OK) == nil && unix.Access(dir+"/cgroup.procs", unix.W_OK) == nil
}

func cgWrite(path, val string) error {
	fd, err := openatRetry(unix.AT_FDCWD, path, unix.O_WRONLY|unix.O_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	for {
		_, err = unix.Write(fd, []byte(val))
		if err != unix.EINTR {
			return err
		}
	}
}

func hasWord(b []byte, w string) bool {
	for _, f := range bytes.Fields(b) {
		if string(f) == w {
			return true
		}
	}
	return false
}

// cgPids reads a cgroup.procs file.
func cgPids(path string, buf *[]byte) ([]int, error) {
	raw, err := readPath(path, buf, maxFileBuf)
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range bytes.Fields(raw) {
		if v, ok := parseUint(f); ok && v > 0 && v < 1<<31 {
			pids = append(pids, int(v))
		}
	}
	return pids, nil
}

// cgTreePids lists the processes of the cgroup at dir and of every cgroup
// below it: a job may create sub-cgroups of its own container (it owns
// it) and move processes into them; they are still the job's.
func cgTreePids(dir string, buf *[]byte) ([]int, error) {
	pids, err := cgPids(dir+"/cgroup.procs", buf)
	if err != nil {
		return nil, err
	}
	return cgSubPids(dir, buf, pids, 0), nil
}

// cgMaxDepth bounds the walk below a job container.
const cgMaxDepth = 16

func cgSubPids(dir string, buf *[]byte, pids []int, depth int) []int {
	if depth >= cgMaxDepth {
		return pids
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return pids
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		sub := dir + "/" + e.Name()
		if p, err := cgPids(sub+"/cgroup.procs", buf); err == nil {
			pids = append(pids, p...)
		}
		pids = cgSubPids(sub, buf, pids, depth+1)
	}
	return pids
}

// rmdirTree removes the empty cgroups below dir, deepest first (a cgroup
// with children cannot be removed). One still holding processes stays
// (EBUSY), and so do its ancestors.
func rmdirTree(dir string, depth int) {
	if depth >= cgMaxDepth {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range ents {
		if e.IsDir() {
			sub := dir + "/" + e.Name()
			rmdirTree(sub, depth+1)
			_ = unix.Rmdir(sub)
		}
	}
}

// enableMemory makes sure dir/cgroup.subtree_control has "memory".
func enableMemory(dir string, buf *[]byte) error {
	raw, err := readPath(dir+"/cgroup.subtree_control", buf, 4096)
	if err != nil {
		return err
	}
	if hasWord(raw, "memory") {
		return nil
	}
	raw, err = readPath(dir+"/cgroup.controllers", buf, 4096)
	if err != nil {
		return err
	}
	if !hasWord(raw, "memory") {
		return fmt.Errorf("memory controller not available in %s", dir)
	}
	return cgWrite(dir+"/cgroup.subtree_control", "+memory")
}

// cgroup2Mount returns the cgroup2 mount point and its root.
func cgroup2Mount(buf *[]byte) (mp, mroot string, err error) {
	raw, err := readPath(procRoot+"/self/mountinfo", buf, maxFileBuf)
	if err != nil {
		return "", "", fmt.Errorf("cgroup: read mountinfo: %w", err)
	}
	mp, mroot, ok := parseMountinfoCgroup2(raw)
	if !ok {
		return "", "", errors.New("cgroup: no cgroup2 mount")
	}
	return mp, mroot, nil
}

// validScope: 1-32 of [a-z0-9-] (a directory name under the pace root that
// can never be a job-<id> container).
func validScope(s string) error {
	if s == "" || len(s) > 32 || strings.HasPrefix(s, "job-") || s == outsideLeaf {
		return fmt.Errorf("cgroup: bad scope %q", s)
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return fmt.Errorf("cgroup: bad scope %q", s)
		}
	}
	return nil
}

// openCgroupContainer opens an existing base (a daemon's, as ADMITTED gave
// it to the wrapper) without detecting or creating anything.
func openCgroupContainer(o LinuxOptions, base string) (*cgroupContainer, error) {
	if !filepath.IsAbs(base) {
		return nil, fmt.Errorf("cgroup: base %q is not absolute", base)
	}
	base = filepath.Clean(base)
	var buf []byte
	mp, _, err := cgroup2Mount(&buf)
	if err != nil {
		return nil, err
	}
	if mp = filepath.Clean(mp); !strings.HasPrefix(base, mp+"/") {
		return nil, fmt.Errorf("cgroup: base %s is not under the cgroup2 mount %s", base, mp)
	}
	if _, err := os.Stat(base + "/cgroup.procs"); err != nil {
		return nil, fmt.Errorf("cgroup: base %s: %w", base, err)
	}
	return &cgroupContainer{base: base, highPct: o.CgroupHighPct, cpuPct: o.ThrottleCPUPct}, nil
}

// detectCgroupContainer finds (and prepares) the daemon's base; scope ""
// puts job containers straight under the pace root (tests only).
func detectCgroupContainer(o LinuxOptions, selfMove bool, scope string) (*cgroupContainer, error) {
	if scope != "" {
		if err := validScope(scope); err != nil {
			return nil, err
		}
	}
	var buf []byte
	mp, mroot, err := cgroup2Mount(&buf)
	if err != nil {
		return nil, err
	}
	raw, err := readPath(procRoot+"/self/cgroup", &buf, 64<<10)
	if err != nil {
		return nil, fmt.Errorf("cgroup: read /proc/self/cgroup: %w", err)
	}
	cgPath, ok := parseSelfCgroup(raw)
	if !ok {
		return nil, errors.New("cgroup: not on the unified (v2) hierarchy")
	}
	own, ok := cgroupDir(mp, mroot, cgPath)
	if !ok {
		return nil, fmt.Errorf("cgroup: own cgroup %s not visible under %s", cgPath, mp)
	}
	mp = filepath.Clean(mp)

	var base, deleg string
	if o.CgroupBase != "" {
		base = filepath.Clean(o.CgroupBase)
		if !strings.HasPrefix(base, mp+"/") {
			return nil, fmt.Errorf("cgroup: PACE_CGROUP_BASE %s is not under %s", base, mp)
		}
		deleg = filepath.Dir(base)
		if !cgWritable(deleg) {
			return nil, fmt.Errorf("cgroup: %s not writable", deleg)
		}
	} else {
		if !cgWritable(own) {
			return nil, fmt.Errorf("cgroup: own cgroup %s is not delegated to us", own)
		}
		deleg = own
		for deleg != mp {
			parent := filepath.Dir(deleg)
			if !cgWritable(parent) {
				break
			}
			deleg = parent
		}
		base = deleg + "/pace"
	}

	if err := enableMemory(deleg, &buf); err != nil {
		if !errors.Is(err, unix.EBUSY) {
			return nil, fmt.Errorf("cgroup: enable memory in %s: %w", deleg, err)
		}
		// No-internal-processes: move ourselves out if we are alone there.
		pids, perr := cgPids(deleg+"/cgroup.procs", &buf)
		if !selfMove || perr != nil || len(pids) != 1 || pids[0] != os.Getpid() {
			return nil, fmt.Errorf("cgroup: %s has processes; cannot enable memory controller", deleg)
		}
		leaf := deleg + "/pace-self"
		if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("cgroup: mkdir %s: %w", leaf, err)
		}
		if err := cgWrite(leaf+"/cgroup.procs", "0"); err != nil {
			return nil, fmt.Errorf("cgroup: move self to %s: %w", leaf, err)
		}
		if err := enableMemory(deleg, &buf); err != nil {
			return nil, fmt.Errorf("cgroup: enable memory in %s: %w", deleg, err)
		}
	}
	if err := os.Mkdir(base, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("cgroup: mkdir %s: %w", base, err)
	}
	if err := enableMemory(base, &buf); err != nil {
		return nil, fmt.Errorf("cgroup: enable memory in %s: %w", base, err)
	}
	// Per-job cpu and io weights (the container throttle) only where the
	// delegation root already schedules base as a group of its own: enabling
	// them there would change how every other child of the root is
	// scheduled, so it is left to whoever owns it (systemd's Delegate=).
	enableIfDelegated(deleg, base, &buf, "cpu", "io")
	if scope != "" {
		root := base
		base = root + "/" + scope
		if err := os.Mkdir(base, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("cgroup: mkdir %s: %w", base, err)
		}
		if err := enableMemory(base, &buf); err != nil {
			return nil, fmt.Errorf("cgroup: enable memory in %s: %w", base, err)
		}
		enableIfDelegated(root, base, &buf, "cpu", "io")
	}
	return &cgroupContainer{base: base, highPct: o.CgroupHighPct, cpuPct: o.ThrottleCPUPct}, nil
}

// outsideLeaf is the cgroup under base that Evict moves pace's own processes
// into: never a job's container, so no job's freeze, kill or memory limit
// reaches them.
const outsideLeaf = "pace-outside"

// Evict moves pid (a pace wrapper or the daemon found inside a job's
// container) into <base>/pace-outside.
func (c *cgroupContainer) Evict(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("cgroup: bad pid %d", pid)
	}
	leaf := c.base + "/" + outsideLeaf
	if err := os.Mkdir(leaf, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("cgroup: mkdir %s: %w", leaf, err)
	}
	if err := cgWrite(leaf+"/cgroup.procs", strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("cgroup: move %d to %s: %w", pid, leaf, err)
	}
	return nil
}

// Close removes the outside leaf and the base itself when they are empty
// (a daemon shutting down; job containers still there keep the base).
func (c *cgroupContainer) Close() {
	_ = unix.Rmdir(c.base + "/" + outsideLeaf)
	_ = unix.Rmdir(c.base)
}

// validJobID: 1-64 of [A-Za-z0-9._-], not starting with '.'.
func validJobID(id string) error {
	if id == "" || len(id) > 64 || id[0] == '.' {
		return fmt.Errorf("cgroup: bad job id %q", id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return fmt.Errorf("cgroup: bad job id %q", id)
		}
	}
	return nil
}

func (c *cgroupContainer) dir(jobID string) (string, error) {
	if err := validJobID(jobID); err != nil {
		return "", err
	}
	return c.base + "/job-" + jobID, nil
}

// Base returns the directory job cgroups are created under.
func (c *cgroupContainer) Base() string { return c.base }

func (c *cgroupContainer) Create(jobID string, memMaxBytes int64) (string, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return "", err
	}
	if memMaxBytes < 0 {
		return "", fmt.Errorf("cgroup: negative memory limit %d", memMaxBytes)
	}
	if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("cgroup: create %s: %w", d, err)
	}
	// Whole-job OOM kill, matching "always the whole process group".
	_ = cgWrite(d+"/memory.oom.group", "1")
	if err := c.setLimits(d, memMaxBytes); err != nil {
		return "", err
	}
	return d, nil
}

// SetMemMax changes the hard memory limit of an existing job container
// (0 = none). Setting it below the current usage makes the kernel reclaim
// and, failing that, OOM-kill the job (memory.oom.group: all of it).
func (c *cgroupContainer) SetMemMax(jobID string, memMaxBytes int64) error {
	d, err := c.dir(jobID)
	if err != nil {
		return err
	}
	if memMaxBytes < 0 {
		return fmt.Errorf("cgroup: negative memory limit %d", memMaxBytes)
	}
	if _, err := os.Stat(d); err != nil {
		return fmt.Errorf("cgroup: %s: %w", jobID, err)
	}
	return c.setLimits(d, memMaxBytes)
}

func (c *cgroupContainer) setLimits(d string, memMaxBytes int64) error {
	high, maxv := "max", "max"
	if memMaxBytes > 0 {
		maxv = strconv.FormatInt(memMaxBytes, 10)
		if c.highPct > 0 && c.highPct < 100 {
			high = strconv.FormatInt(memMaxBytes/100*int64(c.highPct), 10)
		}
	}
	// Raise max before high, so lowering high below an old max never hits a
	// transient high > max.
	if err := cgWrite(d+"/memory.max", maxv); err != nil {
		return fmt.Errorf("cgroup: set memory.max on %s: %w", d, err)
	}
	if err := cgWrite(d+"/memory.high", high); err != nil {
		return fmt.Errorf("cgroup: set memory.high on %s: %w", d, err)
	}
	// A hard cap that can spill into unlimited swap is no cap: the job would
	// drive the machine into swap thrash. Footprints count swap anyway, so a
	// capped job gets no swap (best effort: absent without swap accounting).
	swap := "max"
	if memMaxBytes > 0 {
		swap = "0"
	}
	_ = cgWrite(d+"/memory.swap.max", swap)
	return nil
}

func (c *cgroupContainer) Attach(jobID string, pid int) error {
	d, err := c.dir(jobID)
	if err != nil {
		return err
	}
	if pid <= 0 {
		return fmt.Errorf("cgroup: bad pid %d", pid)
	}
	if err := cgWrite(d+"/cgroup.procs", strconv.Itoa(pid)); err != nil {
		return fmt.Errorf("cgroup: attach %d to %s: %w", pid, d, err)
	}
	return nil
}

// Usage is the job's own memory: memory.stat anon + shmem, plus
// memory.swap.current. The same semantics as the per-process "anon"
// footprint (RssAnon + RssShmem + VmSwap: what a kill frees, clean page
// cache excluded, as macOS's physical footprint) but exact: pages shared
// between the job's processes count once, and tmpfs/shm data counts even
// when no process maps it. Not memory.current - inactive_file (the kubelet
// working set): that also counts active page cache and kernel memory, which
// a kill does not (or not only) free. Measured in Docker for a shell loop
// beside a 285M allocation: working set 299-301M, anon 294M, so a 300M
// budget killed the job the sampled per-process footprint (~290M) left
// alone. File cache still counts against memory.max, which the kernel then
// reclaims first.
func (c *cgroupContainer) Usage(jobID string) (int64, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := readPath(d+"/memory.stat", &c.buf, 64<<10)
	if err != nil {
		return 0, fmt.Errorf("cgroup: usage of %s: %w", jobID, err)
	}
	cur, ok := cgroupOwnMemory(raw)
	if !ok {
		return 0, fmt.Errorf("cgroup: memory.stat of %s has no anon", jobID)
	}
	if raw, err := readPath(d+"/memory.swap.current", &c.buf, 256); err == nil {
		if sw, ok := parseCgroupMax(raw); ok && sw > 0 {
			cur += sw
		}
	}
	return max(cur, 0), nil
}

// OOMKills returns how many processes of the job the kernel OOM-killed for
// the container's own limit: memory.events oom_kill, but 0 unless "oom" (the
// container reached its memory.max and an allocation was about to fail) is
// non-zero too. oom_kill alone also counts kills by the machine-wide OOM
// killer or for an ancestor's limit (a container's --memory), which are not
// governor kills.
func (c *cgroupContainer) OOMKills(jobID string) (int64, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := readPath(d+"/memory.events", &c.buf, 4096)
	if err != nil {
		return 0, err
	}
	return ownOOMKills(raw), nil
}

func (c *cgroupContainer) signalAll(d string, sig syscall.Signal) error {
	c.mu.Lock()
	pids, err := cgTreePids(d, &c.buf)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	for _, p := range pids {
		if p > 1 && p != os.Getpid() {
			_ = ignoreESRCH(unix.Kill(p, sig))
		}
	}
	return nil
}

func (c *cgroupContainer) Freeze(jobID string, on bool) error {
	d, err := c.dir(jobID)
	if err != nil {
		return err
	}
	v := "0"
	if on {
		v = "1"
	}
	err = cgWrite(d+"/cgroup.freeze", v)
	if errors.Is(err, unix.ENOENT) {
		if _, serr := os.Stat(d); serr != nil {
			return fmt.Errorf("cgroup: freeze %s: %w", jobID, serr)
		}
		// Kernel < 5.2: no cgroup freezer; stop/continue every member.
		sig := syscall.SIGCONT
		if on {
			sig = syscall.SIGSTOP
		}
		return c.signalAll(d, sig)
	}
	if err != nil {
		return fmt.Errorf("cgroup: freeze %s: %w", jobID, err)
	}
	return nil
}

// Frozen reads cgroup.freeze back: what was last asked of the kernel,
// whoever wrote it (cgroup.events "frozen" lags behind it while the
// kernel is still stopping the tasks).
func (c *cgroupContainer) Frozen(jobID string) (bool, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := readPath(d+"/cgroup.freeze", &c.buf, 64)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(raw)) == "1", nil
}

// Kill SIGKILLs every process in the job: cgroup.kill (kernel >= 5.14), or
// freeze + kill each member until the cgroup is empty. A missing job is not
// an error (nothing left to kill).
func (c *cgroupContainer) Kill(jobID string) error {
	d, err := c.dir(jobID)
	if err != nil {
		return err
	}
	err = cgWrite(d+"/cgroup.kill", "1")
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("cgroup: kill %s: %w", jobID, err)
	}
	if _, serr := os.Stat(d); serr != nil {
		return nil
	}
	_ = cgWrite(d+"/cgroup.freeze", "1") // stop forks while we sweep
	for round := 0; round < 50; round++ {
		c.mu.Lock()
		pids, err := cgTreePids(d, &c.buf)
		c.mu.Unlock()
		if err != nil || len(pids) == 0 {
			break
		}
		for _, p := range pids {
			if p > 1 && p != os.Getpid() {
				_ = ignoreESRCH(unix.Kill(p, syscall.SIGKILL))
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	_ = cgWrite(d+"/cgroup.freeze", "0")
	return nil
}

// Remove deletes the job cgroup, with the empty cgroups the job created
// below it. Processes that were just killed may take a moment to leave, so
// Remove retries EBUSY for up to 1 s (pace run, at the job's end); it never
// kills.
func (c *cgroupContainer) Remove(jobID string) error { return c.remove(jobID, time.Second) }

// RemoveNow is Remove with a single attempt: the daemon's loop never
// waits on it (an ended job's container still busy is tried again later).
func (c *cgroupContainer) RemoveNow(jobID string) error { return c.remove(jobID, 0) }

func (c *cgroupContainer) remove(jobID string, wait time.Duration) error {
	d, err := c.dir(jobID)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		rmdirTree(d, 0)
		err := unix.Rmdir(d)
		if err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			return fmt.Errorf("cgroup: remove %s: %w", d, err)
		}
		if !time.Now().Before(deadline) {
			c.mu.Lock()
			pids, _ := cgTreePids(d, &c.buf)
			c.mu.Unlock()
			return fmt.Errorf("cgroup: remove %s (%d processes inside): %w", d, len(pids), err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// enableIfDelegated enables each controller in base's subtree_control when
// deleg's subtree_control already has it (best effort).
func enableIfDelegated(deleg, base string, buf *[]byte, ctrls ...string) {
	raw, err := readPath(deleg+"/cgroup.subtree_control", buf, 4096)
	if err != nil {
		return
	}
	var want []string
	for _, c := range ctrls {
		if hasWord(raw, c) {
			want = append(want, c)
		}
	}
	if len(want) == 0 {
		return
	}
	raw, err = readPath(base+"/cgroup.subtree_control", buf, 4096)
	if err != nil {
		return
	}
	for _, c := range want {
		if !hasWord(raw, c) {
			_ = cgWrite(base+"/cgroup.subtree_control", "+"+c)
		}
	}
}

// Path returns the job's container directory (no I/O).
func (c *cgroupContainer) Path(jobID string) (string, error) { return c.dir(jobID) }

// PIDs lists the processes in the job's container (cgroup.procs), and in
// the cgroups the job created below it.
func (c *cgroupContainer) PIDs(jobID string) ([]int, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cgTreePids(d, &c.buf)
}

// Peak returns memory.peak, the job's highest memory.current (kernel >=
// 5.19; an error before).
func (c *cgroupContainer) Peak(jobID string) (int64, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := readPath(d+"/memory.peak", &c.buf, 256)
	if err != nil {
		return 0, err
	}
	v, ok := parseCgroupMax(raw)
	if !ok || v < 0 {
		return 0, fmt.Errorf("cgroup: bad memory.peak %q", raw)
	}
	return v, nil
}

// Throttle weights: cpu.weight and io.weight range 1..10000, default 100.
// A throttled job gets the minimum, relative to its siblings (the other
// jobs under base); a restore writes the default back (pace creates every
// job container at the default, so there is nothing else to remember).
const (
	cgWeightLow     = "1"
	cgWeightDefault = "100"
)

// SetThrottle lowers (on) or restores the job's cpu.weight and io.weight,
// whichever exist (the cpu/io controllers are enabled for base). It reports
// the knobs it set, "" when neither exists. A missing job is an error.
func (c *cgroupContainer) SetThrottle(jobID string, on bool) (string, error) {
	d, err := c.dir(jobID)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(d); err != nil {
		return "", fmt.Errorf("cgroup: %s: %w", jobID, err)
	}
	v := cgWeightDefault
	if on {
		v = cgWeightLow
	}
	var set []string
	var firstErr error
	if _, err := os.Stat(d + "/cpu.weight"); err == nil {
		if err := cgWrite(d+"/cpu.weight", v); err == nil {
			set = append(set, "cpu.weight="+v)
		} else {
			firstErr = err
		}
		// The weight only ranks the job among the other pace jobs; the
		// quota holds against everything (see LinuxOptions.ThrottleCPUPct).
		if c.cpuPct > 0 {
			q := cpuMaxQuota(0)
			if on {
				q = cpuMaxQuota(c.cpuPct)
			}
			if err := cgWrite(d+"/cpu.max", q); err == nil {
				if on {
					set = append(set, "cpu.max="+strconv.Itoa(c.cpuPct)+"%")
				}
			} else if firstErr == nil {
				firstErr = err
			}
		}
	}
	if _, err := os.Stat(d + "/io.weight"); err == nil {
		if err := cgWrite(d+"/io.weight", "default "+v); err == nil {
			set = append(set, "io.weight="+v)
		} else if firstErr == nil {
			firstErr = err
		}
	}
	if len(set) == 0 && firstErr != nil {
		return "", fmt.Errorf("cgroup: weights of %s: %w", jobID, firstErr)
	}
	return strings.Join(set, " "), nil
}

// Jobs lists the ids of the job containers under base.
func (c *cgroupContainer) Jobs() ([]string, error) {
	ents, err := os.ReadDir(c.base)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if id, ok := strings.CutPrefix(e.Name(), "job-"); ok && e.IsDir() && validJobID(id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Weights describes the container part of the throttle (for status notes),
// e.g. "cpu.weight=1, cpu.max=25% of one CPU, io.weight=1"; "" when no
// cpu/io controller is delegated to the job containers.
func (c *cgroupContainer) Weights() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, err := readPath(c.base+"/cgroup.subtree_control", &c.buf, 4096)
	if err != nil {
		return ""
	}
	var w []string
	if hasWord(raw, "cpu") {
		w = append(w, "cpu.weight=1")
		if c.cpuPct > 0 {
			w = append(w, "cpu.max="+strconv.Itoa(c.cpuPct)+"% of one CPU")
		}
	}
	if hasWord(raw, "io") {
		w = append(w, "io.weight=1")
	}
	return strings.Join(w, ", ")
}
