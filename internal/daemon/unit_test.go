package daemon

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

func evN(n int) proto.Event { return proto.Event{Verb: proto.VerbInfo, PID: n} }

func pids(evs []proto.Event) []int {
	out := []int{}
	for _, e := range evs {
		out = append(out, e.PID)
	}
	return out
}

func TestRing(t *testing.T) {
	cases := []struct {
		name        string
		size, adds  int
		from        int64
		wantPIDs    []int
		wantDropped int64
	}{
		{"empty", 4, 0, 0, []int{}, 0},
		{"partial from 0", 4, 3, 0, []int{0, 1, 2}, 0},
		{"partial from 2", 4, 3, 2, []int{2}, 0},
		{"exactly full", 4, 4, 0, []int{0, 1, 2, 3}, 0},
		{"wrapped, reader behind", 4, 10, 0, []int{6, 7, 8, 9}, 6},
		{"wrapped, reader current", 4, 10, 8, []int{8, 9}, 0},
		{"reader at end", 4, 10, 10, []int{}, 0},
		{"size one", 1, 5, 3, []int{4}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRing(c.size)
			for i := 0; i < c.adds; i++ {
				r.add(evN(i))
			}
			got, dropped, next := r.since(c.from, nil)
			if !reflect.DeepEqual(pids(got), c.wantPIDs) || dropped != c.wantDropped || next != int64(c.adds) {
				t.Fatalf("since(%d) = %v dropped=%d next=%d; want %v dropped=%d", c.from, pids(got), dropped, next, c.wantPIDs, c.wantDropped)
			}
		})
	}
	r := newRing(3)
	for i := 0; i < 7; i++ {
		r.add(evN(i))
	}
	for n, want := range map[int][]int{0: {}, 1: {6}, 2: {5, 6}, 3: {4, 5, 6}, 10: {4, 5, 6}} {
		if got := pids(r.last(n)); !reflect.DeepEqual(got, want) {
			t.Errorf("last(%d) = %v, want %v", n, got, want)
		}
	}
}

func TestFeedSlowFollowerGap(t *testing.T) {
	f := newFeed(8)
	fast := f.subscribe(0)
	slow := f.subscribe(0)
	for i := 0; i < 5; i++ {
		f.add(evN(i))
	}
	if got, d := f.read(fast, nil); len(got) != 5 || d != 0 {
		t.Fatalf("fast read %v %d", pids(got), d)
	}
	for i := 5; i < 20; i++ {
		f.add(evN(i)) // never blocks, however slow the reader
	}
	got, dropped := f.read(slow, nil)
	if dropped != 12 || !reflect.DeepEqual(pids(got), []int{12, 13, 14, 15, 16, 17, 18, 19}) {
		t.Fatalf("slow read %v dropped=%d", pids(got), dropped)
	}
	// Notify is coalesced into one pending poke.
	select {
	case <-slow.notify:
	default:
		t.Fatal("no notification")
	}
	select {
	case <-slow.notify:
		t.Fatal("notifications must coalesce")
	default:
	}
	late := f.subscribe(3)
	if got, _ := f.read(late, nil); !reflect.DeepEqual(pids(got), []int{17, 18, 19}) {
		t.Fatalf("backlog %v", pids(got))
	}
	late2 := f.subscribe(100)
	if got, d := f.read(late2, nil); len(got) != 8 || d != 0 {
		t.Fatalf("backlog capped at ring: %v %d", pids(got), d)
	}
	f.unsubscribe(fast)
	f.unsubscribe(slow)
	if f.followers() != 2 {
		t.Fatal("unsubscribe")
	}
}

func TestRegistryStreamLifecycle(t *testing.T) {
	g := newRegistry()
	acc := proto.Event{Verb: proto.VerbAccepted, Job: "j1"}
	s := g.create("j1", proto.SubmitRequest{Argv: []string{"x"}, WrapperPID: 50}, acc)
	if got, _ := g.read(s, nil); len(got) != 1 || got[0].Verb != proto.VerbAccepted {
		t.Fatalf("first event %v", got)
	}
	if p := g.protectedPIDs(nil); !reflect.DeepEqual(p, []int{50}) {
		t.Fatalf("protected %v", p)
	}
	// Not yet in the loop: an empty state does not end the stream.
	empty := policy.State{}
	g.apply(stepResult{nowMS: 1, state: &empty})
	if isClosed(s.done) {
		t.Fatal("stream ended before the submit was stepped")
	}
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "j1"}})
	running := policy.State{Jobs: []policy.Job{{ID: "j1", State: policy.JobRunning, PID: 9, PGID: 9}}}
	g.apply(stepResult{nowMS: 2, state: &running, events: []proto.Event{{Verb: proto.VerbAdmitted, Job: "j1"}, {Verb: proto.VerbInfo, Job: "unknown"}}})
	if isClosed(s.done) {
		t.Fatal("running job's stream ended")
	}
	if got, _ := g.read(s, nil); len(got) != 1 || got[0].Verb != proto.VerbAdmitted {
		t.Fatalf("admitted %v", got)
	}
	if js, _ := g.jobStatus("j1"); js.PID != 9 || js.State != policy.JobRunning || js.Argv[0] != "x" {
		t.Fatalf("status %+v", js)
	}
	// Gap: more events than the ring holds.
	many := make([]proto.Event, jobRingSize+5)
	for i := range many {
		many[i] = proto.Event{Verb: proto.VerbInfo, Job: "j1", PID: i}
	}
	g.apply(stepResult{nowMS: 3, state: &running, events: many})
	got, dropped := g.read(s, nil)
	if dropped != 5 || len(got) != jobRingSize || got[0].PID != 5 {
		t.Fatalf("gap: dropped=%d len=%d first=%d", dropped, len(got), got[0].PID)
	}
	// Disconnect while running -> lost.
	if !g.streamClosed(s, true, false) {
		t.Fatal("disconnect of a running job is a lost wrapper")
	}
	// Exit processed -> terminal, and done ends status.
	g.markExit("j1", proto.ExitRequest{Code: 2})
	g.apply(stepResult{nowMS: 4, state: &empty, processed: []policy.InEvent{{Kind: policy.EvExit, Job: "j1"}}})
	js, _ := g.jobStatus("j1")
	if js.State != policy.JobDone || js.EndTS != 4 || *js.ExitCode != 2 {
		t.Fatalf("final %+v", js)
	}
	if p := g.protectedPIDs(nil); len(p) != 0 {
		t.Fatalf("finished wrapper still protected: %v", p)
	}
	if q, r, d := g.counts(); q != 0 || r != 0 || d != 1 {
		t.Fatalf("counts %d %d %d", q, r, d)
	}
	if rec := g.recent(5); len(rec) != 1 || rec[0].ID != "j1" {
		t.Fatalf("recent %+v", rec)
	}
}

