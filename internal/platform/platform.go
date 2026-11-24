// Package platform hides how each operating system senses and enforces.
// Policy and contract are shared; only this interface differs per OS.
//
// Implementations: darwin (platform_darwin.go, pure Go, CGO_ENABLED=0),
// linux (platform_linux.go, procfs + PSI + optional cgroup v2), and Fake
// (fake.go) for deterministic tests on any OS.
package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"syscall"
)

// Machine is one machine-level sample. All sizes in bytes, times in ms.
type Machine struct {
	NowMS       int64 `json:"now_ms"`
	TotalBytes  int64 `json:"total_bytes"`  // physical memory
	AvailBytes  int64 `json:"avail_bytes"`  // kernel's estimate of memory available without pressure
	KernelLevel int   `json:"kernel_level"` // normalised: 0 unknown, 1 normal, 2 warn, 4 critical
	SwapTotal   int64 `json:"swap_total"`
	SwapUsed    int64 `json:"swap_used"`
	// PSIMemSome10 is Linux memory PSI "some avg10" in permille (x10 of %), -1 if n/a.
	PSIMemSome10 int64 `json:"psi_mem_some10"`
	// PSIMemFull10 is Linux memory PSI "full avg10" in permille, -1 if n/a.
	PSIMemFull10 int64 `json:"psi_mem_full10"`
	Load1Milli   int64 `json:"load1_milli"` // 1-minute load average x1000
	NCPU         int   `json:"ncpu"`
	// DiskFreeBytes is the minimum free space over the configured paths (-1 if none).
	DiskFreeBytes int64 `json:"disk_free_bytes"`
}

// Proc is one process of the governed user.
type Proc struct {
	PID       int    `json:"pid"`
	PPID      int    `json:"ppid"`
	PGID      int    `json:"pgid"`
	UID       int    `json:"uid"`
	StartMS   int64  `json:"start_ms"` // unix ms; (PID, StartMS) identifies a process across pid reuse
	Comm      string `json:"comm"`
	Exe       string `json:"exe,omitempty"` // executable path when known
	Footprint int64  `json:"fp"`            // physical footprint bytes (macOS) / RSS+swap (Linux fallback)
	CPUMS     int64  `json:"cpu_ms"`        // cumulative user+system CPU time
	State     byte   `json:"st"`            // 'R' running, 'S' sleeping, 'T' stopped, 'Z' zombie
	// System marks operating-system processes that must never be signalled
	// (macOS: platform binary under /System, /usr/libexec, /usr/sbin, /sbin,
	// /Library/Apple; or kill(pid,0) == EPERM). Job membership overrides it.
	System bool `json:"sys,omitempty"`
	// JobTag is the PACE_JOB_ID found in the environment, when readable cheaply
	// (best effort; empty if unknown). Backends may leave it empty.
	JobTag string `json:"tag,omitempty"`
	// TermLeader marks a terminal's (login) shell, or what replaced it by
	// exec: the leader of a session that has a controlling terminal, or on
	// macOS a group leader on a terminal whose parent leads the session and
	// is not the user's (the -zsh that Terminal.app and iTerm2 start through
	// root's /usr/bin/login).
	// Attribution never makes a small one part of an unmanaged unit.
	TermLeader bool `json:"tl,omitempty"`
}

// Backend senses the machine and enforces on processes of one user.
// All methods must be safe to call from one goroutine; they must not
// allocate heavily (called every 250ms-1s) and must never block on I/O
// that can stall under memory pressure (no shelling out).
type Backend interface {
	Name() string // "darwin" | "linux" | "fake"
	// SampleMachine returns machine pressure; diskPaths are checked for free space.
	SampleMachine(diskPaths []string) (Machine, error)
	// ListProcs returns all processes owned by uid, with Footprint and CPUMS
	// filled. Processes that vanish mid-scan are skipped silently.
	ListProcs(uid int) ([]Proc, error)
	// Signal sends sig to one process. ESRCH is not an error.
	Signal(pid int, sig syscall.Signal) error
	// SignalGroup sends sig to a process group. ESRCH is not an error.
	SignalGroup(pgid int, sig syscall.Signal) error
	// SetBackground deprioritises (on=true) or restores (on=false) one process'
	// CPU and I/O priority. Must be reversible, except for a part the OS does
	// not let the user undo, which the backend then reports through
	// ThrottleNoter (Linux unprivileged: the raised nice stays).
	SetBackground(pid int, on bool) error
}

// StderrNote is one governor message for the processes of a unit (a
// process group started outside pace run), written to their own stderr:
// such a process has no pace run whose stream would print it.
type StderrNote struct {
	// PIDs are the processes whose stderr gets the text, the group leader
	// first. A stderr that several of them share gets it once.
	PIDs []int
	// Text is whole lines, each ending in "\n" and at most
	// StderrNoteMaxLine bytes long with it: the event line and its hint.
	Text string
}

// StderrNoteMaxLine is the longest line (newline included) a StderrNote
// may carry; the whole text fits PIPE_BUF, so a pipe takes it in one
// atomic write or not at all.
const StderrNoteMaxLine = 512

// StderrNotifier is optional (Linux: /proc/<pid>/fd/2): a backend that can
// write a note to other processes' stderr. NotifyStderr must not block
// the caller for more than a few milliseconds whatever the targets are
// (a full pipe, a hung file system), never retries, and ignores every
// error: the note is a courtesy, the signal that follows is what counts.
// It writes only to a pipe, a regular file or an AF_UNIX socket that the
// process has open for writing as its fd 2 (never a terminal), each note
// starting on a new line. dropped (may be nil; called from any goroutine,
// possibly after NotifyStderr returned) is told when notes are dropped
// because an earlier write is still running or the writes passed their
// deadline.
type StderrNotifier interface {
	NotifyStderr(notes []StderrNote, dropped func(reason string))
}

