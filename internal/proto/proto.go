// Package proto is the agent-facing contract: exit codes, the one-line
// stderr message grammar, the JSON event form, and the HTTP API types
// shared by the daemon and the `pace run` wrapper.
//
// THE CONTRACT IS THE PRODUCT. Field names, verbs and exit codes here are
// what agents parse. Change them only deliberately and update docs/CONTRACT.md.
//
// Line grammar (stderr):
//
//	line  = "pace: " VERB *(SP key "=" value) LF
//	hint  = "pace: hint: " free-text LF          (only when an action is possible)
//	key   = [a-z][a-z0-9_]*
//	value = bare | quoted   ; bare has no SP, '"', '=' or control chars,
//	                        ; otherwise Go strconv.Quote form.
//
// Sizes print as FormatBytes ("5.6G", "512M"); durations as FormatMS
// ("40s", "250ms"). JSON always carries integer *_bytes and *_ms fields.
package proto

import (
	"strconv"
	"strings"

	"github.com/fireharp/pace/internal/units"
)

// Exit codes. An admitted command's own exit code passes through unchanged.
// Governor codes form a reserved range chosen to avoid 0-2, sysexits 64-78,
// 124-127, 128+signal (129-192) and 255. Whenever the wrapper exits with a
// code in [ExitRefused, ExitPaceError] it prints a final EXIT line saying
// whether the governor or the child produced it (source=pace|child).
const (
	ExitRefused   = 200 // refused: no-wait mode, queue timeout, admission impossible, or disk critical at submit
	ExitKilled    = 201 // killed by governor policy (policy= says which)
	ExitPaceError = 202 // governor usage or configuration error
)

// IsReserved reports whether code falls in the governor's reserved range.
func IsReserved(code int) bool { return code >= ExitRefused && code <= ExitPaceError }

// Prefix starts every governor stderr line.
const Prefix = "pace: "

// Verbs.
const (
	VerbAccepted    = "ACCEPTED"    // stream-only: job id assigned (never printed)
	VerbQueued      = "QUEUED"      // waiting for headroom/slots
	VerbAdmitted    = "ADMITTED"    // printed only if the job was queued first
	VerbRefused     = "REFUSED"     // will not run; exit 200
	VerbWarn        = "WARN"        // a policy is about to fire
	VerbOK          = "OK"          // a previous WARN cleared (recovered)
	VerbThrottled   = "THROTTLED"   // deprioritised (CPU/IO background)
	VerbUnthrottled = "UNTHROTTLED" // priority restored
	VerbFrozen      = "FROZEN"      // SIGSTOP'd
	VerbThawed      = "THAWED"      // SIGCONT'd
	VerbKilled      = "KILLED"      // killed by policy; exit 201
	VerbDegraded    = "DEGRADED"    // daemon unreachable / lost: fail-open
	VerbExit        = "EXIT"        // final line when exit code is in reserved range
	VerbInfo        = "INFO"        // informational (status feed only)
)

// Policies (policy= values).
const (
	PolicyMemBudget = "mem_budget" // job footprint vs its budget
	PolicyPressure  = "pressure"   // machine memory pressure
	PolicyStuck     = "stuck"      // no CPU and no output progress past class limit
	PolicyWallClock = "wall_clock" // exceeded wall-clock limit / timeout
	PolicyOrphaned  = "orphaned"   // wrapper vanished; job has no owner
	PolicyCPU       = "cpu"        // CPU overload (throttling)
	PolicyDisk      = "disk"       // disk space low
)

// MsgKernelMemMax is the msg of a KILLED policy=mem_budget line when the
// kernel killed the job at its container's hard limit (Linux memory.max =
// budget * (1 + tolerance)) before any sample saw it get there.
const MsgKernelMemMax = "the kernel enforced the hard memory limit (cgroup memory.max)"

// MsgBeforeWarnDeadline ends the msg of a KILLED policy=mem_budget line that
// came before the deadline an earlier WARN announced (next=kill in=...): that
// deadline holds only below budget + tolerance, and the job went past it.
const MsgBeforeWarnDeadline = ", before the WARN's deadline"

// Reasons (reason= values) for QUEUED/REFUSED/DEGRADED.
const (
	ReasonHeadroom     = "headroom"      // expected footprint does not fit
	ReasonPressure     = "pressure"      // pressure at warn or above: admission paused
	ReasonSlots        = "slots"         // class concurrency slots full
	ReasonDisk         = "disk"          // disk space low
	ReasonTooBig       = "too_big"       // estimate exceeds what the machine can ever give
	ReasonQueueTimeout = "queue_timeout" // waited longer than the queue timeout
	ReasonUnreachable  = "daemon_unreachable"
	ReasonDaemonLost   = "daemon_lost"
	ReasonFailOpenFull = "failopen_full"
)

