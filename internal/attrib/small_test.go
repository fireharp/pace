package attrib

import (
	"reflect"
	"testing"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/policy"
)

// Groups below MinUnitBytes are not Units, but they are counted (every one,
// tracked or not) and the largest untracked ones listed in SmallUnits,
// capped by MaxUnits on their own: a swarm larger than the cap still shows
// its largest members, and the big units keep their own MaxUnits slots.
func TestSmallUnitsSummarisedAndListed(t *testing.T) {
	tr := New(Options{MinUnitBytes: 64 * M, MaxUnits: 2})
	procs := []platform.Proc{
		pr(10, 1, 10, 1, 100, 0), // big
		pr(20, 1, 20, 2, 50, 0),  // small
		pr(21, 20, 20, 3, 5, 0),  //   (55M together)
		pr(30, 1, 30, 4, 30, 0),  // small
		pr(40, 1, 40, 5, 2, 0),   // small
		pr(50, 1, 50, 6, 20, 0),  // small, tracked
		pr(60, 1, 60, 7, 40, 0),  // small
	}
	tracked := []policy.Unit{{Key: "pg:50:6", PGID: 50, StartMS: 6}}
	r := tr.Attribute(procs, nil, tracked)
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:10:1", "pg:50:6"}) {
		t.Errorf("units %v", unitKeys(r))
	}
	var sk []string
	for _, u := range r.SmallUnits {
		sk = append(sk, u.Key)
	}
	// The two largest untracked small groups (55M and 40M), sorted by key.
	if !reflect.DeepEqual(sk, []string{"pg:20:2", "pg:60:7"}) {
		t.Fatalf("small units %v", sk)
	}
	if r.SmallUnits[0].NProcs != 2 || r.SmallUnits[0].UsedBytes != 55*M {
		t.Errorf("small unit %+v", r.SmallUnits[0])
	}
	if r.SmallGroups != 5 || r.SmallBytes != (55+30+2+20+40)*M {
		t.Errorf("small summary %d groups %dM", r.SmallGroups, r.SmallBytes/M)
	}

	// A group that is never a unit (here the daemon's) is not in the
	// summary either.
	tr2 := New(Options{MinUnitBytes: 64 * M, SelfPGIDs: []int{20}})
	r2 := tr2.Attribute(procs, nil, nil)
	if r2.SmallGroups != 4 || r2.SmallBytes != (30+2+20+40)*M {
		t.Errorf("small summary without the daemon's group: %d groups %dM", r2.SmallGroups, r2.SmallBytes/M)
	}
	for _, u := range r2.SmallUnits {
		if u.PGID == 20 {
			t.Errorf("daemon's group listed as small unit: %+v", u)
		}
	}

	// No minimum: nothing is small.
	if r3 := New(Options{}).Attribute(procs, nil, nil); r3.SmallGroups != 0 || len(r3.SmallUnits) != 0 {
		t.Errorf("small without a minimum: %d %v", r3.SmallGroups, r3.SmallUnits)
	}
}

// pace's own clients (the pace binary run as `pace status`, the menu bar
// app) are never small units, with the rest of their group: they are still
// counted. A big group is a unit whatever it holds.
func TestSmallUnitsExcludePaceClients(t *testing.T) {
	procs := []platform.Proc{
		pr(10, 1, 10, 1, 20, 0),  // agent shell ...
		pr(11, 10, 10, 2, 12, 0), // ... polling `pace status`
		pr(20, 1, 20, 3, 30, 0),  // menu bar app
		pr(30, 1, 30, 4, 25, 0),  // unrelated small group
		pr(40, 1, 40, 5, 100, 0), // big group running the pace binary
	}
	procs[1].Exe = "/opt/pace/bin/pace"
	procs[2].Comm, procs[2].Exe = "PaceBar", "/Users/u/Applications/PaceBar.app/Contents/MacOS/PaceBar"
	procs[4].Exe = "/opt/pace/bin/pace"
	tr := New(Options{MinUnitBytes: 64 * M, ClientExes: []string{"/opt/pace/bin/pace"}, ClientNames: []string{"PaceBar"}})
	r := tr.Attribute(procs, nil, nil)
	var sk []string
	for _, u := range r.SmallUnits {
		sk = append(sk, u.Key)
	}
	if !reflect.DeepEqual(sk, []string{"pg:30:4"}) {
		t.Fatalf("small units %v, want only the unrelated group", sk)
	}
	if r.SmallGroups != 3 || r.SmallBytes != (32+30+25)*M {
		t.Errorf("small summary %d groups %dM", r.SmallGroups, r.SmallBytes/M)
	}
	if !reflect.DeepEqual(unitKeys(r), []string{"pg:40:5"}) {
		t.Errorf("units %v", unitKeys(r))
	}
	// Without the options they are small units like any other.
	if r2 := New(Options{MinUnitBytes: 64 * M}).Attribute(procs, nil, nil); len(r2.SmallUnits) != 3 {
		t.Errorf("small units without client options: %d", len(r2.SmallUnits))
	}
}
