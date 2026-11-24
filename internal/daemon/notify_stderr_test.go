package daemon

import (
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// notifyRun runs one step over an unmanaged hog of the given size (leader
// 1000 and helper 1001 in group 1000, plus an operating-system process 1002
// in that group) on a 2G machine and returns the notes and signals.
func notifyRun(t *testing.T, mode string, notify bool, hogBytes int64) ([]platform.FakeNote, []platform.SentSignal, []proto.Event) {
	t.Helper()
	now := int64(10_000_000)
	f := platform.NewFake()
	var evs []proto.Event
	step := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		ns, out := policy.Step(p, s, in)
		evs = append(evs, out.Events...)
		return ns, out
	}
	d := newStepDaemon(t, f, step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.Mode = mode
		c.Params.CapacityBytes = 2 * giB // warn below 307M avail, critical below 143M
		c.NotifyStderr = notify
	})
	sys := proc(1002, 1000, 1000, now-50_000, 1*miB)
	sys.System = true
	setProcs(f,
		proc(1000, 1, 1000, now-60_000, hogBytes-64*miB),
		proc(1001, 1000, 1000, now-59_000, 64*miB),
		sys,
	)
	d.runStep(true)
	return f.StderrNotes(), f.Signals(), evs
}

func noteFor(notes []platform.FakeNote, verb string) (platform.FakeNote, bool) {
	for _, n := range notes {
		if strings.HasPrefix(n.Text, "pace: "+verb+" ") {
			return n, true
		}
	}
	return platform.FakeNote{}, false
}

