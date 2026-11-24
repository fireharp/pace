package policy

import (
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

// Learning defaults, used when the corresponding Params field is <= 0.
const (
	DefaultHistorySize       = 20
	DefaultHistoryMinSamples = 3
	// DefaultHistoryMaxKeys caps the per-command history entries (each is
	// at most HistorySize peaks; the whole State is cloned, hashed and
	// logged every step, so it must stay small).
	DefaultHistoryMaxKeys = 256
	// MaxKeyLen caps a command key in bytes (longer keys are cut at a rune
	// boundary): `bash -c '<script>'` would otherwise key on the script.
	MaxKeyLen = 128
)

// History key prefixes.
const (
	histClassPrefix = "class:"
	histKeyPrefix   = "key:"
)

func historySize(p *Params) int {
	if p.HistorySize > 0 {
		return p.HistorySize
	}
	return DefaultHistorySize
}

func historyMaxKeys(p *Params) int {
	if p.HistoryMaxKeys > 0 {
		return p.HistoryMaxKeys
	}
	return DefaultHistoryMaxKeys
}

func historyMinSamples(p *Params) int {
	if p.HistoryMinSamples > 0 {
		return p.HistoryMinSamples
	}
	return DefaultHistoryMinSamples
}

// DeriveKey returns the command signature used for learning when the
// submitter gave none: the basename of argv[0] plus the first argument that
// does not start with '-' ("go test ./..." -> "go test", "make -j8" -> "make"),
// at most MaxKeyLen bytes.
func DeriveKey(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	a0 := strings.TrimSpace(argv[0])
	if a0 == "" {
		return ""
	}
	base := path.Base(a0)
	for _, a := range argv[1:] {
		if a == "" || strings.HasPrefix(a, "-") {
			continue
		}
		return capKey(base + " " + a)
	}
	return capKey(base)
}

// capKey cuts k to at most MaxKeyLen bytes at a rune boundary.
func capKey(k string) string {
	if len(k) <= MaxKeyLen {
		return k
	}
	n := MaxKeyLen
	for n > 0 && !utf8.RuneStart(k[n]) {
		n--
	}
	return k[:n]
}

// nearestRank returns the permille-th percentile of vals (nearest-rank
// method, integer only). vals is not modified. Empty input returns 0.
func nearestRank(vals []int64, permille int64) int64 {
	n := int64(len(vals))
	if n == 0 {
		return 0
	}
	c := append([]int64(nil), vals...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	idx := (n*permille+999)/1000 - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return c[idx]
}

// withMargin returns ceil(v * (1000+margin) / 1000).
func withMargin(v, marginPermille int64) int64 {
	if v <= 0 {
		return 0
	}
	if marginPermille < 0 {
		marginPermille = 0
	}
	return (v*(1000+marginPermille) + 999) / 1000
}

// findHistory returns the index of key in s.History (sorted by Key) or -1.
func findHistory(s *State, key string) int {
	i := sort.Search(len(s.History), func(i int) bool { return s.History[i].Key >= key })
	if i < len(s.History) && s.History[i].Key == key {
		return i
	}
	return -1
}

// recordHistory appends one observation, made at nowMS, to the ring for
// key. When a new key entry would exceed HistoryMaxKeys, the least recently
// recorded key entries are evicted first (class entries never are).
func recordHistory(p *Params, s *State, key string, peak, durMS, nowMS int64) {
	if key == "" {
		return
	}
	i := sort.Search(len(s.History), func(i int) bool { return s.History[i].Key >= key })
	if i == len(s.History) || s.History[i].Key != key {
		s.History = append(s.History, History{})
		copy(s.History[i+1:], s.History[i:])
		s.History[i] = History{Key: key}
		if strings.HasPrefix(key, histKeyPrefix) {
			evictKeys(p, s, key)
			i = findHistory(s, key)
		}
	}
	h := &s.History[i]
	h.LastMS = nowMS
	// Keep DurMS parallel to Peaks even if an older state had them out of step.
	for len(h.DurMS) < len(h.Peaks) {
		h.DurMS = append(h.DurMS, 0)
	}
	h.DurMS = h.DurMS[:len(h.Peaks)]
	h.Peaks = append(h.Peaks, peak)
	h.DurMS = append(h.DurMS, durMS)
	if n := historySize(p); len(h.Peaks) > n {
		h.Peaks = append([]int64(nil), h.Peaks[len(h.Peaks)-n:]...)
		h.DurMS = append([]int64(nil), h.DurMS[len(h.DurMS)-n:]...)
	}
}

// evictKeys drops the least recently recorded "key:" entries (oldest
// LastMS, ties in Key order) until at most HistoryMaxKeys remain; keep is
// never dropped.
func evictKeys(p *Params, s *State, keep string) {
	limit := historyMaxKeys(p)
	n := 0
	for i := range s.History {
		if strings.HasPrefix(s.History[i].Key, histKeyPrefix) {
			n++
		}
	}
	for ; n > limit; n-- {
		victim := -1
		for i := range s.History {
			h := &s.History[i]
			if !strings.HasPrefix(h.Key, histKeyPrefix) || h.Key == keep {
				continue
			}
			if victim < 0 || h.LastMS < s.History[victim].LastMS {
				victim = i
			}
		}
		if victim < 0 {
			return
		}
		s.History = append(s.History[:victim], s.History[victim+1:]...)
	}
}

// Percentiles (permille) of the recorded peaks used as a learned estimate.
// A command key repeats one command, so its p90 is a safe upper estimate. A
// class mixes different commands: its p90 would be set by the class's
// heaviest command, so the class fallback uses the median.
const (
	keyPeakPermille   = 900
	classPeakPermille = 500
)

// learnedPeak returns the learned estimate for a history key (p90 of the
// peaks for a command key, the median for a class) with the margin
// applied, if at least HistoryMinSamples peaks are recorded.
func learnedPeak(p *Params, s *State, key string) (int64, bool) {
	raw, ok := learnedRawPeak(p, s, key)
	if !ok {
		return 0, false
	}
	v := withMargin(raw, p.MarginPermille)
	return v, v > 0
}

// learnedRawPeak is learnedPeak without the margin: a peak that was
// actually observed on this machine.
func learnedRawPeak(p *Params, s *State, key string) (int64, bool) {
	i := findHistory(s, key)
	if i < 0 || len(s.History[i].Peaks) < historyMinSamples(p) {
		return 0, false
	}
	pm := int64(keyPeakPermille)
	if strings.HasPrefix(key, histClassPrefix) {
		pm = classPeakPermille
	}
	v := nearestRank(s.History[i].Peaks, pm)
	return v, v > 0
}

// learnedDuration returns p50 of recorded durations for key, if trusted.
func learnedDuration(p *Params, s *State, key string) (int64, bool) {
	i := findHistory(s, key)
	if i < 0 {
		return 0, false
	}
	var d []int64
	for _, v := range s.History[i].DurMS {
		if v > 0 {
			d = append(d, v)
		}
	}
	if len(d) < historyMinSamples(p) {
		return 0, false
	}
	return nearestRank(d, 500), true
}

// classFor resolves a class name: the named class, else DefaultClass, else
// a zero class named after DefaultClass ("default" if unset).
func classFor(p *Params, name string) ClassParams {
	for _, c := range p.Classes {
		if c.Name == name && name != "" {
			return c
		}
	}
	def := p.DefaultClass
	if def == "" {
		def = "default"
	}
	for _, c := range p.Classes {
		if c.Name == def {
			return c
		}
	}
	return ClassParams{Name: def, Priority: defaultPriority}
}

// ClassEstimate is the estimate a job of class with command key would get
// without a declared estimate: learned key > learned class > class default.
// It also reports where the value came from ("key", "class", "default").
func ClassEstimate(p Params, s State, class, key string) (int64, string) {
	return estimateFor(&p, &s, classFor(&p, class), key)
}

// undeclaredEstimate is the estimate of a job that declared none, and
// whether it was learned from the command's own runs. A learned value that
// is above what a machine of capacity c can ever admit (limit) is not used
// as is:
//
//   - command key: the margin is dropped if the observed peak (p90) fits
//     the limit; that much was actually given to this command here. If even
//     the observed peak is above the limit, the learned estimate stays and
//     the job is refused too_big (with a hint to declare --mem): its own
//     runs needed more than this machine can give.
//   - class: the class default applies. The class value is a guess for a
//     command never seen before; one that can never be admitted would
//     refuse (or, capped at the limit, block the queue behind) every new
//     command of the class, and those never run to teach a smaller value.
//
// A value is never capped at the limit itself: that fits only when nothing
// else uses any memory, so the job would hold the queue behind it until
// its queue timeout.
func undeclaredEstimate(p *Params, s *State, cls ClassParams, key string, c int64) (int64, bool) {
	est, src := estimateFor(p, s, cls, key)
	limit := maxAdmissible(p, c)
	if limit <= 0 || est <= limit {
		return est, src == "key"
	}
	switch src {
	case "key":
		if raw, ok := learnedRawPeak(p, s, histKeyPrefix+key); ok && raw <= limit {
			return raw, true
		}
		return est, true
	case "class":
		return cls.EstimateBytes, false
	}
	return est, false
}

func estimateFor(p *Params, s *State, cls ClassParams, key string) (int64, string) {
	if key != "" {
		if v, ok := learnedPeak(p, s, histKeyPrefix+key); ok {
			return v, "key"
		}
	}
	if v, ok := learnedPeak(p, s, histClassPrefix+cls.Name); ok {
		return v, "class"
	}
	return cls.EstimateBytes, "default"
}
