package policy

import (
	"fmt"
	"testing"

	"github.com/fireharp/pace/internal/proto"
)

// swarmSim: one innocent wrapped job "inn" (priority 5, 1 GiB) and n
// unmanaged groups of size each, every one below MinVictimBytes (64M) or
// not, started one second apart after the job (pg:1000 is the oldest).
func swarmSim(t *testing.T, n int, size int64) *sim {
	t.Helper()
	m := newSim(t, testParams())
	m.admitStart("inn", proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: 8 * GiB}, 100, GiB)
	for i := 0; i < n; i++ {
		m.unit(swarmKey(i), 1000+i, T0+10_000+int64(i)*1000, size)
	}
	m.tick(100)
	return m
}

func swarmKey(i int) string { return fmt.Sprintf("pg:%d:1", 1000+i) }

// killedSwarm returns the swarm indexes KILLED in o.
func killedSwarm(o Output, n int) []int {
	var out []int
	for i := 0; i < n; i++ {
		if evFor(o, proto.VerbKilled, swarmKey(i)) != nil {
			out = append(out, i)
		}
	}
	return out
}

// A swarm of unmanaged groups, each below min_victim, holds the memory at
// critical pressure: the youngest of them are killed until the deficit is
// covered, and the innocent wrapped job (priority 5 > unmanaged 2) is not
// warned or killed.
func TestSmallSwarmKilledBeforeInnocentJob(t *testing.T) {
	const n = 30
	m := swarmSim(t, n, 50*MiB)
	// critical: avail 1G < 1.12G; deficit = 1.12G + 0.64G - 1G = 0.76G
	// (778M): 16 groups of 50M.
	m.setFree(1 * GiB)
	o := m.tick(100)
	noEv(t, o, proto.VerbWarn, "inn")
	noEv(t, o, proto.VerbKilled, "inn")
	k := killedSwarm(o, n)
	if len(k) != 16 {
		t.Fatalf("killed %v, want the 16 youngest:\n%s", k, dumpEvents(o))
	}
	for j, i := range k {
		if i != n-16+j {
			t.Fatalf("killed %v, want the 16 youngest (%d..%d)", k, n-16, n-1)
		}
	}
	ev := mustEv(t, o, proto.VerbKilled, swarmKey(n-1))
	eq(t, "policy", ev.Policy, proto.PolicyPressure)
	eq(t, "level", ev.Level, proto.LevelCritical)
	if !hasAct(o.Actions, swarmKey(n-1), OpSignalGroup, sigTERM) {
		t.Fatalf("youngest swarm group not signalled: %+v", o.Actions)
	}
}

// At warn pressure the swarm is warned, youngest first, and walks the
// ladder; the innocent job is never warned.
func TestSmallSwarmWarnedBeforeInnocentJob(t *testing.T) {
	const n = 30
	m := swarmSim(t, n, 50*MiB)
	// warn: avail 2G < 2.4G; deficit = 2.4G + 0.64G - 2G = 1.04G: 22 groups.
	m.setFree(2 * GiB)
	o := m.tick(100)
	noEv(t, o, proto.VerbWarn, "inn")
	w := 0
	for i := 0; i < n; i++ {
		if evFor(o, proto.VerbWarn, swarmKey(i)) != nil {
			w++
			if i < n-22 {
				t.Fatalf("older group %d warned before younger ones:\n%s", i, dumpEvents(o))
			}
		}
	}
	eq(t, "warned", w, 22)
	o = m.run(210, 100) // past the 20s deadline: throttled
	noEv(t, o, proto.VerbWarn, "inn")
	noEv(t, o, proto.VerbThrottled, "inn")
	eq(t, "youngest throttled", m.unitState(swarmKey(n-1)).Enf.Stage, StageThrottled)
	eq(t, "job untouched", len(m.job("inn").Enf.Warnings), 0)
}

// One small group (together below min_victim) is never a victim: the job is.
func TestLoneSmallGroupIsNoVictim(t *testing.T) {
	m := swarmSim(t, 1, 50*MiB)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "inn")
	noEv(t, o, proto.VerbKilled, swarmKey(0))
	noEv(t, o, proto.VerbWarn, swarmKey(0))
}

// Groups below min_victim/8 are never victims, however many there are.
func TestTinyGroupsAreNoVictims(t *testing.T) {
	const n = 40
	m := swarmSim(t, n, 7*MiB) // 280M together, each below 8M
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "inn")
	if k := killedSwarm(o, n); len(k) != 0 {
		t.Fatalf("tiny groups killed: %v", k)
	}
}

// A big unmanaged group that covers the deficit is the victim; the small
// groups of the same priority are not touched, though they are younger.
func TestBigUnitCoversSmallSwarmSpared(t *testing.T) {
	const n = 30
	m := swarmSim(t, n, 50*MiB)
	m.unit("pg:900:1", 900, T0-1_000_000, 2*GiB) // old, priority 2
	m.tick(100)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "pg:900:1")
	noEv(t, o, proto.VerbKilled, "inn")
	if k := killedSwarm(o, n); len(k) != 0 {
		t.Fatalf("small groups killed although the big unit covers the deficit: %v", k)
	}
}

