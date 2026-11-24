// Package daemon runs the governor: socket server, sampler loop, policy
// step, actuator, decision log, job table persistence and event fan-out.
//
// Concurrency model: one loop goroutine owns policy.State, the sampler, the
// tracker, the decision log sequence and the actuator. HTTP handlers never
// touch policy state; they push InEvents through a bounded channel and read
// what the loop publishes (an atomic status snapshot, the job registry and
// the event feed, both behind small mutexes). Nothing a client does can
// block the loop: every buffer is bounded and slow readers lose events and
// see a gap marker instead.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/envcompat"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// StepFunc is the policy step (policy.Step unless a test injects one).
type StepFunc func(policy.Params, policy.State, policy.Inputs) (policy.State, policy.Output)

// Tracker attributes processes to jobs and units (*attrib.Tracker).
type Tracker interface {
	SetOptions(attrib.Options)
	SetProtectedPIDs([]int)
	Attribute(procs []platform.Proc, jobs []policy.Job, tracked []policy.Unit) attrib.Result
}

// LogWriter receives decision log records (*dlog.Writer). Write must not block.
type LogWriter interface {
	Write(dlog.Record)
	Close() error
}

// LoadConfigFunc re-reads the configuration (config.LoadResolved).
type LoadConfigFunc func(path string, totalBytes int64, platform string) (config.Resolved, error)

// Options configure a daemon instance. Only Config is required; every hook
// has a production default.
type Options struct {
	Config  config.Resolved
	Backend platform.Backend // nil = platform.New()
	UID     int
	Version string
	// ConfigPath is re-read on SIGHUP / POST /v1/reload ("" = default location).
	ConfigPath string

	// Step is the policy step; nil = policy.Step.
	Step StepFunc
	// Assess computes the status figures; nil = DefaultAssess.
	Assess AssessFunc
	// Now returns unix ms; nil = wall clock. The loop clamps it to be monotonic.
	Now func() int64
	// Tracker attributes processes; nil = attrib.New.
	Tracker Tracker
	// Log is the decision log; nil = dlog.Open(<log dir>).
	Log LogWriter
	// LoadConfig is used by reload; nil = config.LoadResolved.
	LoadConfig LoadConfigFunc
	// Notify shows a desktop notification (only called when
	// Config.NotifyMacOS); nil = osascript on darwin, nothing elsewhere.
	Notify func(title, msg string)

	// Signals delivers SIGHUP (reload) and SIGTERM/SIGINT (shutdown). nil =
	// Run subscribes to the real signals unless NoSignals is set.
	Signals   <-chan os.Signal
	NoSignals bool
	// NoSelfTune skips debug.SetMemoryLimit/SetGCPercent (tests).
	NoSelfTune bool
	// Ready is called once the socket accepts connections.
	Ready func(socketPath string)
}

// Tunables. Bounded buffers everywhere: these are the bounds.
const (
	evChanSize       = 1024    // wrapper events waiting for the loop
	maxPending       = 8192    // events drained but not yet stepped
	jobRingSize      = 100     // events kept per job
	feedRingSize     = 256     // per-follower backlog (global feed)
	recentEvents     = 200     // GET /v1/events without follow
	maxFinishedJobs  = 1000    // finished jobs kept in memory
	maxFinishedBytes = 8 << 20 // and at most about this much of them (recBytes)
	finishedRetainMS = 24 * 3600 * 1000
	persistFinished  = 100  // finished jobs written to state.json
	persistMaxEvents = 5000 // total job events written to state.json
	minResampleMS    = 25   // event-triggered steps reuse a sample younger than this
	persistDebounce  = time.Second
	persistTelemetry = 30 * time.Second // telemetry-only changes persisted this often
	pushTimeout      = 5 * time.Second
	writeTimeout     = 10 * time.Second // per streamed event
	topUnmanaged     = 20
	maxRecentJobs    = 20
	notifyEvery      = 10 * time.Second
	maxSocketPath    = 103
	maxBodyBytes     = 1 << 20
	alertMaxBytes    = 16 << 20
	faultWindowMS    = 10 * 60 * 1000
)

var (
	errStopping = errors.New("daemon is shutting down")
	errBusy     = errors.New("daemon busy: event queue full")
)

// Run serves until ctx is cancelled (or SIGTERM). It returns an error if
// the socket cannot be bound (e.g. another daemon is running). Jobs are
// never killed on shutdown: wrappers re-attach to the next daemon. Every
// throttle and freeze is released on shutdown, so nothing stays stopped or
// deprioritised when no daemon comes back (uninstall, logout).
func Run(ctx context.Context, o Options) error {
	d, err := newDaemon(o)
	if err != nil {
		return err
	}
	return d.run(ctx)
}

