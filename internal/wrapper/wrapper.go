// Package wrapper implements `pace run`: request admission, wait while
// queued, spawn the job in its own process group, relay governor messages
// on stderr, pass the job's exit code through, fail open when the daemon is
// unreachable, and re-attach after a daemon restart.
//
// Lifecycle of one invocation:
//
//  1. PACE_JOB_ID already set (nested call from inside a governed job, e.g.
//     through a PATH shim): exec the command directly, no governance.
//  2. Dial the daemon socket, retrying for Config.FailOpenWaitMS (default 2s).
//     Unreachable: fail open (failopen.go).
//  3. Submit and read the job's NDJSON event stream. QUEUED is printed at
//     once, again at once when the reason changes, and with the latest
//     position at most every QueuedReprint (10s) while only the position
//     changes; ADMITTED only if the job was queued; REFUSED prints its line,
//     a hint and the EXIT line and returns 200. ACCEPTED is never printed.
//  4. Spawn the child in its own process group with PACE_JOB_ID set
//     (spawn.go), POST started, heartbeat POST progress every second, relay
//     live events through the stderr mux (mux.go), forward INT/TERM/HUP/QUIT
//     to the child's group, re-attach if the daemon goes away.
//  5. On child exit: drain output, POST exit, wait briefly for a KILLED
//     attribution, then pick the exit code (see exitCode) and write the
//     status file.
package wrapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/client"
	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/envcompat"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
)

// EnvJobID is exported to every governed child. Its presence makes a nested
// `pace run` exec the command directly.
const EnvJobID = "PACE_JOB_ID"

// EnvStatusFile names the status file when Options.StatusFile is empty.
const EnvStatusFile = "PACE_STATUS_FILE"

// Options for one `pace run` invocation.
type Options struct {
	Config         config.Resolved
	Class          string
	EstimateBytes  int64
	BudgetBytes    int64
	Priority       *int
	WallLimitMS    int64
	NoWait         bool
	QueueTimeoutMS int64
	StatusFile     string // write final JSON status here (also $PACE_STATUS_FILE)
	Quiet          bool   // suppress non-essential lines (ADMITTED after QUEUED still printed)
	Argv           []string
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Env is the environment the command runs with (nil = os.Environ()).
	// PACE_JOB_ID and PACE_STATUS_FILE are looked up here.
	Env []string

	t timings // zero = production timings (tests shorten them)
	h hooks
}

// timings are the wrapper's clocks; zero fields take the defaults.
type timings struct {
	MuxHold       time.Duration // hold a pace line for a child newline (100ms)
	Progress      time.Duration // heartbeat interval (1s)
	Reattach      time.Duration // re-attach retry interval after daemon loss (1s)
	FailOpenPoll  time.Duration // poll daemon/slots while failing open (500ms)
	Drain         time.Duration // read leftover output after the child exits (1s)
	KillWait      time.Duration // wait for KILLED after an abnormal exit (200ms)
	KillWaitClean time.Duration // same after exit code 0 (25ms)
	PostTimeout   time.Duration // bound on started/progress/exit POSTs (1s)
	ResumeWait    time.Duration // wait for our own SIGCONT after a TTY stop (100ms)
	StartedRetry  time.Duration // first retry delay of a failed started POST (250ms, doubling to 2s)
	QueueGrace    time.Duration // client-side queue deadline past the queue timeout (5s)
	Liveness      time.Duration // daemon health probe interval while waiting for admission (5s)
	ProbeTimeout  time.Duration // bound on one such probe (10s: the daemon's own stall limit)
	// QueuedReprint is the least time between two QUEUED lines of the same
	// reason (10s): a position change in between is held, and only the
	// latest one is printed, so a long queue does not flood the caller.
	QueuedReprint time.Duration
}

// livenessFailures is how many health probes in a row must fail while
// queued before the daemon counts as lost.
const livenessFailures = 2

// queueTimeoutRetryAfterMS is the retry_after of the wrapper's own
// queue_timeout refusal: the daemon gave no estimate, so back off at least
// one liveness interval.
const queueTimeoutRetryAfterMS = 5000

