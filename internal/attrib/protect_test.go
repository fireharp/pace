package attrib

import (
	"reflect"
	"testing"

	"github.com/fireharp/pace/internal/platform"
)

// A process named in ProtectNames makes its whole group never a unit (pace
// signals groups), and in init's group its subtree; other groups are not
// affected. Names match the comm, the executable's base name, or a 15-byte
// Linux comm that a longer name starts with.
func TestProtectNames(t *testing.T) {
	orch := pr(200, 1, 200, 1000, 300, 0) // agent-swarm, leading group 200
	orch.Comm, orch.Exe = "agent-swarm", "/usr/local/bin/agent-swarm"
	tool := pr(201, 200, 200, 1010, 900, 0) // a tool it started in its group
	other := pr(300, 1, 300, 1100, 800, 0)  // an unrelated heavy group
	other.Comm = "node"
	long := pr(400, 1, 400, 1200, 700, 0)      // comm cut to 15 bytes by Linux
	long.Comm, long.Exe = "my-orchestrato", "" // 14 bytes: no prefix match
	long15 := pr(500, 1, 500, 1300, 600, 0)
	long15.Comm = "supervisor-long"        // 15 bytes, a prefix of supervisor-long-name
	initKid := pr(600, 1, 1, 1400, 500, 0) // pgid 1: its own subtree
	initKid.Comm = "agent-swarm"
	initGrand := pr(601, 600, 1, 1410, 400, 0)
	procs := []platform.Proc{orch, tool, other, long, long15, initKid, initGrand}

	tr := New(Options{})
	if keys := unitKeys(tr.Attribute(procs, nil, nil)); len(keys) != 5 {
		t.Fatalf("without protection: units %v", keys)
	}
	tr = New(Options{ProtectNames: []string{"agent-swarm", "supervisor-long-name"}})
	r := tr.Attribute(procs, nil, nil)
	if keys := unitKeys(r); !reflect.DeepEqual(keys, []string{"pg:300:1100", "pg:400:1200"}) {
		t.Fatalf("units %v, want only groups 300 and 400", keys)
	}
	// Protected processes still count in scoped usage.
	if want := int64(300+900+800+700+600+500+400) * M; r.ScopedUsedBytes != want {
		t.Errorf("scoped %d MiB, want %d", r.ScopedUsedBytes/M, want/M)
	}
	// By executable name too, and SetOptions (reload) applies a new list.
	tr.SetOptions(Options{ProtectNames: []string{"node"}})
	other.Exe, other.Comm = "/opt/node/bin/node", "MainThread"
	r = tr.Attribute([]platform.Proc{other, tool}, nil, nil)
	for _, k := range unitKeys(r) {
		if k == "pg:300:1100" {
			t.Fatalf("exe-name protection ignored: %v", unitKeys(r))
		}
	}
}
