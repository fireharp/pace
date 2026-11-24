package daemon

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

// A freeze or throttle is recorded in state.json before it takes effect,
// but the control loop never waits for the disk to do it: on a busy disk
// an fsync can take seconds, and the stop, the throttle, sampling and the
// critical ladder would all wait for it. The record is in place (written
// and renamed, visible to a restarted daemon) when the signal is sent; the
// persister fsyncs it afterwards, and a slow durable write of an older
// snapshot never replaces it.
func TestFreezeAndThrottleDoNotWaitForDisk(t *testing.T) {
	for _, c := range []struct {
		name  string
		stage string
		act   policy.Action
	}{
		{"freeze", policy.StageFrozen, policy.Action{Op: policy.OpSignalGroup, Job: "j", PGID: 5200, Signal: int(syscall.SIGSTOP)}},
		{"throttle", policy.StageThrottled, policy.Action{Op: policy.OpBackground, Job: "j", PIDs: []int{5200}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := platform.NewFake()
			fn := fixedState(func(policy.Inputs) (policy.State, []policy.Action) {
				return freezeState(c.stage), []policy.Action{c.act}
			})
			d := newStepDaemon(t, f, fn, nil, nil)
			d.st = freezeState(policy.StageNone)
			setProcs(f, proc(5200, 1, 5200, 1000, 10*miB))

			// A disk whose fsync does not return until released.
			gate := make(chan struct{})
			entered := make(chan struct{}, 64)
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			d.persist.close()
			d.persist = newPersisterSync(d.statePath, d.fault, func(*os.File) error {
				entered <- struct{}{}
				<-gate
				return nil
			})
			t.Cleanup(release) // runs before the fixture closes d.persist

			// A routine durable write is stuck in fsync.
			d.maybePersist(true)
			waitEntered(t, entered, release)

			done := make(chan struct{})
			go func() { d.runStep(true); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				release()
				<-done
				t.Fatal("the step waited for fsync before acting")
			}
			switch c.name {
			case "freeze":
				if !hasSignal(f.Signals(), 0, 5200, syscall.SIGSTOP) {
					t.Fatalf("not stopped: %s", fmtSent(f.Signals()))
				}
			case "throttle":
				f.Lock()
				bg := f.Background[5200]
				f.Unlock()
				if !bg {
					t.Fatal("not backgrounded")
				}
			}
			checkRecord := func(when string) {
				t.Helper()
				b, err := os.ReadFile(d.statePath)
				if err != nil {
					t.Fatalf("%s: %v", when, err)
				}
				var sf stateFile
				if err := json.Unmarshal(b, &sf); err != nil {
					t.Fatalf("%s: %v", when, err)
				}
				if len(sf.State.Jobs) != 1 || sf.State.Jobs[0].Enf.Stage != c.stage {
					t.Fatalf("%s: state.json does not record the %s: %+v", when, c.name, sf.State.Jobs)
				}
				if c.name == "freeze" {
					if h := sf.Held["j:j"]; len(h.Stopped) != 1 || h.Stopped[0].PID != 5200 || h.Stopped[0].StartMS != 1000 {
						t.Fatalf("%s: stopped processes not recorded: %+v", when, sf.Held)
					}
				}
			}
			checkRecord("at the signal") // what a daemon restarted now would load

			// The disk answers: the stale snapshot is dropped, the record is
			// fsynced (file and directory).
			release()
			waitEntered(t, entered, release)
			waitEntered(t, entered, release)
			checkRecord("after the durable write")
			if n := d.faults.count("state_save"); n != 0 {
				t.Fatalf("state_save faults: %d", n)
			}
		})
	}
}

func waitEntered(t *testing.T, entered chan struct{}, release func()) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("no fsync")
	}
}

// syncLog is an fsync stub that records what each fsync covered: the
// content of a regular file, or "<dir>" for a directory.
type syncLog struct {
	mu  sync.Mutex
	got []string
}

