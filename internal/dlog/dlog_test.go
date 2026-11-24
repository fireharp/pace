package dlog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

func sampleParams() policy.Params {
	return policy.Params{
		Mode: policy.ModeEnforce, ReserveBytes: 2 << 30, UseKernelLevel: true,
		WarnAvailPermille: 150, CritAvailPermille: 70, PSIWarn10: 100, PSICrit10: 50,
		BudgetWarnPermille: 900, TolerancePermille: 100, WarnDeadlineMS: 20000, GraceMS: 5000,
		CriticalGraceMS: 1000, StepMS: 2000, CriticalStepMS: 500, RecoverHoldMS: 5000, OrphanGraceMS: 10000,
		MinVictimBytes: 64 << 20, UnmanagedPriority: 2, DefaultQueueTimeoutMS: 1800000,
		CPUWarnPerCoreMilli: 1500, CPUOKPerCoreMilli: 1000, DiskWarnFreeBytes: 10 << 30, DiskCritFreeBytes: 3 << 30,
		MarginPermille: 100, HistorySize: 20, HistoryMinSamples: 3, DefaultClass: "default",
		Classes: []policy.ClassParams{
			{Name: "build", EstimateBytes: 2 << 30, BudgetBytes: 4 << 30, Slots: 2, WallLimitMS: 3600000, NoProgressMS: 600000, Priority: 5},
			{Name: "default", EstimateBytes: 1 << 30, BudgetBytes: 2 << 30, WallLimitMS: 1800000, NoProgressMS: 600000, Priority: 5},
		},
	}
}

func sampleState(now int64) policy.State {
	return policy.State{
		NextSeq: 3, LastMS: now - 1000,
		Jobs: []policy.Job{
			{ID: "j1", Seq: 1, Class: "build", Priority: 5, State: policy.JobRunning, EstimateBytes: 2 << 30,
				BudgetBytes: 4 << 30, ReservedBytes: 2 << 30, UsedBytes: 1 << 30, PeakBytes: 1 << 30, NProcs: 3,
				SubmitMS: now - 5000, AdmitMS: now - 5000, StartMS: now - 4900, PID: 100, PGID: 100, CPUMS: 1234,
				Enf: policy.Enforcement{Warnings: []policy.Warning{{Policy: proto.PolicyMemBudget, IssuedMS: now - 2000, DeadlineMS: now + 18000}}}},
			{ID: "j2", Seq: 2, Class: "default", Priority: 5, State: policy.JobQueued, EstimateBytes: 1 << 30, SubmitMS: now - 100},
		},
		Units:    []policy.Unit{{Key: "pg:500:1700000000000", PGID: 500, LeaderPID: 500, StartMS: 1700000000000, Comm: "node", UsedBytes: 300 << 20, NProcs: 2}},
		History:  []policy.History{{Key: "class:build", Peaks: []int64{1 << 30, 3 << 30}, DurMS: []int64{60000, 120000}}},
		Pressure: policy.Pressure{Level: proto.LevelNormal, SinceMS: now - 100000, NormalSinceMS: now - 100000},
	}
}

func sampleInputs(now int64) policy.Inputs {
	return policy.Inputs{
		NowMS: now,
		Machine: platform.Machine{NowMS: now, TotalBytes: 36 << 30, AvailBytes: 20 << 30, KernelLevel: 1,
			PSIMemSome10: -1, PSIMemFull10: -1, NCPU: 12, Load1Milli: 2500, DiskFreeBytes: 100 << 30},
		ScopedUsedBytes: 5 << 30,
		Jobs:            []policy.JobSample{{ID: "j1", UsedBytes: 1 << 30, NProcs: 3, CPUMS: 1500, Alive: true, ExtraPIDs: []int{120}}},
		Units:           []policy.UnitSample{{Key: "pg:500:1700000000000", PGID: 500, LeaderPID: 500, StartMS: 1700000000000, Comm: "node", UsedBytes: 300 << 20, NProcs: 2, PIDs: []int{500, 501}}},
		Events:          []policy.InEvent{{Kind: policy.EvProgress, Job: "j1", OutputBytes: 4096, LastOutputMS: now - 10}},
	}
}

