package proto

import (
	"os"
	"strings"
	"testing"
)

// Round 3: docs/CONTRACT.md states what the code does where it used to
// promise otherwise. Each entry is a line (by its leading text) and what it
// must say.
func TestContractDocumentsBehaviour(t *testing.T) {
	raw, err := os.ReadFile("../../docs/CONTRACT.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	line := func(prefix string) string {
		for _, l := range strings.Split(doc, "\n") {
			if strings.HasPrefix(l, prefix) {
				return l
			}
		}
		t.Fatalf("CONTRACT.md has no line starting %q", prefix)
		return ""
	}
	for _, c := range []struct{ prefix, want string }{
		// free on pressure lines is the memory available, not headroom.
		{"| `free` |", "On `policy=pressure` lines"},
		{"| `free` |", "`avail_bytes`"},
		// at disk-critical a new submission is refused, --no-wait or not.
		{"| 200 |", "reason=disk"},
		{"| `--no-wait` |", "reason=disk"},
		{"| `REFUSED` |", "with or without `--no-wait`"},
		// pgid 0/1 is split by subtree; the pace run's subtree is never a unit.
		{"Some process groups are never a unit", "subtree that holds the daemon or a `pace run`"},
		// status file for exec failures and usage errors.
		{"The job status file", "(127)"},
		{"The job status file", "(202"},
	} {
		if l := line(c.prefix); !strings.Contains(l, c.want) {
			t.Errorf("CONTRACT.md line %q does not say %q", c.prefix, c.want)
		}
	}
	if strings.Contains(doc, "never a unit, so pace never throttles, freezes or kills them, whatever they hold (their memory still counts as used): pgid 0 or 1,") {
		t.Error("CONTRACT.md still lists pgid 0 or 1 as a whole as never a unit")
	}
}
