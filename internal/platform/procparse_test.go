package platform

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// ---------------------------------------------------------------------------
// Pure parsers.

func TestParseFixed(t *testing.T) {
	cases := []struct {
		in   string
		dec  int
		want int64
		ok   bool
	}{
		{"12.34", 1, 123, true},
		{"12.35", 1, 123, true}, // truncates, never rounds up
		{"0.00", 1, 0, true},
		{"100.00", 1, 1000, true},
		{"7", 1, 70, true},
		{"0.52", 3, 520, true},
		{"1.07", 3, 1070, true},
		{"12.5", 3, 12500, true},
		{"3.", 2, 300, true},
		{".5", 1, 5, true},
		{"", 1, 0, false},
		{".", 1, 0, false},
		{"1.2.3", 1, 0, false},
		{"-1.0", 1, 0, false},
		{"1e3", 1, 0, false},
		{"abc", 1, 0, false},
		{"99999999999999999999999", 1, 0, false},
	}
	for _, c := range cases {
		got, ok := parseFixed([]byte(c.in), c.dec)
		if got != c.want || ok != c.ok {
			t.Errorf("parseFixed(%q,%d) = %d,%v want %d,%v", c.in, c.dec, got, ok, c.want, c.ok)
		}
	}
}

func TestParseUintInt(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"0", 0, true}, {"18446744073709551615", 1<<64 - 1, true}, {"18446744073709551616", 0, false},
		{"", 0, false}, {"12a", 0, false}, {"-1", 0, false}, {" 1", 0, false},
	} {
		got, ok := parseUint([]byte(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("parseUint(%q) = %d,%v", c.in, got, ok)
		}
	}
	for _, c := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"-5", -5, true}, {"5", 5, true}, {"-", 0, false}, {"9223372036854775808", 0, false},
	} {
		got, ok := parseInt([]byte(c.in))
		if got != c.want || ok != c.ok {
			t.Errorf("parseInt(%q) = %d,%v", c.in, got, ok)
		}
	}
}

func statLine(pid, comm, rest string) string {
	return pid + " (" + comm + ") " + rest
}

const statTail = "S 1 100 100 34816 100 4194560 1234 0 5 0 250 50 0 0 20 0 3 0 12345 123456789 2560 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0\n"