// stepRecords builds n consistent step records by actually running policy.Step.
func stepRecords(n int) []Record {
	p := sampleParams()
	now := int64(1_763_800_000_000)
	s := sampleState(now)
	var recs []Record
	for i := 0; i < n; i++ {
		now += 1000
		in := sampleInputs(now)
		in.Jobs[0].UsedBytes += int64(i) << 20
		before := s
		ns, out := policy.Step(p, s, in)
		pp, bs, ii, oo := p, before, in, out
		recs = append(recs, Record{Kind: KindStep, TS: now, CfgHash: "sha256:cfg", Params: &pp, State: &bs, In: &ii, Out: &oo, StateHash: StateHash(ns)})
		// Round-trip the state as the daemon would carry it in memory.
		s = ns
	}
	return recs
}

func readAll(t *testing.T, files ...string) []Record {
	t.Helper()
	var out []Record
	for _, f := range files {
		if err := ReadRecords(f, func(r Record) error { out = append(out, r); return nil }); err != nil {
			t.Fatalf("ReadRecords(%s): %v", f, err)
		}
	}
	return out
}

func TestWriteReadBack(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(filepath.Join(dir, "logs"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if w.Path() != filepath.Join(dir, "logs", FileName) {
		t.Errorf("path %q", w.Path())
	}
	recs := stepRecords(50)
	w.Write(Record{Kind: KindStart, Version: "test", Params: recs[0].Params})
	for _, r := range recs {
		w.Write(r)
	}
	w.Write(Record{Kind: KindHeartbeat, TS: 42})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	got := readAll(t, w.Path())
	if len(got) != 52 {
		t.Fatalf("read %d records, want 52", len(got))
	}
	for i, r := range got {
		if r.Seq != int64(i+1) || r.V != 1 {
			t.Fatalf("record %d: seq %d v %d", i, r.Seq, r.V)
		}
		if r.TS == 0 {
			t.Fatalf("record %d: TS not filled", i)
		}
	}
	if got[0].Kind != KindStart || got[51].Kind != KindHeartbeat || got[51].TS != 42 {
		t.Errorf("kinds %q %q ts %d", got[0].Kind, got[51].Kind, got[51].TS)
	}
	// Content survives the round trip exactly.
	want := recs[10]
	want.V, want.Seq = 1, 12
	if !reflect.DeepEqual(got[11], want) {
		a, _ := json.Marshal(got[11])
		b, _ := json.Marshal(want)
		t.Errorf("round trip differs:\n%s\n%s", a, b)
	}
	st := w.Stats()
	if st.Written != 52 || st.Dropped != 0 || st.Errors != 0 {
		t.Errorf("stats %+v", st)
	}
	fi, err := os.Stat(w.Path())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v %v", fi.Mode(), err)
	}
	// Write after Close is dropped, never panics or blocks.
	w.Write(Record{Kind: KindHeartbeat})
	if w.Stats().Dropped != 1 {
		t.Errorf("write after close not counted")
	}
}

func TestReopenContinuesSeq(t *testing.T) {
	dir := t.TempDir()
	for round := 0; round < 3; round++ {
		w, err := Open(dir, Options{})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			w.Write(Record{Kind: KindHeartbeat})
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got := readAll(t, filepath.Join(dir, FileName))
	if len(got) != 15 {
		t.Fatalf("%d records", len(got))
	}
	for i, r := range got {
		if r.Seq != int64(i+1) {
			t.Fatalf("record %d seq %d", i, r.Seq)
		}
	}
	// A torn last line does not break seq recovery.
	f, _ := os.OpenFile(filepath.Join(dir, FileName), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"v":1,"kind":"heartbeat","seq":99`)
	f.Close()
	if s := lastSeq(filepath.Join(dir, FileName), fileSize(t, filepath.Join(dir, FileName))); s != 15 {
		t.Errorf("lastSeq with torn tail = %d, want 15", s)
	}
}

func fileSize(t *testing.T, p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

func TestRotateAndGzip(t *testing.T) {
	dir := t.TempDir()
	const maxB = 4096
	w, err := Open(dir, Options{MaxBytes: maxB, Keep: 3})
	if err != nil {
		t.Fatal(err)
	}
	const n = 400
	for i := 0; i < n; i++ {
		w.Write(Record{Kind: KindHeartbeat, Msg: strings.Repeat("x", 100)})
		if i%50 == 0 {
			time.Sleep(5 * time.Millisecond) // let the queue drain; no drops wanted here
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	st := w.Stats()
	if st.Dropped != 0 || st.Errors != 0 || st.Rotations < 5 {
		t.Fatalf("stats %+v", st)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	want := []string{FileName, FileName + ".1.gz", FileName + ".2.gz", FileName + ".3.gz"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("files %v, want %v", names, want)
	}
	files := LogFiles(dir)
	wantFiles := []string{
		filepath.Join(dir, FileName+".3.gz"), filepath.Join(dir, FileName+".2.gz"),
		filepath.Join(dir, FileName+".1.gz"), filepath.Join(dir, FileName),
	}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Fatalf("LogFiles %v", files)
	}
	// Rotated files are real gzip and within the size limit when decompressed.
	for _, f := range files[:3] {
		raw, _ := os.ReadFile(f)
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s not gzip: %v", f, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(zr); err != nil {
			t.Fatal(err)
		}
		if buf.Len() > maxB || buf.Len() == 0 {
			t.Errorf("%s: %d bytes uncompressed", f, buf.Len())
		}
	}
	// Seqs are contiguous across files, oldest first, ending at n.
	got := readAll(t, files...)
	if len(got) == 0 || got[len(got)-1].Seq != n {
		t.Fatalf("last seq %v", got[len(got)-1].Seq)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq != got[i-1].Seq+1 {
			t.Fatalf("seq jump %d -> %d", got[i-1].Seq, got[i].Seq)
		}
	}
	if len(got) >= n {
		t.Errorf("Keep=3 should have discarded old records, kept %d", len(got))
	}
}

func TestLeftoverRotatedFilesCompressedOnOpen(t *testing.T) {
	dir := t.TempDir()
	plain := filepath.Join(dir, FileName+".2")
	if err := os.WriteFile(plain, []byte(`{"v":1,"kind":"heartbeat","seq":1,"ts":1}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LogFiles(dir); len(got) != 1 || got[0] != plain {
		t.Fatalf("LogFiles with plain leftover %v", got)
	}
	w, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(plain); !os.IsNotExist(err) {
		t.Errorf("plain leftover still present: %v", err)
	}
	recs := readAll(t, plain+".gz")
	if len(recs) != 1 || recs[0].Seq != 1 {
		t.Errorf("compressed leftover %v", recs)
	}
}

// blockingHook blocks the writer goroutine on its first record until release.
func blockingHook() (hook func(Record), entered, release chan struct{}) {
	entered, release = make(chan struct{}), make(chan struct{})
	var once sync.Once
	hook = func(Record) {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	return
}

func TestOverflowWritesGap(t *testing.T) {
	dir := t.TempDir()
	hook, entered, release := blockingHook()
	w, err := open(dir, Options{Queue: 4}, hook)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(Record{Kind: KindHeartbeat, Msg: "first"})
	<-entered // goroutine holds "first", queue empty
	start := time.Now()
	for i := 0; i < 14; i++ {
		w.Write(Record{Kind: KindHeartbeat, Msg: fmt.Sprintf("r%d", i)})
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Errorf("Write blocked for %v", d)
	}
	if st := w.Stats(); st.Dropped != 10 {
		t.Errorf("dropped %d, want 10", st.Dropped)
	}
	close(release)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, w.Path())
	var kinds []string
	for _, r := range got {
		kinds = append(kinds, r.Kind+":"+r.Msg)
	}
	if len(got) != 6 {
		t.Fatalf("records %v", kinds)
	}
	if got[0].Msg != "first" || got[1].Kind != KindGap || got[1].Dropped != 10 {
		t.Fatalf("records %v", kinds)
	}
	for i, want := range []string{"r0", "r1", "r2", "r3"} {
		if got[i+2].Msg != want {
			t.Errorf("record %d = %q, want %q", i+2, got[i+2].Msg, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Errorf("seq not increasing: %d then %d", got[i-1].Seq, got[i].Seq)
		}
	}
	// Replay reports the gap.
	var out bytes.Buffer
	rep, err := Replay([]string{w.Path()}, nil, &out)
	if err != nil || rep.Gaps != 1 || rep.Dropped != 10 {
		t.Errorf("replay %+v %v\n%s", rep, err, out.String())
	}
}

func TestGapWrittenOnIdleTick(t *testing.T) {
	dir := t.TempDir()
	hook, entered, release := blockingHook()
	w, err := open(dir, Options{Queue: 1, FlushEvery: 10 * time.Millisecond}, hook)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Write(Record{Kind: KindHeartbeat})
	<-entered
	w.Write(Record{Kind: KindHeartbeat}) // fills queue
	w.Write(Record{Kind: KindHeartbeat}) // dropped
	close(release)
	// Without further writes, the gap record appears on a flush tick.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var gaps int
		_ = ReadRecords(w.Path(), func(r Record) error {
			if r.Kind == KindGap && r.Dropped == 1 {
				gaps++
			}
			return nil
		})
		if gaps == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gap record not flushed within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentWriters(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{Queue: 64, MaxBytes: 64 << 10, Keep: 50})
	if err != nil {
		t.Fatal(err)
	}
	const G, N = 8, 500
	var wg sync.WaitGroup
	for g := 0; g < G; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < N; i++ {
				w.Write(Record{Kind: KindHeartbeat, Msg: fmt.Sprintf("g%d-%d", g, i)})
			}
		}(g)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, LogFiles(dir)...)
	var plain, dropped int64
	seen := map[string]bool{}
	for i, r := range got {
		if i > 0 && r.Seq != got[i-1].Seq+1 {
			t.Fatalf("seq gap %d -> %d", got[i-1].Seq, r.Seq)
		}
		switch r.Kind {
		case KindGap:
			dropped += r.Dropped
		default:
			if seen[r.Msg] {
				t.Fatalf("duplicate %s", r.Msg)
			}
			seen[r.Msg] = true
			plain++
		}
	}
	if plain+dropped != G*N {
		t.Errorf("written %d + dropped %d != %d", plain, dropped, G*N)
	}
	st := w.Stats()
	if st.Dropped != dropped || st.Errors != 0 {
		t.Errorf("stats %+v, gap total %d", st, dropped)
	}
	t.Logf("concurrent: %d written, %d dropped, %d rotations", plain, dropped, st.Rotations)
}

func TestPeriodicFlush(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{FlushEvery: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Write(Record{Kind: KindHeartbeat})
	deadline := time.Now().Add(5 * time.Second)
	for fileSize(t, w.Path()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("record not flushed without Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	os.WriteFile(f, nil, 0o600)
	if _, err := Open(filepath.Join(f, "sub"), Options{}); err == nil {
		t.Error("Open under a regular file succeeded")
	}
}

func TestStateHash(t *testing.T) {
	now := int64(1_763_800_000_000)
	s := sampleState(now)
	h := StateHash(s)
	if !strings.HasPrefix(h, "sha256:") || len(h) != 7+64 {
		t.Fatalf("format %q", h)
	}
	for i := 0; i < 50; i++ {
		if StateHash(s) != h {
			t.Fatal("not deterministic")
		}
	}
	// JSON round trip (what replay sees) keeps the hash.
	b, _ := json.Marshal(s)
	var s2 policy.State
	if err := json.Unmarshal(b, &s2); err != nil {
		t.Fatal(err)
	}
	if StateHash(s2) != h {
		t.Error("round trip changed the hash")
	}
	// Large int64 values survive (no float conversion).
	s3 := s
	s3.LastMS = 1<<62 + 1
	b, _ = json.Marshal(s3)
	var s4 policy.State
	json.Unmarshal(b, &s4)
	if StateHash(s4) != StateHash(s3) {
		t.Error("large int64 changed the hash")
	}
	// Any change changes the hash.
	s5 := sampleState(now)
	s5.Jobs[0].UsedBytes++
	if StateHash(s5) == h {
		t.Error("changed state, same hash")
	}
	if StateHash(policy.State{}) == h {
		t.Error("empty state, same hash")
	}
}

func writeFile(t *testing.T, path string, recs []Record) {
	t.Helper()
	var buf bytes.Buffer
	for i, r := range recs {
		if r.V == 0 {
			r.V = 1
		}
		if r.Seq == 0 {
			r.Seq = int64(i + 1)
		}
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReplayMatches(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	recs := stepRecords(30)
	w.Write(Record{Kind: KindStart, Params: recs[0].Params})
	for _, r := range recs {
		w.Write(r)
	}
	w.Write(Record{Kind: KindHeartbeat})
	w.Close()
	var out bytes.Buffer
	rep, err := Replay(LogFiles(dir), nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Records != 32 || rep.Replayed != 30 || rep.Mismatches != 0 || rep.Corrupt != 0 || rep.Skipped != 0 {
		t.Errorf("report %+v\n%s", rep, out.String())
	}
	if !strings.Contains(out.String(), "replay: records=32 replayed=30 mismatches=0") {
		t.Errorf("summary line missing:\n%s", out.String())
	}
}

func TestReplayDetectsTampering(t *testing.T) {
	dir := t.TempDir()
	recs := stepRecords(6)

	// 1: output tampered (an extra kill action).
	recs[1].Out = &policy.Output{Actions: append(append([]policy.Action(nil), recs[1].Out.Actions...),
		policy.Action{Op: policy.OpSignalGroup, PGID: 4242, Signal: 9, Policy: proto.PolicyPressure})}
	// 3: inputs tampered (time moved) so the new state differs from the logged hash.
	in := *recs[3].In
	in.NowMS += 777
	recs[3].In = &in
	// 4: hash tampered.
	recs[4].StateHash = "sha256:" + strings.Repeat("0", 64)
	for i := range recs {
		recs[i].Seq = int64(100 + i)
	}
	p := filepath.Join(dir, "log.jsonl")
	writeFile(t, p, recs)

	var out bytes.Buffer
	rep, err := Replay([]string{p}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Replayed != 6 || rep.Mismatches != 3 {
		t.Fatalf("report %+v\n%s", rep, out.String())
	}
	o := out.String()
	for _, want := range []string{
		"seq=101 ", "out.actions", "4242",
		"seq=103 ", "state_hash: logged sha256:",
		"seq=104 ", "logged sha256:000000000000",
	} {
		if !strings.Contains(o, want) {
			t.Errorf("output missing %q:\n%s", want, o)
		}
	}
	for _, notWant := range []string{"seq=100 ", "seq=102 ", "seq=105 "} {
		if strings.Contains(o, notWant) {
			t.Errorf("unexpected mismatch %q:\n%s", notWant, o)
		}
	}
	// One line per mismatching record plus the summary.
	if n := strings.Count(o, "\n"); n != 4 {
		t.Errorf("%d lines:\n%s", n, o)
	}
}

func TestReplayParamsFallbackAndOverride(t *testing.T) {
	dir := t.TempDir()
	recs := stepRecords(4)
	params := recs[0].Params
	for i := range recs {
		recs[i].Params = nil
	}
	p := filepath.Join(dir, "a.jsonl")
	// No params anywhere: skipped.
	writeFile(t, p, recs)
	var out bytes.Buffer
	rep, err := Replay([]string{p}, nil, &out)
	if err != nil || rep.Skipped != 4 || rep.Replayed != 0 {
		t.Errorf("no params: %+v %v", rep, err)
	}
	// Override supplies them.
	out.Reset()
	rep, err = Replay([]string{p}, params, &out)
	if err != nil || rep.Replayed != 4 || rep.Skipped != 0 || rep.Mismatches != 0 {
		t.Errorf("override: %+v %v\n%s", rep, err, out.String())
	}
	// A preceding start record supplies them, even across files.
	p0 := filepath.Join(dir, "0.jsonl")
	writeFile(t, p0, []Record{{Kind: KindStart, Params: params}})
	out.Reset()
	rep, err = Replay([]string{p0, p}, nil, &out)
	if err != nil || rep.Replayed != 4 || rep.Skipped != 0 || rep.Mismatches != 0 || rep.Records != 5 {
		t.Errorf("start params: %+v %v\n%s", rep, err, out.String())
	}
}

func TestReplayCorruptLines(t *testing.T) {
	dir := t.TempDir()
	recs := stepRecords(3)
	var buf bytes.Buffer
	enc := func(r Record) {
		r.V = 1
		b, _ := json.Marshal(r)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	enc(recs[0])
	buf.WriteString("this is not json\n")
	buf.WriteString("\n\n")
	buf.WriteString("42\n")
	buf.WriteString(`{"hello":"world"}` + "\n")
	enc(recs[1])
	buf.WriteString("{\"v\":1,\"kind\":\"step\",\"seq\":") // torn line, no newline at EOF
	p := filepath.Join(dir, "c.jsonl")
	os.WriteFile(p, buf.Bytes(), 0o600)

	var badLines []int
	var n int
	if err := ScanRecords(p, func(Record) error { n++; return nil }, func(l int, err error) { badLines = append(badLines, l) }); err != nil {
		t.Fatal(err)
	}
	if n != 2 || !reflect.DeepEqual(badLines, []int{2, 5, 6, 8}) {
		t.Errorf("records %d bad lines %v", n, badLines)
	}
	var out bytes.Buffer
	rep, err := Replay([]string{p}, nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Corrupt != 4 || rep.Replayed != 2 || rep.Mismatches != 0 {
		t.Errorf("report %+v\n%s", rep, out.String())
	}
	if !strings.Contains(out.String(), p+":2: warning: skipped corrupt line") {
		t.Errorf("warning missing:\n%s", out.String())
	}
	// fn errors stop the scan.
	stop := fmt.Errorf("stop")
	if err := ReadRecords(p, func(Record) error { return stop }); err != stop {
		t.Errorf("fn error not returned: %v", err)
	}
	// Missing files are an error.
	if _, err := Replay([]string{filepath.Join(dir, "missing")}, nil, &out); err == nil {
		t.Error("missing file: no error")
	}
}

func TestReplayTruncatedGzip(t *testing.T) {
	dir := t.TempDir()
	var plain bytes.Buffer
	for _, r := range stepRecords(40) {
		r.V = 1
		b, _ := json.Marshal(r)
		plain.Write(b)
		plain.WriteByte('\n')
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain.Bytes())
	zw.Close()
	p := filepath.Join(dir, "t.jsonl.gz")
	full := filepath.Join(dir, "f.jsonl.gz")
	os.WriteFile(full, gz.Bytes(), 0o600)
	os.WriteFile(p, gz.Bytes()[:gz.Len()/2], 0o600)

	var out bytes.Buffer
	rep, err := Replay([]string{full}, nil, &out)
	if err != nil || rep.Replayed != 40 || rep.Mismatches != 0 || rep.Corrupt != 0 {
		t.Fatalf("full gzip: %+v %v\n%s", rep, err, out.String())
	}
	out.Reset()
	rep, err = Replay([]string{p, full}, nil, &out)
	if err != nil {
		t.Fatalf("truncated gzip is not an error: %v", err)
	}
	if rep.Corrupt == 0 || rep.Replayed < 40 || rep.Replayed >= 80 || rep.Mismatches != 0 {
		t.Errorf("truncated: %+v\n%s", rep, out.String())
	}
	// Garbage after a gzip header.
	bad := filepath.Join(dir, "bad.gz")
	os.WriteFile(bad, []byte{0x1f, 0x8b, 0, 0, 1, 2, 3}, 0o600)
	out.Reset()
	if rep, err := Replay([]string{bad}, nil, &out); err != nil || rep.Corrupt != 1 {
		t.Errorf("bad gzip: %+v %v", rep, err)
	}
}

func TestReadLineLimit(t *testing.T) {
	old := maxLine
	maxLine = 18
	defer func() { maxLine = old }()
	dir := t.TempDir()
	p := filepath.Join(dir, "l.jsonl")
	line := `{"v":1,"kind":"x"}` // exactly 18 bytes: accepted
	os.WriteFile(p, []byte(line+"\n"+strings.Repeat("y", 100000)+"\n"+line+"\n"), 0o600)
	var n int
	var bad []int
	if err := ScanRecords(p, func(Record) error { n++; return nil }, func(l int, _ error) { bad = append(bad, l) }); err != nil {
		t.Fatal(err)
	}
	if n != 2 || !reflect.DeepEqual(bad, []int{2}) {
		t.Errorf("n=%d bad=%v", n, bad)
	}
	// Direct readLine behaviour across buffer boundaries.
	r := bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", 40)+"\nbb\n"), 16)
	l, long, err := readLine(r, 100)
	if string(l) != strings.Repeat("a", 40) || long || err != nil {
		t.Errorf("readLine 1: %q %v %v", l, long, err)
	}
	l, _, _ = readLine(r, 100)
	if string(l) != "bb" {
		t.Errorf("readLine 2: %q", l)
	}
}

func TestJSONDiff(t *testing.T) {
	tests := []struct {
		a, b string
		want []string
	}{
		{`{"x":1}`, `{"x":1}`, nil},
		{`{"x":1}`, `{"x":2}`, []string{".x: want 1, got 2"}},
		{`{"x":1}`, `{}`, []string{".x: want 1, got absent"}},
		{`{}`, `{"y":[1]}`, []string{".y: want absent, got [1]"}},
		{`{"a":[1,2]}`, `{"a":[1]}`, []string{".a: want 2 elements, got 1"}},
		{`{"a":[{"s":15}]}`, `{"a":[{"s":9}]}`, []string{".a[0].s: want 15, got 9"}},
		{`{"a":"1"}`, `{"a":1}`, []string{`.a: want "1", got 1`}},
		{`{"n":9007199254740993}`, `{"n":9007199254740992}`, []string{".n: want 9007199254740993, got 9007199254740992"}},
		{`{"a":1,"b":2,"c":3,"d":4,"e":5}`, `{}`, []string{".a: want 1, got absent", ".b: want 2, got absent", ".c: want 3, got absent", ".d: want 4, got absent"}},
		{`{"a":{}}`, `{"a":[]}`, []string{".a: want {}, got []"}},
	}
	for _, tc := range tests {
		got := JSONDiff([]byte(tc.a), []byte(tc.b), 4)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("JSONDiff(%s, %s) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
	if d := JSONDiff([]byte(`{`), []byte(`{}`), 4); len(d) != 1 || !strings.Contains(d[0], "cannot decode logged") {
		t.Errorf("bad json: %v", d)
	}
}

func BenchmarkWrite(b *testing.B) {
	dir := b.TempDir()
	w, err := Open(dir, Options{Queue: 1 << 16})
	if err != nil {
		b.Fatal(err)
	}
	rec := stepRecords(1)[0]
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w.Write(rec)
	}
	b.StopTimer()
	w.Close()
}

// BenchmarkGzipLevel measured the rotated-file compression choice on ~8 MB of
// realistic step records (M3 Pro): BestSpeed vs DefaultCompression.
func BenchmarkGzipLevel(b *testing.B) {
	var plain bytes.Buffer
	for plain.Len() < 8<<20 {
		for _, r := range stepRecords(50) {
			r.V = 1
			j, _ := json.Marshal(r)
			plain.Write(j)
			plain.WriteByte('\n')
		}
	}
	for _, lvl := range []struct {
		name string
		l    int
	}{{"BestSpeed", gzip.BestSpeed}, {"Default", gzip.DefaultCompression}} {
		b.Run(lvl.name, func(b *testing.B) {
			b.SetBytes(int64(plain.Len()))
			var n int
			for i := 0; i < b.N; i++ {
				var out bytes.Buffer
				zw, _ := gzip.NewWriterLevel(&out, lvl.l)
				zw.Write(plain.Bytes())
				zw.Close()
				n = out.Len()
			}
			b.ReportMetric(float64(plain.Len())/float64(n), "ratio")
		})
	}
}

func TestRotationFailureKeepsWriting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes: the read-only directory cannot make renames fail")
	}
	dir := t.TempDir()
	w, err := Open(dir, Options{MaxBytes: 1024, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // renames in dir now fail
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	for i := 0; i < 40; i++ {
		w.Write(Record{Kind: KindHeartbeat, Msg: strings.Repeat("z", 100)})
		time.Sleep(time.Millisecond)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o700)
	st := w.Stats()
	if st.Errors == 0 || st.Rotations != 0 || st.Dropped != 0 || st.Written != 40 {
		t.Errorf("stats %+v", st)
	}
	if got := readAll(t, w.Path()); len(got) != 40 {
		t.Errorf("records in active file: %d", len(got))
	}
}

func TestRecoverAfterDirRemoved(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "logs")
	w, err := Open(dir, Options{MaxBytes: 512, Keep: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	// The first record goes to the unlinked file (size 0 < MaxBytes); the
	// next ones need a rotation that cannot reopen the file: dropped.
	for i := 0; i < 10; i++ {
		w.Write(Record{Kind: KindHeartbeat, Msg: strings.Repeat("q", 200)})
		time.Sleep(time.Millisecond)
	}
	deadline := time.Now().Add(5 * time.Second)
	for w.Stats().Dropped == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no drops: %+v", w.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	w.Write(Record{Kind: KindHeartbeat, Msg: "after"})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, LogFiles(dir)...)
	var gap int64
	var after bool
	for _, r := range got {
		if r.Kind == KindGap {
			gap += r.Dropped
		}
		if r.Msg == "after" {
			after = true
		}
	}
	st := w.Stats()
	if !after || gap == 0 || gap != st.Dropped {
		t.Errorf("after=%v gap=%d stats=%+v records=%d", after, gap, st, len(got))
	}
}

// Finding 34: a caller's seq that would repeat or go backwards (its own
// counter restarted from a stale state.json) is renumbered, and LastSeq
// lets the caller continue after the log.
func TestCallerSeqNeverGoesBackwards(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := int64(1); i <= 10; i++ {
		w.Write(Record{Kind: KindHeartbeat, Seq: i})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	w, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := w.LastSeq(); got != 10 {
		t.Fatalf("LastSeq %d, want 10", got)
	}
	w.Write(Record{Kind: KindStart, Seq: 7}) // stale caller counter
	w.Write(Record{Kind: KindHeartbeat, Seq: 8})
	w.Write(Record{Kind: KindHeartbeat, Seq: 20})
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, filepath.Join(dir, FileName))
	var seqs []int64
	for _, r := range got {
		seqs = append(seqs, r.Seq)
	}
	want := []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 20}
	if !reflect.DeepEqual(seqs, want) {
		t.Fatalf("seqs %v, want %v", seqs, want)
	}
}

// Right after a rotation the active file is empty: the last seq is in the
// newest rotated file (plain or compressed).
func TestLastSeqFromRotatedFile(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		w.Write(Record{Kind: KindHeartbeat})
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, FileName), filepath.Join(dir, FileName+".1")); err != nil {
		t.Fatal(err)
	}
	if err := gzipFile(filepath.Join(dir, FileName+".1")); err != nil {
		t.Fatal(err)
	}
	w, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if got := w.LastSeq(); got != 5 {
		t.Fatalf("LastSeq after rotation %d, want 5", got)
	}
}