func TestRegistryStreamClosedRules(t *testing.T) {
	cases := []struct {
		name                       string
		disconnected, stopping     bool
		terminal, exited, replaced bool
		want                       bool
	}{
		{"disconnect", true, false, false, false, false, true},
		{"clean end", false, false, false, false, false, false},
		{"shutdown", true, true, false, false, false, false},
		{"terminal", true, false, true, false, false, false},
		{"exit reported", true, false, false, true, false, false},
		{"replaced by attach", true, false, false, false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := newRegistry()
			s := g.create("j", proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted})
			if c.terminal {
				g.mu.Lock()
				g.terminateLocked(g.jobs["j"], 1)
				g.mu.Unlock()
			}
			if c.exited {
				g.markExit("j", proto.ExitRequest{})
			}
			if c.replaced {
				g.attach("j", proto.AttachRequest{}, proto.Event{Verb: proto.VerbAccepted})
				if !isClosed(s.done) {
					t.Fatal("old stream not ended by attach")
				}
			}
			if got := g.streamClosed(s, c.disconnected, c.stopping); got != c.want {
				t.Fatalf("lost=%v want %v", got, c.want)
			}
		})
	}
}

// #3: a done job never keeps a live enforcement stage; a governor kill
// shows as stage=killed.
func TestRegistryDoneStage(t *testing.T) {
	g := newRegistry()
	run := func(id string, killed bool) proto.JobStatus {
		g.create(id, proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted, Job: id})
		g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: id}})
		live := policy.State{Jobs: []policy.Job{{ID: id, State: policy.JobRunning,
			Enf: policy.Enforcement{Stage: policy.StageTerminating}}}}
		var evs []proto.Event
		if killed {
			evs = []proto.Event{{Verb: proto.VerbKilled, Job: id, Policy: "mem_limit"}}
		}
		g.apply(stepResult{nowMS: 1, state: &live, events: evs})
		if js, _ := g.jobStatus(id); js.Stage != policy.StageTerminating {
			t.Fatalf("%s live stage %q", id, js.Stage)
		}
		g.apply(stepResult{nowMS: 2, state: &policy.State{}})
		js, _ := g.jobStatus(id)
		if js.State != policy.JobDone {
			t.Fatalf("%s state %q", id, js.State)
		}
		return js
	}
	if js := run("k", true); js.Stage != policy.StageKilled || js.KilledBy != "mem_limit" {
		t.Fatalf("killed job: stage %q killed_by %q", js.Stage, js.KilledBy)
	}
	if js := run("t", false); js.Stage != "" {
		t.Fatalf("done job kept stage %q", js.Stage)
	}
}

func TestRegistryRefusedAndVanished(t *testing.T) {
	g := newRegistry()
	s := g.create("r", proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted})
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "r"}})
	st := policy.State{Jobs: []policy.Job{{ID: "r", State: policy.JobQueued}}}
	g.apply(stepResult{nowMS: 1, state: &st, events: []proto.Event{{Verb: proto.VerbRefused, Job: "r"}}})
	if !isClosed(s.done) {
		t.Fatal("REFUSED must end the stream even while the job is in state")
	}
	// A stepped job that is simply absent from the state ends too.
	s2 := g.create("v", proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted})
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "v"}})
	empty := policy.State{}
	g.apply(stepResult{nowMS: 2, state: &empty})
	if !isClosed(s2.done) {
		t.Fatal("vanished job's stream must end")
	}
	// A done job in state counts as not active.
	s3 := g.create("d", proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted})
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "d"}})
	done := policy.State{Jobs: []policy.Job{{ID: "d", State: policy.JobDone}}}
	g.apply(stepResult{nowMS: 3, state: &done})
	if !isClosed(s3.done) {
		t.Fatal("done job's stream must end")
	}
	// The policy knows a job the daemon does not: tracked without stream.
	ghost := policy.State{Jobs: []policy.Job{{ID: "ghost", State: policy.JobRunning}}}
	if !g.apply(stepResult{nowMS: 4, state: &ghost}) || !g.known("ghost") {
		t.Fatal("ghost job not tracked")
	}
	// abandon ends streams of submits that could not be stepped.
	s4 := g.create("p", proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted})
	g.abandon([]policy.InEvent{{Kind: policy.EvSubmit, Job: "p"}}, 5)
	if !isClosed(s4.done) {
		t.Fatal("abandoned stream still open")
	}
}

