package policy

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

const (
	MiB = int64(1) << 20
	GiB = int64(1) << 30
	T0  = int64(1_700_000_000_000)
	// warnB is testParams' warn threshold (15% of 16G), which is also its
	// effective reserve (above the configured 2G reserve).
	warnB = 16 * GiB * 150 / 1000
)

var (
	sigTERM = int(syscall.SIGTERM)
	sigKILL = int(syscall.SIGKILL)
	sigSTOP = int(syscall.SIGSTOP)
	sigCONT = int(syscall.SIGCONT)
)

// testParams is a 16 GiB simulated machine with realistic defaults.
func testParams() Params {
	return Params{
		Mode:                  ModeEnforce,
		CapacityBytes:         16 * GiB,
		ReserveBytes:          2 * GiB,
		WarnAvailPermille:     150,
		CritAvailPermille:     70,
		PSIWarn10:             100,
		PSICrit10:             50,
		BudgetWarnPermille:    900,
		TolerancePermille:     100,
		WarnDeadlineMS:        20_000,
		GraceMS:               5_000,
		CriticalGraceMS:       1_000,
		StepMS:                2_000,
		CriticalStepMS:        500,
		RecoverHoldMS:         5_000,
		OrphanGraceMS:         10_000,
		MinVictimBytes:        64 * MiB,
		UnmanagedPriority:     2,
		DefaultQueueTimeoutMS: 30 * 60_000,
		CPUWarnPerCoreMilli:   1_500,
		CPUOKPerCoreMilli:     1_000,
		DiskWarnFreeBytes:     10 * GiB,
		DiskCritFreeBytes:     3 * GiB,
		MarginPermille:        100,
		HistorySize:           20,
		HistoryMinSamples:     3,
		Classes: []ClassParams{
			{Name: "build", EstimateBytes: 2 * GiB, BudgetBytes: 4 * GiB, Slots: 2, WallLimitMS: 3_600_000, NoProgressMS: 600_000, Priority: 5},
			{Name: "default", EstimateBytes: 1 * GiB, BudgetBytes: 2 * GiB, WallLimitMS: 1_800_000, NoProgressMS: 600_000, Priority: 5},
			{Name: "model", EstimateBytes: 6 * GiB, BudgetBytes: 9 * GiB, Slots: 1, WallLimitMS: 4 * 3_600_000, NoProgressMS: 900_000, Priority: 7},
			{Name: "test", EstimateBytes: 1 * GiB, BudgetBytes: 2 * GiB, Slots: 4, WallLimitMS: 1_800_000, NoProgressMS: 600_000, Priority: 4},
		},
		DefaultClass: "default",
	}
}

// sim drives Step tick by tick, like the daemon would, and checks the
// package invariants on every tick.
type sim struct {
	t    *testing.T
	p    Params
	s    State
	now  int64
	base int64 // footprint of in-scope processes outside jobs/units
	mach platform.Machine

	samples map[string]*JobSample
	units   map[string]*UnitSample
	events  []InEvent

	lastIn  Inputs
	lastOut Output
	all     []Output
	prevRes map[string]int64
	// strict: the mode never changed, so non-enforce modes must not produce
	// any real action at all.
	strict bool
	// stale: inputs carry Stale (a failed sample).
	stale bool
}

func newSim(t *testing.T, p Params) *sim {
	t.Helper()
	return &sim{
		t: t, p: p, now: T0, base: 2 * GiB,
		mach: platform.Machine{
			TotalBytes: 36 * GiB, AvailBytes: 20 * GiB, KernelLevel: 1,
			PSIMemSome10: -1, PSIMemFull10: -1, NCPU: 8, Load1Milli: 2_000,
			DiskFreeBytes: 100 * GiB,
		},
		samples: map[string]*JobSample{},
		units:   map[string]*UnitSample{},
		prevRes: map[string]int64{},
		strict:  true,
	}
}

func intp(v int) *int { return &v }

func (m *sim) submit(id string, r proto.SubmitRequest) {
	if r.Argv == nil {
		r.Argv = []string{"cmd-" + id}
	}
	m.events = append(m.events, InEvent{Kind: EvSubmit, Job: id, Submit: &r})
}

