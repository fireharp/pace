package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// blockFixture builds a /sys/block-like tree: device -> scheduler file.
func blockFixture(t *testing.T, devs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for dev, sched := range devs {
		d := filepath.Join(root, dev, "queue")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "scheduler"), []byte(sched), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// Finding 42 (reporting): whether the idle I/O class can act is detected
// from the disks' active schedulers (only BFQ honours it).
func TestIOIdleHonoured(t *testing.T) {
	for _, c := range []struct {
		name            string
		devs            map[string]string
		honoured, known bool
	}{
		{"nvme none", map[string]string{"nvme0n1": "[none] mq-deadline kyber bfq\n"}, false, true},
		{"one bfq disk", map[string]string{"nvme0n1": "[none] bfq\n", "sda": "mq-deadline [bfq] none\n"}, true, true},
		{"single scheduler bare", map[string]string{"vda": "bfq\n"}, true, true},
		{"virtual devices only", map[string]string{"loop0": "[bfq]\n", "dm-0": "none\n", "zram0": "none\n"}, false, false},
		{"empty", map[string]string{}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, k := ioIdleHonoured(blockFixture(t, c.devs))
			if h != c.honoured || k != c.known {
				t.Errorf("honoured %v known %v, want %v %v", h, k, c.honoured, c.known)
			}
		})
	}
	if h, k := ioIdleHonoured(filepath.Join(t.TempDir(), "missing")); h || k {
		t.Errorf("missing root: %v %v", h, k)
	}
}

// F5 (stress s10): a container CPU quota on the cgroup-namespace root is
// read from cpu.max, and it binds only with fewer CPUs of runtime than
// CPUs to run on.
func TestParseCPUMaxAndBinds(t *testing.T) {
	for _, c := range []struct {
		in            string
		quota, period int64
		ok            bool
	}{
		{"200000 100000\n", 200000, 100000, true},
		{"max 100000\n", -1, 100000, true},
		{"50000", 50000, 100000, true},
		{"max", -1, 100000, true},
		{"", 0, 0, false},
		{"0 100000", 0, 0, false},
		{"200000 0", 0, 0, false},
		{"x 100000", 0, 0, false},
		{"1 2 3", 0, 0, false},
	} {
		q, p, ok := parseCPUMax([]byte(c.in))
		if q != c.quota || p != c.period || ok != c.ok {
			t.Errorf("parseCPUMax(%q) = %d %d %v, want %d %d %v", c.in, q, p, ok, c.quota, c.period, c.ok)
		}
	}
	for _, c := range []struct {
		q    cpuQuota
		want bool
	}{
		{cpuQuota{200000, 100000, 8}, true},  // docker --cpus 2 on 8 CPUs (the s10 host)
		{cpuQuota{150000, 100000, 2}, true},  // 1.5 of 2
		{cpuQuota{200000, 100000, 2}, false}, // as many as the mask: never binds
		{cpuQuota{800000, 100000, 4}, false},
		{cpuQuota{50000, 100000, 1}, false}, // one CPU: one runqueue, nice holds
		{cpuQuota{-1, 100000, 8}, false},    // max
		{cpuQuota{}, false},
	} {
		if got := c.q.binds(); got != c.want {
			t.Errorf("%+v.binds() = %v, want %v", c.q, got, c.want)
		}
	}
	if s := (cpuQuota{150000, 100000, 2}).cpus(); s != "1.5" {
		t.Errorf("cpus %q", s)
	}
}

// F5: under a binding quota the status note explains why the throttle is
// limited and the THROTTLED caveat says so, except for a job its own job
// container's cpu.max holds; without one neither says anything.
func TestQuotaThrottleText(t *testing.T) {
	q := cpuQuota{200000, 100000, 8}
	n := quotaThrottleNote(q, false)
	for _, w := range []string{"cpu.max 2 of 8 CPUs", "is limited", "only threads that share a CPU", "may not slow a job"} {
		if !strings.Contains(n, w) {
			t.Errorf("note %q lacks %q", n, w)
		}
	}
	if strings.Contains(n, "pin") {
		t.Errorf("note %q claims pinning", n)
	}
	if n := quotaThrottleNote(q, true); !strings.Contains(n, "held by that container's cpu.max") {
		t.Errorf("cpu.max containers note %q", n)
	}
	if c := quotaThrottleCaveat(q, false); c != "is limited by the container's CPU quota (nice acts per CPU only)" {
		t.Errorf("caveat %q", c)
	}
	if c := quotaThrottleCaveat(q, true); c != "" {
		t.Errorf("caveat for a job held by its container's cpu.max %q", c)
	}
	for _, none := range []cpuQuota{{}, {-1, 100000, 8}, {800000, 100000, 8}, {50000, 100000, 1}} {
		if n, c := quotaThrottleNote(none, false), quotaThrottleCaveat(none, false); n != "" || c != "" {
			t.Errorf("%+v: note %q caveat %q, want none", none, n, c)
		}
	}
}
