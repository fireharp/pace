package daemon

import (
	"sort"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// ring is a fixed-size event buffer addressed by a monotonically increasing
// sequence number: event seq s lives at s % size while it is retained.
type ring struct {
	buf  []proto.Event
	size int
	next int64 // seq of the next event appended
}

func newRing(size int) ring { return ring{size: size} }

func (r *ring) add(e proto.Event) {
	if len(r.buf) < r.size {
		r.buf = append(r.buf, e)
	} else {
		r.buf[int(r.next%int64(r.size))] = e
	}
	r.next++
}

func (r *ring) oldest() int64 { return r.next - int64(len(r.buf)) }

// since appends events with seq >= from to dst; dropped counts events that
// were overwritten before the reader got to them.
func (r *ring) since(from int64, dst []proto.Event) (out []proto.Event, dropped, next int64) {
	if old := r.oldest(); from < old {
		dropped, from = old-from, old
	}
	for s := from; s < r.next; s++ {
		dst = append(dst, r.buf[int(s%int64(r.size))])
	}
	return dst, dropped, r.next
}

// last returns up to n most recent events, oldest first.
func (r *ring) last(n int) []proto.Event {
	from := r.next - int64(n)
	if from < r.oldest() {
		from = r.oldest()
	}
	out, _, _ := r.since(from, make([]proto.Event, 0, r.next-from))
	return out
}

// jobRec is the daemon's view of one job: what the wrapper told us, its
// event history and its (at most one) live stream. Guarded by registry.mu.
type jobRec struct {
	id     string
	submit proto.SubmitRequest
	events ring

	// stream: gen increments when a new stream (submit or attach) takes over.
	gen       int64
	notify    chan struct{} // cap 1, poked when an event is appended
	done      chan struct{} // closed when the current stream must end
	streaming bool

	inLoop       bool // the loop has processed this job's submit/attach
	active       bool // queued/running in policy state after the last step
	terminal     bool // stream-terminal: refused, exit processed, or left the state
	exitReported bool
	exit         *proto.ExitRequest
	lost         bool
	endMS        int64
	status       proto.JobStatus
}

// stream is one handler's subscription to a job's events.
type stream struct {
	id     string
	gen    int64
	notify chan struct{}
	done   chan struct{}
	cursor int64
}

type registry struct {
	mu   sync.Mutex
	jobs map[string]*jobRec
	live map[string]*jobRec // !terminal || active
}

func newRegistry() *registry {
	return &registry{jobs: map[string]*jobRec{}, live: map[string]*jobRec{}}
}

func closeChan(c chan struct{}) {
	if c == nil {
		return
	}
	select {
	case <-c:
	default:
		close(c)
	}
}

func poke(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// openStreamLocked replaces any current stream of rec with a new one and
// appends the ACCEPTED event that starts every stream.
func (g *registry) openStreamLocked(rec *jobRec, accepted proto.Event) *stream {
	closeChan(rec.done)
	rec.gen++
	rec.notify = make(chan struct{}, 1)
	rec.done = make(chan struct{})
	rec.streaming = true
	cursor := rec.events.next
	rec.events.add(accepted)
	return &stream{id: rec.id, gen: rec.gen, notify: rec.notify, done: rec.done, cursor: cursor}
}

// maxStoredArgvBytes bounds the argv the registry keeps per job (status,
// GET /v1/jobs/{id}, state.json). A request may carry up to maxBodyBytes;
// keeping that for every finished job would cost the daemon far more than
// its own memory limit. The policy still sees the full argv once.
const maxStoredArgvBytes = 4 << 10

// boundSubmit returns s with its argv cut to maxStoredArgvBytes: leading
// arguments are kept (they carry the command and subcommand), and the last
// kept element says how much was dropped.
func boundSubmit(s proto.SubmitRequest) proto.SubmitRequest {
	total := 0
	for _, a := range s.Argv {
		total += len(a)
	}
	if total <= maxStoredArgvBytes {
		return s
	}
	var out []string
	used := 0
	for _, a := range s.Argv {
		if used+len(a) > maxStoredArgvBytes {
			if room := maxStoredArgvBytes - used; room > 0 {
				out = append(out, truncUTF8(a, room))
				used += room
			}
			break
		}
		out = append(out, a)
		used += len(a)
	}
	out = append(out, "...[argv truncated: "+strconv.Itoa(total-used)+" more bytes]")
	s.Argv = out
	return s
}

// truncUTF8 cuts s to at most n bytes without splitting a rune.
func truncUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// recBytes estimates a finished job's retained size (argv and events).
func recBytes(rec *jobRec) int64 {
	n := int64(256) // the record itself
	for _, a := range rec.submit.Argv {
		n += int64(len(a)) + 16
	}
	n += int64(len(rec.submit.Cwd))
	n += int64(len(rec.events.buf)) * 256
	return n
}

// create registers a new job (POST /v1/jobs) and opens its stream.
func (g *registry) create(id string, req proto.SubmitRequest, accepted proto.Event) *stream {
	g.mu.Lock()
	defer g.mu.Unlock()
	req = boundSubmit(req)
	rec := &jobRec{id: id, submit: req, events: newRing(jobRingSize)}
	rec.status = proto.JobStatus{ID: id, Class: req.Class, State: policy.JobQueued, Argv: req.Argv, SubmitTS: accepted.TS, EstimateBytes: req.EstimateBytes, BudgetBytes: req.BudgetBytes}
	g.jobs[id] = rec
	g.live[id] = rec
	return g.openStreamLocked(rec, accepted)
}

// remove drops a job whose submit never reached the loop.
func (g *registry) remove(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rec := g.jobs[id]; rec != nil {
		closeChan(rec.done)
	}
	delete(g.jobs, id)
	delete(g.live, id)
}

// attachUndo is what an attach replaced, to put back if the attach never
// reaches the loop.
type attachUndo struct {
	existed, wasLive bool
	prev             jobRec // fields restored (not the stream channels)
}

// attach (re)opens a stream for a job after a daemon restart. Unknown ids
// are registered from the attach's submit info.
func (g *registry) attach(id string, req proto.AttachRequest, accepted proto.Event) (*stream, proto.SubmitRequest, attachUndo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var u attachUndo
	rec := g.jobs[id]
	if rec == nil {
		rec = &jobRec{id: id, events: newRing(jobRingSize)}
		rec.status = proto.JobStatus{ID: id, Class: req.Submit.Class, State: policy.JobRunning, SubmitTS: req.SubmitMS}
		g.jobs[id] = rec
	} else {
		u.existed, u.wasLive = true, g.live[id] != nil
		u.prev = jobRec{submit: rec.submit, inLoop: rec.inLoop, active: rec.active, terminal: rec.terminal,
			exitReported: rec.exitReported, exit: rec.exit, lost: rec.lost, endMS: rec.endMS, status: rec.status}
	}
	if len(req.Submit.Argv) > 0 {
		rec.submit = boundSubmit(req.Submit)
	}
	rec.status.Argv = rec.submit.Argv
	rec.status.EndTS, rec.status.ExitCode = 0, nil
	rec.inLoop, rec.terminal, rec.exitReported, rec.exit, rec.lost, rec.endMS = false, false, false, nil, false, 0
	g.live[id] = rec
	st := g.openStreamLocked(rec, accepted)
	if req.LastEventTS > 0 {
		// Replay what the wrapper's previous stream never delivered: the
		// events after the last one it got (a restarted daemon's KILLED or
		// FROZEN sent before this re-attach), oldest first, then ACCEPTED.
		from := st.cursor
		for from > rec.events.oldest() && rec.events.buf[int((from-1)%int64(rec.events.size))].TS > req.LastEventTS {
			from--
		}
		st.cursor = from
	}
	return st, rec.submit, u
}

// undoAttach reverts an attach whose event could not be queued, unless a
// newer stream has taken the job over since.
func (g *registry) undoAttach(s *stream, u attachUndo) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.jobs[s.id]
	if rec == nil || rec.gen != s.gen {
		return
	}
	rec.streaming = false
	closeChan(rec.done)
	if !u.existed {
		delete(g.jobs, s.id)
		delete(g.live, s.id)
		return
	}
	p := &u.prev
	rec.submit, rec.inLoop, rec.active, rec.terminal = p.submit, p.inLoop, p.active, p.terminal
	rec.exitReported, rec.exit, rec.lost, rec.endMS, rec.status = p.exitReported, p.exit, p.lost, p.endMS, p.status
	if !u.wasLive {
		delete(g.live, s.id)
	}
}

// known reports whether id is a job the daemon knows (live or finished).
func (g *registry) known(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.jobs[id] != nil
}

// submitOf returns the stored submit request of a known job.
func (g *registry) submitOf(id string) (proto.SubmitRequest, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if rec := g.jobs[id]; rec != nil {
		return rec.submit, true
	}
	return proto.SubmitRequest{}, false
}

// markExit records the wrapper's exit report before it is queued, so a
// stream closing right after the report is not taken as a lost wrapper.
func (g *registry) markExit(id string, x proto.ExitRequest) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.jobs[id]
	if rec == nil {
		return false
	}
	rec.exitReported = true
	rec.exit = &x
	code := x.Code
	if x.Signaled {
		code = 128 + x.Signal
	}
	rec.status.ExitCode = &code
	// A kernel kill at the container's hard limit came before any sample
	// saw that peak: the record's peak agrees with its KILLED line.
	rec.status.PeakBytes = max(rec.status.PeakBytes, x.PeakBytes)
	return true
}

