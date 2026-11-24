package policy

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// randomScenario drives a sim through n ticks of seeded random load: jobs
// arrive with random classes/estimates/priorities, start after admission,
// grow, emit output, exit, lose their wrapper; unmanaged groups come and
// go; background memory, load and disk swing. It behaves like the wrapper
// would: killed jobs die and exit a few ticks later.
//
// Besides the per-tick invariants of sim.check (determinism, no input
// mutation, admission never exceeding headroom, hint rules, signal safety,
// reservation never shrinking) it checks the ladder's ordering promises:
// every non-immediate kill is preceded by a WARN to the same target.
func randomScenario(t *testing.T, seed int64, mode string, n int) []string {
	r := rand.New(rand.NewSource(seed))
	p := testParams()
	p.Mode = mode
	p.Classes[1].NoProgressMS = 30_000
	p.Classes[3].WallLimitMS = 120_000
	m := newSim(t, p)
	var trace []string
	nextID, nextPID := 0, 1000
	type life struct {
		admitted, started, killed bool
		killedAt                  int
		peakGoal                  int64
		lost                      bool
	}
	jobs := map[string]*life{}
	warned := map[string]map[string]bool{} // target -> policy -> warned
	classes := []string{"build", "default", "model", "test", "unknown"}
	unitSeq := 0
	for tick := 0; tick < n; tick++ {
		// Arrivals.
		if r.Intn(4) == 0 {
			nextID++
			id := fmt.Sprintf("j%03d", nextID)
			req := proto.SubmitRequest{Class: classes[r.Intn(len(classes))], Argv: []string{"tool", fmt.Sprintf("sub%d", r.Intn(3))}}
			if r.Intn(2) == 0 {
				req.EstimateBytes = int64(r.Intn(4096)+1) * MiB
			}
			if r.Intn(40) == 0 {
				req.EstimateBytes = 15 * GiB
			}
			if r.Intn(3) == 0 {
				req.Priority = intp(r.Intn(10))
			}
			req.NoWait = r.Intn(6) == 0
			if r.Intn(5) == 0 {
				req.QueueTimeoutMS = int64(r.Intn(20_000))
			}
			m.submit(id, req)
			jobs[id] = &life{peakGoal: int64(r.Intn(3000)+50) * MiB}
		}
		// Per-job wrapper behaviour (sorted for reproducibility).
		ids := make([]string, 0, len(jobs))
		for id := range jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			l := jobs[id]
			switch {
			case l.killed:
				if tick-l.killedAt == 2 {
					m.dead(id)
				}
				if tick-l.killedAt == 3 {
					if !l.lost {
						m.exit(id, 201)
					}
					delete(jobs, id)
				}
			case l.admitted && !l.started && !l.lost:
				nextPID++
				m.started(id, nextPID)
				l.started = true
			case l.started && !l.lost:
				js := m.samples[id]
				if js.UsedBytes < l.peakGoal {
					js.UsedBytes += int64(r.Intn(400)) * MiB
				} else if r.Intn(3) == 0 {
					js.UsedBytes -= js.UsedBytes / 4
				}
				if r.Intn(3) > 0 {
					js.CPUMS += int64(r.Intn(500))
				}
				if r.Intn(4) == 0 {
					m.output(id, int64(tick*100))
				}
				if r.Intn(5) == 0 {
					js.ExtraPIDs = []int{nextPID + 5000 + r.Intn(10)}
				}
				switch r.Intn(60) {
				case 0:
					m.exit(id, 0)
					delete(jobs, id)
				case 1:
					m.lost(id)
					l.lost = true
				}
			case l.lost && r.Intn(30) == 0:
				m.dead(id)
			}
		}
		// Unmanaged groups.
		if r.Intn(15) == 0 {
			unitSeq++
			m.unit(fmt.Sprintf("pg:%d:%d", 5000+unitSeq, unitSeq), 5000+unitSeq, T0+int64(unitSeq), int64(r.Intn(2000))*MiB)
		}
		if r.Intn(20) == 0 && len(m.units) > 0 {
			keys := make([]string, 0, len(m.units))
			for k := range m.units {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			delete(m.units, keys[r.Intn(len(keys))])
		}
		// Machine swings.
		if r.Intn(60) == 0 {
			m.base = int64(r.Intn(12000)) * MiB
		}
		if r.Intn(10) == 0 {
			m.mach.Load1Milli = int64(r.Intn(8 * 2500))
		}
		if r.Intn(25) == 0 {
			m.mach.DiskFreeBytes = int64(r.Intn(20)) * GiB
		}
		m.mach.KernelLevel = []int{1, 1, 1, 2, 4}[r.Intn(5)]
		m.p.UseKernelLevel = r.Intn(10) == 0

		o := m.tick(int64(100 + r.Intn(900)))

		for _, e := range o.Events {
			id := e.Job + e.Unit
			switch e.Verb {
			case proto.VerbAdmitted:
				if l := jobs[e.Job]; l != nil {
					l.admitted = true
				}
			case proto.VerbRefused:
				delete(jobs, e.Job)
			case proto.VerbWarn:
				if warned[id] == nil {
					warned[id] = map[string]bool{}
				}
				warned[id][e.Policy] = true
			case proto.VerbKilled:
				switch e.Policy {
				case proto.PolicyStuck, proto.PolicyWallClock, proto.PolicyPressure:
					if !warned[id][e.Policy] {
						t.Fatalf("seed %d tick %d: %s killed by %s without a prior WARN", seed, tick, id, e.Policy)
					}
				}
				if mode == ModeEnforce && !hasAct(o.Actions, id, OpSignalGroup, sigTERM) && !hasAct(o.Actions, id, OpSignal, sigTERM) {
					t.Fatalf("seed %d: KILLED without SIGTERM for %s", seed, id)
				}
			}
			if e.Verb == proto.VerbKilled || (e.Verb == proto.VerbInfo && strings.HasPrefix(e.Msg, "would kill")) {
				if l := jobs[e.Job]; l != nil && mode == ModeEnforce {
					l.killed, l.killedAt = true, tick
				}
			}
			trace = append(trace, e.Line())
		}
		for _, a := range o.Actions {
			trace = append(trace, fmt.Sprintf("act %+v", a))
		}
		for _, a := range o.Would {
			trace = append(trace, fmt.Sprintf("would %+v", a))
		}
	}
	trace = append(trace, mustJSON(t, m.s))
	return trace
}