func (t timings) withDefaults() timings {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&t.MuxHold, 100*time.Millisecond)
	def(&t.Progress, time.Second)
	def(&t.Reattach, time.Second)
	def(&t.FailOpenPoll, 500*time.Millisecond)
	def(&t.Drain, time.Second)
	def(&t.KillWait, 200*time.Millisecond)
	def(&t.KillWaitClean, 25*time.Millisecond)
	def(&t.PostTimeout, time.Second)
	def(&t.ResumeWait, 100*time.Millisecond)
	def(&t.StartedRetry, 250*time.Millisecond)
	def(&t.QueueGrace, 5*time.Second)
	def(&t.Liveness, 5*time.Second)
	def(&t.ProbeTimeout, 10*time.Second)
	def(&t.QueuedReprint, 10*time.Second)
	return t
}

// hooks replace process-global effects in tests.
type hooks struct {
	raise         func(sig syscall.Signal) // re-raise a child's fatal signal on ourselves
	quickPressure func() int               // platform.QuickPressure
	exec          func(path string, argv, env []string) error
	openTTY       func() *os.File // the controlling terminal (/dev/tty), nil if none
	// containers is platform.ContainersAt (the job container facility at
	// the daemon's base).
	containers func(base string) (platform.JobContainer, error)
	// waitReport runs in the reaper before a wait4 status of the child is
	// passed on (tests delay a stop's report with it).
	waitReport func(ws syscall.WaitStatus)
}

// Outcomes in the status file.
const (
	OutcomeAdmitted    = "admitted"
	OutcomeRefused     = "refused"
	OutcomeKilled      = "killed"
	OutcomeDegraded    = "degraded"    // ran without (full) governance: fail-open or daemon lost
	OutcomeError       = "error"       // governor error (exit 202)
	OutcomeInterrupted = "interrupted" // signalled while waiting, never ran
)

// Run executes the command and returns the process exit code to use.
// If the child died of a signal the governor did not send, Run re-raises
// it on the current process (and only returns if that fails).
func Run(o Options) int {
	r := newRunner(o)
	defer r.cleanup()
	return r.run()
}

type runner struct {
	o   Options
	t   timings
	env []string
	mux *mux

	path string // resolved argv[0]

	sigc         chan os.Signal
	contc        chan os.Signal
	tstpc        chan os.Signal // SIGTSTP, caught while the child does not own the terminal
	ttyWaitShown bool           // the INFO line about a job waiting for the terminal was printed
	sigSubs      []os.Signal
	frozen       bool // the daemon froze the job (FROZEN without THAWED since)

	// governed session
	c          *client.Client
	submit     proto.SubmitRequest
	submitMS   int64
	admitMS    int64
	id         string
	queued     bool
	lastQueued string // reason|position of the last QUEUED line printed
	refused    *proto.Event
	killed     *proto.Event
	everLost   bool
	lastEvTS   atomic.Int64 // ts of the last event any daemon stream delivered
	budget     int64        // the job's budget as the daemon resolved it (ACCEPTED)
	memMax     int64        // the container's hard limit from ADMITTED (0 = none)
	cgPath     string       // the job container path from ADMITTED ("" = the daemon runs none)
	cg         *jobCgroup   // the job's container (Linux cgroup v2), nil = none
	// memWarnUntil is the kill deadline (unix ms) the last enforce-mode
	// mem_budget WARN announced, 0 = none (or cleared by its OK).
	memWarnUntil atomic.Int64

	// QUEUED reprint limit (timings.QueuedReprint).
	lastQueuedReason string       // reason of the last QUEUED line printed
	queuedAt         time.Time    // when it was printed
	heldQueued       *proto.Event // a later position not printed yet

	failOpen     bool
	failOpenSlot string
	slot         *os.File

	evc    chan streamItem
	quit   chan struct{}
	gen    int
	stream *client.Stream
	postc  chan postReq

	outBytes  atomic.Int64 // relayed bytes
	lastOutMS atomic.Int64
	// A regular-file stdout the child owns directly is counted from its
	// file offset (see watchStdout).
	outFile      *os.File
	outBase      int64
	outFileBytes atomic.Int64
	// progressQueued is set while a heartbeat waits in the poster queue.
	progressQueued atomic.Bool

	rec statusRecord
}