func TestRegistryPrune(t *testing.T) {
	g := newRegistry()
	now := int64(finishedRetainMS * 3)
	add := func(id string, end int64, live bool) {
		rec := &jobRec{id: id, events: newRing(jobRingSize), endMS: end, terminal: !live}
		g.jobs[id] = rec
		if live {
			g.live[id] = rec
		}
	}
	add("old", now-finishedRetainMS-1, false)
	add("fresh", now-1000, false)
	add("live-old", 1, true) // live jobs are never pruned
	g.prune(now)
	if g.known("old") || !g.known("fresh") || !g.known("live-old") {
		t.Fatalf("prune by age: %v", g.jobs)
	}
	for i := 0; i < maxFinishedJobs+10; i++ {
		add("n"+strconv.Itoa(i), now-int64(i), false)
	}
	g.prune(now)
	if len(g.jobs) != maxFinishedJobs+1 { // + the live one
		t.Fatalf("prune by count: %d", len(g.jobs))
	}
	if !g.known("n0") || g.known("n"+strconv.Itoa(maxFinishedJobs+9)) {
		t.Fatal("prune must keep the newest")
	}
}

func TestExportRestoreRoundTrip(t *testing.T) {
	g := newRegistry()
	s := g.create("a", proto.SubmitRequest{Argv: []string{"make"}, WrapperPID: 7}, proto.Event{Verb: proto.VerbAccepted, Job: "a"})
	_ = s
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "a"}})
	st := policy.State{Jobs: []policy.Job{{ID: "a", State: policy.JobRunning, PGID: 70}}}
	g.apply(stepResult{nowMS: 1, state: &st, events: []proto.Event{{Verb: proto.VerbAdmitted, Job: "a"}}})
	g.create("b", proto.SubmitRequest{Argv: []string{"b"}}, proto.Event{Verb: proto.VerbAccepted, Job: "b"})
	g.markInLoop([]policy.InEvent{{Kind: policy.EvSubmit, Job: "b"}})
	g.apply(stepResult{nowMS: 2, state: &st}) // b finished (absent)

	exp := g.export()
	b, err := json.Marshal(exp)
	if err != nil {
		t.Fatal(err)
	}
	var back []persistedJob
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	g2 := newRegistry()
	g2.restore(back, map[string]bool{"a": true})
	ja, _ := g2.jobStatus("a")
	jb, _ := g2.jobStatus("b")
	if len(ja.Events) != 2 || ja.Argv[0] != "make" || jb.State != policy.JobDone || len(jb.Events) != 1 {
		t.Fatalf("restored a=%+v b=%+v", ja, jb)
	}
	if p := g2.protectedPIDs(nil); !reflect.DeepEqual(p, []int{7}) {
		t.Fatalf("restored protected %v", p)
	}
	g2.mu.Lock()
	ra := g2.jobs["a"]
	if !ra.lost || ra.streaming || ra.terminal || g2.live["b"] != nil {
		t.Fatalf("restored flags lost=%v streaming=%v terminal=%v", ra.lost, ra.streaming, ra.terminal)
	}
	g2.mu.Unlock()
}

func TestExportBounds(t *testing.T) {
	g := newRegistry()
	for i := 0; i < persistFinished+20; i++ {
		id := "f" + strconv.Itoa(i)
		rec := &jobRec{id: id, events: newRing(jobRingSize), endMS: int64(i), terminal: true}
		for k := 0; k < jobRingSize; k++ {
			rec.events.add(evN(k))
		}
		g.jobs[id] = rec
	}
	exp := g.export()
	total := 0
	for _, j := range exp {
		total += len(j.Events)
	}
	if len(exp) != persistFinished || total > persistMaxEvents {
		t.Fatalf("export %d jobs %d events", len(exp), total)
	}
	if exp[0].ID != "f"+strconv.Itoa(persistFinished+19) {
		t.Fatalf("export must keep the newest first, got %s", exp[0].ID)
	}
}

