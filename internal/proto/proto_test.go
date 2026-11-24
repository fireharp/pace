package proto

import "testing"

func TestUnitRef(t *testing.T) {
	for in, want := range map[string]string{
		"pg:4242:1795361324119": "pg:4242",
		"pg:4242":               "pg:4242",
		"pg:1:":                 "pg:1",
		"other":                 "other",
		"":                      "",
	} {
		if got := UnitRef(in); got != want {
			t.Errorf("UnitRef(%q) = %q, want %q", in, got, want)
		}
	}
}