type streamItem struct {
	gen int
	ev  proto.Event
	err error
}

type postReq struct {
	fn   func(ctx context.Context) error
	done chan error // nil = fire and forget
}

func newRunner(o Options) *runner {
	r := &runner{o: o, t: o.t.withDefaults()}
	if r.o.Stdin == nil {
		r.o.Stdin = os.Stdin
	}
	if r.o.Stdout == nil {
		r.o.Stdout = os.Stdout
	}
	if r.o.Stderr == nil {
		r.o.Stderr = os.Stderr
	}
	r.env = o.Env
	if r.env == nil {
		r.env = os.Environ()
	}
	if r.o.h.raise == nil {
		r.o.h.raise = raiseSignal
	}
	if r.o.h.quickPressure == nil {
		r.o.h.quickPressure = platform.QuickPressure
	}
	if r.o.h.exec == nil {
		r.o.h.exec = syscall.Exec
	}
	if r.o.h.openTTY == nil {
		r.o.h.openTTY = openCtty
	}
	if r.o.h.containers == nil {
		r.o.h.containers = platform.ContainersAt
	}
	// Until a child is spawned the mux writes straight through.
	r.mux = newMux(r.o.Stderr, r.t.MuxHold, true)
	r.evc = make(chan streamItem, 64)
	r.quit = make(chan struct{})
	r.rec = statusRecord{V: 1}
	return r
}

// envJobID is the PACE_JOB_ID given to the child: the daemon's job id, or
// failopen-<wrapper pid> for an ungoverned fail-open run (which no daemon
// knows; it only makes nested pace run calls pass straight through).
func (r *runner) envJobID() string {
	if r.id != "" {
		return r.id
	}
	return fmt.Sprintf("failopen-%d", os.Getpid())
}

func (r *runner) cleanup() {
	if r.stream != nil {
		r.stream.Close()
		r.stream = nil
	}
	close(r.quit)
	if r.postc != nil {
		close(r.postc)
		r.postc = nil
	}
	if r.sigc != nil {
		signal.Stop(r.sigc)
	}
	if r.contc != nil {
		signal.Stop(r.contc)
	}
	if r.tstpc != nil {
		signal.Stop(r.tstpc)
	}
	r.releaseSlot()
	if r.c != nil {
		r.c.CloseIdle()
	}
}

func (r *runner) run() int {
	if len(r.o.Argv) == 0 || r.o.Argv[0] == "" {
		return r.paceError("no command given")
	}
	path, err := lookPath(r.o.Argv[0], getenv(r.env, "PATH"))
	if err != nil {
		code := r.execFailed(err)
		r.writeStatus(OutcomeError, code, "child")
		return code
	}
	r.path = path
	if getenv(r.env, EnvJobID) != "" {
		return r.execNested()
	}
	r.setupSignals()
	r.submit = proto.SubmitRequest{
		Class:          r.o.Class,
		EstimateBytes:  r.o.EstimateBytes,
		BudgetBytes:    r.o.BudgetBytes,
		Priority:       r.o.Priority,
		WallLimitMS:    r.o.WallLimitMS,
		NoWait:         r.o.NoWait,
		QueueTimeoutMS: r.o.QueueTimeoutMS,
		Argv:           r.o.Argv,
		WrapperPID:     os.Getpid(),
	}
	if wd, err := os.Getwd(); err == nil {
		r.submit.Cwd = wd
	}
	deadline := time.Now().Add(r.failOpenWait())
	c, sig, err := r.dial()
	if sig != 0 {
		return r.interrupted(sig)
	}
	if err != nil {
		return r.runFailOpen(err)
	}
	return r.governed(c, deadline)
}