func (m *sim) started(id string, pid int) {
	m.events = append(m.events, InEvent{Kind: EvStarted, Job: id, PID: pid, PGID: pid, StartMS: m.now})
	m.samples[id] = &JobSample{ID: id, Alive: true, NProcs: 1}
}

func (m *sim) use(id string, b int64) {
	js := m.samples[id]
	if js == nil {
		m.t.Fatalf("use(%s): job not started", id)
	}
	js.UsedBytes = b
}

func (m *sim) cpu(id string, addMS int64) { m.samples[id].CPUMS += addMS }

func (m *sim) output(id string, total int64) {
	m.events = append(m.events, InEvent{Kind: EvProgress, Job: id, OutputBytes: total, LastOutputMS: m.now})
}

func (m *sim) exit(id string, code int) {
	m.events = append(m.events, InEvent{Kind: EvExit, Job: id, Code: code})
	delete(m.samples, id)
}

func (m *sim) lost(id string) { m.events = append(m.events, InEvent{Kind: EvLost, Job: id}) }

func (m *sim) dead(id string) {
	if js := m.samples[id]; js != nil {
		js.Alive, js.UsedBytes = false, 0
	}
}

func (m *sim) unit(key string, pgid int, start int64, used int64) {
	m.units[key] = &UnitSample{Key: key, PGID: pgid, LeaderPID: pgid, StartMS: start, Comm: "u" + key, UsedBytes: used, NProcs: 1, PIDs: []int{pgid}}
}

func (m *sim) scoped() int64 {
	t := m.base
	for _, js := range m.samples {
		if js.Alive {
			t += js.UsedBytes
		}
	}
	for _, u := range m.units {
		t += u.UsedBytes
	}
	return t
}

// setFree sets the background usage so that avail == free.
func (m *sim) setFree(free int64) {
	m.base = 0
	m.base = m.p.CapacityBytes - free - m.scoped()
}

