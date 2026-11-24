package attrib

import (
	"reflect"
	"testing"

	"github.com/fireharp/pace/internal/platform"
)

// Safety (e): an unmanaged unit is never the scope root's group, the
// daemon's or a wrapper's group, nor all of pgid 0/1 (those are split by
// subtree, see TestInitGroupSplitBySubtree); their processes still count in
// scoped usage.
func TestUnitNeverRootDaemonWrapperGroup(t *testing.T) {
	tr := New(Options{SelfPIDs: []int{10}, SelfPGIDs: []int{7}, ScopeRootPID: 100})
	tr.SetProtectedPIDs([]int{120})
	procs := []platform.Proc{
		pr(100, 90, 90, 1000, 5, 0),    // scope root (a test shell), in its caller's group 90
		pr(101, 100, 90, 1100, 900, 0), // its job without job control: shares group 90
		pr(102, 100, 102, 1200, 800, 0),
		pr(103, 102, 102, 1210, 100, 0), // group 102: a unit of its own
		pr(10, 100, 9, 1300, 30, 0),     // the daemon, in group 9
		pr(104, 100, 9, 1310, 700, 0),   // shares the daemon's group
		pr(105, 100, 7, 1320, 600, 0),   // in the daemon's configured group
		pr(120, 100, 120, 1400, 5, 0),   // a wrapper, leading group 120
		pr(121, 120, 120, 1410, 500, 0), // in the wrapper's group (not its job's)
		pr(106, 100, 0, 1500, 400, 0),   // pgid 0 (outside a pid namespace)
		pr(107, 100, 1, 1510, 300, 0),   // pgid 1
	}
	r := tr.Attribute(procs, nil, nil)
	if keys := unitKeys(r); !reflect.DeepEqual(keys, []string{"pg:102:1200", "pg:106:1500", "pg:107:1510"}) {
		t.Fatalf("units %v, want group 102 and the pgid 0/1 subtrees 106, 107", keys)
	}
	// Everything in scope except the daemon (self) and the wrapper
	// (protected) counts.
	if want := int64(900+800+100+700+600+500+400+300) * M; r.ScopedUsedBytes != want {
		t.Errorf("scoped %d MiB, want %d", r.ScopedUsedBytes/M, want/M)
	}

	// The root's group stays excluded after the root exits (its jobs are
	// reparented and still share the group).
	r = tr.Attribute([]platform.Proc{pr(101, 1, 90, 1100, 900, 0), pr(102, 1, 102, 1200, 800, 0)}, nil, nil)
	if keys := unitKeys(r); !reflect.DeepEqual(keys, []string{"pg:102:1200"}) {
		t.Fatalf("after the root exited: units %v", keys)
	}
}

// Safety (e), production (no scope): a terminal's shell is never a unit
// member, whatever runs in its group; the rest of the group still is.
// A TermLeader as big as a victim is not a shell (exec replaced it).
func TestTermLeaderShellNeverUnitMember(t *testing.T) {
	tr := New(Options{MinUnitBytes: 64 * M})
	shell := pr(500, 1, 500, 1000, 8, 0)
	shell.TermLeader = true
	execd := pr(600, 1, 600, 1000, 2048, 0)
	execd.TermLeader = true
	idle := pr(700, 1, 700, 1000, 6, 0) // a terminal with nothing heavy in it
	idle.TermLeader = true
	procs := []platform.Proc{
		shell,
		pr(501, 500, 500, 1100, 900, 0), // `sh -c 'make'` style: no job control
		execd,
		idle,
	}
	r := tr.Attribute(procs, nil, nil)
	if keys := unitKeys(r); !reflect.DeepEqual(keys, []string{"pg:500:1000", "pg:600:1000"}) {
		t.Fatalf("units %v", keys)
	}
	u, _ := unitByPG(r, 500)
	if !reflect.DeepEqual(u.PIDs, []int{501}) || u.LeaderPID != 501 || u.UsedBytes != 900*M {
		t.Errorf("shell's group unit %+v: want only its heavy member", u)
	}
	if r.ScopedUsedBytes != (8+900+2048+6)*M {
		t.Errorf("scoped %d MiB", r.ScopedUsedBytes/M)
	}
}
