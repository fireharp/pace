package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
)

const ts0 = int64(1_763_800_000_000) // fixed "now" for deterministic ages

func richStatus() proto.Status {
	return proto.Status{
		V: 1, TS: ts0, Version: "v0.1.0", Platform: "darwin", Mode: "enforce",
		CapacityBytes: 36 << 30, AvailBytes: 6 << 30, ReserveBytes: 4608 << 20, ReservedBytes: 3 << 30,
		HeadroomBytes: 0, Pressure: proto.LevelWarn, KernelLevel: 2, SwapUsedBytes: 7900 << 20,
		Load1Milli: 9250, NCPU: 12, DiskFreeBytes: 120 << 30,
		Admitting: false, NotAdmittingReasons: []string{"pressure", "headroom"},
		SuggestedConcurrency: 1, SuggestedByClass: map[string]int{"test": 1, "build": 0, "browser": 1},
		Running: []proto.JobStatus{
			{ID: "j3", Class: "build", State: "running", Priority: 5, Argv: []string{"cargo", "build", "--release"},
				PID: 100, PGID: 100, NProcs: 14, EstimateBytes: 4 << 30, BudgetBytes: 8 << 30, ReservedBytes: 6 << 30,
				UsedBytes: 5600 << 20, PeakBytes: 5800 << 20, SubmitTS: ts0 - 200_000, StartTS: ts0 - 150_000, Stage: "throttled"},
			{ID: "j4", Class: "test", State: "running", Priority: 3, Argv: []string{"npm", "test", "--", strings.Repeat("x", 80)},
				UsedBytes: 900 << 20, BudgetBytes: 2 << 30, SubmitTS: ts0 - 5000, StartTS: ts0 - 4000},
		},
		Queued: []proto.JobStatus{
			{ID: "j5", Class: "test", State: "queued", Priority: 5, Argv: []string{"go", "test", "./..."}, EstimateBytes: 2 << 30,
				Position: 1, SubmitTS: ts0 - 40_000},
		},
		Unmanaged: []proto.UnitStatus{
			{Key: "pg:900", PGID: 900, LeaderPID: 900, Comm: "Google Chrome", NProcs: 82, UsedBytes: 12 << 30, StartTS: ts0 - 3_600_000, Priority: 2},
			{Key: "pg:901", PGID: 901, Comm: "node", NProcs: 3, UsedBytes: 400 << 20, StartTS: ts0 - 60_000, Priority: 2, Stage: "frozen"},
		},
		UnmanagedBelowMinVictim: &proto.SmallUnitsStatus{Groups: 24, UsedBytes: 1224 << 20, MinVictimBytes: 64 << 20},
		Warnings: []proto.Event{
			{V: 1, TS: ts0, Verb: proto.VerbWarn, Job: "j3", Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(5600 << 20),
				BudgetBytes: proto.I64(6 << 30), Next: "kill", InMS: proto.I64(20_000), Hint: "lower parallelism now or the job will be killed"},
		},
		Faults: []string{"sampler: 1 slow sample (12ms)"},
	}
}

func sampleJob() proto.JobStatus {
	return proto.JobStatus{
		ID: "j7", Class: "build", State: "done", Priority: 5, Argv: []string{"go", "build", "./..."},
		PID: 4242, PGID: 4242, NProcs: 3, EstimateBytes: 2 << 30, BudgetBytes: 4 << 30, ReservedBytes: 2 << 30,
		UsedBytes: 0, PeakBytes: 9100 << 20, SubmitTS: ts0 - 60_000, StartTS: ts0 - 59_000, EndTS: ts0 - 1000,
		ExitCode: proto.Int(201), KilledBy: proto.PolicyMemBudget,
		Events: []proto.Event{
			{V: 1, TS: ts0 - 10_000, Verb: proto.VerbWarn, Job: "j7", Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(5600 << 20), BudgetBytes: proto.I64(4 << 30)},
			{V: 1, TS: ts0 - 1000, Verb: proto.VerbKilled, Job: "j7", Policy: proto.PolicyMemBudget, UsedBytes: proto.I64(9100 << 20),
				BudgetBytes: proto.I64(4 << 30), Hint: "retry with lower parallelism or request a larger budget"},
		},
	}
}

func TestRenderStatusGolden(t *testing.T) {
	var b bytes.Buffer
	renderStatus(&b, richStatus())
	golden(t, "status_rich.txt", b.String())

	b.Reset()
	renderStatus(&b, proto.Status{Mode: "observe", Pressure: proto.LevelNormal, Admitting: true, SuggestedConcurrency: 4, CapacityBytes: 8 << 30, AvailBytes: 6 << 30, HeadroomBytes: 3 << 30})
	golden(t, "status_minimal.txt", b.String())
}

func TestRenderJobGolden(t *testing.T) {
	t.Setenv("TZ", "UTC")
	var b bytes.Buffer
	renderJob(&b, sampleJob(), ts0)
	got := b.String()
	// fmtTS uses the local zone; normalise by checking structure, not zone.
	for _, want := range []string{"job", "j7", "exit code", "201", "killed by", "mem_budget", "EVENTS (2)",
		"pace: KILLED policy=mem_budget used=8.9G budget=4.0G job=j7", "pace: hint: retry with lower parallelism"} {
		if !strings.Contains(got, want) {
			t.Errorf("job view missing %q:\n%s", want, got)
		}
	}
}