// Pressure levels.
const (
	LevelNormal   = "normal"
	LevelWarn     = "warn"
	LevelCritical = "critical"
)

// Event is one governor message. The same content renders as one stderr
// line (Lines) and as JSON. Optional numeric fields are pointers so that a
// meaningful zero (free=0) is distinguishable from absent.
type Event struct {
	V    int    `json:"v"`
	TS   int64  `json:"ts"` // unix ms
	Verb string `json:"verb"`

	Job   string `json:"job,omitempty"`
	Unit  string `json:"unit,omitempty"` // unmanaged unit key, e.g. "pg:4242"
	Class string `json:"class,omitempty"`
	PID   int    `json:"pid,omitempty"`
	PGID  int    `json:"pgid,omitempty"`
	Comm  string `json:"comm,omitempty"`

	Reason string `json:"reason,omitempty"`
	Policy string `json:"policy,omitempty"`
	Level  string `json:"level,omitempty"`

	UsedBytes   *int64 `json:"used_bytes,omitempty"`
	BudgetBytes *int64 `json:"budget_bytes,omitempty"`
	// FreeBytes: the headroom left for the job (admission lines); free
	// disk space on reason=disk / policy=disk lines; memory available now
	// on policy=pressure lines and the memory pressure INFO.
	FreeBytes *int64 `json:"free_bytes,omitempty"`
	NeedBytes *int64 `json:"need_bytes,omitempty"`
	Priority  *int   `json:"priority,omitempty"` // ACCEPTED: the resolved priority

	Position     *int   `json:"position,omitempty"`
	ETAMS        *int64 `json:"eta_ms,omitempty"`
	RetryAfterMS *int64 `json:"retry_after_ms,omitempty"`
	WaitedMS     *int64 `json:"waited_ms,omitempty"`
	IdleMS       *int64 `json:"idle_ms,omitempty"`    // stuck: time without progress
	ElapsedMS    *int64 `json:"elapsed_ms,omitempty"` // wall_clock
	LimitMS      *int64 `json:"limit_ms,omitempty"`   // wall_clock / stuck limit
	Next         string `json:"next,omitempty"`       // action that will follow: kill|freeze|throttle|refuse
	InMS         *int64 `json:"in_ms,omitempty"`      // time until Next fires
	// Mode is set (observe|warn-only) on a line whose next action, or whose
	// "would ..." action, the governor will not carry out in this mode.
	Mode string `json:"mode,omitempty"`

	// MemMaxBytes (ADMITTED, stream only, never printed): the hard memory
	// limit the wrapper gives the job's container when it creates it (Linux
	// cgroup v2, enforce mode, enforce.hard_cap), so that it holds from the
	// child's first instruction.
	MemMaxBytes *int64 `json:"mem_max_bytes,omitempty"`
	// Cgroup (ADMITTED, stream only, never printed): the job container path
	// the wrapper creates and spawns the child into (Linux cgroup v2: set
	// only when the daemon runs jobs in containers). The wrapper prints an
	// INFO line when it cannot use it.
	Cgroup string `json:"cgroup,omitempty"`

	Code   *int   `json:"code,omitempty"`   // EXIT
	Source string `json:"source,omitempty"` // EXIT: pace|child
	Msg    string `json:"msg,omitempty"`    // free text (DEGRADED/INFO)
	Hint   string `json:"hint,omitempty"`   // next action for the agent
}

// I64 / Int return pointers for optional Event fields.
func I64(v int64) *int64 { return &v }
func Int(v int) *int     { return &v }

type kv struct{ k, v string }