func TestDefaultAssess(t *testing.T) {
	classes := []policy.ClassParams{{Name: "build", EstimateBytes: 2 << 30, Slots: 2}, {Name: "default", EstimateBytes: 1 << 30}}
	// AdmitBelowWarn: these figures use the plain 1G reserve (the warn-line
	// reserve is policy's, tested there).
	base := policy.Params{Mode: policy.ModeEnforce, ReserveBytes: 1 << 30, AdmitBelowWarn: true, WarnAvailPermille: 150, CritAvailPermille: 70, Classes: classes, DefaultClass: "default"}
	mach := platform.Machine{TotalBytes: 16 << 30, AvailBytes: 10 << 30, KernelLevel: 1}
	run := func(id, class string, reserved, used int64) policy.Job {
		return policy.Job{ID: id, Class: class, State: policy.JobRunning, ReservedBytes: reserved, UsedBytes: used}
	}
	cases := []struct {
		name      string
		p         func(*policy.Params)
		s         policy.State
		in        policy.Inputs
		capacity  int64
		avail     int64
		reserved  int64
		headroom  int64
		level     string
		admitting bool
		reasons   []string
		sugg      int
		build     int
	}{
		{name: "kernel figures", in: policy.Inputs{Machine: mach},
			capacity: 16 << 30, avail: 10 << 30, headroom: 9 << 30, level: "normal", admitting: true, sugg: 9, build: 2},
		{name: "capacity override uses scoped usage", p: func(p *policy.Params) { p.CapacityBytes = 8 << 30 }, in: policy.Inputs{Machine: mach, ScopedUsedBytes: 3 << 30},
			capacity: 8 << 30, avail: 5 << 30, headroom: 4 << 30, level: "normal", admitting: true, sugg: 4, build: 2},
		{name: "scoped usage above capacity clamps", p: func(p *policy.Params) { p.CapacityBytes = 4 << 30 }, in: policy.Inputs{Machine: mach, ScopedUsedBytes: 5 << 30},
			capacity: 4 << 30, avail: 0, headroom: 0, level: "critical", admitting: false, reasons: []string{"pressure", "headroom"}},
		{name: "unconsumed reservations and slots", in: policy.Inputs{Machine: mach},
			s:        policy.State{Jobs: []policy.Job{run("a", "build", 3<<30, 1<<30), run("b", "build", 1<<30, 2<<30), {ID: "q", Class: "build", State: policy.JobQueued, ReservedBytes: 5 << 30}}},
			capacity: 16 << 30, avail: 10 << 30, reserved: 2 << 30, headroom: 7 << 30, level: "normal", admitting: true, sugg: 7, build: 0},
		{name: "warn by avail threshold", in: policy.Inputs{Machine: platform.Machine{TotalBytes: 10 << 30, AvailBytes: 1 << 30}},
			capacity: 10 << 30, avail: 1 << 30, headroom: 0, level: "warn", admitting: false, reasons: []string{"pressure", "headroom"}},
		{name: "kernel critical folded in", p: func(p *policy.Params) { p.UseKernelLevel = true }, in: policy.Inputs{Machine: platform.Machine{TotalBytes: 16 << 30, AvailBytes: 10 << 30, KernelLevel: 4}},
			capacity: 16 << 30, avail: 10 << 30, headroom: 9 << 30, level: "critical", admitting: false, reasons: []string{"pressure"}},
		{name: "kernel level ignored when off", in: policy.Inputs{Machine: platform.Machine{TotalBytes: 16 << 30, AvailBytes: 10 << 30, KernelLevel: 4}},
			capacity: 16 << 30, avail: 10 << 30, headroom: 9 << 30, level: "normal", admitting: true, sugg: 9, build: 2},
		{name: "psi warn", p: func(p *policy.Params) { p.UseKernelLevel, p.PSIWarn10, p.PSICrit10 = true, 100, 50 }, in: policy.Inputs{Machine: platform.Machine{TotalBytes: 16 << 30, AvailBytes: 10 << 30, PSIMemSome10: 120, PSIMemFull10: 10}},
			capacity: 16 << 30, avail: 10 << 30, headroom: 9 << 30, level: "warn", admitting: false, reasons: []string{"pressure"}},
		{name: "observe always admits", p: func(p *policy.Params) { p.Mode = policy.ModeObserve }, in: policy.Inputs{Machine: mach}, s: policy.State{Pressure: policy.Pressure{Level: "critical"}},
			capacity: 16 << 30, avail: 10 << 30, headroom: 9 << 30, level: "critical", admitting: true, sugg: 9, build: 2},
		{name: "huge headroom capped", in: policy.Inputs{Machine: platform.Machine{TotalBytes: 1 << 40, AvailBytes: 1 << 40}},
			capacity: 1 << 40, avail: 1 << 40, headroom: 1<<40 - 1<<30, level: "normal", admitting: true, sugg: maxSuggested, build: 2},
		{name: "no sample", in: policy.Inputs{},
			level: "normal", admitting: false, reasons: []string{"headroom"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			if c.p != nil {
				c.p(&p)
			}
			a := DefaultAssess(p, c.s, c.in)
			if a.CapacityBytes != c.capacity || a.AvailBytes != c.avail || a.ReservedBytes != c.reserved || a.HeadroomBytes != c.headroom {
				t.Fatalf("cap=%d avail=%d reserved=%d headroom=%d; want %d %d %d %d", a.CapacityBytes, a.AvailBytes, a.ReservedBytes, a.HeadroomBytes, c.capacity, c.avail, c.reserved, c.headroom)
			}
			if a.Level != c.level || a.Admitting != c.admitting || !reflect.DeepEqual(a.NotAdmittingReasons, c.reasons) {
				t.Fatalf("level=%s admitting=%v reasons=%v", a.Level, a.Admitting, a.NotAdmittingReasons)
			}
			if a.SuggestedConcurrency != c.sugg || a.SuggestedByClass["build"] != c.build {
				t.Fatalf("suggested=%d build=%d", a.SuggestedConcurrency, a.SuggestedByClass["build"])
			}
			// Deterministic.
			if b := DefaultAssess(p, c.s, c.in); !reflect.DeepEqual(a, b) {
				t.Fatal("not deterministic")
			}
		})
	}
}