// read copies the stream's unread events. dropped > 0 means a gap.
func (g *registry) read(s *stream, dst []proto.Event) ([]proto.Event, int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.jobs[s.id]
	if rec == nil || rec.gen != s.gen {
		return dst, 0
	}
	out, dropped, next := rec.events.since(s.cursor, dst)
	s.cursor = next
	return out, dropped
}

// streamClosed is called when a stream handler returns. It reports whether
// the wrapper must be considered lost (EvLost): only for the job's current
// stream, before any terminal state or exit report, and never during
// daemon shutdown.
func (g *registry) streamClosed(s *stream, disconnected, stopping bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.jobs[s.id]
	if rec == nil || rec.gen != s.gen {
		return false
	}
	rec.streaming = false
	if !disconnected || stopping || rec.terminal || rec.exitReported {
		return false
	}
	rec.lost = true
	return true
}

// jobStatus returns a copy of a job's status with its last events.
func (g *registry) jobStatus(id string) (proto.JobStatus, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.jobs[id]
	if rec == nil {
		return proto.JobStatus{}, false
	}
	js := rec.status
	js.Events = rec.events.last(jobRingSize)
	for i := range js.Events {
		js.Events[i] = js.Events[i].WithoutStreamFields() // the job stream's alone
	}
	return js, true
}