// A big unmanaged group too small for the deficit goes first, then the
// youngest small groups; the job still survives.
func TestBigUnitThenSmallSwarm(t *testing.T) {
	const n = 30
	m := swarmSim(t, n, 50*MiB)
	m.unit("pg:900:1", 900, T0-1_000_000, 300*MiB)
	m.tick(100)
	m.setFree(1 * GiB) // deficit 778M: 300M + 10 x 50M
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "pg:900:1")
	noEv(t, o, proto.VerbKilled, "inn")
	k := killedSwarm(o, n)
	if len(k) != 10 || k[0] != n-10 {
		t.Fatalf("killed swarm %v, want the 10 youngest", k)
	}
	// Big before small within the priority: the big unit's KILLED comes
	// first, although it is the oldest.
	for _, e := range o.Events {
		if e.Verb == proto.VerbKilled {
			eq(t, "first KILLED", e.Unit, "pg:900")
			break
		}
	}
}

// A job of lower priority than unmanaged groups (priority 1 < 2) that
// covers the deficit is the victim before any small group.
func TestLowerPriorityJobBeforeSmallSwarm(t *testing.T) {
	const n = 30
	m := swarmSim(t, n, 50*MiB)
	m.admitStart("low", proto.SubmitRequest{EstimateBytes: GiB, BudgetBytes: 8 * GiB, Priority: intp(1)}, 200, GiB)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "low")
	noEv(t, o, proto.VerbKilled, "inn")
	if k := killedSwarm(o, n); len(k) != 0 {
		t.Fatalf("small groups killed although a lower-priority job covers the deficit: %v", k)
	}
}

// noSwarmEvent fails when any swarm group got verb in o.
func noSwarmEvent(t *testing.T, o Output, n int, verb string) {
	t.Helper()
	for i := 0; i < n; i++ {
		if evFor(o, verb, swarmKey(i)) != nil {
			t.Fatalf("swarm group %d got %s:\n%s", i, verb, dumpEvents(o))
		}
	}
}

// A swarm (at least min_victim together) that cannot cover the deficit
// before the job would be chosen is left alone at critical: killing it
// would not spare the job, so only the job dies, as without the swarm.
func TestSmallSwarmShortOfDeficitSpared(t *testing.T) {
	const n = 10
	m := swarmSim(t, n, 20*MiB) // 200M together, deficit 778M
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "inn")
	noSwarmEvent(t, o, n, proto.VerbWarn)
	noSwarmEvent(t, o, n, proto.VerbKilled)
}

// The same at warn: a swarm that cannot cover the deficit is not warned;
// the job is.
func TestSmallSwarmShortOfDeficitNotWarned(t *testing.T) {
	const n = 10
	m := swarmSim(t, n, 50*MiB) // 500M together, deficit 1.04G
	m.setFree(2 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbWarn, "inn")
	noSwarmEvent(t, o, n, proto.VerbWarn)
}

// Small groups that cover the deficit together with a big unmanaged group
// (of the same, lower priority) are victims, the job is not: 300M + 10 x
// 50M >= 778M.
func TestSmallSwarmCoversWithBigUnit(t *testing.T) {
	const n = 10
	m := swarmSim(t, n, 50*MiB)
	m.unit("pg:900:1", 900, T0-1_000_000, 300*MiB)
	m.tick(100)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "pg:900:1")
	noEv(t, o, proto.VerbWarn, "inn")
	noEv(t, o, proto.VerbKilled, "inn")
	if k := killedSwarm(o, n); len(k) != n {
		t.Fatalf("killed swarm %v, want all %d", k, n)
	}
}

// One small group less (300M + 9 x 50M < 778M) and they cannot spare the
// job: the big unit and the job are the victims, no small group is.
func TestSmallSwarmShortWithBigUnit(t *testing.T) {
	const n = 9
	m := swarmSim(t, n, 50*MiB)
	m.unit("pg:900:1", 900, T0-1_000_000, 300*MiB)
	m.tick(100)
	m.setFree(1 * GiB)
	o := m.tick(100)
	mustEv(t, o, proto.VerbKilled, "pg:900:1")
	mustEv(t, o, proto.VerbKilled, "inn")
	noSwarmEvent(t, o, n, proto.VerbWarn)
	noSwarmEvent(t, o, n, proto.VerbKilled)
}

// A unit below min_victim is never a mem_budget victim, even with an
// unmanaged budget below its size; a big unit over the budget still is.
func TestSmallUnitNoMemBudget(t *testing.T) {
	p := testParams()
	p.UnmanagedBudgetBytes = 32 * MiB
	m := newSim(t, p)
	m.unit("pg:500:1", 500, T0, 50*MiB)
	m.unit("pg:600:1", 600, T0, 100*MiB)
	o := m.run(300, 100) // 30s: past the warn deadline
	for _, e := range o.Events {
		if e.Unit == "pg:500" && e.Policy == proto.PolicyMemBudget {
			t.Fatalf("small unit got a mem_budget %s:\n%s", e.Verb, dumpEvents(o))
		}
	}
	mustEv(t, o, proto.VerbKilled, "pg:600:1")
}