func TestRandomScenarios(t *testing.T) {
	seeds := 40
	ticks := 400
	if testing.Short() {
		seeds, ticks = 8, 200
	}
	for _, mode := range []string{ModeEnforce, ModeWarnOnly, ModeObserve} {
		for seed := int64(1); seed <= int64(seeds); seed++ {
			t.Run(fmt.Sprintf("%s/%d", mode, seed), func(t *testing.T) {
				a := randomScenario(t, seed, mode, ticks)
				if seed <= 3 {
					b := randomScenario(t, seed, mode, ticks)
					if strings.Join(a, "\n") != strings.Join(b, "\n") {
						t.Fatal("same seed produced a different trace")
					}
				}
			})
		}
	}
}

// TestRandomScenariosExercisePolicies guards the generator itself: across
// seeds every verb and policy must actually occur, or the property test
// above would be vacuous.
func TestRandomScenariosExercisePolicies(t *testing.T) {
	seen := map[string]bool{}
	for seed := int64(1); seed <= 20; seed++ {
		for _, l := range randomScenario(t, seed, ModeEnforce, 400) {
			verb, kv, ok := proto.ParseLine(l)
			if !ok {
				continue
			}
			seen[verb] = true
			if kv["policy"] != "" {
				seen[verb+"/"+kv["policy"]] = true
			}
			if kv["reason"] != "" {
				seen[verb+"/"+kv["reason"]] = true
			}
		}
	}
	for _, want := range []string{
		"ACCEPTED", "QUEUED", "ADMITTED", "REFUSED", "WARN", "OK", "THROTTLED", "UNTHROTTLED", "FROZEN", "THAWED", "KILLED", "INFO",
		"KILLED/pressure", "KILLED/mem_budget", "KILLED/orphaned", "WARN/mem_budget", "WARN/pressure", "WARN/disk",
		"THROTTLED/cpu", "QUEUED/headroom", "QUEUED/pressure", "REFUSED/too_big",
	} {
		if !seen[want] {
			t.Errorf("random scenarios never produced %s", want)
		}
	}
}

// TestDeterministicJSON: identical inputs give byte-identical results even
// for a state full of jobs, units, warnings and history.
func TestDeterministicJSON(t *testing.T) {
	m := twoJobs(t, ModeEnforce)
	m.unit("pg:900:1", 900, T0, GiB)
	m.unit("pg:800:1", 800, T0, 2*GiB)
	m.s.History = []History{{Key: "class:default", Peaks: []int64{1, 2, 3}, DurMS: []int64{1, 2, 3}}}
	m.setFree(1500 * MiB)
	m.tick(100)
	in := m.inputs()
	in.NowMS = m.now + 100
	var first string
	for i := 0; i < 20; i++ {
		ns, out := Step(m.p, m.s, in)
		got := mustJSON(t, ns) + mustJSON(t, out)
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("run %d differs", i)
		}
	}
}
