package platform

import (
	"slices"
	"sort"
	"sync"
	"syscall"
)

// Fake is a deterministic in-memory Backend for tests. Tests mutate
// Machine/Procs directly (under Lock) and inspect Sent/Background.
type Fake struct {
	mu         sync.Mutex
	Mach       Machine
	Procs      []Proc
	Sent       []SentSignal
	Background map[int]bool
	Err        error // returned by SampleMachine/ListProcs when set
	Notes      []FakeNote
	// NoteDrop, when set, makes NotifyStderr drop the notes and report
	// this reason to its dropped func instead.
	NoteDrop string
	cont     *FakeContainers
}

// FakeNote records one StderrNote and how many signals had been sent when
// it was written (to check it came before the signal).
type FakeNote struct {
	StderrNote
	SignalsBefore int
}

// SentSignal records one signal delivery attempt.
type SentSignal struct {
	PID  int // 0 when sent to a group
	PGID int // 0 when sent to a single pid
	Sig  syscall.Signal
}

// NewFake returns a fake 16 GiB, 8-core machine with nothing running.
func NewFake() *Fake {
	return &Fake{
		Mach: Machine{
			TotalBytes: 16 << 30, AvailBytes: 12 << 30, KernelLevel: 1,
			PSIMemSome10: -1, PSIMemFull10: -1, NCPU: 8, DiskFreeBytes: 100 << 30,
		},
		Background: map[int]bool{},
	}
}

func (f *Fake) Name() string { return "fake" }

// Lock / Unlock let tests mutate state safely while a daemon samples it.
func (f *Fake) Lock()   { f.mu.Lock() }
func (f *Fake) Unlock() { f.mu.Unlock() }

func (f *Fake) SampleMachine(diskPaths []string) (Machine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return Machine{}, f.Err
	}
	return f.Mach, nil
}

func (f *Fake) ListProcs(uid int) ([]Proc, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	out := make([]Proc, 0, len(f.Procs))
	for _, p := range f.Procs {
		if p.UID == uid && p.State != 'X' {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// apply simulates signal effects: SIGKILL/SIGTERM remove processes
// (SIGTERM only if the process is not marked to ignore it via State 'I'),
// SIGSTOP/SIGCONT toggle 'T'.
func (f *Fake) apply(match func(p *Proc) bool, sig syscall.Signal) {
	kept := f.Procs[:0]
	for i := range f.Procs {
		p := f.Procs[i]
		if match(&p) {
			switch sig {
			case syscall.SIGKILL:
				continue
			case syscall.SIGTERM:
				if p.State != 'I' {
					continue
				}
			case syscall.SIGSTOP:
				p.State = 'T'
			case syscall.SIGCONT:
				if p.State == 'T' {
					p.State = 'S'
				}
			}
		}
		kept = append(kept, p)
	}
	f.Procs = kept
}

func (f *Fake) Signal(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Sent = append(f.Sent, SentSignal{PID: pid, Sig: sig})
	f.apply(func(p *Proc) bool { return p.PID == pid }, sig)
	return nil
}

func (f *Fake) SignalGroup(pgid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Sent = append(f.Sent, SentSignal{PGID: pgid, Sig: sig})
	f.apply(func(p *Proc) bool { return p.PGID == pgid }, sig)
	return nil
}

func (f *Fake) SetBackground(pid int, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Background[pid] = on
	return nil
}

// NotifyStderr records the notes (StderrNotifier), or drops them when
// NoteDrop is set.
func (f *Fake) NotifyStderr(notes []StderrNote, dropped func(reason string)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.NoteDrop != "" {
		if dropped != nil {
			dropped(f.NoteDrop)
		}
		return
	}
	for _, n := range notes {
		f.Notes = append(f.Notes, FakeNote{StderrNote{PIDs: append([]int(nil), n.PIDs...), Text: n.Text}, len(f.Sent)})
	}
}

// StderrNotes returns a copy of the notes written so far.
func (f *Fake) StderrNotes() []FakeNote {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FakeNote(nil), f.Notes...)
}

// Signals returns a copy of the signals sent so far.
func (f *Fake) Signals() []SentSignal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SentSignal(nil), f.Sent...)
}

// FakeCgroup is one job container of a FakeContainers.
type FakeCgroup struct {
	PIDs      []int
	Usage     int64
	Peak      int64
	OOMKills  int64
	MemMax    int64 // last limit set (0 = none)
	MaxSets   int   // how many times SetMemMax was called
	Frozen    bool
	Throttled bool
	Kills     int
	// Busy: rmdir fails with EBUSY although no process is inside (a
	// sub-cgroup the job left behind that cannot be removed).
	Busy bool
}