func TestStatusCommand(t *testing.T) {
	te := newTestEnv(t)
	// Unreachable first.
	if code := te.run("status"); code != exitUnreachable || !strings.Contains(te.err.String(), "unreachable") || !strings.Contains(te.err.String(), "pace: hint:") {
		t.Fatalf("unreachable: %d %q", code, te.err.String())
	}
	if code := te.run("status", "--json"); code != exitUnreachable || te.out.Len() != 0 {
		t.Fatalf("unreachable json: %d %q", code, te.out.String())
	}
	startDaemon(t, te.sock, richStatus(), nil)
	if code := te.run("status"); code != 0 {
		t.Fatalf("status: %d %s", code, te.err.String())
	}
	var want bytes.Buffer
	renderStatus(&want, richStatus())
	if te.out.String() != want.String() {
		t.Fatalf("status output differs from renderStatus:\n%s", te.out.String())
	}
	if code := te.run("status", "--json"); code != 0 {
		t.Fatal(code)
	}
	var s proto.Status
	if err := json.Unmarshal(te.out.Bytes(), &s); err != nil || s.Mode != "enforce" || len(s.Running) != 2 {
		t.Fatalf("json: %v %q", err, te.out.String())
	}
	if !strings.HasSuffix(te.out.String(), "}\n") || strings.Count(te.out.String(), "\n") != 1 {
		t.Fatalf("json must be one line: %q", te.out.String())
	}
	if code := te.run("status", "--job", "j7"); code != 0 || !strings.Contains(te.out.String(), "killed by") {
		t.Fatalf("job: %d %q", code, te.out.String())
	}
	if code := te.run("status", "--job", "j7", "--json"); code != 0 || !strings.Contains(te.out.String(), `"killed_by":"mem_budget"`) {
		t.Fatalf("job json: %d %q", code, te.out.String())
	}
	if code := te.run("status", "--job", "nope"); code != exitFail || !strings.Contains(te.err.String(), "not found") {
		t.Fatalf("missing job: %d %q", code, te.err.String())
	}
}

func TestWatch(t *testing.T) {
	te := newTestEnv(t)
	evs := []proto.Event{
		{V: 1, TS: ts0, Verb: proto.VerbQueued, Job: "j1", Reason: proto.ReasonHeadroom, FreeBytes: proto.I64(1200 << 20), NeedBytes: proto.I64(4 << 30), Position: proto.Int(2), ETAMS: proto.I64(40_000)},
		{V: 1, TS: ts0 + 1, Verb: proto.VerbKilled, Unit: "pg:9", Policy: proto.PolicyPressure, Hint: "close something"},
	}
	cl := &integrate.Client{Socket: te.sock}
	// Unreachable without reconnect -> 3.
	if code := te.c.watch(context.Background(), cl, false, false, false, time.Millisecond); code != exitUnreachable {
		t.Fatalf("unreachable: %d", code)
	}
	startDaemon(t, te.sock, richStatus(), evs)
	te.out.Reset()
	if code := te.c.watch(context.Background(), cl, false, false, false, time.Millisecond); code != 0 {
		t.Fatalf("watch: %d %s", code, te.err.String())
	}
	want := "pace: QUEUED reason=headroom free=1.2G need=4.0G position=2 eta=40s job=j1\n" +
		"pace: KILLED policy=pressure unit=pg:9\n" +
		"pace: hint: close something\n" +
		"{\"weird\":true}\n"
	if te.out.String() != want {
		t.Fatalf("watch output:\n%s\nwant:\n%s", te.out.String(), want)
	}
	te.out.Reset()
	te.c.watch(context.Background(), cl, true, false, false, time.Millisecond)
	lines := strings.Split(strings.TrimSpace(te.out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("json lines: %q", te.out.String())
	}
	for _, l := range lines {
		if !json.Valid([]byte(l)) {
			t.Fatalf("not JSON: %q", l)
		}
	}
	te.out.Reset()
	te.c.watch(context.Background(), cl, false, true, false, time.Millisecond)
	if !strings.Contains(te.out.String(), " pace: QUEUED") {
		t.Fatalf("time prefix: %q", te.out.String())
	}
	// With reconnect, cancelling the context ends the loop with 0.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	te.out.Reset()
	if code := te.c.watch(ctx, cl, false, false, true, 10*time.Millisecond); code != 0 {
		t.Fatalf("reconnect loop: %d", code)
	}
	if strings.Count(te.out.String(), "pace: QUEUED") < 2 {
		t.Fatalf("did not reconnect: %q", te.out.String())
	}
}

func TestCmdline(t *testing.T) {
	if got := cmdline([]string{"a", "b\nc"}, 50); got != "a b c" {
		t.Fatalf("%q", got)
	}
	if got := cmdline([]string{strings.Repeat("é", 60)}, 10); len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("%q", got)
	}
}
