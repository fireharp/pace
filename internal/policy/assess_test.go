package policy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

func TestMemLevel(t *testing.T) {
	base := func() (Params, Inputs) {
		p := testParams()
		in := Inputs{Machine: platform.Machine{TotalBytes: 32 * GiB, AvailBytes: 20 * GiB, KernelLevel: 1, PSIMemSome10: -1, PSIMemFull10: -1}}
		return p, in
	}
	cases := []struct {
		name string
		mod  func(p *Params, in *Inputs)
		want string
	}{
		{"override normal", func(p *Params, in *Inputs) { in.ScopedUsedBytes = 10 * GiB }, proto.LevelNormal},
		{"override warn", func(p *Params, in *Inputs) { in.ScopedUsedBytes = 14 * GiB }, proto.LevelWarn},
		{"override critical", func(p *Params, in *Inputs) { in.ScopedUsedBytes = 15 * GiB }, proto.LevelCritical},
		{"override negative avail", func(p *Params, in *Inputs) { in.ScopedUsedBytes = 40 * GiB }, proto.LevelCritical},
		{"boundary: avail == warn threshold is normal", func(p *Params, in *Inputs) {
			in.ScopedUsedBytes = 16*GiB - 16*GiB*150/1000
		}, proto.LevelNormal},
		{"machine avail", func(p *Params, in *Inputs) { p.CapacityBytes = 0; in.Machine.AvailBytes = 3 * GiB }, proto.LevelWarn},
		{"machine unknown", func(p *Params, in *Inputs) { p.CapacityBytes = 0; in.Machine.TotalBytes = 0; in.Machine.AvailBytes = 0 }, proto.LevelNormal},
		{"kernel ignored without flag", func(p *Params, in *Inputs) { in.Machine.KernelLevel = 4 }, proto.LevelNormal},
		{"kernel warn", func(p *Params, in *Inputs) { p.UseKernelLevel = true; in.Machine.KernelLevel = 2 }, proto.LevelWarn},
		{"kernel critical", func(p *Params, in *Inputs) { p.UseKernelLevel = true; in.Machine.KernelLevel = 4 }, proto.LevelCritical},
		{"kernel does not lower", func(p *Params, in *Inputs) {
			p.UseKernelLevel = true
			in.ScopedUsedBytes = 15 * GiB
		}, proto.LevelCritical},
		{"psi some warn", func(p *Params, in *Inputs) { p.UseKernelLevel = true; in.Machine.PSIMemSome10 = 100 }, proto.LevelWarn},
		{"psi full critical", func(p *Params, in *Inputs) { p.UseKernelLevel = true; in.Machine.PSIMemFull10 = 50 }, proto.LevelCritical},
		{"psi below", func(p *Params, in *Inputs) {
			p.UseKernelLevel = true
			in.Machine.PSIMemSome10, in.Machine.PSIMemFull10 = 99, 49
		}, proto.LevelNormal},
		{"psi ignored without flag", func(p *Params, in *Inputs) { in.Machine.PSIMemFull10 = 900 }, proto.LevelNormal},
		{"psi disabled thresholds", func(p *Params, in *Inputs) {
			p.UseKernelLevel = true
			p.PSIWarn10, p.PSICrit10 = 0, 0
			in.Machine.PSIMemSome10, in.Machine.PSIMemFull10 = 999, 999
		}, proto.LevelNormal},
		{"avail thresholds disabled", func(p *Params, in *Inputs) {
			p.WarnAvailPermille, p.CritAvailPermille = 0, 0
			in.ScopedUsedBytes = 16 * GiB
		}, proto.LevelNormal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, in := base()
			tc.mod(&p, &in)
			eq(t, "level", memLevel(&p, &in), tc.want)
		})
	}
}

func TestDiskAndCPUHelpers(t *testing.T) {
	p := testParams()
	for _, tc := range []struct {
		free int64
		want string
	}{{-1, proto.LevelNormal}, {0, proto.LevelCritical}, {3*GiB - 1, proto.LevelCritical}, {3 * GiB, proto.LevelWarn}, {10 * GiB, proto.LevelNormal}} {
		eq(t, "disk", diskLevel(&p, &platform.Machine{DiskFreeBytes: tc.free}), tc.want)
	}
	p.DiskCritFreeBytes, p.DiskWarnFreeBytes = 0, 0
	eq(t, "disk disabled", diskLevel(&p, &platform.Machine{DiskFreeBytes: 0}), proto.LevelNormal)
	eq(t, "cpu", cpuPerCore(&platform.Machine{Load1Milli: 12_000, NCPU: 8}), int64(1500))
	eq(t, "cpu unknown", cpuPerCore(&platform.Machine{Load1Milli: 12_000}), int64(-1))
	eq(t, "maxLevel", maxLevel(proto.LevelCritical, proto.LevelWarn), proto.LevelCritical)
}

