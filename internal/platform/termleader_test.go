package platform

import "testing"

// Finding (e), Linux: /proc/pid/stat session (field 6) and tty_nr (field 7)
// are parsed; the scanner marks only a session leader with a terminal as
// TermLeader (the fixture's pid 100 in TestScannerFixture).
func TestParseStatSessionTTY(t *testing.T) {
	const tail = " 4194560 1234 0 5 0 250 50 0 0 20 0 3 0 12345 123456789 2560\n"
	for _, c := range []struct {
		line       string
		sess       int
		tty        int64
		termLeader bool
	}{
		{"200 (zsh) S 1 200 200 34816 200" + tail, 200, 34816, true},
		{"201 (cc) R 200 201 200 34816 201" + tail, 200, 34816, false}, // a job of that shell
		{"202 (app) S 1 202 202 0 -1" + tail, 202, 0, false},           // setsid, no terminal
	} {
		s, err := parseStat([]byte(c.line))
		if err != nil {
			t.Fatalf("%q: %v", c.line, err)
		}
		if s.Session != c.sess || s.TTYNr != c.tty {
			t.Errorf("%q: session %d tty %d, want %d %d", c.line, s.Session, s.TTYNr, c.sess, c.tty)
		}
		if got := s.Session == s.PID && s.TTYNr != 0; got != c.termLeader {
			t.Errorf("%q: term leader %v", c.line, got)
		}
	}
}