// fields returns key=value pairs in the fixed, documented order.
func (e *Event) fields() []kv {
	var out []kv
	add := func(k, v string) {
		if v != "" {
			out = append(out, kv{k, v})
		}
	}
	b := func(p *int64) string {
		if p == nil {
			return ""
		}
		return units.FormatBytes(*p)
	}
	d := func(p *int64) string {
		if p == nil {
			return ""
		}
		return units.FormatMS(*p)
	}
	add("reason", e.Reason)
	add("policy", e.Policy)
	add("level", e.Level)
	add("used", b(e.UsedBytes))
	add("budget", b(e.BudgetBytes))
	add("free", b(e.FreeBytes))
	add("need", b(e.NeedBytes))
	if e.Priority != nil {
		add("priority", strconv.Itoa(*e.Priority))
	}
	if e.Position != nil {
		add("position", strconv.Itoa(*e.Position))
	}
	add("eta", d(e.ETAMS))
	add("retry_after", d(e.RetryAfterMS))
	add("waited", d(e.WaitedMS))
	add("idle", d(e.IdleMS))
	add("elapsed", d(e.ElapsedMS))
	add("limit", d(e.LimitMS))
	add("next", e.Next)
	add("in", d(e.InMS))
	add("mode", e.Mode)
	if e.Code != nil {
		add("code", strconv.Itoa(*e.Code))
	}
	add("source", e.Source)
	add("unit", e.Unit)
	if e.PID != 0 {
		add("pid", strconv.Itoa(e.PID))
	}
	add("comm", e.Comm)
	add("class", e.Class)
	add("job", e.Job)
	add("msg", e.Msg)
	return out
}

// QuoteValue returns v bare if safe, else strconv.Quote(v).
func QuoteValue(v string) string {
	if v == "" {
		return `""`
	}
	for _, r := range v {
		if r <= ' ' || r == '"' || r == '=' || r == 0x7f || r > 0x7e {
			return strconv.Quote(v)
		}
	}
	return v
}

// Line renders the main stderr line, without trailing newline.
func (e *Event) Line() string {
	var sb strings.Builder
	sb.WriteString(Prefix)
	sb.WriteString(e.Verb)
	for _, f := range e.fields() {
		sb.WriteByte(' ')
		sb.WriteString(f.k)
		sb.WriteByte('=')
		sb.WriteString(QuoteValue(f.v))
	}
	return sb.String()
}

// Lines renders the event as 1 or 2 stderr lines (main line + optional
// hint), without trailing newlines.
func (e *Event) Lines() []string {
	l := []string{e.Line()}
	if h := strings.TrimSpace(e.Hint); h != "" {
		h = strings.ReplaceAll(h, "\n", " ")
		l = append(l, Prefix+"hint: "+h)
	}
	return l
}

// ParseLine parses a governor line back into verb and raw key/value
// strings (values unquoted). ok=false if the line is not a governor line or
// is a hint line. Used by tests and by agents' tooling.
func ParseLine(line string) (verb string, kvs map[string]string, ok bool) {
	if !strings.HasPrefix(line, Prefix) {
		return "", nil, false
	}
	rest := line[len(Prefix):]
	if strings.HasPrefix(rest, "hint: ") {
		return "", nil, false
	}
	sp := strings.IndexByte(rest, ' ')
	if sp < 0 {
		return rest, map[string]string{}, rest != ""
	}
	verb, rest = rest[:sp], rest[sp+1:]
	kvs = map[string]string{}
	for len(rest) > 0 {
		eq := strings.IndexByte(rest, '=')
		if eq <= 0 {
			return verb, kvs, false
		}
		k := rest[:eq]
		rest = rest[eq+1:]
		var v string
		if strings.HasPrefix(rest, `"`) {
			// find closing quote honoring escapes
			q, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return verb, kvs, false
			}
			v, _ = strconv.Unquote(q)
			rest = rest[len(q):]
		} else {
			sp := strings.IndexByte(rest, ' ')
			if sp < 0 {
				v, rest = rest, ""
			} else {
				v, rest = rest[:sp], rest[sp:]
			}
		}
		kvs[k] = v
		rest = strings.TrimPrefix(rest, " ")
	}
	return verb, kvs, true
}

// UnitRef returns the public id of an unmanaged unit, "pg:<pgid>", for a
// unit key. Internal keys also carry the leader's start time
// ("pg:<pgid>:<startMS>") so that a reused pgid is a new unit; that suffix
// is not part of the contract and never appears in events or status.
func UnitRef(key string) string {
	if strings.HasPrefix(key, "pg:") {
		if i := strings.IndexByte(key[3:], ':'); i >= 0 {
			return key[:3+i]
		}
	}
	return key
}

// WithoutStreamFields returns e without the fields that belong to the job's
// own stream only (MemMaxBytes, Cgroup): what the global feed, alerts, job
// records and status files keep.
func (e Event) WithoutStreamFields() Event {
	e.MemMaxBytes, e.Cgroup = nil, ""
	return e
}