func (m *sim) inputs() Inputs {
	in := Inputs{NowMS: m.now, Machine: m.mach, ScopedUsedBytes: m.scoped(), Events: m.events, Stale: m.stale}
	in.Machine.NowMS = m.now
	ids := make([]string, 0, len(m.samples))
	for id := range m.samples {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		js := *m.samples[id]
		js.ExtraPIDs = append([]int(nil), js.ExtraPIDs...)
		in.Jobs = append(in.Jobs, js)
	}
	keys := make([]string, 0, len(m.units))
	for k := range m.units {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		u := *m.units[k]
		u.PIDs = append([]int(nil), u.PIDs...)
		in.Units = append(in.Units, u)
	}
	return in
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// tick advances the clock by dt and runs one Step.
func (m *sim) tick(dt int64) Output {
	m.t.Helper()
	m.now += dt
	in := m.inputs()
	m.events = nil
	before := mustJSON(m.t, m.s)
	inBefore := mustJSON(m.t, in)
	ns, out := Step(m.p, m.s, in)
	if got := mustJSON(m.t, m.s); got != before {
		m.t.Fatalf("Step mutated its input state\nbefore %s\nafter  %s", before, got)
	}
	if got := mustJSON(m.t, in); got != inBefore {
		m.t.Fatalf("Step mutated its inputs")
	}
	ns2, out2 := Step(m.p, m.s, in)
	if a, b := mustJSON(m.t, ns)+mustJSON(m.t, out), mustJSON(m.t, ns2)+mustJSON(m.t, out2); a != b {
		m.t.Fatalf("Step is not deterministic\n%s\n%s", a, b)
	}
	m.check(in, ns, out)
	m.s, m.lastIn, m.lastOut = ns, in, out
	m.all = append(m.all, out)
	return out
}

// run ticks n times by dt and returns all outputs concatenated.
func (m *sim) run(n int, dt int64) Output {
	var all Output
	for i := 0; i < n; i++ {
		o := m.tick(dt)
		all.Events = append(all.Events, o.Events...)
		all.Actions = append(all.Actions, o.Actions...)
		all.Would = append(all.Would, o.Would...)
	}
	return all
}

var noHintVerbs = map[string]bool{
	proto.VerbAccepted: true, proto.VerbQueued: true, proto.VerbAdmitted: true,
	proto.VerbOK: true, proto.VerbInfo: true, proto.VerbThrottled: true,
	proto.VerbUnthrottled: true, proto.VerbFrozen: true, proto.VerbThawed: true,
}

func (m *sim) check(in Inputs, ns State, out Output) {
	t := m.t
	t.Helper()
	for _, e := range out.Events {
		if e.V != 1 || e.TS != m.now {
			t.Fatalf("event %s: v=%d ts=%d want v=1 ts=%d", e.Verb, e.V, e.TS, m.now)
		}
		if e.Job != "" && e.Unit != "" {
			t.Fatalf("event %s has both job and unit", e.Line())
		}
		if noHintVerbs[e.Verb] && e.Hint != "" {
			t.Fatalf("%s must carry no hint: %q", e.Verb, e.Hint)
		}
		switch e.Verb {
		case proto.VerbWarn, proto.VerbRefused:
			if e.Hint == "" {
				t.Fatalf("%s must carry a hint: %s", e.Verb, e.Line())
			}
		case proto.VerbKilled:
			if (e.Policy == proto.PolicyOrphaned) != (e.Hint == "") {
				t.Fatalf("KILLED hint rule violated: %s hint=%q", e.Line(), e.Hint)
			}
		}
		if _, kv, ok := proto.ParseLine(e.Line()); !ok || kv == nil {
			t.Fatalf("event line does not parse: %s", e.Line())
		}
	}
	for _, list := range [][]Action{out.Actions, out.Would} {
		for _, a := range list {
			switch a.Op {
			case OpSignalGroup:
				if a.PGID <= 1 {
					t.Fatalf("signal_group to pgid %d", a.PGID)
				}
			case OpSignal:
				if len(a.PIDs) == 0 {
					t.Fatalf("signal with no pids")
				}
			case OpBackground, OpForeground:
				if a.PGID <= 1 && len(a.PIDs) == 0 {
					t.Fatalf("%s without target", a.Op)
				}
			default:
				t.Fatalf("unknown op %q", a.Op)
			}
			for _, p := range a.PIDs {
				if p <= 1 {
					t.Fatalf("action %s to pid %d", a.Op, p)
				}
			}
			if (a.Job == "") == (a.Unit == "") {
				t.Fatalf("action must name exactly one of job/unit: %+v", a)
			}
		}
	}
	if m.strict && m.p.Mode != ModeEnforce && m.p.Mode != "" && len(out.Actions) > 0 {
		t.Fatalf("mode %s produced real actions: %+v", m.p.Mode, out.Actions)
	}
	if m.p.Mode == ModeEnforce && len(out.Would) > 0 {
		t.Fatalf("enforce mode produced would-actions: %+v", out.Would)
	}
	// State shape.
	seen := map[string]bool{}
	for i, j := range ns.Jobs {
		if i > 0 && ns.Jobs[i-1].Seq >= j.Seq {
			t.Fatalf("jobs not sorted by seq")
		}
		if seen[j.ID] {
			t.Fatalf("duplicate job %s", j.ID)
		}
		seen[j.ID] = true
		if j.State != JobQueued && j.State != JobRunning {
			t.Fatalf("job %s in state %q kept", j.ID, j.State)
		}
		checkWarnings(t, j.Enf)
		if j.State == JobRunning {
			if prev, ok := m.prevRes[j.ID]; ok && j.ReservedBytes < prev {
				t.Fatalf("reservation of %s shrank %d -> %d", j.ID, prev, j.ReservedBytes)
			}
			m.prevRes[j.ID] = j.ReservedBytes
			if j.ReservedBytes < j.EstimateBytes {
				t.Fatalf("reservation %d below estimate %d", j.ReservedBytes, j.EstimateBytes)
			}
		}
	}
	for i, u := range ns.Units {
		if i > 0 && ns.Units[i-1].Key >= u.Key {
			t.Fatalf("units not sorted")
		}
		checkWarnings(t, u.Enf)
	}
	for i, h := range ns.History {
		if i > 0 && ns.History[i-1].Key >= h.Key {
			t.Fatalf("history not sorted")
		}
		if len(h.Peaks) != len(h.DurMS) || len(h.Peaks) > historySize(&m.p) {
			t.Fatalf("history %s malformed: %d peaks %d durs", h.Key, len(h.Peaks), len(h.DurMS))
		}
	}
	if ns.LastMS != m.now {
		t.Fatalf("LastMS = %d", ns.LastMS)
	}
	// Admission never exceeds headroom; never admits while paused.
	admitted := false
	for _, e := range out.Events {
		if e.Verb == proto.VerbAdmitted {
			admitted = true
		}
	}
	if admitted && m.p.Mode != ModeObserve {
		a := Assess(m.p, ns, in)
		if a.HeadroomBytes < 0 {
			t.Fatalf("admission exceeded headroom: headroom after = %d", a.HeadroomBytes)
		}
		if levelRank(a.Level) > 0 || a.DiskLevel == proto.LevelCritical {
			t.Fatalf("admitted while level=%s disk=%s", a.Level, a.DiskLevel)
		}
	}
}

func checkWarnings(t *testing.T, e Enforcement) {
	t.Helper()
	for i := range e.Warnings {
		if i > 0 && e.Warnings[i-1].Policy >= e.Warnings[i].Policy {
			t.Fatalf("warnings not sorted/unique: %+v", e.Warnings)
		}
	}
}

// ---- lookups ---------------------------------------------------------------

func evs(o Output, verb string) []proto.Event {
	var r []proto.Event
	for _, e := range o.Events {
		if e.Verb == verb {
			r = append(r, e)
		}
	}
	return r
}

func evFor(o Output, verb, id string) *proto.Event {
	for i := range o.Events {
		e := &o.Events[i]
		if e.Verb == verb && (e.Job == id || (e.Unit != "" && e.Unit == proto.UnitRef(id))) {
			return e
		}
	}
	return nil
}

func mustEv(t *testing.T, o Output, verb, id string) *proto.Event {
	t.Helper()
	e := evFor(o, verb, id)
	if e == nil {
		t.Fatalf("no %s event for %q in:\n%s", verb, id, dumpEvents(o))
	}
	return e
}

func noEv(t *testing.T, o Output, verb, id string) {
	t.Helper()
	if e := evFor(o, verb, id); e != nil {
		t.Fatalf("unexpected %s for %q: %s", verb, id, e.Line())
	}
}

func dumpEvents(o Output) string {
	var sb strings.Builder
	for _, e := range o.Events {
		for _, l := range e.Lines() {
			sb.WriteString("  " + l + "\n")
		}
	}
	for _, a := range o.Actions {
		fmt.Fprintf(&sb, "  action %+v\n", a)
	}
	for _, a := range o.Would {
		fmt.Fprintf(&sb, "  would %+v\n", a)
	}
	return sb.String()
}

func acts(list []Action, id string, op string, sig int) []Action {
	var r []Action
	for _, a := range list {
		if (a.Job == id || a.Unit == id || (a.Unit != "" && proto.UnitRef(a.Unit) == id)) && a.Op == op && (sig == 0 || a.Signal == sig) {
			r = append(r, a)
		}
	}
	return r
}

func hasAct(list []Action, id, op string, sig int) bool { return len(acts(list, id, op, sig)) > 0 }

func (m *sim) job(id string) *Job {
	for i := range m.s.Jobs {
		if m.s.Jobs[i].ID == id {
			return &m.s.Jobs[i]
		}
	}
	return nil
}

func (m *sim) unitState(key string) *Unit {
	for i := range m.s.Units {
		if m.s.Units[i].Key == key {
			return &m.s.Units[i]
		}
	}
	return nil
}

// admitStart submits, admits and starts a job; returns after the start tick.
func (m *sim) admitStart(id string, r proto.SubmitRequest, pid int, used int64) {
	m.t.Helper()
	m.submit(id, r)
	o := m.tick(100)
	mustEv(m.t, o, proto.VerbAdmitted, id)
	m.started(id, pid)
	m.use(id, used)
	m.tick(100)
}

func i64(p *int64) int64 {
	if p == nil {
		return -1
	}
	return *p
}

func eq(t *testing.T, name string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}
