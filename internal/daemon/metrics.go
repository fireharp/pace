package daemon

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// metrics holds the counters behind GET /metrics. Gauges come from the
// published snapshot.
type metrics struct {
	mu       sync.Mutex
	events   map[string]int64 // by verb
	kills    map[string]int64 // by policy
	sampleNS atomic.Int64     // duration of the last sample
}

func newMetrics() *metrics {
	return &metrics{events: map[string]int64{}, kills: map[string]int64{}}
}

// maxLabelValues bounds label cardinality (verbs and policies are a fixed
// vocabulary; anything beyond this is folded into "other").
const maxLabelValues = 64

func bump(m map[string]int64, k string) {
	if _, ok := m[k]; !ok && len(m) >= maxLabelValues {
		k = "other"
	}
	m[k]++
}

func (m *metrics) countEvent(e *proto.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	bump(m.events, e.Verb)
	if e.Verb == proto.VerbKilled {
		p := e.Policy
		if p == "" {
			p = "unknown"
		}
		bump(m.kills, p)
	}
}

func sortedKeys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// labelValue escapes a Prometheus label value.
func labelValue(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// writeMetrics renders the Prometheus text exposition format.
func (d *daemon) writeMetrics(w io.Writer) error {
	bw := bufio.NewWriter(w)
	s := d.snap.Load()
	st := s.status
	gauge := func(name, help string, v int64) {
		fmt.Fprintf(bw, "# HELP %s %s\n# TYPE %s gauge\n%s %d\n", name, help, name, name, v)
	}
	gauge("pace_capacity_bytes", "Memory capacity the governor plans against.", st.CapacityBytes)
	gauge("pace_avail_bytes", "Memory available now.", st.AvailBytes)
	gauge("pace_headroom_bytes", "Memory a new job may use now.", st.HeadroomBytes)
	gauge("pace_reserved_bytes", "Unconsumed reservations of running jobs.", st.ReservedBytes)
	gauge("pace_pressure_level", "Memory pressure: 0 normal, 1 warn, 2 critical.", int64(s.level))
	adm := int64(0)
	if st.Admitting {
		adm = 1
	}
	gauge("pace_admitting", "1 when new jobs are admitted.", adm)

	fmt.Fprintf(bw, "# HELP pace_jobs Governed jobs by state.\n# TYPE pace_jobs gauge\n")
	fmt.Fprintf(bw, "pace_jobs{state=\"queued\"} %d\npace_jobs{state=\"running\"} %d\npace_jobs{state=\"done\"} %d\n", s.queued, s.running, s.done)

	d.metrics.mu.Lock()
	fmt.Fprintf(bw, "# HELP pace_events_total Governor events by verb.\n# TYPE pace_events_total counter\n")
	for _, k := range sortedKeys(d.metrics.events) {
		fmt.Fprintf(bw, "pace_events_total{verb=\"%s\"} %d\n", labelValue(k), d.metrics.events[k])
	}
	fmt.Fprintf(bw, "# HELP pace_kills_total Policy kills by policy.\n# TYPE pace_kills_total counter\n")
	for _, k := range sortedKeys(d.metrics.kills) {
		fmt.Fprintf(bw, "pace_kills_total{policy=\"%s\"} %d\n", labelValue(k), d.metrics.kills[k])
	}
	d.metrics.mu.Unlock()

	sec := float64(d.metrics.sampleNS.Load()) / float64(time.Second)
	fmt.Fprintf(bw, "# HELP pace_sample_seconds Duration of the last sample.\n# TYPE pace_sample_seconds gauge\npace_sample_seconds %s\n", strconv.FormatFloat(sec, 'g', 6, 64))
	fmt.Fprintf(bw, "# HELP pace_faults_total Daemon faults recorded.\n# TYPE pace_faults_total counter\npace_faults_total %d\n", d.faults.total.Load())
	fmt.Fprintf(bw, "# HELP pace_followers Connected event followers.\n# TYPE pace_followers gauge\npace_followers %d\n", d.feed.followers())
	return bw.Flush()
}

// faultSet keeps the latest occurrence of each fault kind.
type faultSet struct {
	mu    sync.Mutex
	m     map[string]*fault
	total atomic.Int64
}

type fault struct {
	kind, msg       string
	count           int64
	firstMS, lastMS int64
}

const maxFaultKinds = 64

func newFaultSet() *faultSet { return &faultSet{m: map[string]*fault{}} }

func (f *faultSet) record(kind, msg string, nowMS int64) {
	f.total.Add(1)
	if len(msg) > 512 {
		msg = msg[:512] + "..."
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	x := f.m[kind]
	if x == nil {
		if len(f.m) >= maxFaultKinds {
			kind = "other"
			x = f.m[kind]
		}
		if x == nil {
			x = &fault{kind: kind, firstMS: nowMS}
			f.m[kind] = x
		}
	}
	x.msg, x.lastMS = msg, nowMS
	x.count++
}

// active lists faults seen in the last faultWindowMS, sorted by kind.
func (f *faultSet) active(nowMS int64) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, k := range sortedFaultKinds(f.m) {
		x := f.m[k]
		if nowMS-x.lastMS > faultWindowMS {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s (count=%d, last=%s ago)", x.kind, x.msg, x.count, proto.QuoteValue(time.Duration((nowMS-x.lastMS)*int64(time.Millisecond)).Round(time.Second).String())))
	}
	return out
}

func (f *faultSet) count(kind string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if x := f.m[kind]; x != nil {
		return x.count
	}
	return 0
}

func sortedFaultKinds(m map[string]*fault) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