type daemon struct {
	o        Options
	backend  platform.Backend
	step     StepFunc
	assess   AssessFunc
	nowFn    func() int64
	tracker  Tracker
	log      LogWriter
	loadCfg  LoadConfigFunc
	selfPID  int
	selfPGID int
	selfExes []string // this binary's path (and its resolved form): pace's own clients run it too
	allowUID int      // peer uid allowed on the socket

	home, sockPath, logDir, statePath, alertsPath string

	evCh     chan policy.InEvent
	reloadCh chan reloadReq
	stopCh   chan struct{} // closed when shutting down
	stopping atomic.Bool
	loopDone chan struct{}

	reg      *registry
	feed     *feed
	alerts   *alertWriter
	persist  *persister
	metrics  *metrics
	faults   *faultSet
	snap     atomic.Pointer[snapshot]
	idSeq    atomic.Int64
	curIvMS  atomic.Int64
	lastStep atomic.Int64 // wall unix ms of the last step (healthz)
	// Wall unix ms of the last successful SampleMachine / ListProcs (healthz).
	lastMachOK  atomic.Int64
	lastProcsOK atomic.Int64
	notifyIn    atomic.Bool
	noteDrops   noteDropLimiter // stderr_note faults (any goroutine)

	ln        net.Listener
	lockFiles []*os.File

	// Loop-owned below (and startup/shutdown before/after the loop runs).
	cfg           config.Resolved
	st            policy.State // never mutated in place: Step gets a clone
	pending       []policy.InEvent
	lastMach      platform.Machine
	haveMach      bool
	lastAttr      attrib.Result
	lastProcs     []platform.Proc
	procIdx       map[int]int         // pid -> index in lastProcs
	owner         map[int]string      // pid -> target key ("j:<id>", "u:<unit key>")
	owned         map[string][]int    // target key -> member pids
	held          map[string]*heldRec // what the actuator throttled/stopped, per target key
	sampleStale   bool                // the last SampleMachine or ListProcs failed
	procsFresh    bool                // the last ListProcs succeeded (lastProcs is current)
	lastSampleAt  time.Time
	lastNowMS     int64
	lastMaterial  uint64
	logSeq        int64
	lastLogAt     time.Time
	lastPersistAt time.Time
	dirty         bool // material change not yet persisted
	telemetry     bool // telemetry change not yet persisted
	lastPrune     time.Time
	lastNotify    time.Time
	lastWarn      map[string]proto.Event
	recentAlerts  []proto.Event // loop goroutine only; see noteAlert
	actProt       []int         // wrapper pids, refreshed before actions run
	// Job containers (container.go): the backend's facility (nil = none),
	// why there is none, and the adopted container per job id.
	cont        platform.JobContainer
	contReason  string
	contWeights string // "cpu.weight+io.weight", "cpu.weight", ... or ""
	cg          map[string]*cgJob

	pushTimeout time.Duration // handler wait for room in the event queue
}

func newDaemon(o Options) (*daemon, error) {
	d := &daemon{
		o:        o,
		backend:  o.Backend,
		step:     o.Step,
		assess:   o.Assess,
		nowFn:    o.Now,
		tracker:  o.Tracker,
		log:      o.Log,
		loadCfg:  o.LoadConfig,
		selfPID:  os.Getpid(),
		selfPGID: syscall.Getpgrp(),
		selfExes: selfExecutables(),
		allowUID: os.Geteuid(),
		evCh:     make(chan policy.InEvent, evChanSize),
		reloadCh: make(chan reloadReq),
		stopCh:   make(chan struct{}),
		loopDone: make(chan struct{}),
		reg:      newRegistry(),
		feed:     newFeed(feedRingSize),
		metrics:  newMetrics(),
		faults:   newFaultSet(),
		lastWarn: map[string]proto.Event{},
		held:     map[string]*heldRec{},

		pushTimeout: pushTimeout,
	}
	t0 := time.Now().UnixMilli()
	d.lastStep.Store(t0)
	d.lastMachOK.Store(t0)
	d.lastProcsOK.Store(t0)
	if d.step == nil {
		d.step = policy.Step
	}
	if d.assess == nil {
		d.assess = DefaultAssess
	}
	if d.nowFn == nil {
		d.nowFn = func() int64 { return time.Now().UnixMilli() }
	}
	if d.loadCfg == nil {
		d.loadCfg = config.LoadResolved
	}
	if d.backend == nil {
		b, err := platform.New()
		if err != nil {
			return nil, err
		}
		d.backend = b
	}
	d.cfg = normalizeConfig(o.Config)
	if d.tracker == nil {
		d.tracker = attrib.New(d.attribOptions())
	}
	// PACE_JOB_ID tags (attribution rule 4) when the backend can read them.
	if tr, ok := d.tracker.(interface{ SetTagReader(platform.TagReader) }); ok {
		if r, ok := d.backend.(platform.TagReader); ok {
			tr.SetTagReader(r)
		}
	}

	d.home = d.cfg.Home
	if d.home == "" {
		d.home = defaultHome()
	}
	if d.home == "" {
		return nil, errors.New("pace daemon: no home directory (set PACE_HOME)")
	}
	d.sockPath = d.cfg.Socket
	if d.sockPath == "" {
		d.sockPath = filepath.Join(d.home, "run", "pace.sock")
	}
	d.initContainers() // scoped by the socket: one daemon per socket
	d.logDir = d.cfg.LogDir
	if d.logDir == "" {
		d.logDir = filepath.Join(d.home, "logs")
	}
	d.statePath = filepath.Join(d.home, "state.json")
	d.alertsPath = filepath.Join(d.logDir, "alerts.jsonl")
	d.curIvMS.Store(d.cfg.SampleMS)
	return d, nil
}

