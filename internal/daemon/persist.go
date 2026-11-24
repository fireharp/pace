package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// stateFileV is the state.json format version.
const stateFileV = 1

// stateFile is <home>/state.json: the policy state plus the daemon's job
// table (submit info, event history) so a restarted daemon can re-adopt
// running jobs and still answer GET /v1/jobs/{id} for recent ones.
type stateFile struct {
	V       int            `json:"v"`
	SavedMS int64          `json:"saved_ms"`
	Version string         `json:"version,omitempty"`
	LogSeq  int64          `json:"log_seq"`
	IDSeq   int64          `json:"id_seq"`
	State   policy.State   `json:"state"`
	Jobs    []persistedJob `json:"jobs,omitempty"`
	// Held is what the actuator backgrounded or stopped, per target key
	// (see actuate.go), so that the next daemon releases exactly those
	// processes, attributed or not.
	Held map[string]persistedHeld `json:"held,omitempty"`
}

type persistedHeld struct {
	BG      []heldProc `json:"bg,omitempty"`
	Stopped []heldProc `json:"stopped,omitempty"`
}

type heldProc struct {
	PID     int   `json:"pid"`
	StartMS int64 `json:"start_ms"`
}

func heldProcs(m map[int]int64) []heldProc {
	var out []heldProc
	for _, pid := range sortedPIDs(m) {
		out = append(out, heldProc{PID: pid, StartMS: m[pid]})
	}
	return out
}

// exportHeld snapshots the actuator's records (loop goroutine).
func (d *daemon) exportHeld() map[string]persistedHeld {
	var out map[string]persistedHeld
	for k, h := range d.held {
		if len(h.bg) == 0 && len(h.stopped) == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]persistedHeld, len(d.held))
		}
		out[k] = persistedHeld{BG: heldProcs(h.bg), Stopped: heldProcs(h.stopped)}
	}
	return out
}

// restoreHeld loads the records of a previous daemon.
func (d *daemon) restoreHeld(in map[string]persistedHeld) {
	for k, ph := range in {
		if k == "" {
			continue
		}
		h := d.heldFor(k)
		for _, p := range ph.BG {
			if p.PID > 1 {
				h.bg[p.PID] = p.StartMS
			}
		}
		for _, p := range ph.Stopped {
			if p.PID > 1 {
				h.stopped[p.PID] = p.StartMS
			}
		}
	}
}

type persistedJob struct {
	ID           string              `json:"id"`
	Submit       proto.SubmitRequest `json:"submit"`
	Status       proto.JobStatus     `json:"status"`
	Events       []proto.Event       `json:"events,omitempty"`
	Active       bool                `json:"active,omitempty"`
	Terminal     bool                `json:"terminal,omitempty"`
	ExitReported bool                `json:"exit_reported,omitempty"`
	Exit         *proto.ExitRequest  `json:"exit,omitempty"`
	EndMS        int64               `json:"end_ms,omitempty"`
}

// export snapshots the job table: every live job, plus the most recent
// finished ones (bounded in count and total events).
func (g *registry) export() []persistedJob {
	g.mu.Lock()
	defer g.mu.Unlock()
	var live, fin []*jobRec
	for id, rec := range g.jobs {
		if g.live[id] != nil {
			live = append(live, rec)
		} else {
			fin = append(fin, rec)
		}
	}
	sortFinished(live)
	sortFinished(fin)
	if len(fin) > persistFinished {
		fin = fin[:persistFinished]
	}
	out := make([]persistedJob, 0, len(live)+len(fin))
	budget := persistMaxEvents
	for _, set := range [][]*jobRec{live, fin} {
		for _, rec := range set {
			evs := rec.events.last(jobRingSize)
			if len(evs) > budget {
				evs = evs[len(evs)-budget:]
			}
			budget -= len(evs)
			for i := range evs {
				evs[i] = evs[i].WithoutStreamFields()
			}
			js := rec.status
			js.Events = nil
			js.Argv = nil // restored from Submit
			out = append(out, persistedJob{
				ID: rec.id, Submit: rec.submit, Status: js, Events: evs, Active: rec.active,
				Terminal: rec.terminal, ExitReported: rec.exitReported, Exit: rec.exit, EndMS: rec.endMS,
			})
		}
	}
	return out
}

// restore loads a persisted job table. Streams are gone: every job that
// was still queued/running is marked lost until its wrapper re-attaches.
func (g *registry) restore(jobs []persistedJob, active map[string]bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, pj := range jobs {
		if pj.ID == "" {
			continue
		}
		// Status.Argv is always the bounded submit argv: a state.json of an
		// older version may hold the full one there.
		pj.Submit = boundSubmit(pj.Submit)
		pj.Status.Argv = pj.Submit.Argv
		rec := &jobRec{
			id: pj.ID, submit: pj.Submit, events: newRing(jobRingSize), status: pj.Status,
			inLoop: true, active: active[pj.ID], terminal: pj.Terminal, exitReported: pj.ExitReported,
			exit: pj.Exit, endMS: pj.EndMS,
		}
		for _, e := range pj.Events {
			rec.events.add(e)
		}
		if rec.active {
			rec.lost = true
			rec.terminal = false
		}
		g.jobs[pj.ID] = rec
		if rec.active || !rec.terminal {
			g.live[pj.ID] = rec
		}
	}
}