// At critical pressure an unmanaged hog is warned and killed in one step:
// both lines, each with its hint, go to its members' stderr before the
// SIGTERM, leader first; the operating-system process in its group gets
// nothing (no action may reach it either).
func TestUnitKilledLineToStderrBeforeSignal(t *testing.T) {
	notes, sent, evs := notifyRun(t, policy.ModeEnforce, true, 1950*miB)
	term := -1
	for i, s := range sent {
		if s.PID == 1000 && s.Sig == syscall.SIGTERM {
			term = i
			break
		}
	}
	if term < 0 {
		t.Fatalf("hog not signalled: %s (events %+v)", fmtSent(sent), evs)
	}
	for _, n := range notes {
		t.Logf("note to %v:\n%s", n.PIDs, n.Text)
	}
	k, ok := noteFor(notes, proto.VerbKilled)
	if !ok {
		t.Fatalf("no KILLED note: %+v (events %+v)", notes, evs)
	}
	if k.SignalsBefore > term {
		t.Fatalf("KILLED note written after the SIGTERM (signals before it %d, SIGTERM at %d)", k.SignalsBefore, term)
	}
	lines := strings.Split(strings.TrimSuffix(k.Text, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "pace: KILLED policy=pressure ") ||
		!strings.Contains(lines[0], " unit=pg:1000 pid=1000 ") ||
		lines[1] != "pace: hint: "+policy.HintPressureKill {
		t.Fatalf("KILLED note text %q", k.Text)
	}
	if verb, kv, ok := proto.ParseLine(lines[0]); !ok || verb != proto.VerbKilled || kv["policy"] != proto.PolicyPressure {
		t.Fatalf("KILLED note line does not parse: %q", lines[0])
	}
	if len(k.PIDs) != 2 || k.PIDs[0] != 1000 || k.PIDs[1] != 1001 {
		t.Fatalf("note pids %v, want [1000 1001] (leader first, never the system process)", k.PIDs)
	}
	for _, n := range notes {
		if len(n.Text) > 2*platform.StderrNoteMaxLine || !strings.HasSuffix(n.Text, "\n") {
			t.Fatalf("note text %q", n.Text)
		}
		if n.SignalsBefore > term {
			t.Fatalf("note %q written after the SIGTERM", n.Text)
		}
	}
	// The critical WARN (in=0ms) comes in the same step, just before.
	if len(notes) != 2 || !strings.HasPrefix(notes[0].Text, "pace: WARN policy=pressure level=critical ") ||
		!strings.Contains(notes[0].Text, " next=kill in=0ms ") ||
		!strings.HasSuffix(notes[0].Text, "\npace: hint: "+policy.HintPressureWarn+"\n") || notes[1].Text != k.Text {
		t.Fatalf("want the WARN then the KILLED note, got %+v", notes)
	}
}

// At warn pressure the unit gets its WARN (next=throttle) and hint; nothing
// is signalled yet.
func TestUnitWarnLineToStderr(t *testing.T) {
	notes, sent, evs := notifyRun(t, policy.ModeEnforce, true, 1800*miB)
	w, ok := noteFor(notes, proto.VerbWarn)
	if !ok {
		t.Fatalf("no WARN note: %+v (events %+v)", notes, evs)
	}
	if !strings.HasPrefix(w.Text, "pace: WARN policy=pressure ") || !strings.HasSuffix(w.Text, "\npace: hint: "+policy.HintPressureWarn+"\n") {
		t.Fatalf("WARN note text %q", w.Text)
	}
	if _, ok := noteFor(notes, proto.VerbKilled); ok || len(sent) != 0 {
		t.Fatalf("killed at warn pressure: notes %+v signals %s", notes, fmtSent(sent))
	}
}

// unmanaged.notify_stderr = false, and observe / warn-only mode (whose lines
// carry mode= or are INFO "would" lines): no note.
func TestUnitStderrNoteOffAndVirtual(t *testing.T) {
	notes, sent, _ := notifyRun(t, policy.ModeEnforce, false, 1950*miB)
	if len(notes) != 0 {
		t.Fatalf("notes with notify_stderr off: %+v", notes)
	}
	if !hasSignal(sent, 1000, 0, syscall.SIGTERM) { // per pid: the group holds a system process
		t.Fatalf("notify_stderr off changed the kill: %s", fmtSent(sent))
	}
	for _, m := range []string{policy.ModeObserve, policy.ModeWarnOnly} {
		notes, sent, evs := notifyRun(t, m, true, 1950*miB)
		if len(notes) != 0 || len(sent) != 0 {
			t.Fatalf("%s: notes %+v signals %s (events %+v)", m, notes, fmtSent(sent), evs)
		}
	}
}

func TestStderrNoteText(t *testing.T) {
	e := proto.Event{Verb: proto.VerbKilled, Policy: proto.PolicyPressure, Unit: "pg:7", PID: 7, Comm: "gen", Hint: "retry"}
	got, ok := stderrNoteText(&e)
	if !ok || got != "pace: KILLED policy=pressure unit=pg:7 pid=7 comm=gen\npace: hint: retry\n" {
		t.Fatalf("text %q ok=%v", got, ok)
	}
	e.Msg = strings.Repeat("x", platform.StderrNoteMaxLine)
	if _, ok := stderrNoteText(&e); ok {
		t.Fatal("an over-long line is written (it must be dropped, never cut)")
	}
}

// budgetRun: an unmanaged unit (leader 2000, helper 2001) over
// unmanaged.budget 300M on a 16G machine (no pressure).
func budgetRun(t *testing.T, hogBytes int64) (*platform.Fake, *daemon, *int64, *[]proto.Event) {
	t.Helper()
	now := int64(10_000_000)
	f := platform.NewFake()
	evs := new([]proto.Event)
	step := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		ns, out := policy.Step(p, s, in)
		*evs = append(*evs, out.Events...)
		return ns, out
	}
	d := newStepDaemon(t, f, step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.UnmanagedBudgetBytes = 300 * miB
		c.NotifyStderr = true
	})
	setProcs(f,
		proc(2000, 1, 2000, now-60_000, hogBytes-32*miB),
		proc(2001, 2000, 2000, now-59_000, 32*miB),
	)
	return f, d, &now, evs
}