func (l *syncLog) fsync(f *os.File) error {
	what := "<dir>"
	if fi, err := f.Stat(); err == nil && fi.Mode().IsRegular() {
		b, _ := io.ReadAll(io.NewSectionReader(f, 0, fi.Size()))
		what = string(b)
	}
	l.mu.Lock()
	l.got = append(l.got, what)
	l.mu.Unlock()
	return f.Sync()
}

// durable: content was fsynced, and a directory fsync came after it.
func (l *syncLog) durable(content string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := false
	for _, g := range l.got {
		if g == content {
			seen = true
		} else if g == "<dir>" && seen {
			return true
		}
	}
	return false
}

func (l *syncLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.got, ",")
}

// A writeNow snapshot is made durable by the persister, at the latest when
// it closes, whichever way the persister and writeNow interleave.
func TestPersisterFsyncsWriteNow(t *testing.T) {
	for i := 0; i < 20; i++ {
		dir := shortDir(t)
		path := filepath.Join(dir, "s.json")
		var l syncLog
		p := newPersisterSync(path, func(k, m string) { t.Errorf("%s: %s", k, m) }, l.fsync)
		p.submit([]byte("old"))
		p.writeNow([]byte("now"))
		if b, _ := os.ReadFile(path); string(b) != "now" {
			t.Fatalf("after writeNow: %q", b)
		}
		p.close()
		if b, _ := os.ReadFile(path); string(b) != "now" {
			t.Fatalf("after close: %q, want the writeNow snapshot", b)
		}
		if !l.durable("now") {
			t.Fatalf("the writeNow snapshot was never fsynced (fsyncs: %s)", &l)
		}
		if b, _ := os.ReadFile(prevPath(path)); string(b) != "now" {
			t.Fatalf(".prev %q, want the fsynced writeNow snapshot", b)
		}
	}
}

// A writeNow that cannot write hands its snapshot to the persister, which
// writes it durably, and the failure is a state_save fault.
func TestPersisterRetriesFailedWriteNow(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "s.json")
	var l syncLog
	var faults atomic.Int64
	p := newPersisterIdle(path, func(k, m string) {
		if k == "state_save" {
			faults.Add(1)
		}
	}, l.fsync)
	var fail atomic.Bool
	fail.Store(true)
	p.rename = func(o, n string) error {
		if fail.CompareAndSwap(true, false) {
			return errors.New("injected: no space left on device")
		}
		return os.Rename(o, n)
	}
	go p.run()
	p.writeNow([]byte("now"))
	if faults.Load() != 1 {
		t.Fatalf("state_save faults %d, want 1", faults.Load())
	}
	p.close()
	if b, _ := os.ReadFile(path); string(b) != "now" {
		t.Fatalf("after close: %q, want the snapshot of the failed writeNow", b)
	}
	if !l.durable("now") {
		t.Fatalf("the retried snapshot was not written durably (fsyncs: %s)", &l)
	}
	if faults.Load() != 1 {
		t.Fatalf("state_save faults %d, want 1", faults.Load())
	}
}

// When the durable write that would have covered an unsynced writeNow
// snapshot fails, the persister still fsyncs that snapshot later.
func TestPersisterFailedDurableWriteKeepsUnsynced(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "s.json")
	var l syncLog
	var faults atomic.Int64
	p := newPersisterIdle(path, func(string, string) { faults.Add(1) }, l.fsync)
	p.writeNow([]byte("now")) // unsynced
	p.submit([]byte("next"))
	p.rename = func(string, string) error { return errors.New("injected: I/O error") }
	p.flush() // the durable write of "next" fails
	if faults.Load() != 1 {
		t.Fatalf("faults %d, want 1", faults.Load())
	}
	if !p.unsynced {
		t.Fatal("the unsynced writeNow snapshot was forgotten")
	}
	p.rename = os.Rename
	p.flush() // what close would do
	if b, _ := os.ReadFile(path); string(b) != "now" || !l.durable("now") {
		t.Fatalf("path %q, fsyncs %s: want \"now\" fsynced", b, &l)
	}
}