// dial connects within the fail-open wait; a forwarded signal arriving
// meanwhile aborts the wait at once (Ctrl-C must not take 2s).
func (r *runner) dial() (*client.Client, syscall.Signal, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got os.Signal
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case got = <-r.sigc:
			cancel()
		case <-done:
		}
	}()
	c, err := client.Dial(ctx, r.o.Config.Socket, r.failOpenWait())
	close(done)
	<-stopped
	if s, ok := got.(syscall.Signal); ok {
		if c != nil {
			c.CloseIdle()
		}
		return nil, s, err
	}
	return c, 0, err
}

// interrupted ends a run that was signalled before the command started.
func (r *runner) interrupted(s syscall.Signal) int {
	r.closeStream()
	r.rec.Signal = int(s)
	r.writeStatus(OutcomeInterrupted, 128+int(s), "pace")
	return r.reraise(s)
}

// retryable reports whether a submit error means "daemon not usable": no
// answer at all, or a server-side failure (5xx). 4xx is the request's fault.
func retryable(err error) bool {
	var ae *client.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return client.IsUnreachable(err)
}

func (r *runner) failOpenWait() time.Duration {
	if ms := r.o.Config.FailOpenWaitMS; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 2 * time.Second
}

func (r *runner) queueTimeout() time.Duration {
	if ms := r.o.QueueTimeoutMS; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	if ms := r.o.Config.Params.DefaultQueueTimeoutMS; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return 30 * time.Minute
}

// setupSignals subscribes to the signals we forward (unless they were
// ignored when we started: nohup must keep working for the child) and to
// SIGPIPE so that a closed stdout turns into EPIPE instead of killing us.
func (r *runner) setupSignals() {
	r.sigc = make(chan os.Signal, 8)
	for _, s := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT} {
		if !signal.Ignored(s) {
			r.sigSubs = append(r.sigSubs, s)
		}
	}
	if len(r.sigSubs) > 0 {
		signal.Notify(r.sigc, r.sigSubs...)
	}
	if !signal.Ignored(syscall.SIGPIPE) {
		signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
	}
}

// --- governed path ---------------------------------------------------------

func (r *runner) governed(c *client.Client, deadline time.Time) int {
	r.c = c
	for {
		st, err := r.submitWithRetry(deadline)
		if err != nil {
			if retryable(err) {
				return r.runFailOpen(err)
			}
			return r.paceError("daemon rejected the request: " + err.Error())
		}
		r.startStream(st)
		res, code := r.waitAdmission()
		switch res {
		case admRefused, admSignaled:
			return code
		case admAdmitted:
			return r.runChild(true)
		case admLost:
			if !r.everLost {
				r.everLost = true
				r.emit(proto.Event{Verb: proto.VerbDegraded, Reason: proto.ReasonDaemonLost, Job: r.id,
					Msg: "daemon lost while queued; resubmitting"})
			}
			r.closeStream()
			r.id, r.queued, r.lastQueued, r.lastQueuedReason, r.heldQueued = "", false, "", "", nil
			deadline = time.Now().Add(r.failOpenWait())
			nc, sig, err := r.dial()
			if sig != 0 {
				return r.interrupted(sig)
			}
			if err != nil {
				return r.runFailOpen(err)
			}
			r.c.CloseIdle()
			r.c = nc
		}
	}
}

func (r *runner) submitWithRetry(deadline time.Time) (*client.Stream, error) {
	backoff := 10 * time.Millisecond
	for {
		if r.submitMS == 0 {
			r.submitMS = time.Now().UnixMilli()
		}
		st, err := r.c.Submit(context.Background(), r.submit)
		if err == nil || !retryable(err) {
			return st, err
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			return nil, err
		}
		time.Sleep(min(backoff, rem))
		backoff = min(2*backoff, 250*time.Millisecond)
	}
}

func (r *runner) startStream(st *client.Stream) {
	r.closeStream()
	r.gen++
	r.stream = st
	go r.readStream(r.gen, st)
}

// closeStream closes the current stream. Its reader's last items (the
// close error included) become stale: they must not read as a daemon loss.
func (r *runner) closeStream() {
	if r.stream != nil {
		r.stream.Close()
		r.stream = nil
		r.gen++
	}
}