// ThrottleNoter is optional: a backend whose SetBackground(pid, true)
// cannot fully be undone, or does little on this machine, says so, for
// `pace status` notes ("" = a strong, fully reversible throttle).
//
// cpuMax: the daemon's job containers cap a throttled job with a cpu.max
// quota (Linux, cpu controller delegated), which holds whatever the
// per-thread throttle does; the note then says which jobs it is weak for.
type ThrottleNoter interface {
	ThrottleNote(cpuMax bool) string
}

// ThrottleCaveater is optional: the short, line-sized form of the
// ThrottleNote, which the daemon puts in msg= of THROTTLED lines ("" =
// none), so an agent told it is throttled also learns the throttle is
// weak or leaves something behind.
//
// cpuMax: this throttle also caps the job with its container's cpu.max
// quota, so a per-thread throttle that keeps the CPU share is not weak.
type ThrottleCaveater interface {
	ThrottleCaveat(cpuMax bool) string
}

// JobContainer is an optional capability (Linux cgroup v2): a per-job
// container that gives exact accounting and kernel enforcement. The wrapper
// (job parent) creates it and places the child in it; the daemon reads and
// acts on it by job id. Backends without it return ok=false from Containers().
//
// Who does what (docs/ARCHITECTURE.md, "Linux job containers"): the daemon
// detects its base (ContainerProvider.Containers, scoped to the daemon) and
// puts the job's Path on ADMITTED. The wrapper opens that base
// (ContainersAt), Creates the container (with ADMITTED's limit), spawns the
// child straight into it (CLONE_INTO_CGROUP, else Attach right after the
// spawn), reports Path to the daemon, and after the child's death reads
// OOMKills/Peak and Removes an empty container. The daemon, once the
// reported path equals its own Path for the job, reads PIDs and Usage every
// tick, moves pace's own processes out (Evict), sets the hard cap
// (SetMemMax), freezes, throttles and kills through it, and Removes it when
// the job has ended and it is empty.
type JobContainer interface {
	// Create makes the container for jobID with a hard memory limit
	// (0 = none) and returns its path (the same as Path). Creating an
	// existing container only sets the limit.
	Create(jobID string, memMaxBytes int64) (path string, err error)
	// Path is the container's path for jobID, computed without I/O.
	Path(jobID string) (string, error)
	// Attach moves pid into the job's container.
	Attach(jobID string, pid int) error
	// PIDs lists the processes inside the container.
	PIDs(jobID string) ([]int, error)
	// Usage is the job's own memory (anonymous + shmem + swap: the
	// footprint semantics, but exact: every process inside, shared pages
	// once).
	Usage(jobID string) (bytes int64, err error)
	// Peak is the highest usage the kernel saw.
	Peak(jobID string) (bytes int64, err error)
	// OOMKills counts kills by the kernel for the hard memory limit.
	OOMKills(jobID string) (int64, error)
	// SetMemMax changes the hard memory limit (0 = none).
	SetMemMax(jobID string, memMaxBytes int64) error
	// SetThrottle lowers (on) or restores the job's CPU and I/O weights and
	// reports what it set ("" = no weight knob available).
	SetThrottle(jobID string, on bool) (string, error)
	Freeze(jobID string, on bool) error
	// Frozen reads the container's freeze state back from the kernel
	// (cgroup.freeze): a pace run that lost its daemon, or forwarded a
	// signal, thaws its job's container behind the daemon's back.
	Frozen(jobID string) (bool, error)
	Kill(jobID string) error
	// Remove deletes the container (and the empty cgroups the job created
	// below it), waiting up to 1 s for processes just killed to leave.
	Remove(jobID string) error
	// RemoveNow is Remove with a single attempt and no wait.
	RemoveNow(jobID string) error
	// Jobs lists the job ids that have a container.
	Jobs() ([]string, error)
	// Evict moves pid (a pace wrapper, or the daemon itself, found inside a
	// job's container) out of every job container, so that no freeze, kill
	// or memory limit of that job reaches it.
	Evict(pid int) error
}

// ContainersAt opens the job container facility at base, the directory a
// daemon detected (its job paths' parent, given to the wrapper on
// ADMITTED), without constructing a Backend, or says why it cannot (always
// on macOS).
func ContainersAt(base string) (JobContainer, error) {
	if newContainersAt == nil {
		return nil, errNoContainers
	}
	return newContainersAt(base)
}

var newContainersAt func(base string) (JobContainer, error)

type noContainersErr struct{}

func (noContainersErr) Error() string { return "platform: no job containers on this operating system" }

var errNoContainers error = noContainersErr{}

// ContainerNoter is optional: a ContainerProvider that can say why it has
// no containers ("" when it has them or was not probed yet).
type ContainerNoter interface {
	ContainersReason() string
}

// ContainerProvider is implemented by backends that support JobContainer.
// scope names the caller's own subtree of job containers (a daemon's,
// derived from its socket path): two daemons of one user never share one.
type ContainerProvider interface {
	Containers(scope string) (JobContainer, bool)
}

// ContainerScope is the scope a daemon listening on socket uses: stable
// across its restarts, distinct for every other socket (one daemon per
// socket).
func ContainerScope(socket string) string {
	if abs, err := filepath.Abs(socket); err == nil {
		socket = abs
	}
	sum := sha256.Sum256([]byte(filepath.Clean(socket)))
	return "d-" + hex.EncodeToString(sum[:6])
}

// TagReader is an optional capability: read PACE_JOB_ID from a process'
// environment (best effort; "" if unreadable). Attribution calls it only for
// new processes it cannot place otherwise, so it may cost ~10-20 µs.
type TagReader interface {
	JobTag(pid int) string
}