// normalizeConfig fills zero values so a partially resolved config (or the
// config stub) still yields a working daemon.
func normalizeConfig(c config.Resolved) config.Resolved {
	if c.SampleMS <= 0 {
		c.SampleMS = 1000
	}
	if c.FastMS <= 0 || c.FastMS > c.SampleMS {
		c.FastMS = min(250, c.SampleMS)
	}
	if c.HeartbeatMS <= 0 {
		c.HeartbeatMS = 60000
	}
	if c.LogMaxBytes <= 0 {
		c.LogMaxBytes = 64 << 20
	}
	if c.LogKeep <= 0 {
		c.LogKeep = 8
	}
	if c.Params.Mode == "" {
		c.Params.Mode = policy.ModeEnforce
	}
	return c
}

func defaultHome() string {
	if h := envcompat.Getenv("PACE_HOME"); h != "" {
		return h
	}
	uh, err := os.UserHomeDir()
	if err != nil || uh == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(uh, "Library", "Application Support", "pace")
	default:
		if x := os.Getenv("XDG_STATE_HOME"); x != "" {
			return filepath.Join(x, "pace")
		}
		return filepath.Join(uh, ".local", "state", "pace")
	}
}

func (d *daemon) attribOptions() attrib.Options {
	return attrib.Options{
		SelfPIDs:     []int{d.selfPID},
		SelfPGIDs:    []int{d.selfPGID},
		ScopeRootPID: d.cfg.ScopeRootPID,
		ProtectNames: d.cfg.ProtectNames,
		MinUnitBytes: d.cfg.Params.MinVictimBytes,
		MaxUnits:     64,
		ClientExes:   d.selfExes,
		ClientNames:  clientNames(d.selfExes),
	}
}

// TrayName is the process name of the menu bar app (tray/macos), one of
// pace's own clients: its process group is never a small-unit victim.
const TrayName = "PaceBar"

// clientNames are the names pace's own clients run under: the tray, and this
// binary's file name, so `pace status` from an upgraded install at another
// path is still recognised while an older daemon runs.
func clientNames(selfExes []string) []string {
	names := []string{TrayName}
	seen := map[string]bool{TrayName: true}
	for _, e := range selfExes {
		if b := filepath.Base(e); b != "" && b != "." && b != "/" && !seen[b] {
			seen[b] = true
			names = append(names, b)
		}
	}
	return names
}

// selfExecutables is the path of this binary, and its resolved form when
// that differs. pace's clients (`pace status`, `pace watch`) run the same
// binary: attribution never offers their small groups as victims.
func selfExecutables() []string {
	p, err := os.Executable()
	if err != nil || p == "" {
		return nil
	}
	out := []string{p}
	if r, err := filepath.EvalSymlinks(p); err == nil && r != p {
		out = append(out, r)
	}
	return out
}

func (d *daemon) now() int64 { return d.nowFn() }

// selfTune keeps the daemon small: it must stay responsive under the very
// memory pressure it governs.
func selfTune() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(32 << 20)
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
}