func (r *runner) readStream(gen int, st *client.Stream) {
	for {
		ev, err := st.Next()
		var de *client.DecodeError
		if errors.As(err, &de) {
			continue
		}
		if err == nil && ev.TS > r.lastEvTS.Load() {
			r.lastEvTS.Store(ev.TS)
		}
		select {
		case r.evc <- streamItem{gen: gen, ev: ev, err: err}:
		case <-r.quit:
			return
		}
		if err != nil {
			return
		}
	}
}

type admission int

const (
	admAdmitted admission = iota
	admRefused
	admLost
	admSignaled
)

// waitAdmission consumes the stream until ADMITTED or REFUSED.
//
// It does not trust the daemon to end the wait: past the queue timeout
// (from the first submit) plus QueueGrace it refuses on its own
// (reason=queue_timeout), and a daemon whose health check fails
// livenessFailures times in a row (unreachable, 5xx "loop stalled", or no
// answer within ProbeTimeout) is treated as lost, so a wedged daemon that keeps the stream open cannot
// block the caller forever.
func (r *runner) waitAdmission() (admission, int) {
	start := time.UnixMilli(r.submitMS)
	if r.submitMS == 0 {
		start = time.Now()
	}
	deadline := time.NewTimer(time.Until(start.Add(r.queueTimeout() + r.t.QueueGrace)))
	defer deadline.Stop()
	probeT := time.NewTicker(r.t.Liveness)
	defer probeT.Stop()
	probec := make(chan error, 1)
	probing, fails := false, 0
	// held fires when a held QUEUED position is due (nil channel: none).
	var held *time.Timer
	var heldC <-chan time.Time
	defer func() {
		if held != nil {
			held.Stop()
		}
		r.heldQueued = nil // ADMITTED waited= (or REFUSED) supersedes it
	}()
	// printNow prints a QUEUED line at once. The held timer belongs to the
	// line printed before it: stop it, so that a position change after this
	// line waits the whole interval from this line, not from that one.
	printNow := func(ev proto.Event) {
		if held != nil {
			held.Stop()
		}
		heldC = nil
		r.printQueued(ev)
	}
	for {
		select {
		case <-heldC:
			heldC = nil
			if ev := r.heldQueued; ev != nil {
				r.printQueued(*ev)
			}
		case <-deadline.C:
			w := time.Since(start).Milliseconds()
			ev := proto.Event{Verb: proto.VerbRefused, Reason: proto.ReasonQueueTimeout, Job: r.id, WaitedMS: &w,
				RetryAfterMS: proto.I64(queueTimeoutRetryAfterMS),
				Msg:          "the daemon did not decide within the queue timeout"}
			r.refused = &ev
			r.closeStream() // dequeues us
			r.emit(ev)
			return admRefused, r.finishPace(proto.ExitRefused, OutcomeRefused)
		case <-probeT.C:
			if !probing {
				probing = true
				c := r.c
				go func() {
					// Bounded like the daemon's own stall signal (/healthz 503
					// after 10s without a loop step), not like a POST: a daemon
					// slow to answer under memory pressure is not lost, and
					// treating it so would requeue us at the back.
					probec <- c.Probe(context.Background(), r.t.ProbeTimeout)
				}()
			}
		case err := <-probec:
			probing = false
			if !client.IsDown(err) {
				fails = 0
				continue
			}
			if fails++; fails >= livenessFailures {
				return admLost, 0
			}
		case it := <-r.evc:
			if it.gen != r.gen {
				continue
			}
			if it.err != nil {
				return admLost, 0
			}
			ev := it.ev
			switch ev.Verb {
			case proto.VerbAccepted:
				if ev.Job != "" {
					r.id = ev.Job
				}
				if ev.BudgetBytes != nil {
					r.budget = *ev.BudgetBytes
				}
			case proto.VerbQueued:
				if ev.Job == "" {
					ev.Job = r.id
				}
				switch {
				case !r.queued || ev.Reason != r.lastQueuedReason:
					// The first line, or why it waits changed: at once.
					printNow(ev)
				case queuedKey(ev) == r.lastQueued:
					r.heldQueued = nil // back where the last line said
				case time.Since(r.queuedAt) >= r.t.QueuedReprint:
					printNow(ev)
				default:
					// Only the position moved, soon after the last line:
					// hold the latest one until the interval is over.
					r.heldQueued = &ev
					if heldC == nil {
						d := r.t.QueuedReprint - time.Since(r.queuedAt)
						if held == nil {
							held = time.NewTimer(d)
						} else {
							held.Reset(d)
						}
						heldC = held.C
					}
				}
			case proto.VerbAdmitted:
				if ev.Job != "" {
					r.id = ev.Job
				}
				if ev.MemMaxBytes != nil && *ev.MemMaxBytes > 0 {
					r.memMax = *ev.MemMaxBytes
				}
				r.cgPath = ev.Cgroup
				ev = ev.WithoutStreamFields() // never printed nor recorded
				r.admitMS = time.Now().UnixMilli()
				if r.queued {
					if ev.Job == "" {
						ev.Job = r.id
					}
					r.emit(ev)
				}
				return admAdmitted, 0
			case proto.VerbRefused:
				if ev.Job == "" {
					ev.Job = r.id
				}
				r.refused = &ev
				r.emit(ev)
				r.closeStream()
				return admRefused, r.finishPace(proto.ExitRefused, OutcomeRefused)
			case proto.VerbExit:
			default:
				r.emit(ev)
			}
		case sig := <-r.sigc:
			// Interrupted while waiting: closing the stream dequeues us.
			s, _ := sig.(syscall.Signal)
			return admSignaled, r.interrupted(s)
		}
	}
}

