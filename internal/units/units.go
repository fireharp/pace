// Package units parses and formats byte sizes and durations used in config,
// CLI flags and governor messages.
//
// Bytes use binary multiples: 1K = 1024. Formatting is stable:
//
//	>= 1G  -> one decimal, e.g. "5.6G"
//	>= 1M  -> integer, e.g. "512M"
//	>= 1K  -> integer, e.g. "64K"
//	else   -> "123B"
package units

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
	TiB int64 = 1 << 40
)

// ParseBytes parses "4G", "512M", "1.5G", "1024", "64k", "2GiB", "2GB".
// Suffixes are case-insensitive and binary (G = 2^30). Empty string is an error.
func ParseBytes(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	u := strings.ToUpper(t)
	u = strings.TrimSuffix(u, "IB")
	u = strings.TrimSuffix(u, "B")
	mult := int64(1)
	if n := len(u); n > 0 {
		switch u[n-1] {
		case 'K':
			mult, u = KiB, u[:n-1]
		case 'M':
			mult, u = MiB, u[:n-1]
		case 'G':
			mult, u = GiB, u[:n-1]
		case 'T':
			mult, u = TiB, u[:n-1]
		}
	}
	u = strings.TrimSpace(u)
	if u == "" {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if i, err := strconv.ParseInt(u, 10, 64); err == nil {
		if i < 0 {
			return 0, fmt.Errorf("negative size %q", s)
		}
		return i * mult, nil
	}
	f, err := strconv.ParseFloat(u, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatBytes renders n with the stable governor format ("5.6G", "512M").
func FormatBytes(n int64) string {
	neg := ""
	if n < 0 {
		neg, n = "-", -n
	}
	switch {
	case n >= GiB:
		// one decimal, truncated toward the nearest tenth (rounded half up)
		tenths := (n*10 + GiB/2) / GiB
		return fmt.Sprintf("%s%d.%dG", neg, tenths/10, tenths%10)
	case n >= MiB:
		return fmt.Sprintf("%s%dM", neg, (n+MiB/2)/MiB)
	case n >= KiB:
		return fmt.Sprintf("%s%dK", neg, (n+KiB/2)/KiB)
	default:
		return fmt.Sprintf("%s%dB", neg, n)
	}
}

// ParseDuration accepts Go durations ("40s", "2m30s", "250ms") and bare
// integers as seconds ("40").
func ParseDuration(s string) (time.Duration, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if i, err := strconv.ParseInt(t, 10, 64); err == nil {
		return time.Duration(i) * time.Second, nil
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return d, nil
}

// FormatMS renders milliseconds compactly: "250ms", "40s", "2m30s", "1h5m".
// Values >= 1s are rounded to whole seconds.
func FormatMS(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	sec := (ms + 500) / 1000
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 || (h == 0 && m == 0) {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