func TestInterval(t *testing.T) {
	d := &daemon{cfg: normalizeConfig(config.Resolved{SampleMS: 1000, FastMS: 250})}
	run := func(used, budget int64) policy.Job {
		return policy.Job{State: policy.JobRunning, UsedBytes: used, BudgetBytes: budget}
	}
	cases := []struct {
		name  string
		level string
		s     policy.State
		want  int64
	}{
		{"idle", "normal", policy.State{}, 1000},
		{"warn", "warn", policy.State{}, 250},
		{"critical", "critical", policy.State{}, 250},
		{"state level warn", "normal", policy.State{Pressure: policy.Pressure{Level: "warn"}}, 250},
		{"job at 79%", "normal", policy.State{Jobs: []policy.Job{run(79, 100)}}, 1000},
		{"job at 80%", "normal", policy.State{Jobs: []policy.Job{run(80, 100)}}, 250},
		{"job over budget", "normal", policy.State{Jobs: []policy.Job{run(200, 100)}}, 250},
		{"no budget", "normal", policy.State{Jobs: []policy.Job{run(1<<40, 0)}}, 1000},
		{"queued job ignored", "normal", policy.State{Jobs: []policy.Job{{State: policy.JobQueued, UsedBytes: 90, BudgetBytes: 100}}}, 1000},
	}
	for _, c := range cases {
		if got := d.interval(Assessment{Level: c.level}, c.s); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestNormalizeConfig(t *testing.T) {
	c := normalizeConfig(config.Resolved{})
	if c.SampleMS != 1000 || c.FastMS != 250 || c.HeartbeatMS != 60000 || c.LogMaxBytes != 64<<20 || c.LogKeep != 8 || c.Params.Mode != policy.ModeEnforce {
		t.Fatalf("defaults %+v", c)
	}
	c = normalizeConfig(config.Resolved{SampleMS: 100, FastMS: 500})
	if c.FastMS != 100 {
		t.Fatalf("fast interval must not exceed the normal one: %d", c.FastMS)
	}
}

// newTestDaemon builds an un-started daemon around a Fake for unit tests.
func newTestDaemon(t *testing.T) (*daemon, *platform.Fake) {
	t.Helper()
	f := platform.NewFake()
	d, err := newDaemon(Options{Config: baseConfig(shortDir(t)), Backend: f, Log: &memLog{}, NoSignals: true, NoSelfTune: true})
	if err != nil {
		t.Fatal(err)
	}
	return d, f
}

func TestExecuteSafety(t *testing.T) {
	self := os.Getpid()
	selfPG := syscall.Getpgrp()
	procs := []platform.Proc{
		{PID: 600, PGID: 600},
		{PID: 601, PGID: 600},
		{PID: 700, PGID: 700},
		{PID: 701, PGID: 700},
		{PID: 4242, PGID: 700}, // a wrapper inside unit group 700
		{PID: 800, PGID: 800, System: true},
		{PID: 801, PGID: 800},
	}
	term := int(syscall.SIGTERM)
	cases := []struct {
		name       string
		a          policy.Action
		wantSent   []platform.SentSignal
		wantBG     map[int]bool
		wantFaults int64
	}{
		{"group", policy.Action{Op: policy.OpSignalGroup, PGID: 600, Signal: term}, []platform.SentSignal{{PGID: 600, Sig: syscall.SIGTERM}}, nil, 0},
		{"group 0", policy.Action{Op: policy.OpSignalGroup, PGID: 0, Signal: term}, nil, nil, 1},
		{"group 1", policy.Action{Op: policy.OpSignalGroup, PGID: 1, Signal: term}, nil, nil, 1},
		{"group negative", policy.Action{Op: policy.OpSignalGroup, PGID: -5, Signal: term}, nil, nil, 1},
		{"own group", policy.Action{Op: policy.OpSignalGroup, PGID: selfPG, Signal: term}, nil, nil, 1},
		{"group with wrapper is split", policy.Action{Op: policy.OpSignalGroup, Unit: "pg:700", PGID: 700, Signal: term},
			[]platform.SentSignal{{PID: 700, Sig: syscall.SIGTERM}, {PID: 701, Sig: syscall.SIGTERM}}, nil, 1},
		{"unit group with system proc is split", policy.Action{Op: policy.OpSignalGroup, Unit: "pg:800", PGID: 800, Signal: term},
			[]platform.SentSignal{{PID: 801, Sig: syscall.SIGTERM}}, nil, 1},
		{"job membership overrides system", policy.Action{Op: policy.OpSignalGroup, Job: "j", PGID: 800, Signal: term},
			[]platform.SentSignal{{PGID: 800, Sig: syscall.SIGTERM}}, nil, 0},
		{"pids", policy.Action{Op: policy.OpSignal, PIDs: []int{601, 0, 1, self, 4242, 800}, Signal: term},
			[]platform.SentSignal{{PID: 601, Sig: syscall.SIGTERM}}, nil, 5},
		{"pids for job may hit system-flagged member", policy.Action{Op: policy.OpSignal, Job: "j", PIDs: []int{800}, Signal: term},
			[]platform.SentSignal{{PID: 800, Sig: syscall.SIGTERM}}, nil, 0},
		{"background by pgid skips wrapper", policy.Action{Op: policy.OpBackground, PGID: 700}, nil, map[int]bool{700: true, 701: true}, 1},
		{"foreground by pids", policy.Action{Op: policy.OpForeground, PIDs: []int{600, 1}}, nil, map[int]bool{600: false}, 1},
		{"unknown op", policy.Action{Op: "explode", PGID: 600}, nil, nil, 1},
	}
	// Attribution for unit actions: units pg:700 (700, 701; the wrapper 4242
	// is excluded) and pg:800 (801; the system process is no candidate). For
	// job actions: job j owns group 800, system process included.
	unitAttr := attrib.Result{
		Units: []policy.UnitSample{{Key: "pg:700", PGID: 700, PIDs: []int{700, 701}}, {Key: "pg:800", PGID: 800, PIDs: []int{801}}},
	}
	jobAttr := attrib.Result{JobPIDs: map[string][]int{"j": {800, 801}}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, f := newTestDaemon(t)
			if c.a.Job != "" {
				d.setAttribution(jobAttr, procs)
			} else {
				d.setAttribution(unitAttr, procs)
			}
			d.actProt = []int{4242}
			d.execute([]policy.Action{c.a}, 1)
			got := f.Signals()
			if len(got) != len(c.wantSent) || (len(got) > 0 && !reflect.DeepEqual(got, c.wantSent)) {
				t.Fatalf("sent %s, want %s", fmtSent(got), fmtSent(c.wantSent))
			}
			f.Lock()
			bg := map[int]bool{}
			for k, v := range f.Background {
				bg[k] = v
			}
			f.Unlock()
			if len(c.wantBG) > 0 || len(bg) > 0 {
				if !reflect.DeepEqual(bg, c.wantBG) {
					t.Fatalf("background %v, want %v", bg, c.wantBG)
				}
			}
			if n := d.faults.total.Load(); n != c.wantFaults {
				t.Fatalf("faults %d, want %d: %v", n, c.wantFaults, d.faults.active(time.Now().UnixMilli()))
			}
		})
	}
}

func TestStepPanicRecovered(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.step = func(policy.Params, policy.State, policy.Inputs) (policy.State, policy.Output) { panic("boom") }
	d.persist = newPersister(filepath.Join(d.home, "state.json"), d.fault)
	d.alerts = newAlertWriter(filepath.Join(d.home, "a.jsonl"), 1<<20, d.fault)
	defer d.persist.close()
	defer d.alerts.close()
	s := d.reg.create("j", proto.SubmitRequest{Argv: []string{"x"}}, proto.Event{Verb: proto.VerbAccepted})
	d.pending = []policy.InEvent{{Kind: policy.EvSubmit, Job: "j"}}
	d.st = policy.State{NextSeq: 7}
	d.runStep(true)
	if d.st.NextSeq != 7 || d.faults.count("step_panic") != 1 {
		t.Fatalf("state %+v faults %d", d.st, d.faults.count("step_panic"))
	}
	if !isClosed(s.done) {
		t.Fatal("submit of a panicking step must end its stream (wrapper fails open)")
	}
	if len(d.pending) != 0 {
		t.Fatal("events of a panicking step must be dropped, not retried forever")
	}
}

