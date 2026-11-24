package fleet

import (
	"fmt"
	"html/template"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
)

// Ago renders the age of a snapshot ("now", "42s", "5m", "3h", "2d").
func Ago(now time.Time, ms int64) string {
	if ms == 0 {
		return "never"
	}
	d := now.Sub(time.UnixMilli(ms))
	switch {
	case d < 2*time.Second:
		return "now"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// clean makes a peer-supplied string safe for one terminal table cell:
// control characters (tab, newline, ESC sequences) become spaces.
func clean(s string) string {
	if strings.IndexFunc(s, unicode.IsControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// Row is the rendered columns of one machine (shared by text and HTML).
type Row struct {
	Name, State, Seen, Mode, Pressure, Memory, Headroom, Load, Jobs, Top, Note string
	Self, Fresh                                                                bool
}

func topUnit(s *proto.Status) string {
	var best string
	var bytes int64
	for _, j := range s.Running {
		if j.UsedBytes > bytes {
			bytes, best = j.UsedBytes, fmt.Sprintf("%s %s", j.ID, firstWord(j.Argv))
		}
	}
	for _, u := range s.Unmanaged {
		if u.UsedBytes > bytes {
			bytes, best = u.UsedBytes, u.Comm
		}
	}
	if best == "" {
		return "-"
	}
	return best + " " + units.FormatBytes(bytes)
}

func firstWord(argv []string) string {
	if len(argv) == 0 {
		return "?"
	}
	w := argv[0]
	if i := strings.LastIndexByte(w, '/'); i >= 0 {
		w = w[i+1:]
	}
	if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
		w += " " + argv[1]
	}
	return w
}

// Rows renders ms for display at now.
func Rows(ms []Machine, now time.Time) []Row {
	rows := make([]Row, 0, len(ms))
	for _, m := range ms {
		r := Row{Name: m.Name, State: m.State, Self: m.Self, Fresh: m.Fresh(), Seen: Ago(now, m.SeenMS),
			Mode: "-", Pressure: "-", Memory: "-", Headroom: "-", Load: "-", Jobs: "-", Top: "-"}
		if m.Self {
			r.Name += "*"
		}
		if s := m.Status; s != nil {
			r.Mode, r.Pressure = dash(s.Mode), dash(s.Pressure)
			r.Memory = units.FormatBytes(s.AvailBytes) + "/" + units.FormatBytes(s.CapacityBytes)
			r.Headroom = units.FormatBytes(s.HeadroomBytes)
			if s.NCPU > 0 {
				r.Load = fmt.Sprintf("%.1f/%d", float64(s.Load1Milli)/1000, s.NCPU)
			}
			r.Jobs = fmt.Sprintf("%d/%d", len(s.Running), len(s.Queued))
			r.Top = topUnit(s)
		}
		if !m.Fresh() {
			r.Note = m.Error
		}
		for _, f := range []*string{&r.Name, &r.State, &r.Mode, &r.Pressure, &r.Top, &r.Note} {
			*f = clean(*f)
		}
		rows = append(rows, r)
	}
	return rows
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// RenderText writes the fleet table. Machines that did not answer show
// their last snapshot; a note under the table says why and how old.
func RenderText(w io.Writer, ms []Machine, now time.Time, verbose bool) {
	rows := Rows(ms, now)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tSTATE\tSEEN\tMODE\tPRESSURE\tAVAIL/TOTAL\tHEADROOM\tLOAD\tRUN/Q\tTOP")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.State, r.Seen, r.Mode, r.Pressure, r.Memory, r.Headroom, r.Load, r.Jobs, r.Top)
	}
	tw.Flush()
	notes := false
	for i, r := range rows {
		if r.Note == "" {
			continue
		}
		if !notes {
			fmt.Fprintln(w)
			notes = true
		}
		what := "no snapshot yet"
		if ms[i].Status != nil {
			what = "showing its snapshot from " + r.Seen + " ago"
			if r.Seen == "now" {
				what = "showing its last snapshot"
			}
		}
		fmt.Fprintf(w, "%s: %s; %s\n", clean(ms[i].Name), r.Note, what)
	}
	if verbose {
		for _, m := range ms {
			renderDetail(w, m, now)
		}
	}
}

func renderDetail(w io.Writer, m Machine, now time.Time) {
	fmt.Fprintf(w, "\n== %s (%s via %s", clean(m.Name), clean(m.State), m.Via)
	if m.Target != "" {
		fmt.Fprintf(w, " %s", clean(m.Target))
	}
	if m.LatencyMS > 0 {
		fmt.Fprintf(w, ", %dms", m.LatencyMS)
	}
	fmt.Fprintln(w, ")")
	s := m.Status
	if s == nil {
		fmt.Fprintln(w, "  no snapshot")
		return
	}
	fmt.Fprintf(w, "  pace %s (%s)  admitting %v", clean(dash(s.Version)), clean(dash(s.Platform)), s.Admitting)
	if len(s.NotAdmittingReasons) > 0 {
		fmt.Fprintf(w, " (%s)", clean(strings.Join(s.NotAdmittingReasons, ", ")))
	}
	fmt.Fprintf(w, "  suggested concurrency %d  swap %s  disk free %s\n", s.SuggestedConcurrency, units.FormatBytes(s.SwapUsedBytes), units.FormatBytes(s.DiskFreeBytes))
	for _, title := range []string{"running", "queued"} {
		jobs := s.Running
		if title == "queued" {
			jobs = s.Queued
		}
		for _, j := range jobs {
			fmt.Fprintf(w, "  %-7s %-5s %-7s used %-6s budget %-6s %s\n", title, clean(j.ID), clean(j.Class), units.FormatBytes(j.UsedBytes), units.FormatBytes(j.BudgetBytes), clean(strings.Join(j.Argv, " ")))
		}
	}
	us := append([]proto.UnitStatus(nil), s.Unmanaged...)
	sort.Slice(us, func(i, j int) bool { return us[i].UsedBytes > us[j].UsedBytes })
	if len(us) > 3 {
		us = us[:3]
	}
	for _, u := range us {
		fmt.Fprintf(w, "  outside %-14s %d procs %s\n", clean(u.Comm), u.NProcs, units.FormatBytes(u.UsedBytes))
	}
	for _, e := range s.Warnings {
		fmt.Fprintf(w, "  warning %s\n", clean(e.Line()))
	}
	for _, f := range s.Faults {
		fmt.Fprintf(w, "  fault   %s\n", clean(f))
	}
}

var pageTmpl = template.Must(template.New("fleet").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="{{.Refresh}}">
<title>pace fleet</title>
<style>
:root { color-scheme: light dark; --ok:#2e7d32; --warn:#b26a00; --bad:#c62828; --mute:#888; }
body { font: 14px/1.4 -apple-system, system-ui, sans-serif; margin: 1.5rem; }
h1 { font-size: 1.1rem; margin: 0 0 .25rem; }
p.sub { color: var(--mute); margin: 0 0 1rem; }
table { border-collapse: collapse; width: 100%; }
th, td { text-align: left; padding: .4rem .6rem; border-bottom: 1px solid #8883; white-space: nowrap; }
th { font-weight: 600; color: var(--mute); font-size: 12px; text-transform: uppercase; }
td.num { font-variant-numeric: tabular-nums; }
.live { color: var(--ok); } .stale, .pace-down { color: var(--warn); } .unreachable, .no-pace, .error { color: var(--bad); }
.p-warn { color: var(--warn); font-weight: 600; } .p-critical { color: var(--bad); font-weight: 600; }
tr.old td:not(.state) { opacity: .6; }
.note { color: var(--mute); font-size: 12px; white-space: normal; }
</style></head><body>
<h1>pace fleet</h1>
<p class="sub">{{.Summary}} · updated {{.Updated}} · refreshes every {{.Refresh}}s · stale rows show the last snapshot</p>
<table>
<tr><th>Machine</th><th>State</th><th>Seen</th><th>Mode</th><th>Pressure</th><th>Avail/Total</th><th>Headroom</th><th>Load</th><th>Run/Q</th><th>Top</th></tr>
{{range .Rows}}<tr class="{{if not .Fresh}}old{{end}}">
<td>{{.Name}}</td><td class="state {{.State}}">{{.State}}</td><td>{{.Seen}}</td><td>{{.Mode}}</td>
<td class="p-{{.Pressure}}">{{.Pressure}}</td><td class="num">{{.Memory}}</td><td class="num">{{.Headroom}}</td>
<td class="num">{{.Load}}</td><td class="num">{{.Jobs}}</td><td>{{.Top}}</td></tr>
{{if .Note}}<tr><td></td><td colspan="9" class="note">{{.Note}}</td></tr>{{end}}
{{end}}</table>
</body></html>
`))

// RenderHTML writes the fleet page (auto-refreshing every refresh).
func RenderHTML(w io.Writer, ms []Machine, now time.Time, refresh time.Duration) error {
	sec := int(refresh.Seconds())
	if sec < 1 {
		sec = 1
	}
	return pageTmpl.Execute(w, map[string]any{
		"Rows": Rows(ms, now), "Summary": Summary(ms), "Updated": now.Format("15:04:05"), "Refresh": sec,
	})
}