// protectedPIDs appends wrapper pids of jobs that are not finished.
func (g *registry) protectedPIDs(dst []int) []int {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, rec := range g.live {
		if rec.submit.WrapperPID > 1 && (rec.active || !rec.terminal) {
			dst = append(dst, rec.submit.WrapperPID)
		}
	}
	sort.Ints(dst)
	return dst
}

// isWrapper reports whether pid is the wrapper of a live job (as
// protectedPIDs, but current: a wrapper that submitted since the last
// refresh counts).
func (g *registry) isWrapper(pid int) bool {
	if pid <= 1 {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, rec := range g.live {
		if rec.submit.WrapperPID == pid && (rec.active || !rec.terminal) {
			return true
		}
	}
	return false
}

// stepResult is what the loop learned from one step, applied under the
// registry lock: event fan-out and terminal detection.
type stepResult struct {
	nowMS     int64
	state     *policy.State
	events    []proto.Event    // Output.Events (V/TS filled)
	processed []policy.InEvent // events consumed by this step
}

// apply appends job events, updates statuses and ends streams whose job
// reached a terminal state. It returns whether anything about the job
// table changed (for persistence).
func (g *registry) apply(r stepResult) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	changed := false

	for i := range r.events {
		e := &r.events[i]
		if e.Job == "" {
			continue
		}
		rec := g.jobs[e.Job]
		if rec == nil {
			continue
		}
		rec.events.add(*e)
		if rec.notify != nil {
			poke(rec.notify)
		}
		changed = true
		switch e.Verb {
		case proto.VerbAccepted, proto.VerbRefused:
			// A job refused in its first step never appears in the state:
			// take what the policy resolved (class, estimate, and on its
			// ACCEPTED the budget and priority) from the events. A job that
			// stays is refreshed from the state below.
			if e.Class != "" {
				rec.status.Class = e.Class
			}
			if e.NeedBytes != nil {
				rec.status.EstimateBytes = *e.NeedBytes
			}
			if e.Verb == proto.VerbAccepted && e.NeedBytes != nil { // the policy's, not the daemon's
				rec.status.BudgetBytes = 0
				if e.BudgetBytes != nil {
					rec.status.BudgetBytes = *e.BudgetBytes
				}
				if e.Priority != nil {
					rec.status.Priority = *e.Priority
				}
			}
		}
		if e.Verb == proto.VerbRefused {
			g.terminateLocked(rec, r.nowMS)
		}
		if e.Verb == proto.VerbKilled && e.Policy != "" {
			rec.status.KilledBy = e.Policy
		}
	}

	// Mark which jobs are active in the new state; refresh their status.
	for _, rec := range g.live {
		rec.active = false
	}
	for i := range r.state.Jobs {
		j := &r.state.Jobs[i]
		if j.State == policy.JobDone {
			continue
		}
		rec := g.jobs[j.ID]
		if rec == nil {
			// The policy knows a job we do not (e.g. state from an older
			// daemon without its job table): track it without a stream.
			rec = &jobRec{id: j.ID, events: newRing(jobRingSize), inLoop: true, lost: true}
			g.jobs[j.ID] = rec
			changed = true
		}
		g.live[j.ID] = rec
		rec.active = true
		fillJobStatus(&rec.status, j, rec)
	}

	for _, ev := range r.processed {
		rec := g.jobs[ev.Job]
		if rec == nil {
			continue
		}
		switch ev.Kind {
		case policy.EvExit:
			g.terminateLocked(rec, r.nowMS)
		}
	}

	for id, rec := range g.live {
		if rec.inLoop && !rec.active {
			if !rec.terminal {
				g.terminateLocked(rec, r.nowMS)
			}
			if rec.status.State != policy.JobDone {
				rec.status.State = policy.JobDone
				rec.status.Position = 0
				// A finished job is no longer throttled, frozen or being
				// terminated: only a governor kill stays visible, as
				// stage=killed next to killed_by.
				rec.status.Stage = ""
				if rec.status.KilledBy != "" {
					rec.status.Stage = policy.StageKilled
				}
				if rec.status.EndTS == 0 {
					rec.status.EndTS = r.nowMS
				}
				if rec.endMS == 0 {
					rec.endMS = r.nowMS
				}
				changed = true
			}
		}
		if rec.terminal && !rec.active {
			delete(g.live, id)
		}
	}
	return changed
}