// FakeContainers is an in-memory JobContainer. Kill removes the container's
// processes from the Fake it belongs to.
type FakeContainers struct {
	mu   sync.Mutex
	Base string
	CG   map[string]*FakeCgroup
	// Weights is what SetThrottle reports ("" = no weight knobs).
	Weights string
	// Scope is the scope the daemon asked for; Evicted the pids Evict
	// moved out (in order); FreezeErr makes Freeze(…, true) fail; Closed
	// counts Close calls.
	Scope     string
	Evicted   []int
	FreezeErr error
	Closed    int
	// Removes and RemoveNows count the calls of each.
	Removes, RemoveNows int
	fake                *Fake
}

// EnableContainers gives the fake backend job containers under base.
func (f *Fake) EnableContainers(base string) *FakeContainers {
	c := &FakeContainers{Base: base, CG: map[string]*FakeCgroup{}, Weights: "cpu.weight=1", fake: f}
	f.mu.Lock()
	f.cont = c
	f.mu.Unlock()
	return c
}

// Containers implements ContainerProvider (ok only after EnableContainers).
// The scope is recorded (FakeContainers.Scope); the base stays as given.
func (f *Fake) Containers(scope string) (JobContainer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cont == nil {
		return nil, false
	}
	f.cont.mu.Lock()
	f.cont.Scope = scope
	f.cont.mu.Unlock()
	return f.cont, true
}

// Get returns a copy of the job's container state.
func (c *FakeContainers) Get(id string) (FakeCgroup, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.CG[id]
	if !ok {
		return FakeCgroup{}, false
	}
	cp := *g
	cp.PIDs = append([]int(nil), g.PIDs...)
	return cp, true
}

// Set replaces the job's container state (creating it).
func (c *FakeContainers) Set(id string, g FakeCgroup) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := g
	c.CG[id] = &cp
}

func (c *FakeContainers) get(id string) (*FakeCgroup, error) {
	g, ok := c.CG[id]
	if !ok {
		return nil, syscall.ENOENT
	}
	return g, nil
}

func (c *FakeContainers) Create(id string, memMax int64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.CG[id]
	if !ok {
		g = &FakeCgroup{}
		c.CG[id] = g
	}
	g.MemMax = memMax
	return c.Base + "/job-" + id, nil
}

func (c *FakeContainers) Path(id string) (string, error) { return c.Base + "/job-" + id, nil }

func (c *FakeContainers) Attach(id string, pid int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return err
	}
	g.PIDs = append(g.PIDs, pid)
	return nil
}

func (c *FakeContainers) PIDs(id string) ([]int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return nil, err
	}
	return append([]int(nil), g.PIDs...), nil
}

func (c *FakeContainers) Usage(id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return 0, err
	}
	return g.Usage, nil
}

func (c *FakeContainers) Peak(id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return 0, err
	}
	return g.Peak, nil
}

func (c *FakeContainers) OOMKills(id string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return 0, err
	}
	return g.OOMKills, nil
}

func (c *FakeContainers) SetMemMax(id string, memMax int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return err
	}
	g.MemMax = memMax
	g.MaxSets++
	return nil
}

func (c *FakeContainers) SetThrottle(id string, on bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return "", err
	}
	g.Throttled = on
	return c.Weights, nil
}

func (c *FakeContainers) Freeze(id string, on bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return err
	}
	if on && c.FreezeErr != nil {
		return c.FreezeErr
	}
	g.Frozen = on
	return nil
}

func (c *FakeContainers) Frozen(id string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, err := c.get(id)
	if err != nil {
		return false, err
	}
	return g.Frozen, nil
}

func (c *FakeContainers) Kill(id string) error {
	c.mu.Lock()
	g, ok := c.CG[id]
	pids := map[int]bool{}
	if ok {
		g.Kills++
		for _, p := range g.PIDs {
			pids[p] = true
		}
		g.PIDs = nil
		g.Usage = 0
	}
	c.mu.Unlock()
	if ok && c.fake != nil {
		c.fake.mu.Lock()
		c.fake.apply(func(p *Proc) bool { return pids[p.PID] }, syscall.SIGKILL)
		c.fake.mu.Unlock()
	}
	return nil
}

func (c *FakeContainers) Remove(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Removes++
	return c.removeLocked(id)
}

func (c *FakeContainers) RemoveNow(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.RemoveNows++
	return c.removeLocked(id)
}

func (c *FakeContainers) removeLocked(id string) error {
	g, ok := c.CG[id]
	if !ok {
		return nil
	}
	if len(g.PIDs) > 0 || g.Busy {
		return syscall.EBUSY
	}
	delete(c.CG, id)
	return nil
}

func (c *FakeContainers) Jobs() ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids := make([]string, 0, len(c.CG))
	for id := range c.CG {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// Evict moves pid out of whichever container holds it.
func (c *FakeContainers) Evict(pid int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, g := range c.CG {
		g.PIDs = slices.DeleteFunc(g.PIDs, func(p int) bool { return p == pid })
	}
	c.Evicted = append(c.Evicted, pid)
	return nil
}

// Close counts the daemon's shutdown cleanup.
func (c *FakeContainers) Close() {
	c.mu.Lock()
	c.Closed++
	c.mu.Unlock()
}