func TestCloneStateDeep(t *testing.T) {
	s := policy.State{
		Jobs:    []policy.Job{{ID: "a", Enf: policy.Enforcement{Warnings: []policy.Warning{{Policy: "p"}}}}},
		Units:   []policy.Unit{{Key: "u", Enf: policy.Enforcement{Warnings: []policy.Warning{{Policy: "q"}}}}},
		History: []policy.History{{Key: "h", Peaks: []int64{1}, DurMS: []int64{2}, LastMS: 7}},
	}
	orig, _ := json.Marshal(s)
	c := cloneState(s)
	if cj, _ := json.Marshal(c); !bytes.Equal(cj, orig) {
		t.Fatalf("clone differs from the original:\n%s\n%s", orig, cj)
	}
	c.Jobs[0].ID = "x"
	c.Jobs[0].Enf.Warnings[0].Policy = "x"
	c.Units[0].Enf.Warnings[0].Policy = "x"
	c.History[0].Peaks[0] = 99
	c.History[0].DurMS[0] = 99
	if now, _ := json.Marshal(s); !bytes.Equal(now, orig) {
		t.Fatalf("clone aliases the original:\n%s\n%s", orig, now)
	}
	if e := cloneState(policy.State{}); e.Jobs != nil || e.Units != nil || e.History != nil {
		t.Fatal("nil slices must stay nil (stable JSON)")
	}

	prio := 3
	in := policy.Inputs{
		Jobs:   []policy.JobSample{{ID: "a", ExtraPIDs: []int{1}}},
		Units:  []policy.UnitSample{{Key: "u", PIDs: []int{2}}},
		Events: []policy.InEvent{{Kind: policy.EvSubmit, Submit: &proto.SubmitRequest{Argv: []string{"x"}, Priority: &prio}}},
	}
	origIn, _ := json.Marshal(in)
	ci := cloneInputs(in)
	ci.Jobs[0].ExtraPIDs[0] = 9
	ci.Units[0].PIDs[0] = 9
	ci.Events[0].Submit.Argv[0] = "y"
	*ci.Events[0].Submit.Priority = 9
	if now, _ := json.Marshal(in); !bytes.Equal(now, origIn) {
		t.Fatalf("input clone aliases the original")
	}
}

func TestMaterialHash(t *testing.T) {
	base := policy.State{LastMS: 1, Jobs: []policy.Job{{ID: "a", State: policy.JobRunning, UsedBytes: 1, PeakBytes: 1, CPUMS: 1}}, Units: []policy.Unit{{Key: "u", UsedBytes: 1}}}
	h0 := materialHash(base)
	tele := cloneState(base)
	tele.LastMS, tele.Jobs[0].UsedBytes, tele.Jobs[0].PeakBytes, tele.Jobs[0].CPUMS, tele.Jobs[0].ReservedBytes = 99, 99, 99, 99, 99
	tele.Jobs[0].LastProgressMS, tele.Jobs[0].OutputBytes, tele.Jobs[0].NProcs, tele.Units[0].UsedBytes, tele.Units[0].NProcs = 9, 9, 9, 9, 9
	if materialHash(tele) != h0 {
		t.Fatal("telemetry changed the material hash")
	}
	for name, mut := range map[string]func(*policy.State){
		"job state":  func(s *policy.State) { s.Jobs[0].State = policy.JobQueued },
		"stage":      func(s *policy.State) { s.Jobs[0].Enf.Stage = policy.StageFrozen },
		"warning":    func(s *policy.State) { s.Jobs[0].Enf.Warnings = []policy.Warning{{Policy: "p"}} },
		"pressure":   func(s *policy.State) { s.Pressure.Level = "warn" },
		"new job":    func(s *policy.State) { s.Jobs = append(s.Jobs, policy.Job{ID: "b"}) },
		"history":    func(s *policy.State) { s.History = []policy.History{{Key: "k"}} },
		"wrapper":    func(s *policy.State) { s.Jobs[0].WrapperLostMS = 5 },
		"unit stage": func(s *policy.State) { s.Units[0].Enf.Stage = policy.StageTerminating },
	} {
		m := cloneState(base)
		mut(&m)
		if materialHash(m) == h0 {
			t.Errorf("%s: material change not detected", name)
		}
	}
	if materialHash(base) != h0 {
		t.Fatal("hashing mutated its input")
	}
	if !strings.HasPrefix(stateHash(base), "sha256:") || stateHash(base) != stateHash(cloneState(base)) {
		t.Fatal("stateHash")
	}
}

func TestDecodeLimitsAndErrors(t *testing.T) {
	d, _ := newTestDaemon(t)
	cases := []struct {
		name string
		body string
		code int
	}{
		{"too large", `{"argv":["` + strings.Repeat("a", maxBodyBytes) + `"]}`, 413},
		{"trailing garbage after object", `{"argv":["x"]} {"bogus"`, 400},
		{"not json", `hello`, 400},
		{"empty", ``, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/v1/jobs", strings.NewReader(c.body))
			d.hSubmit(w, r)
			if w.Code != c.code {
				t.Fatalf("code %d (%s), want %d", w.Code, w.Body.String(), c.code)
			}
		})
	}
}