// markInLoop flags jobs whose submit/attach is part of the step about to run.
func (g *registry) markInLoop(evs []policy.InEvent) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ev := range evs {
		if ev.Kind != policy.EvSubmit && ev.Kind != policy.EvAttach {
			continue
		}
		if rec := g.jobs[ev.Job]; rec != nil {
			rec.inLoop = true
		}
	}
}

// abandon ends the streams of jobs whose submit/attach could not be
// stepped (policy panic): wrappers see the stream end without a verdict.
func (g *registry) abandon(evs []policy.InEvent, nowMS int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, ev := range evs {
		if ev.Kind != policy.EvSubmit && ev.Kind != policy.EvAttach {
			continue
		}
		if rec := g.jobs[ev.Job]; rec != nil && !rec.active {
			g.terminateLocked(rec, nowMS)
			delete(g.live, rec.id)
		}
	}
}

func (g *registry) terminateLocked(rec *jobRec, nowMS int64) {
	if rec.terminal {
		return
	}
	rec.terminal = true
	if rec.endMS == 0 {
		rec.endMS = nowMS
	}
	closeChan(rec.done)
}

// prune drops finished jobs older than the retention window, then the
// oldest beyond maxFinishedJobs or maxFinishedBytes.
func (g *registry) prune(nowMS int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var fin []*jobRec
	for id, rec := range g.jobs {
		if g.live[id] != nil {
			continue
		}
		if rec.endMS > 0 && nowMS-rec.endMS > finishedRetainMS {
			delete(g.jobs, id)
			continue
		}
		fin = append(fin, rec)
	}
	sortFinished(fin)
	var bytes int64
	for i, rec := range fin {
		bytes += recBytes(rec)
		if i >= maxFinishedJobs || bytes > maxFinishedBytes {
			delete(g.jobs, rec.id)
		}
	}
}

// sortFinished orders newest end first, then id.
func sortFinished(fin []*jobRec) {
	sort.Slice(fin, func(i, j int) bool {
		if fin[i].endMS != fin[j].endMS {
			return fin[i].endMS > fin[j].endMS
		}
		return fin[i].id < fin[j].id
	})
}

// recent returns up to n most recently finished jobs (newest first).
func (g *registry) recent(n int) []proto.JobStatus {
	g.mu.Lock()
	defer g.mu.Unlock()
	var fin []*jobRec
	for id, rec := range g.jobs {
		if g.live[id] == nil && rec.status.State == policy.JobDone {
			fin = append(fin, rec)
		}
	}
	sortFinished(fin)
	if len(fin) > n {
		fin = fin[:n]
	}
	out := make([]proto.JobStatus, len(fin))
	for i, rec := range fin {
		out[i] = rec.status
	}
	return out
}

