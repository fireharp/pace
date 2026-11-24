package proto

// HTTP/1.1 + JSON over a unix domain socket (0700 dir, peer uid checked).
//
// Endpoints (all JSON; streams are NDJSON, one Event per line, flushed):
//
//	POST /v1/jobs                 SubmitRequest -> stream of Event for the job's life.
//	                              First event is ACCEPTED (job id). Then QUEUED*,
//	                              then ADMITTED or REFUSED. After ADMITTED the
//	                              stream stays open and carries WARN/OK/THROTTLED/
//	                              FROZEN/THAWED/KILLED... until the job ends.
//	                              The wrapper closing this stream = wrapper lost
//	                              (queued: dequeue; running: orphaned -> kill group).
//	POST /v1/jobs/{id}/started    StartedRequest  -> 204
//	POST /v1/jobs/{id}/progress   ProgressRequest -> 204 (heartbeat, ~1s)
//	POST /v1/jobs/{id}/exit       ExitRequest     -> 204
//	POST /v1/jobs/{id}/attach     AttachRequest   -> stream (re-attach after daemon restart)
//	GET  /v1/jobs/{id}            -> JobStatus (with Events), kept 24h after end
//	GET  /v1/status               -> Status
//	GET  /v1/events?follow=1      -> stream of all Events (the alert channel)
//	POST /v1/reload               -> 204 (re-read config)
//	GET  /metrics                 -> Prometheus text format
//	GET  /healthz                 -> 200 "ok"

// SubmitRequest asks for admission of one command.
type SubmitRequest struct {
	Class          string   `json:"class,omitempty"`          // build|test|browser|model|default|...
	EstimateBytes  int64    `json:"estimate_bytes,omitempty"` // declared expected peak footprint
	BudgetBytes    int64    `json:"budget_bytes,omitempty"`   // requested budget (else class default)
	Priority       *int     `json:"priority,omitempty"`       // 0..9, higher = more important (default from class)
	WallLimitMS    int64    `json:"wall_limit_ms,omitempty"`  // timeout; 0 = class default
	NoWait         bool     `json:"no_wait,omitempty"`        // refuse instead of queueing
	QueueTimeoutMS int64    `json:"queue_timeout_ms,omitempty"`
	Argv           []string `json:"argv"`
	Cwd            string   `json:"cwd,omitempty"`
	WrapperPID     int      `json:"wrapper_pid"`
	Key            string   `json:"key,omitempty"` // command signature for learning (argv0+subcommand); derived if empty
}

// StartedRequest reports the spawned job process.
type StartedRequest struct {
	PID     int   `json:"pid"`
	PGID    int   `json:"pgid"`
	StartMS int64 `json:"start_ms"`
	// Cgroup is the job container (Linux cgroup v2 directory) the child was
	// spawned into; "" = none. The daemon uses it only when it is the path
	// its own container facility gives for the job.
	Cgroup string `json:"cgroup,omitempty"`
}

// ProgressRequest is the wrapper's heartbeat with output progress.
type ProgressRequest struct {
	OutputBytes  int64 `json:"output_bytes"`   // cumulative bytes the job wrote to stdout+stderr
	LastOutputMS int64 `json:"last_output_ms"` // unix ms of last write (0 = none yet)
}

// ExitRequest reports how the job ended.
type ExitRequest struct {
	Code     int  `json:"code"`     // exit status if exited normally
	Signaled bool `json:"signaled"` // terminated by a signal
	Signal   int  `json:"signal,omitempty"`
	// OOMKill: the kernel killed the job for its container's hard memory
	// limit (memory.max); PeakBytes is the container's peak usage then.
	OOMKill   bool  `json:"oom_kill,omitempty"`
	PeakBytes int64 `json:"peak_bytes,omitempty"`
}

// AttachRequest re-registers a running job with a restarted daemon.
type AttachRequest struct {
	Submit      SubmitRequest `json:"submit"`
	PID         int           `json:"pid"`
	PGID        int           `json:"pgid"`
	StartMS     int64         `json:"start_ms"`
	SubmitMS    int64         `json:"submit_ms"`
	OutputBytes int64         `json:"output_bytes"`
	Cgroup      string        `json:"cgroup,omitempty"` // as StartedRequest.Cgroup
	// LastEventTS is the ts of the last event the wrapper got on its
	// previous stream (0 = none): the daemon replays the job's later
	// events first (those its stream never delivered, such as a KILLED
	// or FROZEN a restarted daemon sent before this re-attach).
	LastEventTS int64 `json:"last_event_ts,omitempty"`
}

