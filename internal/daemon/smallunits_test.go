package daemon

import (
	"syscall"
	"testing"

	"github.com/fireharp/pace/internal/attrib"
	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// A swarm of unmanaged groups, each below min_victim: status counts them at
// any pressure; the policy sees them (and pace acts on them, youngest
// first) only once memory pressure is above normal.
func TestSmallUnitSwarmUnderPressure(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	var lastIn policy.Inputs
	step := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		lastIn = in
		return policy.Step(p, s, in)
	}
	d := newStepDaemon(t, f, step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.CapacityBytes = 2 * giB // warn below 307M avail, critical below 143M
	})
	swarm := func(n int) []platform.Proc {
		var ps []platform.Proc
		for i := 0; i < n; i++ {
			ps = append(ps, proc(1000+i, 1, 1000+i, now-100_000+int64(i)*1000, 50*miB))
		}
		return ps
	}

	// 10 x 50M: normal pressure. Not offered to the policy, but counted.
	setProcs(f, swarm(10)...)
	d.runStep(true)
	if len(lastIn.Units) != 0 {
		t.Fatalf("small groups offered at normal pressure: %+v", lastIn.Units)
	}
	st := d.snap.Load().status
	if st.UnmanagedBelowMinVictim == nil || st.UnmanagedBelowMinVictim.Groups != 10 ||
		st.UnmanagedBelowMinVictim.UsedBytes != 500*miB || st.UnmanagedBelowMinVictim.MinVictimBytes != 64*miB {
		t.Fatalf("status small summary: %+v", st.UnmanagedBelowMinVictim)
	}
	if len(st.Unmanaged) != 0 {
		t.Fatalf("status lists small groups at normal pressure: %+v", st.Unmanaged)
	}
	if len(f.Signals()) != 0 {
		t.Fatalf("signals at normal pressure: %s", fmtSent(f.Signals()))
	}

	// 39 x 50M = 1950M (avail 98M): critical. The policy sees them and kills the
	// youngest until the deficit is covered; the oldest is spared.
	setProcs(f, swarm(39)...)
	now += 100
	d.runStep(true)
	if len(lastIn.Units) != 39 {
		t.Fatalf("units offered under pressure: %d, want 39", len(lastIn.Units))
	}
	sent := f.Signals()
	if !hasSignal(sent, 0, 1038, syscall.SIGTERM) {
		t.Fatalf("youngest swarm group not signalled: %s", fmtSent(sent))
	}
	if hasSignal(sent, 0, 1000, syscall.SIGTERM) {
		t.Fatalf("oldest swarm group signalled although the youngest cover the deficit: %s", fmtSent(sent))
	}
	st = d.snap.Load().status
	if len(st.Unmanaged) == 0 {
		t.Fatal("status lists no unmanaged group under pressure")
	}
}

// The way back down: at warn the youngest small groups are WARNed and then
// THROTTLED; once pressure is normal again, the warned ones stay tracked
// (still in the inputs) and get OK and UNTHROTTLED, while the untracked
// small groups leave the inputs.
func TestSmallUnitSwarmWarnThenNormal(t *testing.T) {
	now := int64(10_000_000)
	f := platform.NewFake()
	var lastIn policy.Inputs
	var evs []proto.Event
	step := func(p policy.Params, s policy.State, in policy.Inputs) (policy.State, policy.Output) {
		lastIn = in
		ns, out := policy.Step(p, s, in)
		evs = append(evs, out.Events...)
		return ns, out
	}
	d := newStepDaemon(t, f, step, &now, func(c *config.Resolved) {
		c.Params = realParams()
		c.Params.CapacityBytes = 2 * giB // warn below 307M avail, critical below 143M
	})
	t0 := now - 100_000
	group := func(i int) platform.Proc {
		return proc(1000+i, 1, 1000+i, t0+int64(i)*1000, 50*miB)
	}
	swarm := func(from, to int) []platform.Proc {
		var ps []platform.Proc
		for i := from; i < to; i++ {
			ps = append(ps, group(i))
		}
		return ps
	}
	key := func(i int) string { return attrib.UnitKey(1000+i, t0+int64(i)*1000) }
	unitIn := func(i int) bool {
		for _, u := range lastIn.Units {
			if u.Key == key(i) {
				return true
			}
		}
		return false
	}
	verbFor := func(verb string, i int) bool {
		for _, e := range evs {
			if e.Verb == verb && e.Unit == proto.UnitRef(key(i)) {
				return true
			}
		}
		return false
	}
	ps := swarm(0, 36) // 1800M: avail 248M, warn; deficit ~141M: the 3 youngest
	tick := func(dt int64) {
		now += dt
		setProcs(f, ps...)
		d.runStep(true)
	}
	tick(100)
	if len(lastIn.Units) != 36 {
		t.Fatalf("units offered at warn: %d, want 36", len(lastIn.Units))
	}
	for i := 0; i < 36; i++ {
		if w := verbFor(proto.VerbWarn, i); w != (i >= 33) {
			t.Fatalf("group %d warned=%v, want the 3 youngest: %+v", i, w, evs)
		}
	}
	tick(20_100) // past the warn deadline: throttled
	for i := 33; i < 36; i++ {
		if !verbFor(proto.VerbThrottled, i) || !f.Background[1000+i] {
			t.Fatalf("group %d not throttled: background=%v %+v", i, f.Background, evs)
		}
	}

	// Pressure eases: 6 groups left (300M), three of them the warned ones.
	ps = swarm(30, 36)
	evs = nil
	tick(100)
	for i := 30; i < 33; i++ {
		if unitIn(i) {
			t.Fatalf("untracked small group %d still offered at normal pressure: %+v", i, lastIn.Units)
		}
	}
	for i := 33; i < 36; i++ {
		if !unitIn(i) {
			t.Fatalf("warned group %d left the inputs before it was released: %+v", i, lastIn.Units)
		}
	}
	for n := 0; n < 10; n++ {
		tick(2_000) // recover hold, then one step down per StepMS
	}
	for i := 33; i < 36; i++ {
		if !verbFor(proto.VerbOK, i) || !verbFor(proto.VerbUnthrottled, i) || f.Background[1000+i] {
			t.Fatalf("group %d not released (OK, UNTHROTTLED): background=%v %+v", i, f.Background, evs)
		}
	}
	if len(lastIn.Units) != 0 {
		t.Fatalf("released small groups still offered at normal pressure: %+v", lastIn.Units)
	}
	if sent := f.Signals(); len(sent) != 0 {
		t.Fatalf("signal sent to a small group: %s", fmtSent(sent))
	}
}

func TestClientNamesIncludeBinaryName(t *testing.T) {
	got := clientNames([]string{"/opt/homebrew/Cellar/pace/0.2/bin/pace", "/usr/local/bin/pace"})
	want := map[string]bool{TrayName: true, "pace": true}
	for _, n := range got {
		if !want[n] {
			t.Fatalf("unexpected client name %q in %v", n, got)
		}
		delete(want, n)
	}
	if len(want) != 0 {
		t.Fatalf("missing %v in %v", want, got)
	}
}