func TestValidJobID(t *testing.T) {
	for id, ok := range map[string]bool{"j1-2": true, "J_x-9": true, "": false, "a b": false, "../x": false, "a/b": false, strings.Repeat("a", 64): true, strings.Repeat("a", 65): false, "é": false} {
		if validJobID(id) != ok {
			t.Errorf("validJobID(%q) != %v", id, ok)
		}
	}
}

func TestNewJobIDUnique(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.nowFn = func() int64 { return 1_700_000_000_000 }
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := d.newJobID()
		if seen[id] || !validJobID(id) || !strings.HasPrefix(id, "j"+strconv.FormatInt(1_700_000_000_000, 36)+"-") {
			t.Fatalf("id %q", id)
		}
		seen[id] = true
	}
}

func TestFaultSet(t *testing.T) {
	f := newFaultSet()
	f.record("b", "two", 1000)
	f.record("a", "one", 1000)
	f.record("a", "one again", 2000)
	got := f.active(2000)
	if len(got) != 2 || !strings.HasPrefix(got[0], "a: one again (count=2") || !strings.HasPrefix(got[1], "b: two (count=1") {
		t.Fatalf("active %v", got)
	}
	if got := f.active(2000 + faultWindowMS + 1); len(got) != 0 {
		t.Fatalf("faults must age out of status: %v", got)
	}
	if f.total.Load() != 3 {
		t.Fatal("total")
	}
	for i := 0; i < 2*maxFaultKinds; i++ {
		f.record("k"+strconv.Itoa(i), strings.Repeat("x", 2000), 3000)
	}
	f.mu.Lock()
	n := len(f.m)
	long := len(f.m["a"].msg)
	f.mu.Unlock()
	if n > maxFaultKinds+1 || long > 600 {
		t.Fatalf("fault set unbounded: kinds=%d", n)
	}
}

func TestMetricsLabelBounds(t *testing.T) {
	m := newMetrics()
	for i := 0; i < 3*maxLabelValues; i++ {
		m.countEvent(&proto.Event{Verb: "V" + strconv.Itoa(i)})
	}
	if len(m.events) > maxLabelValues+1 || m.events["other"] == 0 {
		t.Fatalf("label cardinality %d", len(m.events))
	}
	m.countEvent(&proto.Event{Verb: proto.VerbKilled})
	if m.kills["unknown"] != 1 {
		t.Fatal("kill without policy")
	}
	if labelValue(`a"b\c`+"\n") != `a\"b\\c\n` {
		t.Fatal("label escaping")
	}
}

func TestAlertWriterRotationAndGap(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "alerts.jsonl")
	a := newAlertWriter(path, 300, func(string, string) {})
	for i := 0; i < 10; i++ {
		a.write(&proto.Event{V: 1, Verb: proto.VerbWarn, Msg: strings.Repeat("x", 40), PID: i + 2})
	}
	a.close()
	cur, _ := os.ReadFile(path)
	old, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatal("no rotated file")
	}
	if len(cur) > 300 || len(old) > 300 {
		t.Fatalf("rotation: cur=%d old=%d", len(cur), len(old))
	}
	if !bytes.HasSuffix(cur, []byte("\n")) || !strings.Contains(string(cur), `"pid":11`) {
		t.Fatalf("latest alert missing: %s", cur)
	}

	// Gap marker after drops.
	path2 := filepath.Join(dir, "b.jsonl")
	b := &alertWriter{path: path2, maxBytes: 1 << 20, ch: make(chan []byte, 1), done: make(chan struct{}), fault: func(string, string) {}}
	b.write(&proto.Event{Verb: proto.VerbWarn})
	b.write(&proto.Event{Verb: proto.VerbWarn}) // dropped: channel full, writer not running
	b.write(&proto.Event{Verb: proto.VerbWarn}) // dropped
	go b.run()
	b.close()
	out, _ := os.ReadFile(path2)
	if !strings.Contains(string(out), "alerts gap dropped=2") {
		t.Fatalf("no gap marker: %s", out)
	}
}