func TestAssess(t *testing.T) {
	p := testParams()
	in := Inputs{NowMS: T0, ScopedUsedBytes: 4 * GiB, Machine: platform.Machine{NCPU: 8, Load1Milli: 4000, DiskFreeBytes: 50 * GiB, PSIMemSome10: -1, PSIMemFull10: -1}}
	s := State{Jobs: []Job{
		{ID: "r1", Seq: 1, Class: "build", State: JobRunning, EstimateBytes: 2 * GiB, ReservedBytes: 3 * GiB, UsedBytes: 1 * GiB},
		{ID: "r2", Seq: 2, Class: "build", State: JobRunning, EstimateBytes: 2 * GiB, ReservedBytes: 2 * GiB, UsedBytes: 5 * GiB}, // over-consumed
		{ID: "q1", Seq: 3, Class: "test", State: JobQueued, EstimateBytes: 1 * GiB},
	}}
	sCopy := mustJSON(t, s)
	a := Assess(p, s, in)
	eq(t, "state untouched", mustJSON(t, s), sCopy)
	eq(t, "capacity", a.CapacityBytes, 16*GiB)
	eq(t, "avail", a.AvailBytes, 12*GiB)
	eq(t, "reserved", a.ReservedBytes, 2*GiB)        // r1: 3-1; r2: max(0, 2-5)
	eq(t, "reserve", a.ReserveBytes, warnB)          // max(2G reserve, 15% warn line)
	eq(t, "headroom", a.HeadroomBytes, 10*GiB-warnB) // 12 - 2 - 2.4
	eq(t, "level", a.Level, proto.LevelNormal)
	eq(t, "disk", a.DiskLevel, proto.LevelNormal)
	eq(t, "cpu", a.CPUPerCore, int64(500))
	eq(t, "admitting", a.Admitting, true)
	// left after queue = 6.6G. build: 6.6/2=3 capped by 2 slots - 2 running = 0.
	want := map[string]int{"build": 0, "default": 6, "model": 1, "test": 4}
	eq(t, "by class", a.SuggestedByClass, want)
	eq(t, "suggested", a.Suggested, 6)
	if _, err := json.Marshal(a); err != nil {
		t.Fatal(err)
	}

	t.Run("paused", func(t *testing.T) {
		in2 := in
		in2.Machine.DiskFreeBytes = 1 * GiB
		in2.ScopedUsedBytes = 14 * GiB
		a := Assess(p, s, in2)
		eq(t, "admitting", a.Admitting, false)
		eq(t, "reasons", a.Reasons, []string{proto.ReasonDisk, proto.ReasonPressure})
		eq(t, "suggested", a.Suggested, 0)
		eq(t, "by class", a.SuggestedByClass["test"], 0)
	})
	t.Run("pressure outstanding", func(t *testing.T) {
		s2 := cloneState(s)
		s2.Units = []Unit{{Key: "pg:1:1", Enf: Enforcement{Stage: StageFrozen}}}
		a := Assess(p, s2, in)
		eq(t, "reasons", a.Reasons, []string{proto.ReasonPressure})
	})
	t.Run("observe admits", func(t *testing.T) {
		p2 := p
		p2.Mode = ModeObserve
		in2 := in
		in2.ScopedUsedBytes = 15 * GiB
		a := Assess(p2, s, in2)
		eq(t, "admitting", a.Admitting, true)
		eq(t, "reasons", len(a.Reasons), 0)
		eq(t, "suggested still honest", a.Suggested, 0)
	})
	t.Run("no estimate, no slots -> capped", func(t *testing.T) {
		p2 := p
		p2.Classes = []ClassParams{{Name: "default"}}
		a := Assess(p2, State{}, in)
		eq(t, "cap", a.Suggested, maxSuggest)
	})
	t.Run("no default class configured", func(t *testing.T) {
		p2 := p
		p2.DefaultClass = ""
		p2.Classes = nil
		a := Assess(p2, State{}, in)
		eq(t, "suggested", a.Suggested, maxSuggest)
		eq(t, "by class", len(a.SuggestedByClass), 0)
	})
}