func TestParseStat(t *testing.T) {
	good := procStat{PID: 100, State: 'S', PPID: 1, PGRP: 100, Session: 100, TTYNr: 34816, Flags: 4194560, UTime: 250, STime: 50, StartTime: 12345, RSSPages: 2560}
	cases := []struct {
		name, in, comm string
		want           *procStat // nil = error expected
	}{
		{"plain", statLine("100", "bash", statTail), "bash", &good},
		{"paren space paren", statLine("100", "we) (ird", statTail), "we) (ird", &good},
		{"trailing paren", statLine("100", "a b)", statTail), "a b)", &good},
		{"only parens", statLine("100", ")(", statTail), ")(", &good},
		{"empty comm", statLine("100", "", statTail), "", &good},
		{"fake fields in comm", statLine("100", "x) R 9 9 9", statTail), "x) R 9 9 9", &good},
		{"no trailing newline", strings.TrimSuffix(statLine("100", "c", statTail), "\n"), "c", &good},
		{"exactly 24 fields", statLine("100", "c", "S 1 100 100 34816 100 4194560 1234 0 5 0 250 50 0 0 20 0 3 0 12345 123456789 2560"), "c", &good},
		{"missing fields", statLine("100", "c", "S 1 100 100"), "", nil},
		{"23 fields", statLine("100", "c", "S 1 100 100 34816 100 4194560 1234 0 5 0 250 50 0 0 20 0 3 0 12345 123456789"), "", nil},
		{"no parens", "100 bash S 1 100", "", nil},
		{"no close paren", "100 (bash S 1 100", "", nil},
		{"bad pid", statLine("x", "c", statTail), "", nil},
		{"pid zero", statLine("0", "c", statTail), "", nil},
		{"bad ppid", statLine("100", "c", strings.Replace(statTail, "S 1 ", "S z ", 1)), "", nil},
		{"two-char state", statLine("100", "c", strings.Replace(statTail, "S 1 ", "SS 1 ", 1)), "", nil},
		{"empty", "", "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseStat([]byte(c.in))
			if c.want == nil {
				if err == nil {
					t.Fatalf("want error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if string(got.Comm) != c.comm {
				t.Errorf("comm = %q want %q", got.Comm, c.comm)
			}
			got.Comm = nil
			if !reflect.DeepEqual(got, *c.want) {
				t.Errorf("got %+v want %+v", got, *c.want)
			}
		})
	}
	// negative rss clamps to 0
	got, err := parseStat([]byte(statLine("5", "c", strings.Replace(statTail, " 123456789 2560 ", " 123456789 -3 ", 1))))
	if err != nil || got.RSSPages != 0 {
		t.Errorf("negative rss: %+v %v", got, err)
	}
}

func TestNormState(t *testing.T) {
	want := map[byte]byte{'R': 'R', 'S': 'S', 'D': 'D', 'T': 'T', 'Z': 'Z', 't': 'T', 'X': 'X', 'x': 'X', 'I': 'S', 'P': 'S', 'K': 'S', 'W': 'S', '?': 'S'}
	for in, w := range want {
		if g := normState(in); g != w {
			t.Errorf("normState(%c) = %c want %c", in, g, w)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	full := "MemTotal: 1000 kB\nMemFree: 100 kB\nMemAvailable: 600 kB\nBuffers: 10 kB\nCached: 20 kB\nSwapTotal: 50 kB\nSwapFree: 20 kB\nHugePages_Total: 0\n"
	m, err := parseMeminfo([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if (m != memInfo{Total: 1000 << 10, Avail: 600 << 10, SwapTotal: 50 << 10, SwapFree: 20 << 10}) {
		t.Errorf("got %+v", m)
	}
	// No MemAvailable (kernel < 3.14): MemFree + Buffers + Cached.
	m, err = parseMeminfo([]byte("MemTotal: 1000 kB\nMemFree: 100 kB\nBuffers: 10 kB\nCached: 20 kB\n"))
	if err != nil || m.Avail != 130<<10 {
		t.Errorf("fallback: %+v %v", m, err)
	}
	// Avail > Total and SwapFree > SwapTotal are clamped.
	m, err = parseMeminfo([]byte("MemTotal: 10 kB\nMemAvailable: 20 kB\nSwapTotal: 1 kB\nSwapFree: 5 kB\n"))
	if err != nil || m.Avail != m.Total || m.SwapFree != m.SwapTotal {
		t.Errorf("clamp: %+v %v", m, err)
	}
	for _, bad := range []string{"", "MemFree: 1 kB\n", "MemTotal: x kB\n", "MemTotal: 0 kB\n", "MemTotal: 10 kB\nMemAvailable: -1 kB\n"} {
		if _, err := parseMeminfo([]byte(bad)); err == nil {
			t.Errorf("parseMeminfo(%q): want error", bad)
		}
	}
}

func TestParsePSI(t *testing.T) {
	cases := []struct {
		in         string
		some, full int64
	}{
		{"some avg10=12.34 avg60=5.00 avg300=1.00 total=1\nfull avg10=6.78 avg60=2.00 avg300=0.50 total=2\n", 123, 67},
		{"some avg10=0.00 avg60=0.00 avg300=0.00 total=0\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n", 0, 0},
		{"some avg10=100.00 avg60=0 avg300=0 total=0\n", 1000, -1}, // cpu-style: no full line
		{"full avg10=1.00 avg60=0 avg300=0 total=0\n", -1, 10},
		{"", -1, -1},
		{"some avg10=abc\nfull avg10=\n", -1, -1},
		{"some avg60=1.00\n", -1, -1},
		{"some avg10=1000.0\n", -1, -1}, // impossible > 100 %
		{"garbage line\nsome avg10=5.5\n", 55, -1},
	}
	for _, c := range cases {
		s, f := parsePSI([]byte(c.in))
		if s != c.some || f != c.full {
			t.Errorf("parsePSI(%q) = %d,%d want %d,%d", c.in, s, f, c.some, c.full)
		}
	}
}

func TestParseLoadavgBtime(t *testing.T) {
	for in, want := range map[string]int64{"0.52 1.07 2.50 3/467 12345\n": 520, "12.00 1 1 1/1 1": 12000, "0.00": 0} {
		got, err := parseProcLoadavg([]byte(in))
		if err != nil || got != want {
			t.Errorf("parseProcLoadavg(%q) = %d,%v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x 1 1", "-1 2 3"} {
		if _, err := parseProcLoadavg([]byte(bad)); err == nil {
			t.Errorf("parseProcLoadavg(%q): want error", bad)
		}
	}
	bt, err := parseBtime([]byte("cpu 1 2\nbtime 1700000000\nprocesses 3\n"))
	if err != nil || bt != 1700000000 {
		t.Errorf("btime = %d,%v", bt, err)
	}
	for _, bad := range []string{"", "cpu 1\n", "btime x\n", "btime 0\n"} {
		if _, err := parseBtime([]byte(bad)); err == nil {
			t.Errorf("parseBtime(%q): want error", bad)
		}
	}
}

func TestParseStatus(t *testing.T) {
	in := "Name:\tx\nUid:\t1000\t0\t0\t0\nVmRSS:\t   100 kB\nRssAnon:\t  60 kB\nRssFile:\t 30 kB\nRssShmem:\t 10 kB\nVmSwap:\t 5 kB\n"
	s := parseStatus([]byte(in))
	want := procStatus{UID: 1000, VmRSS: 100 << 10, RssAnon: 60 << 10, RssShmem: 10 << 10, VmSwap: 5 << 10, HaveRssAnon: true}
	if s != want {
		t.Errorf("got %+v", s)
	}
	if s.anonFootprint() != 75<<10 {
		t.Errorf("anon = %d", s.anonFootprint())
	}
	old := parseStatus([]byte("Uid:\t5 5 5 5\nVmRSS:\t100 kB\nVmSwap:\t7 kB\n"))
	if old.HaveRssAnon || old.anonFootprint() != 107<<10 || old.UID != 5 {
		t.Errorf("old kernel: %+v", old)
	}
	kthread := parseStatus([]byte("Name:\tkthreadd\nUid:\t0\t0\t0\t0\n"))
	if kthread.anonFootprint() != 0 || kthread.UID != 0 {
		t.Errorf("kthread: %+v", kthread)
	}
	if s := parseStatus(nil); s.UID != -1 {
		t.Errorf("empty: %+v", s)
	}
	if s := parseStatus([]byte("VmRSS:\tjunk kB\nUid:\tx\n")); s.VmRSS != 0 || s.UID != -1 {
		t.Errorf("junk: %+v", s)
	}
}

func TestParseStatmSmaps(t *testing.T) {
	r, sh, err := parseStatm([]byte("50000 2560 1024 100 0 3000 0\n"))
	if err != nil || r != 2560 || sh != 1024 {
		t.Errorf("statm: %d %d %v", r, sh, err)
	}
	for _, bad := range []string{"", "1 2", "1 x 3"} {
		if _, _, err := parseStatm([]byte(bad)); err == nil {
			t.Errorf("statm %q: want error", bad)
		}
	}
	sr := parseSmapsRollup([]byte("x-y ---p 0 00:00 0 [rollup]\nRss: 10 kB\nPss: 8 kB\nPss_Anon: 5 kB\nPss_Shmem: 1 kB\nSwap: 2 kB\nSwapPss: 1 kB\n"))
	if (sr != smapsRollup{Rss: 10 << 10, Pss: 8 << 10, PssAnon: 5 << 10, PssShmem: 1 << 10, Swap: 2 << 10, SwapPss: 1 << 10, HavePss: true, HavePssAnon: true}) {
		t.Errorf("rollup: %+v", sr)
	}
	if sr := parseSmapsRollup(nil); sr.HavePss {
		t.Errorf("empty rollup has pss")
	}
}

func TestEnvTag(t *testing.T) {
	z := func(s ...string) []byte { return []byte(strings.Join(s, "\x00")) }
	cases := []struct {
		in   []byte
		want string
	}{
		{z("A=1", "PACE_JOB_ID=j-1", "B=2", ""), "j-1"},
		{z("PACE_JOB_ID=first", "PACE_JOB_ID=second"), "first"},
		{z("XPACE_JOB_ID=no", "B=2"), ""},
		{z("PACE_JOB_ID="), ""},
		{z("A=1", "PACE_JOB_ID=last"), "last"}, // no trailing NUL
		{z("PACE_JOB_ID=has space"), ""},
		{z("PACE_JOB_ID=bad\x01ctl"), ""},
		{z("PACE_JOB_ID=" + strings.Repeat("a", 257)), ""},
		{z("PACE_JOB_ID=" + strings.Repeat("a", 256)), strings.Repeat("a", 256)},
		{nil, ""},
		{[]byte("PACE_JOB_ID"), ""},
	}
	for _, c := range cases {
		if got := envTag(c.in); got != c.want {
			t.Errorf("envTag(%q) = %q want %q", c.in, got, c.want)
		}
	}
}

func TestIsSystemProc(t *testing.T) {
	cases := []struct {
		name       string
		pid, ppid  int
		flags      uint64
		exe        string
		denied     bool
		wantSystem bool
	}{
		{"init", 1, 0, 0, "/home/u/app", false, true},
		{"kthreadd", 2, 0, pfKthread, "", false, true},
		{"kworker", 50, 2, pfKthread, "", false, true},
		{"kthread flag", 50, 7, pfKthread, "", false, true},
		// Finding 39: inside a PID namespace (podman --init, unshare --pid)
		// pid 2 is the user's entrypoint and its children have ppid 2.
		{"pidns pid 2", 2, 1, 0, "/usr/bin/bash", false, false},
		{"pidns child of pid 2", 40, 2, 0, "/home/u/bin/pacetest", false, false},
		{"pidns child of pid 2 no exe", 41, 2, 0, "", false, false},
		// Finding 39: toolchains under /usr/libexec subdirectories are work.
		{"fedora cc1plus", 41, 40, 0, "/usr/libexec/gcc/x86_64-redhat-linux/13/cc1plus", false, false},
		{"git-core helper", 42, 40, 0, "/usr/libexec/git-core/git-remote-http", false, false},
		{"podman conmon", 43, 40, 0, "/usr/libexec/podman/conmon", false, false},
		{"rhel qemu-kvm", 44, 40, 0, "/usr/libexec/qemu-kvm", false, false},
		{"exe denied", 50, 1, 0, "", true, true},
		{"dockerd", 50, 1, 0, "/usr/bin/dockerd", false, true},
		{"containerd", 50, 1, 0, "/usr/bin/containerd", false, true},
		{"containerd shim", 50, 1, 0, "/usr/bin/containerd-shim-runc-v2", false, true},
		{"docker-proxy", 50, 1, 0, "/usr/bin/docker-proxy", false, true},
		{"rootless docker: rootlesskit", 50, 1, 0, "/usr/bin/rootlesskit", false, true},
		{"rootless docker: exe child", 51, 50, 0, "/usr/bin/rootlesskit", false, true},
		{"rootless docker: slirp4netns", 52, 50, 0, "/usr/bin/slirp4netns", false, true},
		{"a container's own process", 50, 49, 0, "/usr/local/bin/agent-swarm", false, false},
		{"systemd user", 50, 1, 0, "/usr/lib/systemd/systemd", false, true},
		{"lib systemd", 50, 1, 0, "/lib/systemd/systemd-journald", false, true},
		{"libexec", 50, 1, 0, "/usr/libexec/gvfsd", false, true},
		{"libexec gsd", 50, 1, 0, "/usr/libexec/gsd-power", false, true},
		{"libexec deleted", 50, 1, 0, "/usr/libexec/xdg-desktop-portal (deleted)", false, true},
		{"libexec dir itself", 50, 1, 0, "/usr/libexec/", false, false},
		{"usr sbin", 50, 1, 0, "/usr/sbin/sshd", false, true},
		{"sbin", 50, 1, 0, "/sbin/agetty", false, true},
		{"dbus-daemon", 50, 1, 0, "/usr/bin/dbus-daemon", false, true},
		{"dbus-broker nix", 50, 1, 0, "/nix/store/abc-dbus/bin/dbus-broker", false, true},
		{"dbus-broker-launch", 50, 1, 0, "/usr/bin/dbus-broker-launch", false, true},
		{"deleted systemd", 50, 1, 0, "/usr/lib/systemd/systemd (deleted)", false, true},
		{"deleted dbus", 50, 1, 0, "/usr/bin/dbus-daemon (deleted)", false, true},
		{"bash", 50, 1, 0, "/usr/bin/bash", false, false},
		{"node", 50, 1, 0, "/home/u/.nvm/node", false, false},
		{"usr lib other", 50, 1, 0, "/usr/lib/firefox/firefox", false, false},
		{"prefix lookalike", 50, 1, 0, "/usr/sbinx/thing", false, false},
		{"systemd-ish name", 50, 1, 0, "/usr/bin/systemd-run", false, false},
		{"unknown exe", 50, 1, 0, "", false, false},
	}
	for _, c := range cases {
		if got := isSystemProc(c.pid, c.flags, c.exe, c.denied); got != c.wantSystem {
			t.Errorf("%s: isSystemProc = %v", c.name, got)
		}
	}
}

// Finding 39 at the scanner level: a child of pid 2 (PID namespace) has its
// exe read and is not System; a compiler under /usr/libexec/gcc is not System.
func TestScannerPIDNamespaceChildIsUserProc(t *testing.T) {
	root := fixtureRoot(t)
	stat := "107 (cc1plus) R 2 107 107 34816 107 4194560 1234 0 5 0 0 0 0 0 20 0 1 0 800 123456789 1000 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0 0 0 0 0 0 0 0\n"
	if err := os.WriteFile(filepath.Join(root, "107", "stat"), []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(root, "107", "exe")
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/libexec/gcc/x86_64-redhat-linux/13/cc1plus", exe); err != nil {
		t.Fatal(err)
	}
	got, err := fixtureScanner(root, FootprintAnon).list(1000)
	if err != nil {
		t.Fatal(err)
	}
	p := findProcIn(got, 107)
	if p == nil || p.PPID != 2 {
		t.Fatalf("pid 107 = %+v", p)
	}
	if p.System || p.Exe != "/usr/libexec/gcc/x86_64-redhat-linux/13/cc1plus" || p.Footprint == 0 {
		t.Errorf("child of pid 2 classified as OS process: %+v", *p)
	}
}

func TestParseCgroupFiles(t *testing.T) {
	for in, want := range map[string]string{
		"0::/user.slice/a.scope\n":                       "/user.slice/a.scope",
		"12:memory:/x\n1:name=systemd:/y\n0::/unified\n": "/unified",
		"0::/\n":                   "/",
		"0::/gone (deleted)\n":     "/gone",
		"12:memory:/x\n1:cpu:/y\n": "",
		"":                         "",
		"0::relative\n":            "",
	} {
		got, ok := parseSelfCgroup([]byte(in))
		if got != want || ok != (want != "") {
			t.Errorf("parseSelfCgroup(%q) = %q,%v", in, got, ok)
		}
	}
	mi := "22 1 0:21 / /proc rw - proc proc rw\n" +
		"30 23 0:26 / /sys/fs/cgroup/unified rw,nosuid shared:9 master:1 - cgroup2 cgroup2 rw\n" +
		"31 23 0:27 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory\n"
	mp, root, ok := parseMountinfoCgroup2([]byte(mi))
	if !ok || mp != "/sys/fs/cgroup/unified" || root != "/" {
		t.Errorf("mountinfo: %q %q %v", mp, root, ok)
	}
	mp, root, ok = parseMountinfoCgroup2([]byte("40 1 0:30 /ns\\040root /my\\040cg rw - cgroup2 none rw\n"))
	if !ok || mp != "/my cg" || root != "/ns root" {
		t.Errorf("escaped mountinfo: %q %q %v", mp, root, ok)
	}
	if _, _, ok := parseMountinfoCgroup2([]byte("22 1 0:21 / /proc rw - proc proc rw\nshort line\n")); ok {
		t.Errorf("no cgroup2: ok")
	}
	if got := unescapeMount([]byte(`a\134b\04`)); got != `a\b\04` {
		t.Errorf("unescape = %q", got)
	}

	dirs := []struct {
		mp, root, path, want string
		ok                   bool
	}{
		{"/sys/fs/cgroup", "/", "/user.slice/x", "/sys/fs/cgroup/user.slice/x", true},
		{"/sys/fs/cgroup", "/", "/", "/sys/fs/cgroup", true},
		{"/sys/fs/cgroup/", "/", "/a/", "/sys/fs/cgroup/a", true},
		{"/cg", "/docker/abc", "/docker/abc/sub", "/cg/sub", true},
		{"/cg", "/docker/abc", "/docker/abc", "/cg", true},
		{"/cg", "/docker/abc", "/docker/abcd", "", false},
		{"/cg", "/docker/abc", "/other", "", false},
		{"/cg", "/", "/a/../../etc", "", false},
	}
	for _, d := range dirs {
		got, ok := cgroupDir(d.mp, d.root, d.path)
		if got != d.want || ok != d.ok {
			t.Errorf("cgroupDir(%q,%q,%q) = %q,%v want %q,%v", d.mp, d.root, d.path, got, ok, d.want, d.ok)
		}
	}

	for in, want := range map[string]int64{"max\n": -1, "2147483648\n": 2147483648, "0": 0} {
		got, ok := parseCgroupMax([]byte(in))
		if !ok || got != want {
			t.Errorf("parseCgroupMax(%q) = %d,%v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "-1", "12k", "maxx"} {
		if _, ok := parseCgroupMax([]byte(bad)); ok {
			t.Errorf("parseCgroupMax(%q): want !ok", bad)
		}
	}
	stat := "anon 100\nfile 200\nactive_file 50\ninactive_file 150\ninactive_filex 9\n"
	if v, ok := parseCgroupStatKey([]byte(stat), "inactive_file"); !ok || v != 150 {
		t.Errorf("inactive_file = %d,%v", v, ok)
	}
	if v, ok := parseCgroupStatKey([]byte(stat), "file"); !ok || v != 200 {
		t.Errorf("file = %d,%v", v, ok)
	}
	if _, ok := parseCgroupStatKey([]byte(stat), "shmem"); ok {
		t.Errorf("shmem found")
	}
	if _, ok := parseCgroupStatKey([]byte("oom_kill x\n"), "oom_kill"); ok {
		t.Errorf("bad value accepted")
	}
}

func TestLinuxOptionsFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	o, err := LinuxOptionsFromEnv(env(nil))
	if err != nil || o != (LinuxOptions{Footprint: FootprintAnon, CgroupMemLimit: true, ThrottleNice: 10, ThrottleCPUPct: 25, Load: LoadAuto}) {
		t.Errorf("defaults: %+v %v", o, err)
	}
	o, err = LinuxOptionsFromEnv(env(map[string]string{
		"PACE_LINUX_FOOTPRINT": "pss", "PACE_LINUX_CGROUP_LIMIT": "0", "PACE_LINUX_CGROUP": "off", "PACE_CGROUP_BASE": "/sys/fs/cgroup/x/",
		"PACE_LINUX_CGROUP_HIGH_PCT": "90", "PACE_LINUX_THROTTLE_NICE": "0", "PACE_LINUX_THROTTLE_CPU_PCT": "0", "PACE_LINUX_LOAD": "loadavg",
		"PACE_LINUX_BTIME_FILE": "/var/x/btime", "PACE_HOME": "/h",
	}))
	if err != nil || o != (LinuxOptions{Footprint: FootprintPSS, CgroupBase: "/sys/fs/cgroup/x", NoContainers: true, CgroupHighPct: 90,
		Load: LoadAvg, BtimeFile: "/var/x/btime"}) {
		t.Errorf("set: %+v %v", o, err)
	}
	// btime anchor file defaults: PACE_HOME, then the runtime dir, then state.
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"PACE_HOME": "/h", "XDG_RUNTIME_DIR": "/run/user/1", "HOME": "/home/u"}, "/h/run/btime"},
		{map[string]string{"PACE_HOME": "rel", "XDG_RUNTIME_DIR": "/run/user/1"}, "/run/user/1/pace/btime"},
		{map[string]string{"XDG_STATE_HOME": "/st", "HOME": "/home/u"}, "/st/pace/run/btime"},
		{map[string]string{"HOME": "/home/u"}, "/home/u/.local/state/pace/run/btime"},
		{map[string]string{"PACE_LINUX_BTIME_FILE": "off", "PACE_HOME": "/h"}, ""},
		{nil, ""},
	} {
		if o, err := LinuxOptionsFromEnv(env(c.env)); err != nil || o.BtimeFile != c.want {
			t.Errorf("%v: BtimeFile = %q,%v want %q", c.env, o.BtimeFile, err, c.want)
		}
	}
	for _, bad := range []map[string]string{
		{"PACE_LINUX_FOOTPRINT": "PSS"},
		{"PACE_LINUX_CGROUP_LIMIT": "yes"},
		{"PACE_LINUX_CGROUP": "2"},
		{"PACE_CGROUP_BASE": "rel/path"},
		{"PACE_LINUX_THROTTLE_CPU_PCT": "-1"},
		{"PACE_LINUX_THROTTLE_CPU_PCT": "lots"},
		{"PACE_LINUX_CGROUP_HIGH_PCT": "101"},
		{"PACE_LINUX_CGROUP_HIGH_PCT": "x"},
		{"PACE_LINUX_THROTTLE_NICE": "20"},
		{"PACE_LINUX_THROTTLE_NICE": "-1"},
		{"PACE_LINUX_LOAD": "psi"},
		{"PACE_LINUX_BTIME_FILE": "rel/btime"},
	} {
		if _, err := LinuxOptionsFromEnv(env(bad)); err == nil {
			t.Errorf("%v: want error", bad)
		}
	}
	for _, m := range FootprintMethods {
		if got, err := ParseFootprintMethod(string(m)); err != nil || got != m {
			t.Errorf("ParseFootprintMethod(%q) = %q,%v", m, got, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Scanner against the fixture tree.

// fixtureRoot copies testdata/linux/proc into a temp dir, turning
// "exe.target" files into exe symlinks and "environ.txt" into NUL-separated
// environ files (symlinks and NULs are kept out of the repository).
func fixtureRoot(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "linux", "proc")
	dst := filepath.Join(t.TempDir(), "proc")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		switch d.Name() {
		case "exe.target":
			return os.Symlink(strings.TrimSpace(string(b)), filepath.Join(filepath.Dir(out), "exe"))
		case "environ.txt":
			return os.WriteFile(filepath.Join(filepath.Dir(out), "environ"), []byte(strings.ReplaceAll(string(b), "\n", "\x00")), 0o644)
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// fixtureOwners is who "owns" each fixture pid dir.
var fixtureOwners = map[int]int{1: 0, 2: 0, 105: 2000}

func fixtureScanner(root string, m FootprintMethod) *procScanner {
	s := newProcScanner(root, m)
	s.pageSize = 4096
	s.ownerOf = func(pid int, _ *unix.Stat_t) int {
		if u, ok := fixtureOwners[pid]; ok {
			return u
		}
		return 1000
	}
	return s
}

const fixtureBootMS = 1700000000 * 1000

func TestScannerFixture(t *testing.T) {
	root := fixtureRoot(t)
	const kB = 1024
	fp := map[FootprintMethod]map[int]int64{
		FootprintAnon:    {100: (6144 + 1024 + 2048) * kB, 103: 4000 * kB, 104: 0, 106: 1000 * kB, 107: (4000 + 512) * kB, 108: 1024 * kB},
		FootprintRSS:     {100: 2560 * 4096, 103: 2000 * 4096, 104: 0, 106: 500 * 4096, 107: 1000 * 4096, 108: 256 * 4096},
		FootprintRSSSwap: {100: 2560*4096 + 2048*kB, 103: 2000 * 4096, 104: 0, 106: 500 * 4096, 107: 1000*4096 + 512*kB, 108: 256 * 4096},
		FootprintPSS:     {100: (8192 + 1536) * kB, 103: 4000 * kB, 104: 0, 106: 1000 * kB, 107: (3000 + 256) * kB, 108: 1024 * kB},
		FootprintPSSAnon: {100: (5120 + 512 + 1536) * kB, 103: 4000 * kB, 104: 0, 106: 1000 * kB, 107: (3000 + 256) * kB, 108: 1024 * kB},
	}
	base := []Proc{
		{PID: 100, PPID: 1, PGID: 100, UID: 1000, StartMS: fixtureBootMS + 123450, Comm: "we) (ird", Exe: "/usr/bin/python3.11", CPUMS: 3000, State: 'S', TermLeader: true},
		{PID: 103, PPID: 1, PGID: 103, UID: 1000, StartMS: fixtureBootMS + 5000, Comm: "systemd", Exe: "/usr/lib/systemd/systemd", State: 'S', System: true},
		{PID: 104, PPID: 100, PGID: 100, UID: 1000, StartMS: fixtureBootMS + 200000, Comm: "defunct thing", CPUMS: 100, State: 'Z'},
		{PID: 106, PPID: 103, PGID: 106, UID: 1000, StartMS: fixtureBootMS + 7000, Comm: "dbus-broker", Exe: "/usr/bin/dbus-broker", State: 'S', System: true},
		{PID: 107, PPID: 1, PGID: 107, UID: 1000, StartMS: fixtureBootMS + 8000, Comm: "old", Exe: "/opt/old/bin/old", State: 'S'},
		{PID: 108, PPID: 100, PGID: 100, UID: 1000, StartMS: fixtureBootMS + 9000, Comm: "app", Exe: "/home/u/bin/app (deleted)", State: 'T'},
	}
	for _, m := range FootprintMethods {
		t.Run(string(m), func(t *testing.T) {
			s := fixtureScanner(root, m)
			got, err := s.list(1000)
			if err != nil {
				t.Fatal(err)
			}
			want := make([]Proc, len(base))
			copy(want, base)
			for i := range want {
				want[i].Footprint = fp[m][want[i].PID]
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("list mismatch\n got  %+v\n want %+v", got, want)
			}
			// Second scan (cached path) is identical: determinism.
			again, err := s.list(1000)
			if err != nil || !reflect.DeepEqual(again, got) {
				t.Errorf("second scan differs: %v\n%+v", err, again)
			}
		})
	}
}

func TestScannerUIDFilterAndKernelThreads(t *testing.T) {
	root := fixtureRoot(t)
	s := fixtureScanner(root, FootprintAnon)
	got, err := s.list(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].PID != 1 || got[1].PID != 2 {
		t.Fatalf("root procs = %+v", got)
	}
	if !got[0].System || !got[1].System {
		t.Errorf("init/kthreadd not system: %+v", got)
	}
	if got[1].Footprint != 0 || got[1].Exe != "" {
		t.Errorf("kthread footprint/exe: %+v", got[1])
	}
	if got[0].Footprint != 6000*1024 || got[0].CPUMS != 3000 {
		t.Errorf("init: %+v", got[0])
	}
	other, err := s.list(2000)
	if err != nil || len(other) != 1 || other[0].PID != 105 || other[0].State != 'R' {
		t.Errorf("uid 2000: %+v %v", other, err)
	}
	none, err := s.list(4242)
	if err != nil || len(none) != 0 {
		t.Errorf("uid 4242: %+v %v", none, err)
	}
	if _, err := s.list(-1); err == nil {
		t.Errorf("negative uid accepted")
	}
}

func TestScannerErrors(t *testing.T) {
	s := fixtureScanner(filepath.Join(t.TempDir(), "nope"), FootprintAnon)
	if _, err := s.list(1000); err == nil {
		t.Errorf("missing root: want error")
	}
	// Root without /stat (no btime).
	root := t.TempDir()
	if _, err := fixtureScanner(root, FootprintAnon).list(1000); err == nil {
		t.Errorf("no btime: want error")
	}
	// Empty but valid root.
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fixtureScanner(root, FootprintAnon).list(1000)
	if err != nil || len(got) != 0 {
		t.Errorf("empty root: %+v %v", got, err)
	}
}

func TestScannerPermissionErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file modes")
	}
	root := fixtureRoot(t)
	// Unreadable stat: process skipped. Unreadable status: footprint falls
	// back to stat rss. Unreadable smaps_rollup: falls back to status.
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Chmod(filepath.Join(root, "103", "stat"), 0))
	must(os.Chmod(filepath.Join(root, "100", "status"), 0))
	must(os.Chmod(filepath.Join(root, "100", "smaps_rollup"), 0))
	must(os.Chmod(filepath.Join(root, "107", "smaps_rollup"), 0))
	t.Cleanup(func() {
		for _, p := range []string{"103/stat", "100/status", "100/smaps_rollup", "107/smaps_rollup"} {
			_ = os.Chmod(filepath.Join(root, p), 0o644)
		}
	})
	got, err := fixtureScanner(root, FootprintAnon).list(1000)
	if err != nil {
		t.Fatal(err)
	}
	pids := []int{}
	for _, p := range got {
		pids = append(pids, p.PID)
		if p.PID == 100 && p.Footprint != 2560*4096 {
			t.Errorf("100 footprint with unreadable status = %d, want rss fallback", p.Footprint)
		}
	}
	if !reflect.DeepEqual(pids, []int{100, 104, 106, 107, 108}) {
		t.Errorf("pids = %v", pids)
	}
	got, err = fixtureScanner(root, FootprintPSS).list(1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		switch p.PID {
		case 100: // no smaps_rollup, no status -> rss
			if p.Footprint != 2560*4096 {
				t.Errorf("pss 100 = %d", p.Footprint)
			}
		case 107: // no smaps_rollup -> anon from status
			if p.Footprint != 4512*1024 {
				t.Errorf("pss 107 = %d", p.Footprint)
			}
		}
	}
}

func TestScannerCacheAndBtime(t *testing.T) {
	root := fixtureRoot(t)
	s := fixtureScanner(root, FootprintAnon)
	if _, err := s.list(1000); err != nil {
		t.Fatal(err)
	}
	// btime is read once: a wall-clock step must not move StartMS.
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("btime 1800000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same (pid, start, comm): exe is cached even if the link changes.
	exe := filepath.Join(root, "107", "exe")
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/sbin/changed", exe); err != nil {
		t.Fatal(err)
	}
	got, _ := s.list(1000)
	p107 := findProcIn(got, 107)
	if p107 == nil || p107.Exe != "/opt/old/bin/old" || p107.System || p107.StartMS != fixtureBootMS+8000 {
		t.Fatalf("cached 107 = %+v", p107)
	}
	// exec changes comm: exe is re-read and the process reclassified.
	st := filepath.Join(root, "107", "stat")
	b, _ := os.ReadFile(st)
	if err := os.WriteFile(st, []byte(strings.Replace(string(b), "(old)", "(changed)", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ = s.list(1000)
	p107 = findProcIn(got, 107)
	if p107 == nil || p107.Exe != "/usr/sbin/changed" || !p107.System || p107.Comm != "changed" {
		t.Fatalf("re-exec 107 = %+v", p107)
	}
	// Vanished pid: cache entry pruned; zombie (no exe) not cached.
	if err := os.RemoveAll(filepath.Join(root, "108")); err != nil {
		t.Fatal(err)
	}
	got, _ = s.list(1000)
	if findProcIn(got, 108) != nil {
		t.Errorf("108 still listed")
	}
	if _, ok := s.cache[108]; ok {
		t.Errorf("108 still cached")
	}
	if _, ok := s.cache[104]; ok {
		t.Errorf("zombie with unreadable exe cached")
	}
	// pid reuse: new start time -> re-read exe.
	st = filepath.Join(root, "106", "stat")
	b, _ = os.ReadFile(st)
	if err := os.WriteFile(st, []byte(strings.Replace(string(b), " 700 ", " 701 ", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	exe = filepath.Join(root, "106", "exe")
	_ = os.Remove(exe)
	_ = os.Symlink("/home/u/bin/reused", exe)
	got, _ = s.list(1000)
	if p := findProcIn(got, 106); p == nil || p.Exe != "/home/u/bin/reused" || p.System || p.StartMS != fixtureBootMS+7010 {
		t.Errorf("reused 106 = %+v", p)
	}
}

func findProcIn(ps []Proc, pid int) *Proc {
	for i := range ps {
		if ps[i].PID == pid {
			return &ps[i]
		}
	}
	return nil
}

func TestScannerJobTag(t *testing.T) {
	root := fixtureRoot(t)
	s := fixtureScanner(root, FootprintAnon)
	if got := s.jobTag(100); got != "job-42" {
		t.Errorf("jobTag(100) = %q", got)
	}
	for _, pid := range []int{103, 999, 0, -1} {
		if got := s.jobTag(pid); got != "" {
			t.Errorf("jobTag(%d) = %q", pid, got)
		}
	}
}

func TestReadFdGrowthAndLimit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big")
	data := []byte(strings.Repeat("0123456789", 3000)) // 30 kB > initial 4 kB
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	got, err := readPath(p, &buf, maxFileBuf)
	if err != nil || string(got) != string(data) {
		t.Fatalf("read %d bytes, err %v", len(got), err)
	}
	got, err = readPath(p, &buf, 8192)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 30000 || string(got) != string(data[:len(got)]) {
		t.Errorf("limited read: %d bytes", len(got))
	}
	small := make([]byte, 0, 16)
	if got, err := readPath(p, &small, 100); err != nil || len(got) != 100 {
		t.Errorf("limit 100: %d %v", len(got), err)
	}
	if _, err := readPath(filepath.Join(t.TempDir(), "missing"), &buf, 100); !isVanished(err) {
		t.Errorf("missing: %v", err)
	}
	if errnoOf(unix.ENOENT) != unix.ENOENT || errnoOf(nil) != 0 {
		t.Errorf("errnoOf")
	}
}

// The parsers must not allocate on the hot path (called for every process
// every tick).
func TestParsersDoNotAllocate(t *testing.T) {
	stat := []byte(statLine("100", "we) (ird", statTail))
	status := []byte("Name:\tx\nUid:\t1000\t0\t0\t0\nVmRSS:\t   100 kB\nRssAnon:\t  60 kB\nRssFile:\t 30 kB\nRssShmem:\t 10 kB\nVmSwap:\t 5 kB\n")
	psi := []byte("some avg10=12.34 avg60=5.00 avg300=1.00 total=1\nfull avg10=6.78 avg60=2.00 avg300=0.50 total=2\n")
	n := testing.AllocsPerRun(100, func() {
		_, _ = parseStat(stat)
		_ = parseStatus(status)
		_, _, _ = parseStatm(stat[:13])
	})
	if n != 0 {
		t.Errorf("stat/status parse allocates %.1f times", n)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = parsePSI(psi) }); n > 1 {
		t.Errorf("parsePSI allocates %.1f times", n)
	}
}