func TestPersisterWriteAtomic(t *testing.T) {
	dir := shortDir(t)
	path := filepath.Join(dir, "state.json")
	p := newPersister(path, func(k, m string) { t.Errorf("%s: %s", k, m) })
	defer p.close()
	for i, content := range []string{"one", "two-longer", "3", "four"} {
		durable := i%2 == 0
		if placed, err := p.write([]byte(content), uint64(i+1), durable); err != nil || !placed {
			t.Fatal(placed, err)
		}
		if b, _ := os.ReadFile(path); string(b) != content {
			t.Fatalf("content %q", b)
		}
		// .prev is the last durable snapshot: an unsynced write leaves it.
		want := content
		if !durable {
			want = []string{"one", "two-longer", "3", "four"}[i-1]
		}
		if b, _ := os.ReadFile(path + ".prev"); string(b) != want {
			t.Fatalf("after %q: .prev %q, want %q", content, b, want)
		}
	}
	// An older snapshot never replaces a newer one.
	if placed, err := p.write([]byte("old"), 2, true); err != nil || placed {
		t.Fatal(placed, err)
	}
	if b, _ := os.ReadFile(path); string(b) != "four" {
		t.Fatalf("an older snapshot replaced the newer one: %q", b)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	m, _ := filepath.Glob(filepath.Join(dir, ".*.tmp"))
	if len(m) != 0 {
		t.Fatalf("temp files left: %v", m)
	}
	q := newPersister(filepath.Join(dir, "missing", "x"), func(string, string) {})
	defer q.close()
	if _, err := q.write([]byte("x"), 1, true); err == nil {
		t.Fatal("write into a missing dir must fail")
	}
}

func TestPersisterKeepsNewest(t *testing.T) {
	dir := shortDir(t)
	p := newPersister(filepath.Join(dir, "s.json"), func(k, m string) { t.Errorf("%s: %s", k, m) })
	for i := 0; i < 100; i++ {
		p.submit([]byte(strconv.Itoa(i)))
	}
	p.close()
	b, _ := os.ReadFile(filepath.Join(dir, "s.json"))
	if string(b) != "99" {
		t.Fatalf("persisted %q, want the newest", b)
	}
	if p.writes > 100 || p.writes < 1 {
		t.Fatalf("writes %d", p.writes)
	}
}

func TestLoadStateInjectsLostForActiveJobs(t *testing.T) {
	d, _ := newTestDaemon(t)
	sf := stateFile{V: 1, LogSeq: 41, IDSeq: 9, State: policy.State{NextSeq: 3, Jobs: []policy.Job{
		{ID: "q", State: policy.JobQueued}, {ID: "r", State: policy.JobRunning}, {ID: "d", State: policy.JobDone},
	}}}
	b, _ := json.Marshal(sf)
	if err := os.WriteFile(d.statePath, b, 0o600); err != nil {
		t.Fatal(err)
	}
	d.loadState()
	want := []policy.InEvent{{Kind: policy.EvLost, Job: "q"}, {Kind: policy.EvLost, Job: "r"}}
	if !reflect.DeepEqual(d.pending, want) || d.logSeq != 41 || d.idSeq.Load() != 9 || d.st.NextSeq != 3 {
		t.Fatalf("pending %+v seq %d id %d", d.pending, d.logSeq, d.idSeq.Load())
	}
	if d.faults.total.Load() != 0 {
		t.Fatal("good state recorded a fault")
	}
}

func TestHealthzStalled(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.curIvMS.Store(1000)
	d.lastStep.Store(time.Now().UnixMilli())
	w := httptest.NewRecorder()
	d.hHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	d.lastStep.Store(time.Now().UnixMilli() - 11_000)
	w = httptest.NewRecorder()
	d.hHealthz(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "stalled") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestActiveWarnings(t *testing.T) {
	d, _ := newTestDaemon(t)
	announced := proto.Event{V: 1, TS: 5, Verb: proto.VerbWarn, Job: "a", Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(9)}
	d.lastWarn[warnKey("a", "", proto.PolicyMemBudget)] = announced
	d.lastWarn[warnKey("gone", "", proto.PolicyStuck)] = proto.Event{Verb: proto.VerbWarn, Job: "gone"}
	d.lastWarn[warnKey("", "", proto.PolicyPressure)] = proto.Event{Verb: proto.VerbWarn, Policy: proto.PolicyPressure}
	s := policy.State{
		Pressure: policy.Pressure{Level: proto.LevelWarn},
		Jobs:     []policy.Job{{ID: "a", Class: "build", Enf: policy.Enforcement{Warnings: []policy.Warning{{Policy: proto.PolicyMemBudget, IssuedMS: 5, DeadlineMS: 1000}}}}},
		Units:    []policy.Unit{{Key: "pg:9:1", Enf: policy.Enforcement{Warnings: []policy.Warning{{Policy: proto.PolicyPressure, IssuedMS: 7, DeadlineMS: 50}}}}},
	}
	got := d.activeWarnings(400, s)
	if len(got) != 3 {
		t.Fatalf("warnings %+v", got)
	}
	if got[0].Job != "a" || *got[0].UsedBytes != 9 || *got[0].InMS != 600 {
		t.Fatalf("job warning %+v", got[0])
	}
	if got[1].Unit != "pg:9" || *got[1].InMS != 0 || got[1].TS != 7 {
		t.Fatalf("unit warning %+v", got[1])
	}
	if got[2].Policy != proto.PolicyPressure || got[2].Job != "" {
		t.Fatalf("machine warning %+v", got[2])
	}
	if _, ok := d.lastWarn[warnKey("gone", "", proto.PolicyStuck)]; ok {
		t.Fatal("cleared warning not forgotten")
	}
	if announced.InMS != nil {
		t.Fatal("stored event mutated")
	}
	// Pressure back to normal: machine warning drops out.
	s.Pressure.Level = proto.LevelNormal
	if got := d.activeWarnings(400, s); len(got) != 2 {
		t.Fatalf("after recovery %+v", got)
	}
}

func TestTopUnits(t *testing.T) {
	var in policy.Inputs
	for i := 0; i < 30; i++ {
		in.Units = append(in.Units, policy.UnitSample{Key: "pg:" + strconv.Itoa(100+i), UsedBytes: int64(i % 10)})
	}
	s := policy.State{Units: []policy.Unit{{Key: "pg:109", Enf: policy.Enforcement{Stage: policy.StageFrozen}}}}
	got := topUnits(policy.Params{UnmanagedPriority: 2}, s, in)
	if len(got) != topUnmanaged {
		t.Fatal(len(got))
	}
	// 9,9,9 (keys 109,119,129) first, ties by key.
	if got[0].Key != "pg:109" || got[1].Key != "pg:119" || got[2].Key != "pg:129" || got[0].Stage != policy.StageFrozen || got[0].Priority != 2 {
		t.Fatalf("order %+v", got[:3])
	}
	// The status key is the contract's pg:<pgid>, without the internal
	// start-time suffix; the stage is still found by the internal key.
	in2 := policy.Inputs{Units: []policy.UnitSample{{Key: "pg:7:123", PGID: 7, UsedBytes: 1}}}
	s2 := policy.State{Units: []policy.Unit{{Key: "pg:7:123", Enf: policy.Enforcement{Stage: policy.StageThrottled}}}}
	if got := topUnits(policy.Params{}, s2, in2); len(got) != 1 || got[0].Key != "pg:7" || got[0].Stage != policy.StageThrottled {
		t.Fatalf("public key %+v", got)
	}
	if topUnits(policy.Params{}, s, policy.Inputs{}) != nil {
		t.Fatal("no units -> nil")
	}
}

func TestPeerCredOfNonUnix(t *testing.T) {
	if _, err := connPeerCred(nil); err == nil {
		t.Fatal("nil conn must error")
	}
}

var _ = dlog.KindStep
