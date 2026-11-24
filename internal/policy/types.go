// Package policy is the deterministic core: a pure function
//
//	Step(p Params, s State, in Inputs) (State, Output)
//
// It never reads the clock, the OS, random numbers or map iteration order.
// Same (Params, State, Inputs) => same (State, Output), byte for byte after
// JSON encoding. The daemon logs (state_before, inputs, output, hash(state_after))
// so every decision can be replayed (`pace replay`).
//
// All quantities are int64 bytes / ms; fractions are permille. No floats.
package policy

import (
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// Modes.
const (
	ModeObserve  = "observe"   // M0: inform + warn only; every job admitted at once; nothing enforced
	ModeWarnOnly = "warn-only" // stages 1-2 incl. admission queue; stage 3 logged as would_* INFO events
	ModeEnforce  = "enforce"   // full ladder
)

// ClassParams are per-class defaults (already resolved to bytes for this capacity).
type ClassParams struct {
	Name          string `json:"name"`
	EstimateBytes int64  `json:"estimate_bytes"` // default expected peak when nothing declared/learned
	BudgetBytes   int64  `json:"budget_bytes"`   // default budget
	Slots         int    `json:"slots"`          // max concurrent jobs of this class; 0 = unlimited
	WallLimitMS   int64  `json:"wall_limit_ms"`  // 0 = none
	NoProgressMS  int64  `json:"no_progress_ms"` // stuck limit; 0 = none
	Priority      int    `json:"priority"`       // default priority 0..9
}

// Params is the resolved, validated configuration the policy runs with.
type Params struct {
	Mode string `json:"mode"`

	// CapacityBytes > 0 overrides machine memory (tests simulate a small
	// machine): available = Capacity - Inputs.ScopedUsedBytes.
	CapacityBytes  int64 `json:"capacity_bytes"`
	UseKernelLevel bool  `json:"use_kernel_level"` // fold macOS kernel pressure level / Linux PSI into the level
	ReserveBytes   int64 `json:"reserve_bytes"`    // never handed out by admission (see EffectiveReserve)
	// AdmitBelowWarn lets admission hand out memory below the pressure warn
	// line (the effective reserve is then ReserveBytes alone). Default
	// false: the effective reserve is max(ReserveBytes, the warn threshold),
	// so a job that stays within its estimate cannot by itself push the
	// machine into warn pressure and onto the pressure ladder.
	AdmitBelowWarn bool `json:"admit_below_warn,omitempty"`

	WarnAvailPermille int64 `json:"warn_avail_permille"` // pressure=warn when avail < this of capacity
	CritAvailPermille int64 `json:"crit_avail_permille"` // pressure=critical when avail < this
	PSIWarn10         int64 `json:"psi_warn10"`          // Linux: some avg10 permille for warn
	PSICrit10         int64 `json:"psi_crit10"`          // Linux: full avg10 permille for critical

	BudgetWarnPermille int64 `json:"budget_warn_permille"` // WARN mem_budget at used >= this of budget
	TolerancePermille  int64 `json:"tolerance_permille"`   // kill immediately when used > budget*(1+tol)

	WarnDeadlineMS  int64 `json:"warn_deadline_ms"`  // time between WARN and enforcement (non-critical)
	GraceMS         int64 `json:"grace_ms"`          // SIGTERM -> SIGKILL
	CriticalGraceMS int64 `json:"critical_grace_ms"` // SIGTERM -> SIGKILL at critical pressure
	StepMS          int64 `json:"step_ms"`           // escalation interval throttle->freeze->kill at warn
	CriticalStepMS  int64 `json:"critical_step_ms"`  // escalation interval at critical
	RecoverHoldMS   int64 `json:"recover_hold_ms"`   // pressure must stay normal this long before thaw/unthrottle
	OrphanGraceMS   int64 `json:"orphan_grace_ms"`   // wait for a lost wrapper to re-attach before reaping

	MinVictimBytes       int64 `json:"min_victim_bytes"`       // smaller targets are victims only as a swarm of units (see candidates)
	UnmanagedPriority    int   `json:"unmanaged_priority"`     // priority of processes started outside the wrapper
	UnmanagedBudgetBytes int64 `json:"unmanaged_budget_bytes"` // per process-group budget for unmanaged; 0 = none

	DefaultQueueTimeoutMS int64 `json:"default_queue_timeout_ms"`

	CPUWarnPerCoreMilli int64 `json:"cpu_warn_per_core_milli"` // load1/ncpu*1000 above which jobs are throttled
	CPUOKPerCoreMilli   int64 `json:"cpu_ok_per_core_milli"`   // below which throttling is lifted

	DiskWarnFreeBytes int64 `json:"disk_warn_free_bytes"`
	DiskCritFreeBytes int64 `json:"disk_crit_free_bytes"` // admissions refused below this

	MarginPermille    int64 `json:"margin_permille"`     // reservation margin over observed peak (sampling lag)
	HistorySize       int   `json:"history_size"`        // peaks kept per class/key
	HistoryMinSamples int   `json:"history_min_samples"` // samples needed before history is trusted
	// HistoryMaxKeys caps the number of per-command ("key:") history
	// entries; the least recently recorded is evicted first. Class entries
	// are never evicted. <= 0 means DefaultHistoryMaxKeys.
	HistoryMaxKeys int `json:"history_max_keys,omitempty"`

	Classes      []ClassParams `json:"classes"` // sorted by Name
	DefaultClass string        `json:"default_class"`

	// AdmitPauseOnJobWarn also pauses admission while any per-job warning
	// (mem_budget, stuck, wall_clock) is outstanding. Default false: only
	// machine-level conditions (pressure >= warn, a pressure warning or
	// throttle/freeze in progress, disk critical) pause admission, because a
	// single job's warning is already covered by its own reservation.
	AdmitPauseOnJobWarn bool `json:"admit_pause_on_job_warn,omitempty"`
}

// Job states.
const (
	JobQueued  = "queued"
	JobRunning = "running" // admitted (child may not have started yet)
	JobDone    = "done"
)

// Enforcement stages (Enforcement.Stage).
const (
	StageNone        = ""
	StageThrottled   = "throttled"
	StageFrozen      = "frozen"
	StageTerminating = "terminating" // SIGTERM sent, waiting grace
	StageKilled      = "killed"      // SIGKILL sent
)

// Warning is an outstanding WARN on a target.
type Warning struct {
	Policy     string `json:"policy"`
	IssuedMS   int64  `json:"issued_ms"`
	DeadlineMS int64  `json:"deadline_ms"`
}

// Enforcement is the ladder position of one target (job or unit).
type Enforcement struct {
	Warnings    []Warning `json:"warnings,omitempty"` // sorted by Policy
	Stage       string    `json:"stage,omitempty"`
	StageMS     int64     `json:"stage_ms,omitempty"`     // when the current stage was entered
	NextStepMS  int64     `json:"next_step_ms,omitempty"` // earliest time for the next escalation
	KillPolicy  string    `json:"kill_policy,omitempty"`  // policy that caused termination
	CPUThrottle bool      `json:"cpu_throttle,omitempty"` // throttled for CPU (separate from memory ladder)
	// Virtual marks Stage/CPUThrottle as bookkeeping only: they were decided
	// in observe/warn-only mode and their actions went to Output.Would.
	Virtual bool `json:"virtual,omitempty"`
}

// Job is a governed job (launched through the wrapper).
type Job struct {
	ID       string `json:"id"`
	Seq      int64  `json:"seq"` // arrival order
	Class    string `json:"class"`
	Key      string `json:"key,omitempty"`
	Priority int    `json:"priority"`
	State    string `json:"state"`

	EstimateBytes int64 `json:"estimate_bytes"`
	// Learned: the estimate was learned from the command's own runs (not
	// declared, not a class value); only changes the too_big hint.
	Learned       bool  `json:"learned,omitempty"`
	BudgetBytes   int64 `json:"budget_bytes"`
	ReservedBytes int64 `json:"reserved_bytes"`
	UsedBytes     int64 `json:"used_bytes"`
	PeakBytes     int64 `json:"peak_bytes"`
	NProcs        int   `json:"nprocs"`

	NoWait          bool  `json:"no_wait,omitempty"`
	SubmitMS        int64 `json:"submit_ms"`
	QueueDeadlineMS int64 `json:"queue_deadline_ms,omitempty"`
	AdmitMS         int64 `json:"admit_ms,omitempty"`
	StartMS         int64 `json:"start_ms,omitempty"`
	WallLimitMS     int64 `json:"wall_limit_ms,omitempty"`
	NoProgressMS    int64 `json:"no_progress_ms,omitempty"`

	PID  int `json:"pid,omitempty"`
	PGID int `json:"pgid,omitempty"`

	CPUMS          int64 `json:"cpu_ms"`
	LastProgressMS int64 `json:"last_progress_ms"` // last time CPU or output advanced
	OutputBytes    int64 `json:"output_bytes"`

	WrapperLostMS int64 `json:"wrapper_lost_ms,omitempty"` // 0 = connected
	LastPosition  int   `json:"last_position,omitempty"`   // last QUEUED position reported
	WasQueued     bool  `json:"was_queued,omitempty"`
	// LastQueueReason is the reason of the last QUEUED reported; a change
	// of reason re-emits QUEUED even when the position is unchanged.
	LastQueueReason string `json:"last_queue_reason,omitempty"`

	Enf Enforcement `json:"enf"`
}

// Unit is an unmanaged process group the policy is tracking (warned/enforced).
type Unit struct {
	Key       string      `json:"key"` // "pg:<pgid>:<leaderStartMS>"
	PGID      int         `json:"pgid"`
	LeaderPID int         `json:"leader_pid"`
	StartMS   int64       `json:"start_ms"`
	Comm      string      `json:"comm"`
	UsedBytes int64       `json:"used_bytes"`
	NProcs    int         `json:"nprocs"`
	Enf       Enforcement `json:"enf"`
}

// History keeps recent observed peaks for one class or command key.
type History struct {
	Key   string  `json:"key"`    // "class:<name>" or "key:<signature>"
	Peaks []int64 `json:"peaks"`  // most recent last, len <= HistorySize
	DurMS []int64 `json:"dur_ms"` // run durations, parallel to Peaks
	// LastMS is when the last observation was recorded (for evicting the
	// least recently used key entries; 0 in states written before it existed).
	LastMS int64 `json:"last_ms,omitempty"`
}

// Pressure is the machine-level ladder state.
type Pressure struct {
	Level         string `json:"level"` // proto.LevelNormal|Warn|Critical
	SinceMS       int64  `json:"since_ms"`
	NormalSinceMS int64  `json:"normal_since_ms"`
	CPUHigh       bool   `json:"cpu_high,omitempty"`
	DiskLow       string `json:"disk_low,omitempty"` // ""|warn|critical

	CPUOKSinceMS  int64 `json:"cpu_ok_since_ms,omitempty"` // load below the OK threshold since (0 = not below)
	CPUNextMS     int64 `json:"cpu_next_ms,omitempty"`     // earliest time to CPU-throttle one more job
	CritNextMS    int64 `json:"crit_next_ms,omitempty"`    // earliest time for the next critical victim round
	RecoverNextMS int64 `json:"recover_next_ms,omitempty"` // earliest time for the next thaw/unthrottle
}

// State is everything the policy remembers between steps. Slices are kept
// sorted (Jobs by Seq, Units by Key, History by Key) for determinism.
type State struct {
	NextSeq  int64     `json:"next_seq"`
	LastMS   int64     `json:"last_ms"`
	Jobs     []Job     `json:"jobs"`
	Units    []Unit    `json:"units,omitempty"`
	History  []History `json:"history,omitempty"`
	Pressure Pressure  `json:"pressure"`
}

// JobSample is the attributed footprint of one governed job this tick.
type JobSample struct {
	ID        string `json:"id"`
	UsedBytes int64  `json:"used_bytes"`
	NProcs    int    `json:"nprocs"`
	CPUMS     int64  `json:"cpu_ms"`
	Alive     bool   `json:"alive"` // any member process still exists
	// ExtraPIDs are members outside the job's process group (setsid'd,
	// re-grouped or orphaned descendants) that must be signalled directly.
	ExtraPIDs []int `json:"extra_pids,omitempty"`
}

// UnitSample is one unmanaged process group this tick (groups at or above
// MinVictimBytes and ones already tracked; under memory pressure the
// daemon adds the largest smaller ones).
type UnitSample struct {
	Key       string `json:"key"`
	PGID      int    `json:"pgid"`
	LeaderPID int    `json:"leader_pid"`
	StartMS   int64  `json:"start_ms"`
	Comm      string `json:"comm"`
	UsedBytes int64  `json:"used_bytes"`
	NProcs    int    `json:"nprocs"`
	CPUMS     int64  `json:"cpu_ms"`
	// PIDs of the group's members (signalled individually in addition to killpg
	// when the group leader is gone).
	PIDs []int `json:"pids,omitempty"`
}

// Event kinds for Inputs.Events (from wrappers via the daemon).
const (
	EvSubmit   = "submit"
	EvStarted  = "started"
	EvProgress = "progress"
	EvExit     = "exit"
	EvLost     = "wrapper_lost" // stream closed while queued/running
	EvAttach   = "attach"       // wrapper re-attached (after daemon restart)
)

// InEvent is one wrapper-originated event, in arrival order.
type InEvent struct {
	Kind string `json:"kind"`
	Job  string `json:"job"`

	Submit *proto.SubmitRequest `json:"submit,omitempty"` // EvSubmit, EvAttach

	PID     int   `json:"pid,omitempty"`      // EvStarted, EvAttach
	PGID    int   `json:"pgid,omitempty"`     // EvStarted, EvAttach
	StartMS int64 `json:"start_ms,omitempty"` // EvStarted, EvAttach

	OutputBytes  int64 `json:"output_bytes,omitempty"`   // EvProgress, EvAttach
	LastOutputMS int64 `json:"last_output_ms,omitempty"` // EvProgress

	Code     int  `json:"code,omitempty"` // EvExit
	Signaled bool `json:"signaled,omitempty"`
	Signal   int  `json:"signal,omitempty"`
	// OOMKill (EvExit): the kernel killed the job for its container's hard
	// memory limit, which the daemon sets at the immediate-kill line of
	// mem_budget; PeakBytes is the container's peak then.
	OOMKill   bool  `json:"oom_kill,omitempty"`
	PeakBytes int64 `json:"peak_bytes,omitempty"`
	// Cgroup (EvStarted, EvAttach): the job container the wrapper reported.
	// The daemon acts on it; the policy ignores it.
	Cgroup string `json:"cgroup,omitempty"`
}

// Inputs is everything one Step sees. The daemon builds it; it is logged.
type Inputs struct {
	NowMS           int64            `json:"now_ms"`
	Machine         platform.Machine `json:"machine"`
	ScopedUsedBytes int64            `json:"scoped_used_bytes"` // sum footprint of in-scope procs (for CapacityBytes)
	Jobs            []JobSample      `json:"jobs,omitempty"`    // sorted by ID
	Units           []UnitSample     `json:"units,omitempty"`   // sorted by Key
	Events          []InEvent        `json:"events,omitempty"`  // arrival order
	// Stale: the machine or process sample could not be refreshed this
	// tick, so Machine, Jobs and Units are the last good ones. Nothing is
	// started or escalated on stale data (no new warning of the pressure
	// ladder or stuck detection, no throttle, freeze, kill, or SIGKILL after
	// grace); relaxing steps still run. What was held happens on the first
	// fresh step, with its signals and events, against fresh samples.
	Stale bool `json:"stale,omitempty"`
}

// Action ops.
const (
	OpSignalGroup = "signal_group" // PGID + Signal
	OpSignal      = "signal"       // PIDs + Signal
	OpBackground  = "background"   // PIDs/PGID: deprioritise
	OpForeground  = "foreground"   // undo background
)

// Action is a side effect the daemon executes after Step.
type Action struct {
	Op     string `json:"op"`
	Job    string `json:"job,omitempty"`
	Unit   string `json:"unit,omitempty"`
	PGID   int    `json:"pgid,omitempty"`
	PIDs   []int  `json:"pids,omitempty"`
	Signal int    `json:"signal,omitempty"` // syscall.Signal number
	Policy string `json:"policy,omitempty"`
}

// Output is what one Step decided.
type Output struct {
	// Events go to the job's stream when Job is set, and always to the
	// alert/status feed and the decision log.
	Events  []proto.Event `json:"events,omitempty"`
	Actions []Action      `json:"actions,omitempty"`
	// Would are actions suppressed by observe/warn-only mode (logged only).
	Would []Action `json:"would,omitempty"`
}

// Empty reports whether nothing was decided.
func (o Output) Empty() bool { return len(o.Events) == 0 && len(o.Actions) == 0 && len(o.Would) == 0 }