// After an OS crash inside the window between a writeNow and its fsync,
// state.json may be torn. The daemon then loads state.json.prev, the last
// snapshot that was durable, instead of starting empty.
func TestLoadStateFallsBackToPrev(t *testing.T) {
	d, _ := newTestDaemon(t)
	enc := func(idSeq int64, job string) []byte {
		b, err := json.Marshal(stateFile{V: stateFileV, SavedMS: 1700000000000, IDSeq: idSeq, LogSeq: idSeq,
			State: policy.State{Jobs: []policy.Job{{ID: job, State: policy.JobRunning}}}})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	p := newPersister(d.statePath, func(k, m string) { t.Errorf("%s: %s", k, m) })
	p.submit(enc(7, "durable"))
	p.close() // durable, and .prev
	p = newPersisterIdle(d.statePath, func(k, m string) { t.Errorf("%s: %s", k, m) }, (*os.File).Sync)
	p.writeNow(enc(8, "unsynced"))
	// The crash: the rename reached the disk, the data did not.
	if err := os.Truncate(d.statePath, 0); err != nil {
		t.Fatal(err)
	}

	d.loadState()
	if d.idSeq.Load() != 7 || len(d.st.Jobs) != 1 || d.st.Jobs[0].ID != "durable" {
		t.Fatalf("loaded id_seq %d jobs %+v, want the .prev snapshot", d.idSeq.Load(), d.st.Jobs)
	}
	if len(d.pending) != 1 || d.pending[0].Kind != policy.EvLost {
		t.Fatalf("pending %+v", d.pending)
	}
	f := strings.Join(d.faults.active(time.Now().UnixMilli()), "\n")
	if !strings.Contains(f, "corrupt state moved to") || !strings.Contains(f, "loaded state.json.prev") {
		t.Fatalf("faults %q", f)
	}
	if m, _ := filepath.Glob(d.statePath + ".corrupt-*"); len(m) != 1 {
		t.Fatalf("torn file not moved aside: %v", m)
	}
	// A daemon that crashes before its first write still finds it.
	if b, _ := os.ReadFile(d.statePath); string(b) != string(enc(7, "durable")) {
		t.Fatalf("state.json not restored from .prev: %q", b)
	}

	// No usable .prev: a fresh start, as before.
	d2, _ := newTestDaemon(t)
	if err := os.WriteFile(d2.statePath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	d2.loadState()
	if len(d2.st.Jobs) != 0 || d2.idSeq.Load() != 0 {
		t.Fatalf("loaded %+v from nothing", d2.st.Jobs)
	}
}

// A good state.json loaded at startup is fsynced and becomes .prev, the
// fallback for this run's first unsynced write.
func TestLoadStateKeepsPrev(t *testing.T) {
	d, _ := newTestDaemon(t)
	b, _ := json.Marshal(stateFile{V: stateFileV, IDSeq: 3})
	if err := os.WriteFile(d.statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d.loadState()
	if d.idSeq.Load() != 3 || d.faults.total.Load() != 0 {
		t.Fatalf("id_seq %d faults %d", d.idSeq.Load(), d.faults.total.Load())
	}
	if pb, _ := os.ReadFile(prevPath(d.statePath)); string(pb) != string(b) {
		t.Fatalf(".prev %q", pb)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(d.statePath), ".*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
}

// A graceful stop leaves state.json and state.json.prev as links to one
// inode; the next start must not leave the temp link behind.
func TestLoadStateKeepsPrevSameInode(t *testing.T) {
	d, _ := newTestDaemon(t)
	b, _ := json.Marshal(stateFile{V: stateFileV, IDSeq: 5})
	if err := os.WriteFile(d.statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(d.statePath, prevPath(d.statePath)); err != nil {
		t.Skipf("no hard links here: %v", err)
	}
	d.loadState()
	if d.idSeq.Load() != 5 || d.faults.total.Load() != 0 {
		t.Fatalf("id_seq %d faults %d", d.idSeq.Load(), d.faults.total.Load())
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(d.statePath), ".*.tmp")); len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
	if pb, _ := os.ReadFile(prevPath(d.statePath)); string(pb) != string(b) {
		t.Fatalf(".prev %q", pb)
	}
}