func TestLearnHelpers(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{nil, ""},
		{[]string{""}, ""},
		{[]string{"  "}, ""},
		{[]string{"make"}, "make"},
		{[]string{"make", "-j8"}, "make"},
		{[]string{"/usr/local/bin/go", "test", "./..."}, "go test"},
		{[]string{"npm", "", "--silent", "run", "build"}, "npm run"},
		{[]string{"./x.sh", "-v", "--", "deploy"}, "x.sh deploy"},
	} {
		eq(t, "DeriveKey", DeriveKey(tc.argv), tc.want)
	}
	for _, tc := range []struct {
		vals     []int64
		permille int64
		want     int64
	}{
		{nil, 900, 0},
		{[]int64{5}, 900, 5},
		{[]int64{3, 1, 2}, 900, 3},
		{[]int64{3, 1, 2}, 500, 2},
		{[]int64{4, 1, 3, 2}, 500, 2},
		{[]int64{10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, 900, 9},
		{[]int64{1, 2}, 0, 1},
		{[]int64{1, 2}, 2000, 2},
	} {
		in := append([]int64(nil), tc.vals...)
		eq(t, "nearestRank", nearestRank(in, tc.permille), tc.want)
		eq(t, "input untouched", in, tc.vals)
	}
	eq(t, "margin", withMargin(1000, 100), int64(1100))
	eq(t, "margin ceil", withMargin(1001, 100), int64(1102))
	eq(t, "margin zero", withMargin(0, 100), int64(0))
	eq(t, "margin negative", withMargin(1000, -5), int64(1000))

	p := Params{}
	eq(t, "default size", historySize(&p), DefaultHistorySize)
	eq(t, "default min", historyMinSamples(&p), DefaultHistoryMinSamples)
	s := State{}
	recordHistory(&p, &s, "", 1, 1, 1) // ignored
	recordHistory(&p, &s, "key:b", 1, 1, 1)
	recordHistory(&p, &s, "key:a", 2, 2, 2)
	recordHistory(&p, &s, "class:z", 3, 3, 3)
	eq(t, "sorted", []string{s.History[0].Key, s.History[1].Key, s.History[2].Key}, []string{"class:z", "key:a", "key:b"})
	// Repairs a ragged entry.
	s.History[1].DurMS = nil
	recordHistory(&p, &s, "key:a", 4, 4, 4)
	eq(t, "repaired", s.History[1].DurMS, []int64{0, 4})
	if _, ok := learnedDuration(&p, &s, "key:none"); ok {
		t.Fatal("no history must not be trusted")
	}
	s.History[1].DurMS = []int64{0, 4, 5}
	s.History[1].Peaks = []int64{1, 1, 1}
	if _, ok := learnedDuration(&p, &s, "key:a"); ok {
		t.Fatal("zero durations do not count as samples")
	}
	cls := classFor(&Params{}, "x")
	eq(t, "fallback class", cls, ClassParams{Name: "default", Priority: defaultPriority})
}

func TestCloneIsDeep(t *testing.T) {
	s := State{
		Jobs:    []Job{{ID: "a", Enf: Enforcement{Warnings: []Warning{{Policy: "x"}}}}},
		Units:   []Unit{{Key: "u", Enf: Enforcement{Warnings: []Warning{{Policy: "y"}}}}},
		History: []History{{Key: "k", Peaks: []int64{1}, DurMS: []int64{2}}},
	}
	c := cloneState(s)
	if !reflect.DeepEqual(c, s) {
		t.Fatal("clone differs")
	}
	c.Jobs[0].Enf.Warnings[0].Policy = "changed"
	c.Units[0].Enf.Warnings[0].Policy = "changed"
	c.History[0].Peaks[0] = 99
	c.History[0].DurMS[0] = 99
	c.Jobs[0].ID = "z"
	if s.Jobs[0].Enf.Warnings[0].Policy != "x" || s.Units[0].Enf.Warnings[0].Policy != "y" ||
		s.History[0].Peaks[0] != 1 || s.History[0].DurMS[0] != 2 || s.Jobs[0].ID != "a" {
		t.Fatalf("clone aliased the original: %+v", s)
	}
	eq(t, "empty", cloneState(State{}), State{})
}

func TestHelpersEdgeCases(t *testing.T) {
	eq(t, "sortedPIDs", sortedPIDs([]int{5, 3, 5, 1, 0, -2, 3}), []int{3, 5})
	eq(t, "sortedPIDs empty", sortedPIDs([]int{1, 0}), []int(nil))
	e := &Enforcement{}
	removeWarning(e, "nope")
	addWarning(e, Warning{Policy: "b", DeadlineMS: 1})
	addWarning(e, Warning{Policy: "a"})
	addWarning(e, Warning{Policy: "b", DeadlineMS: 2})
	eq(t, "warnings", e.Warnings, []Warning{{Policy: "a"}, {Policy: "b", DeadlineMS: 2}})
	removeWarning(e, "a")
	removeWarning(e, "b")
	if e.Warnings != nil {
		t.Fatal("empty warnings should be nil")
	}
	if (Output{}).Empty() != true {
		t.Fatal("empty output")
	}
}

func TestStepOnEmptyState(t *testing.T) {
	p := testParams()
	ns, out := Step(p, State{}, Inputs{NowMS: T0, Machine: platform.Machine{DiskFreeBytes: -1, PSIMemFull10: -1, PSIMemSome10: -1}})
	if !out.Empty() {
		t.Fatalf("unexpected output: %+v", out)
	}
	eq(t, "level", ns.Pressure.Level, proto.LevelNormal)
	eq(t, "normal since", ns.Pressure.NormalSinceMS, T0)
	eq(t, "last", ns.LastMS, T0)
	// Starting under pressure announces it.
	_, out = Step(p, State{}, Inputs{NowMS: T0, ScopedUsedBytes: 16 * GiB, Machine: platform.Machine{DiskFreeBytes: -1}})
	if len(out.Events) != 1 || out.Events[0].Level != proto.LevelCritical {
		t.Fatalf("initial critical: %+v", out.Events)
	}
	// Mode "" behaves as enforce.
	p.Mode = ""
	m := newSim(t, p)
	m.admitStart("j", proto.SubmitRequest{}, 100, 5*GiB)
	if !hasAct(m.lastOut.Actions, "j", OpSignalGroup, sigTERM) {
		t.Fatal("empty mode must enforce")
	}
}