func (d *daemon) run(ctx context.Context) error {
	if !d.o.NoSelfTune {
		selfTune()
	}
	if err := os.MkdirAll(d.home, 0o700); err != nil {
		return fmt.Errorf("pace daemon: home: %w", err)
	}
	ln, err := d.listen()
	if err != nil {
		return err
	}
	d.ln = ln
	defer d.unlock()

	if err := os.MkdirAll(d.logDir, 0o700); err != nil {
		d.fault("log_dir", err.Error())
	}
	if d.log == nil {
		w, err := dlog.Open(d.logDir, dlog.Options{MaxBytes: d.cfg.LogMaxBytes, Keep: d.cfg.LogKeep})
		if err != nil {
			// The governor runs without its log rather than not at all.
			d.fault("decision_log", err.Error())
			d.log = nopLog{}
		} else {
			d.log = w
		}
	}
	d.alerts = newAlertWriter(d.alertsPath, alertMaxBytes, d.fault)
	d.persist = newPersister(d.statePath, d.fault)

	t0 := time.Now().UnixMilli()
	d.lastStep.Store(t0)
	d.lastMachOK.Store(t0)
	d.lastProcsOK.Store(t0)
	// The decision log knows the last seq it holds even when state.json
	// lags (debounced) or is lost: seqs must never repeat or go backwards.
	if ls, ok := d.log.(interface{ LastSeq() int64 }); ok {
		d.logSeq = max(d.logSeq, ls.LastSeq())
	}
	d.loadState()
	d.restoreContainers()
	d.logStart()
	d.publishBootSnapshot()

	loopCtx, cancelLoop := context.WithCancel(context.Background())
	go d.loop(loopCtx)

	srv := &http.Server{
		Handler:           d.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ConnContext:       connContext,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	sigs := d.o.Signals
	if sigs == nil && !d.o.NoSignals {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
		defer signal.Stop(ch)
		sigs = ch
	}
	if d.o.Ready != nil {
		d.o.Ready(d.sockPath)
	}

	var runErr error
wait:
	for {
		select {
		case <-ctx.Done():
			break wait
		case err := <-serveErr:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				runErr = fmt.Errorf("pace daemon: serve: %w", err)
			}
			break wait
		case s := <-sigs:
			if s == syscall.SIGHUP {
				go func() { _ = d.reload() }()
				continue
			}
			break wait
		}
	}

	// Shutdown: stop streams (no EvLost: wrappers re-attach), stop the loop
	// (it persists state on exit), then the server. Jobs keep running.
	d.stopping.Store(true)
	close(d.stopCh)
	cancelLoop()
	<-d.loopDone
	shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = srv.Shutdown(shCtx)
	cancel()
	_ = srv.Close()
	d.persist.close()
	d.alerts.close()
	if err := d.log.Close(); err != nil && runErr == nil {
		runErr = fmt.Errorf("pace daemon: close log: %w", err)
	}
	return runErr
}

type nopLog struct{}

func (nopLog) Write(dlog.Record) {}
func (nopLog) Close() error      { return nil }

// fault records a daemon fault (shown in status, counted in metrics).
// Safe from any goroutine.
func (d *daemon) fault(kind, msg string) {
	d.faults.record(kind, msg, time.Now().UnixMilli())
}

// push hands one wrapper event to the loop.
func (d *daemon) push(ctx context.Context, ev policy.InEvent) error {
	if d.stopping.Load() {
		return errStopping
	}
	t := time.NewTimer(d.pushTimeout)
	defer t.Stop()
	select {
	case d.evCh <- ev:
		return nil
	case <-d.stopCh:
		return errStopping
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		d.fault("event_queue", "event queue full for "+d.pushTimeout.String())
		return errBusy
	}
}

type reloadReq struct {
	cfg  config.Resolved
	err  error
	done chan struct{}
	hook func(*daemon) // tests: run on the loop goroutine instead of reloading
}

// reload re-reads the config off the loop and asks the loop to apply it.
// On error the old config stays and the failure is logged.
func (d *daemon) reload() error {
	total := int64(0)
	if s := d.snap.Load(); s != nil {
		total = s.totalBytes
	}
	r, err := d.loadCfg(d.o.ConfigPath, total, runtime.GOOS)
	req := reloadReq{cfg: r, err: err, done: make(chan struct{})}
	select {
	case d.reloadCh <- req:
	case <-d.stopCh:
		return errStopping
	}
	select {
	case <-req.done:
	case <-d.stopCh:
		return errStopping
	}
	return err
}

var (
	_ Tracker   = (*attrib.Tracker)(nil)
	_ LogWriter = (*dlog.Writer)(nil)
)