// queuedKey identifies what a QUEUED line says: reason|position.
func queuedKey(ev proto.Event) string {
	key := ev.Reason + "|"
	if ev.Position != nil {
		key += strconv.Itoa(*ev.Position)
	}
	return key
}

// printQueued prints a QUEUED line now; anything held is superseded.
func (r *runner) printQueued(ev proto.Event) {
	r.heldQueued = nil
	r.emit(ev)
	r.queued, r.lastQueued, r.lastQueuedReason = true, queuedKey(ev), ev.Reason
	r.queuedAt = time.Now()
}

// emit prints one governor event (adding a default hint where the contract
// requires one) and records it for the status file.
func (r *runner) emit(ev proto.Event) {
	r.emitExtra(ev, "")
}

func (r *runner) emitExtra(ev proto.Event, extra string) {
	r.print(r.record(ev), extra)
}

// record completes ev (version, time, default hint) and keeps it for the
// status file.
func (r *runner) record(ev proto.Event) proto.Event {
	if ev.V == 0 {
		ev.V = 1
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	if ev.Hint == "" {
		ev.Hint = defaultHint(ev)
	}
	if ev.Policy == proto.PolicyMemBudget {
		switch {
		case ev.Verb == proto.VerbWarn && ev.Next == "kill" && ev.Mode == "" && ev.InMS != nil:
			r.memWarnUntil.Store(ev.TS + *ev.InMS)
		case ev.Verb == proto.VerbOK:
			r.memWarnUntil.Store(0)
		}
	}
	r.rec.addEvent(ev)
	return ev
}

// print writes ev's stderr line(s); extra is appended to the first line.
func (r *runner) print(ev proto.Event, extra string) {
	if r.o.Quiet && ev.Verb == proto.VerbInfo {
		return
	}
	lines := ev.Lines()
	if extra != "" {
		lines[0] += " " + extra
	}
	r.mux.Pace(lines...)
}

// exitLine ends the run with an EXIT event: it is recorded, the status file
// is written (whose failure prints an INFO line), and only then the EXIT
// line is printed, so that it is always the last stderr line.
func (r *runner) exitLine(ev proto.Event, outcome string, code int, source string) {
	ev = r.record(ev)
	r.writeStatus(outcome, code, source)
	r.print(ev, "")
}

// finishPace prints the EXIT line for a governor-produced code and writes the
// status file.
func (r *runner) finishPace(code int, outcome string) int {
	r.exitLine(proto.Event{Verb: proto.VerbExit, Code: proto.Int(code), Source: "pace", Job: r.id}, outcome, code, "pace")
	return code
}

// paceError reports a governor usage/configuration error (exit 202).
func (r *runner) paceError(msg string) int {
	r.exitLine(proto.Event{Verb: proto.VerbExit, Code: proto.Int(proto.ExitPaceError), Source: "pace", Job: r.id, Msg: msg},
		OutcomeError, proto.ExitPaceError, "pace")
	return proto.ExitPaceError
}

// execFailed reports a command that cannot be executed, shell style
// (127 not found, 126 otherwise). This is not a governor line.
func (r *runner) execFailed(err error) int {
	code := 126
	if errors.Is(err, errNotFound) || errors.Is(err, os.ErrNotExist) {
		code = 127
	}
	name := ""
	if len(r.o.Argv) > 0 {
		name = r.o.Argv[0]
	}
	msg := err.Error()
	if errors.Is(err, errNotFound) {
		msg = "command not found"
	}
	r.mux.Pace(fmt.Sprintf("pace run: %s: %s", name, msg))
	r.rec.Error = err.Error()
	return code
}

// execNested replaces this process with the command: we are already inside
// a governed job, whose process group the daemon accounts for.
func (r *runner) execNested() int {
	err := r.o.h.exec(r.path, r.o.Argv, r.env)
	return r.execFailed(err)
}

// --- exit-code selection ---------------------------------------------------

// exitCode applies the contract to how the child ended. sig != 0 means the
// child died of a signal the governor did not send and must be re-raised.
func exitCode(ws syscall.WaitStatus, killedByPace bool) (code int, source string, sig syscall.Signal) {
	switch {
	case killedByPace:
		return proto.ExitKilled, "pace", 0
	case ws.Exited():
		return ws.ExitStatus(), "child", 0
	case ws.Signaled():
		return 128 + int(ws.Signal()), "child", ws.Signal()
	}
	return proto.ExitPaceError, "pace", 0
}

func (r *runner) reraise(sig syscall.Signal) int {
	if sig <= 0 {
		return 128
	}
	r.mux.Close()
	r.o.h.raise(sig)
	return 128 + int(sig)
}

// --- helpers ---------------------------------------------------------------

// getenv looks key up in env; PACE_* keys fall back to their old GOV_* names.
func getenv(env []string, key string) string { return envcompat.Lookup(env, key) }

// setenv returns env with key=val, replacing every existing key entry.
func setenv(env []string, key, val string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if k, _, _ := strings.Cut(kv, "="); k == key {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+val)
}

func defaultHint(ev proto.Event) string {
	switch ev.Verb {
	case proto.VerbRefused:
		retry := "retry later"
		if ev.RetryAfterMS != nil && *ev.RetryAfterMS > 0 {
			retry = "retry in " + units.FormatMS(*ev.RetryAfterMS)
		}
		switch ev.Reason {
		case proto.ReasonTooBig:
			return "the estimate exceeds what this machine can ever give: lower parallelism or the declared estimate"
		case proto.ReasonQueueTimeout:
			return "the machine stayed busy for the whole queue timeout: " + retry + " or raise the queue timeout"
		case proto.ReasonDisk:
			return "free disk space, then " + retry
		case proto.ReasonFailOpenFull:
			return "the governor daemon is down and the fail-open slots are taken: start it (pace install, or pace daemon) and retry"
		case proto.ReasonHeadroom, proto.ReasonPressure, proto.ReasonSlots:
			return retry + ", or run with lower parallelism"
		}
		return retry
	case proto.VerbKilled:
		switch ev.Policy {
		case proto.PolicyMemBudget:
			return "retry with lower parallelism or request a larger budget"
		case proto.PolicyPressure:
			return "the machine ran out of memory: retry later or with lower parallelism"
		case proto.PolicyStuck:
			return "the job made no progress: check it for a hang, or raise the class no-progress limit"
		case proto.PolicyWallClock:
			return "the job exceeded its wall-clock limit: raise the limit or split the work"
		case proto.PolicyOrphaned:
			return "the job lost its wrapper: keep `pace run` attached until the job ends"
		case proto.PolicyDisk:
			return "free disk space, then retry"
		}
		return "retry later"
	}
	return ""
}