// loadState reads state.json at startup. A corrupt file is renamed aside
// (never deleted). The daemon then falls back to state.json.prev, the last
// snapshot the persister knew was durable: after an OS crash inside the
// window between a writeNow and its fsync, state.json can be torn (the
// rename reached the disk before the data). Without a usable .prev the
// daemon starts fresh. Every job still queued or running gets an EvLost:
// its stream died with the old daemon, and its wrapper re-attaches within
// OrphanGrace.
func (d *daemon) loadState() {
	sf, err := readStateFile(d.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var bad *badStateError
	if err != nil && !errors.As(err, &bad) {
		d.fault("state_load", err.Error())
		return
	}
	if err != nil {
		aside := d.statePath + ".corrupt-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
		if rerr := os.Rename(d.statePath, aside); rerr != nil {
			d.fault("state_load", fmt.Sprintf("corrupt state (%v) and cannot move it aside: %v", err, rerr))
			return
		}
		prev := prevPath(d.statePath)
		psf, perr := readStateFile(prev)
		if perr != nil {
			d.fault("state_load", fmt.Sprintf("corrupt state moved to %s: %v", aside, err))
			return
		}
		d.fault("state_load", fmt.Sprintf("corrupt state moved to %s: %v; loaded %s (saved %s)",
			aside, err, filepath.Base(prev), time.UnixMilli(psf.SavedMS).UTC().Format(time.RFC3339)))
		// state.json is the durable .prev again, so a crash before the
		// first write still finds it.
		_ = os.Link(prev, d.statePath)
		sf = psf
	} else if perr, serr := syncKeep(d.statePath, (*os.File).Sync); serr != nil || perr != nil {
		// The loaded snapshot is the fallback for this run's first
		// unsynced write.
		d.fault("state_save", "keep "+filepath.Base(prevPath(d.statePath))+": "+errors.Join(serr, perr).Error())
	}
	d.st = sf.State
	d.logSeq = max(d.logSeq, sf.LogSeq)
	d.idSeq.Store(sf.IDSeq)
	active := map[string]bool{}
	for _, j := range d.st.Jobs {
		if j.State == policy.JobQueued || j.State == policy.JobRunning {
			active[j.ID] = true
			d.pending = append(d.pending, policy.InEvent{Kind: policy.EvLost, Job: j.ID})
		}
	}
	d.reg.restore(sf.Jobs, active)
	d.restoreHeld(sf.Held)
	d.seedHeld()
	d.lastMaterial = materialHash(d.st)
}

// badStateError: the file was read but is not a usable state file.
type badStateError struct{ err error }

func (e *badStateError) Error() string { return e.err.Error() }

func readStateFile(path string) (stateFile, error) {
	var sf stateFile
	b, err := os.ReadFile(path)
	if err != nil {
		return sf, err
	}
	err = json.Unmarshal(b, &sf)
	if err == nil && sf.V != stateFileV {
		err = fmt.Errorf("unsupported state file version %d", sf.V)
	}
	if err != nil {
		return stateFile{}, &badStateError{err}
	}
	return sf, nil
}

// encodeState marshals the state file (loop goroutine).
func (d *daemon) encodeState() ([]byte, error) {
	sf := stateFile{
		V: stateFileV, SavedMS: time.Now().UnixMilli(), Version: d.o.Version,
		LogSeq: d.logSeq, IDSeq: d.idSeq.Load(), State: d.st, Jobs: d.reg.export(),
		Held: d.exportHeld(),
	}
	return json.Marshal(sf)
}

// maybePersist hands the state to the persister: material changes at most
// once per second, telemetry-only changes every 30s.
func (d *daemon) maybePersist(force bool) {
	since := time.Since(d.lastPersistAt)
	if !force && !(d.dirty && since >= persistDebounce) && !(d.telemetry && since >= persistTelemetry) {
		return
	}
	b, err := d.encodeState()
	if err != nil {
		d.fault("state_save", err.Error())
		return
	}
	d.persist.submit(b)
	d.dirty, d.telemetry = false, false
	d.lastPersistAt = time.Now()
}

// persistNow writes the state before a freeze or throttle takes effect, so
// that a daemon crash right after the signal cannot lose the record. It
// runs on the loop goroutine and never waits for the disk: the snapshot is
// written and renamed into place without fsync (a crashed process does not
// lose the page cache), and the persister goroutine fsyncs it afterwards.
// An OS crash before that fsync can leave state.json torn; loadState then
// falls back to state.json.prev, the last durable snapshot (the stopped
// processes are gone after an OS crash anyway).
func (d *daemon) persistNow() {
	if d.persist == nil {
		return
	}
	b, err := d.encodeState()
	if err != nil {
		d.fault("state_save", err.Error())
		return
	}
	d.persist.writeNow(b)
	d.dirty, d.telemetry = false, false
	d.lastPersistAt = time.Now()
}

func (d *daemon) finalPersist() {
	b, err := d.encodeState()
	if err != nil {
		d.fault("state_save", err.Error())
		return
	}
	d.persist.submit(b)
}

// persister writes state.json off the loop; only the newest pending
// snapshot is kept. Every snapshot gets a sequence number when it is handed
// over, and a write replaces the file only with a newer snapshot than the
// one there, so a slow durable write never overwrites a later writeNow.
//
// path.prev is always a hard link to (or, without hard links, a copy of)
// a snapshot whose data and name are durable: every durable write and
// every fsync of a writeNow snapshot makes it .prev too. A writeNow rename
// that is not yet fsynced replaces only path, so after an OS crash in that
// window .prev is still a whole snapshot.
type persister struct {
	path   string
	fault  func(kind, msg string)
	fsync  func(*os.File) error                // (*os.File).Sync; tests stub it
	rename func(oldpath, newpath string) error // os.Rename onto path; tests stub it

	rmu     sync.Mutex // held only around the rename onto path
	renamed uint64     // seq of the snapshot at path (rmu)

	mu       sync.Mutex
	seq      uint64 // last seq handed out
	pending  []byte
	pendSeq  uint64
	unsynced bool // path holds a writeNow snapshot not yet fsynced
	writes   int64

	prevFailed bool // a .prev failure was reported (persister goroutine)

	kick chan struct{}
	stop chan struct{}
	done chan struct{}
}

func newPersister(path string, fault func(kind, msg string)) *persister {
	return newPersisterSync(path, fault, (*os.File).Sync)
}

func newPersisterSync(path string, fault func(kind, msg string), fsync func(*os.File) error) *persister {
	p := newPersisterIdle(path, fault, fsync)
	go p.run()
	return p
}

// newPersisterIdle has no goroutine: tests call flush themselves.
func newPersisterIdle(path string, fault func(kind, msg string), fsync func(*os.File) error) *persister {
	return &persister{path: path, fault: fault, fsync: fsync, rename: os.Rename,
		kick: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
}

// submit hands b to the persister goroutine for a durable write.
func (p *persister) submit(b []byte) {
	p.mu.Lock()
	p.seq++
	p.pending, p.pendSeq = b, p.seq
	p.mu.Unlock()
	poke(p.kick)
}

func (p *persister) take() (b []byte, seq uint64, unsynced bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, seq, unsynced = p.pending, p.pendSeq, p.unsynced
	p.pending, p.pendSeq, p.unsynced = nil, 0, false
	return b, seq, unsynced
}

// flush (persister goroutine) writes the pending snapshot durably, or
// makes the last writeNow snapshot durable.
func (p *persister) flush() {
	b, seq, unsynced := p.take()
	switch {
	case b != nil:
		// A durable write of a newer snapshot covers an unsynced one; a
		// writeNow that overtakes it marks the file unsynced again.
		placed, err := p.write(b, seq, true)
		if err != nil {
			p.fault("state_save", err.Error())
		}
		if !placed && unsynced {
			// The unsynced snapshot is still at path: the next flush (at
			// the latest close) fsyncs it.
			p.markUnsynced()
		}
	case unsynced:
		prevErr, err := syncKeep(p.path, p.fsync)
		if err != nil {
			p.fault("state_save", err.Error())
			p.markUnsynced()
		} else {
			p.prevResult(prevErr)
		}
	}
}

func (p *persister) markUnsynced() {
	p.mu.Lock()
	p.unsynced = true
	p.mu.Unlock()
}

// writeNow (loop goroutine) has b at path when it returns, written without
// fsync and without waiting for a durable write in progress; an older
// pending snapshot is dropped (b supersedes it). The persister goroutine
// fsyncs the file and its directory afterwards. If the write fails, the
// persister writes b durably instead.
func (p *persister) writeNow(b []byte) {
	p.mu.Lock()
	p.seq++
	seq := p.seq
	p.pending, p.pendSeq = nil, 0
	p.mu.Unlock()
	_, err := p.write(b, seq, false)
	p.mu.Lock()
	if err != nil {
		if p.pending == nil { // the persister retries it, durably
			p.pending, p.pendSeq = b, seq
		}
	} else {
		p.unsynced = true
	}
	p.mu.Unlock()
	if err != nil {
		p.fault("state_save", err.Error())
	}
	poke(p.kick)
}

// write writes b to a temp file and renames it onto path, unless a newer
// snapshot is there already; placed reports whether b is now at path.
// durable (persister goroutine only): the temp file is fsynced and made
// path.prev before the rename, and the directory is fsynced after it.
func (p *persister) write(b []byte, seq uint64, durable bool) (placed bool, err error) {
	dir := filepath.Dir(p.path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(p.path)+".*.tmp")
	if err != nil {
		return false, err
	}
	tmp := f.Name()
	defer func() {
		if !placed {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return false, err
	}
	if durable {
		if err := p.fsync(f); err != nil {
			f.Close()
			return false, err
		}
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if durable {
		// Even if a newer writeNow supersedes b below, b is the newest
		// durable snapshot.
		p.prevResult(keepPrev(p.path, tmp, b, p.fsync))
		defer p.syncDir(dir)
	}
	p.rmu.Lock()
	if seq <= p.renamed {
		p.rmu.Unlock()
		return false, nil // superseded: a newer snapshot is in place
	}
	if err := p.rename(tmp, p.path); err != nil {
		p.rmu.Unlock()
		return false, err
	}
	p.renamed = seq
	p.rmu.Unlock()
	p.mu.Lock()
	p.writes++
	p.mu.Unlock()
	return true, nil
}

// prevResult reports the first failure to keep path.prev (persister
// goroutine); state.json itself is still written.
func (p *persister) prevResult(err error) {
	if err == nil || p.prevFailed {
		return
	}
	p.prevFailed = true
	p.fault("state_save", "keep "+filepath.Base(prevPath(p.path))+": "+err.Error())
}

func (p *persister) syncDir(dir string) { syncDir(dir, p.fsync) }

func syncDir(dir string, fsync func(*os.File) error) {
	if df, err := os.Open(dir); err == nil {
		_ = fsync(df)
		df.Close()
	}
}

// prevPath is the last snapshot known to be durable.
func prevPath(path string) string { return path + ".prev" }

func prevTmp(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".prev.tmp")
}

// keepPrev makes src, a file whose data is durable and holds b, path.prev:
// a hard link, or a fsynced copy of b where links are not supported. The
// caller fsyncs the directory afterwards.
func keepPrev(path, src string, b []byte, fsync func(*os.File) error) error {
	tmp := prevTmp(path)
	os.Remove(tmp)
	if os.Link(src, tmp) != nil {
		if err := writeSynced(tmp, b, fsync); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, prevPath(path)); err != nil {
		os.Remove(tmp)
		return err
	}
	// rename(2) is a no-op when tmp and .prev are already links to the same
	// inode; drop the extra name then (ENOENT otherwise).
	os.Remove(tmp)
	return nil
}

func writeSynced(name string, b []byte, fsync func(*os.File) error) error {
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := fsync(f); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncKeep fsyncs the snapshot at path, makes it path.prev and fsyncs the
// directory. It links path first and fsyncs through the link, so .prev is
// exactly the inode that was fsynced even if a writeNow renames a newer
// snapshot onto path meanwhile (that one is marked unsynced and fsynced by
// a later flush). Where hard links are not supported, .prev is a fsynced
// copy of what path holds. err: path could not be fsynced; prevErr: .prev
// was not kept.
func syncKeep(path string, fsync func(*os.File) error) (prevErr, err error) {
	tmp := prevTmp(path)
	os.Remove(tmp)
	name := path
	linked := os.Link(path, tmp) == nil
	if linked {
		name = tmp
	}
	f, err := os.Open(name)
	if err == nil {
		err = fsync(f)
		f.Close()
	}
	if err != nil {
		if name == tmp {
			os.Remove(tmp)
		}
		return nil, err
	}
	if !linked {
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			prevErr = rerr
		} else if prevErr = writeSynced(tmp, b, fsync); prevErr != nil {
			os.Remove(tmp)
		}
		if prevErr != nil {
			syncDir(filepath.Dir(path), fsync)
			return prevErr, nil
		}
	}
	if prevErr = os.Rename(tmp, prevPath(path)); prevErr != nil {
		os.Remove(tmp)
	}
	// rename(2) is a no-op when tmp and .prev are already links to the same
	// inode (a graceful stop leaves state.json and .prev as one file).
	os.Remove(tmp)
	syncDir(filepath.Dir(path), fsync)
	return prevErr, nil
}

func (p *persister) run() {
	defer close(p.done)
	for {
		select {
		case <-p.kick:
			p.flush()
		case <-p.stop:
			p.flush()
			return
		}
	}
}

// close writes whatever is pending (or fsyncs the last writeNow snapshot)
// and stops.
func (p *persister) close() {
	close(p.stop)
	<-p.done
}
