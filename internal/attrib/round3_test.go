package attrib

import (
	"reflect"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

// Round 3: in init's group (pgid 0/1, split by subtree), the subtree that
// holds a pace run (the agent that ran it, its helpers) or the daemon is
// never a unit, as the group of any pace run is not. Other subtrees still are.
func TestInitGroupSubtreeOfPaceRunNeverUnit(t *testing.T) {
	tr := New(Options{MinUnitBytes: 64 * M, SelfPIDs: []int{30}})
	tr.SetProtectedPIDs([]int{51})
	procs := []platform.Proc{
		pr(1, 0, 1, 100, 5, 0),      // a PID 1 entrypoint script without job control
		pr(30, 1, 1, 150, 20, 0),    // the daemon, in pgid 1
		pr(31, 30, 1, 160, 200, 0),  // a child of the daemon, pgid 1
		pr(50, 1, 1, 200, 900, 0),   // the agent, pgid 1
		pr(51, 50, 1, 300, 5, 0),    // its pace run (protected wrapper), pgid 1
		pr(52, 51, 52, 310, 100, 0), // the job, its own group
		pr(53, 50, 1, 320, 300, 0),  // an agent helper, pgid 1
		pr(60, 1, 1, 400, 700, 0),   // an unrelated subtree of pid 1
		pr(61, 60, 1, 410, 100, 0),  // its child
	}
	jobs := []policy.Job{job("j", 52)}
	for range 2 {
		r := tr.Attribute(procs, jobs, nil)
		if keys := unitKeys(r); !reflect.DeepEqual(keys, []string{"pg:60:400"}) {
			t.Fatalf("units %v, want only the unrelated subtree pg:60:400", keys)
		}
	}
}