// A unit over unmanaged.budget gets the mem_budget WARN with the unit
// hint (no signal yet), and at the deadline the KILLED line with the unit
// hint, before its SIGTERM.
func TestUnitBudgetLinesToStderr(t *testing.T) {
	f, d, now, evs := budgetRun(t, 310*miB)
	d.runStep(true)
	notes := f.StderrNotes()
	w, ok := noteFor(notes, proto.VerbWarn)
	if !ok || len(notes) != 1 {
		t.Fatalf("want one WARN note, got %+v (events %+v)", notes, *evs)
	}
	lines := strings.Split(strings.TrimSuffix(w.Text, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "pace: WARN policy=mem_budget ") ||
		!strings.Contains(lines[0], " next=kill ") || !strings.Contains(lines[0], " unit=pg:2000 pid=2000 ") ||
		lines[1] != "pace: hint: "+policy.HintUnitBudgetWarn {
		t.Fatalf("WARN note text %q", w.Text)
	}
	if len(w.PIDs) != 2 || w.PIDs[0] != 2000 || w.PIDs[1] != 2001 {
		t.Fatalf("WARN note pids %v", w.PIDs)
	}
	if sent := f.Signals(); len(sent) != 0 {
		t.Fatalf("signalled at the WARN: %s", fmtSent(sent))
	}
	*now += 21_000 // past enforce.warn_deadline (20s)
	d.runStep(true)
	notes = f.StderrNotes()
	k, ok := noteFor(notes, proto.VerbKilled)
	if !ok {
		t.Fatalf("no KILLED note: %+v (events %+v)", notes, *evs)
	}
	lines = strings.Split(strings.TrimSuffix(k.Text, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "pace: KILLED policy=mem_budget ") ||
		!strings.Contains(lines[0], " unit=pg:2000 pid=2000 ") || lines[1] != "pace: hint: "+policy.HintUnitBudgetKill {
		t.Fatalf("KILLED note text %q", k.Text)
	}
	sent := f.Signals()
	term := -1
	for i, s := range sent {
		if s.Sig == syscall.SIGTERM {
			term = i
			break
		}
	}
	if term < 0 || k.SignalsBefore > term {
		t.Fatalf("KILLED note (signals before it %d) not before the SIGTERM: %s", k.SignalsBefore, fmtSent(sent))
	}
}

// A step on a stale sample writes no note, even when it emits a unit's
// WARN and KILLED lines (the members may be gone; nothing is signalled
// either).
func TestUnitStderrNoteStaleSample(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	var evs []proto.Event
	step := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		in.Stale = false // the policy acts as if fresh: only the daemon knows
		ns, out := policy.Step(p, s, in)
		evs = append(evs, out.Events...)
		return ns, out
	}
	d := newStepDaemon(t, f, step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.CapacityBytes = 2 * giB
		c.NotifyStderr = true
	})
	setProcs(f,
		proc(1000, 1, 1000, now-60_000, 1950*miB-64*miB),
		proc(1001, 1000, 1000, now-59_000, 64*miB),
	)
	d.sample() // a good sample first
	f.Lock()
	f.Err = errors.New("sample failed")
	f.Unlock()
	d.runStep(true)
	if !d.sampleStale {
		t.Fatal("sample not stale")
	}
	killed := false
	for _, e := range evs {
		killed = killed || e.Verb == proto.VerbKilled && e.Unit == "pg:1000"
	}
	if !killed {
		t.Fatalf("no KILLED event on the stale step (no control): %+v", evs)
	}
	if notes := f.StderrNotes(); len(notes) != 0 {
		t.Fatalf("notes on a stale sample: %+v", notes)
	}
}

// A note the backend drops (an earlier write still stuck, or past its
// deadline) is a stderr_note fault, at most one per noteFaultEveryMS; the
// drops held back are counted into the next.
func TestUnitStderrNoteDropFault(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	f.NoteDrop = "1 stderr note(s) for 2 process(es) dropped: the previous stderr write has not finished"
	d := newStepDaemon(t, f, policy.Step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.CapacityBytes = 2 * giB
		c.NotifyStderr = true
	})
	setProcs(f,
		proc(1000, 1, 1000, now-60_000, 1800*miB-64*miB),
		proc(1001, 1000, 1000, now-59_000, 64*miB),
	)
	d.runStep(true)
	if n := d.faults.count("stderr_note"); n != 1 {
		t.Fatalf("stderr_note faults %d, want 1 (faults %v)", n, d.faults.active(time.Now().UnixMilli()))
	}
	if !strings.Contains(strings.Join(d.faults.active(time.Now().UnixMilli()), "\n"), "stderr_note: 1 stderr note(s) for 2 process(es) dropped") {
		t.Fatalf("status faults %v", d.faults.active(time.Now().UnixMilli()))
	}
	drop := d.noteDrops.report(d.fault)
	drop("again")
	drop("and again")
	if n := d.faults.count("stderr_note"); n != 1 {
		t.Fatalf("stderr_note not rate-limited: %d faults", n)
	}
	d.noteDrops.mu.Lock()
	d.noteDrops.lastMS -= noteFaultEveryMS
	d.noteDrops.mu.Unlock()
	drop("later")
	if n := d.faults.count("stderr_note"); n != 2 {
		t.Fatalf("stderr_note faults %d after the interval, want 2", n)
	}
	if a := strings.Join(d.faults.active(time.Now().UnixMilli()), "\n"); !strings.Contains(a, "later (and 2 more drop(s) since the last report)") {
		t.Fatalf("held-back drops not counted: %v", a)
	}
}