// JobStatus describes one governed job.
type JobStatus struct {
	ID            string   `json:"id"`
	Class         string   `json:"class"`
	State         string   `json:"state"` // queued|running|done
	Stage         string   `json:"stage,omitempty"`
	Priority      int      `json:"priority"`
	Argv          []string `json:"argv,omitempty"`
	PID           int      `json:"pid,omitempty"`
	PGID          int      `json:"pgid,omitempty"`
	NProcs        int      `json:"nprocs,omitempty"`
	EstimateBytes int64    `json:"estimate_bytes"`
	BudgetBytes   int64    `json:"budget_bytes"`
	ReservedBytes int64    `json:"reserved_bytes"`
	UsedBytes     int64    `json:"used_bytes"`
	PeakBytes     int64    `json:"peak_bytes"`
	Position      int      `json:"position,omitempty"`
	SubmitTS      int64    `json:"submit_ts"`
	StartTS       int64    `json:"start_ts,omitempty"`
	EndTS         int64    `json:"end_ts,omitempty"`
	ExitCode      *int     `json:"exit_code,omitempty"`
	KilledBy      string   `json:"killed_by,omitempty"` // policy name if killed by governor
	Events        []Event  `json:"events,omitempty"`
}

// UnitStatus describes an unmanaged process group (started outside the wrapper).
type UnitStatus struct {
	Key       string `json:"key"` // "pg:<pgid>"
	PGID      int    `json:"pgid"`
	LeaderPID int    `json:"leader_pid"`
	Comm      string `json:"comm"`
	NProcs    int    `json:"nprocs"`
	UsedBytes int64  `json:"used_bytes"`
	StartTS   int64  `json:"start_ts"`
	Priority  int    `json:"priority"`
	Stage     string `json:"stage,omitempty"`
}

// SmallUnitsStatus summarises the unmanaged process groups below min_victim
// (each too small to be a unit on its own), which `unmanaged` does not list
// at normal pressure. Their memory counts as used all the same.
type SmallUnitsStatus struct {
	Groups         int   `json:"groups"`
	UsedBytes      int64 `json:"used_bytes"`
	MinVictimBytes int64 `json:"min_victim_bytes"`
}

// Status is the machine-wide view (`pace status`).
type Status struct {
	V        int    `json:"v"`
	TS       int64  `json:"ts"`
	Version  string `json:"version"`
	Platform string `json:"platform"`
	Mode     string `json:"mode"` // enforce|warn-only

	CapacityBytes int64  `json:"capacity_bytes"`
	AvailBytes    int64  `json:"avail_bytes"`
	ReserveBytes  int64  `json:"reserve_bytes"`  // safety reserve never handed out
	ReservedBytes int64  `json:"reserved_bytes"` // sum of unconsumed job reservations
	HeadroomBytes int64  `json:"headroom_bytes"` // what a new job may use now
	Pressure      string `json:"pressure"`       // normal|warn|critical
	KernelLevel   int    `json:"kernel_level"`
	SwapUsedBytes int64  `json:"swap_used_bytes"`
	Load1Milli    int64  `json:"load1_milli"`
	NCPU          int    `json:"ncpu"`
	DiskFreeBytes int64  `json:"disk_free_bytes"`

	Admitting            bool           `json:"admitting"`
	NotAdmittingReasons  []string       `json:"not_admitting_reasons,omitempty"`
	SuggestedConcurrency int            `json:"suggested_concurrency"`
	SuggestedByClass     map[string]int `json:"suggested_by_class"`

	Running   []JobStatus  `json:"running"`
	Queued    []JobStatus  `json:"queued"`
	Recent    []JobStatus  `json:"recent,omitempty"`
	Unmanaged []UnitStatus `json:"unmanaged,omitempty"` // top by footprint
	// UnmanagedBelowMinVictim counts every unmanaged group below min_victim
	// (nil when there is none), whether or not `unmanaged` lists it.
	UnmanagedBelowMinVictim *SmallUnitsStatus `json:"unmanaged_below_min_victim,omitempty"`
	Warnings                []Event           `json:"warnings,omitempty"` // currently active warnings
	// RecentAlerts are the alert events (WARN, KILLED, FROZEN, THROTTLED,
	// and INFO lines naming a policy, such as observe mode's "would kill")
	// of the last 15 minutes, oldest first, at most 32: a client that polls
	// status misses no alert, even one that was over before it polled.
	RecentAlerts []Event  `json:"recent_alerts,omitempty"`
	Faults       []string `json:"faults,omitempty"` // e.g. sampler errors, log gaps
	Notes        []string `json:"notes,omitempty"`  // platform limits, e.g. what UNTHROTTLED cannot undo
}
