//go:build darwin

package platform

import (
	"os"
	"testing"
)

// BenchmarkListProcs is the steady-state sampler tick over every process of
// the user (classification cache warm).
func BenchmarkListProcs(b *testing.B) {
	be := newTestBackend(b)
	uid := os.Getuid()
	ps, err := be.ListProcs(uid)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ps, err = be.ListProcs(uid)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(ps)), "procs")
}

// BenchmarkListProcsCold is the first tick (every process classified:
// proc_pidpath, csops, kill(0), KERN_PROCARGS2).
func BenchmarkListProcsCold(b *testing.B) {
	be := newTestBackend(b)
	uid := os.Getuid()
	var n int
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		be.cache = make(map[int]*procMeta, 1024)
		ps, err := be.ListProcs(uid)
		if err != nil {
			b.Fatal(err)
		}
		n = len(ps)
	}
	b.ReportMetric(float64(n), "procs")
}

func BenchmarkSampleMachine(b *testing.B) {
	be := newTestBackend(b)
	paths := []string{"/", os.TempDir()}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := be.SampleMachine(paths); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSampleMachineNoDisk(b *testing.B) {
	be := newTestBackend(b)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := be.SampleMachine(nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJobTag(b *testing.B) {
	be := newTestBackend(b)
	pid := os.Getpid()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = be.JobTag(pid)
	}
}

func BenchmarkQuickPressure(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = QuickPressure()
	}
}