// counts returns the number of queued, running and finished jobs known.
func (g *registry) counts() (queued, running, done int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, rec := range g.jobs {
		switch {
		case rec.active && rec.status.State == policy.JobQueued:
			queued++
		case rec.active:
			running++
		default:
			done++
		}
	}
	return
}

// statusesOf returns the statuses of active jobs (for /v1/status).
func (g *registry) statusesOf(jobs []policy.Job) (running, queued []proto.JobStatus) {
	g.mu.Lock()
	defer g.mu.Unlock()
	running, queued = []proto.JobStatus{}, []proto.JobStatus{}
	for i := range jobs {
		j := &jobs[i]
		rec := g.jobs[j.ID]
		var js proto.JobStatus
		if rec != nil {
			js = rec.status
		} else {
			fillJobStatus(&js, j, nil)
		}
		switch j.State {
		case policy.JobQueued:
			queued = append(queued, js)
		case policy.JobRunning:
			running = append(running, js)
		}
	}
	// state.Jobs is in arrival order; the queue order is the reported position.
	sort.SliceStable(queued, func(a, b int) bool { return posKey(queued[a].Position) < posKey(queued[b].Position) })
	return running, queued
}

func posKey(p int) int {
	if p <= 0 {
		return int(^uint(0) >> 1)
	}
	return p
}

// fillJobStatus copies policy job fields into a status, keeping the
// daemon-side fields (argv, exit code, end time) already there.
func fillJobStatus(js *proto.JobStatus, j *policy.Job, rec *jobRec) {
	js.ID = j.ID
	js.Class = j.Class
	js.State = j.State
	// Observe / warn-only enforcement is virtual: the job was never
	// throttled, frozen or killed, so status must not say it was (the
	// decision log and INFO "would ..." events carry what would have happened).
	js.Stage = ""
	if !j.Enf.Virtual {
		js.Stage = j.Enf.Stage
	}
	js.Priority = j.Priority
	if rec != nil {
		js.Argv = rec.submit.Argv
	}
	js.PID, js.PGID, js.NProcs = j.PID, j.PGID, j.NProcs
	js.EstimateBytes, js.BudgetBytes = j.EstimateBytes, j.BudgetBytes
	js.ReservedBytes, js.UsedBytes, js.PeakBytes = j.ReservedBytes, j.UsedBytes, j.PeakBytes
	if rec != nil && rec.exit != nil {
		js.PeakBytes = max(js.PeakBytes, rec.exit.PeakBytes) // the kernel's peak (markExit)
	}
	js.Position = 0
	if j.State == policy.JobQueued {
		js.Position = j.LastPosition
	}
	js.SubmitTS = j.SubmitMS
	js.StartTS = j.StartMS
	if js.StartTS == 0 {
		js.StartTS = j.AdmitMS
	}
	if j.Enf.KillPolicy != "" && !j.Enf.Virtual {
		js.KilledBy = j.Enf.KillPolicy
	}
}

// feed is the global event stream (GET /v1/events): one ring shared by
// all followers, each with its own cursor.
type feed struct {
	mu   sync.Mutex
	r    ring
	subs map[*follower]struct{}
}

type follower struct {
	notify chan struct{}
	cursor int64
}

func newFeed(size int) *feed { return &feed{r: newRing(size), subs: map[*follower]struct{}{}} }

func (f *feed) add(e proto.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.r.add(e)
	for s := range f.subs {
		poke(s.notify)
	}
}

// subscribe starts a follower at the current end, minus backlog events.
func (f *feed) subscribe(backlog int) *follower {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := f.r.next - int64(backlog)
	if start < f.r.oldest() {
		start = f.r.oldest()
	}
	s := &follower{notify: make(chan struct{}, 1), cursor: start}
	f.subs[s] = struct{}{}
	return s
}

func (f *feed) unsubscribe(s *follower) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, s)
}

func (f *feed) read(s *follower, dst []proto.Event) ([]proto.Event, int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out, dropped, next := f.r.since(s.cursor, dst)
	s.cursor = next
	return out, dropped
}

func (f *feed) last(n int) []proto.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.r.last(n)
}

func (f *feed) followers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}
