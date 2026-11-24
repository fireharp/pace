package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
)

const statusHelp = `
Usage: pace status [--json] [--job ID] [--config F]

Show the machine-wide view: mode, memory pressure, capacity, headroom, whether
new jobs are admitted (and why not), suggested concurrency, running and queued
jobs, and the largest processes started outside 'pace run'.

--json prints the daemon's JSON as is (see docs/CONTRACT.md). --job shows one
job (kept 24h after it ends) with its events.

Exit codes: 0 ok, 1 job not found, 3 daemon unreachable, 202 usage error.
`

func (c *cli) unreachable(cmd string, err error) int {
	fmt.Fprintf(c.stderr, "pace %s: %v\n", cmd, err)
	fmt.Fprintln(c.stderr, "pace: hint: start the daemon with 'pace install' (supervised) or 'pace daemon' (foreground); 'pace doctor' checks the setup")
	return exitUnreachable
}

func (c *cli) cmdStatus(args []string) int {
	fs := c.newFlags("status", statusHelp)
	asJSON := fs.Bool("json", false, "print raw JSON")
	job := fs.String("job", "", "show one job by id")
	cfg := fs.String("config", "", "config file (for the socket path)")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace status: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	cl := c.client(*cfg)
	ctx := context.Background()
	if *job != "" {
		j, raw, err := cl.Job(ctx, *job)
		if errors.Is(err, integrate.ErrJobNotFound) {
			fmt.Fprintf(c.stderr, "pace status: job %s not found (finished jobs are kept 24h)\n", *job)
			return exitFail
		}
		if err != nil {
			if integrate.IsUnreachable(err) {
				return c.unreachable("status", err)
			}
			fmt.Fprintf(c.stderr, "pace status: %v\n", err)
			return exitFail
		}
		if *asJSON {
			c.stdout.Write(append(trimNL(raw), '\n'))
			return exitOK
		}
		renderJob(c.stdout, j, time.Now().UnixMilli())
		return exitOK
	}
	s, raw, err := cl.Status(ctx)
	if err != nil {
		if integrate.IsUnreachable(err) {
			return c.unreachable("status", err)
		}
		fmt.Fprintf(c.stderr, "pace status: %v\n", err)
		return exitFail
	}
	if *asJSON {
		c.stdout.Write(append(trimNL(raw), '\n'))
		return exitOK
	}
	renderStatus(c.stdout, s)
	return exitOK
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func age(nowMS, tsMS int64) string {
	if tsMS <= 0 || nowMS <= 0 {
		return "-"
	}
	return units.FormatMS(nowMS - tsMS)
}

func bytesOrDash(n int64) string {
	if n <= 0 {
		return "-"
	}
	return units.FormatBytes(n)
}

func cmdline(argv []string, max int) string {
	s := strings.Join(argv, " ")
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// renderStatus prints the compact human view of a Status.
func renderStatus(w io.Writer, s proto.Status) {
	now := s.TS
	fmt.Fprintf(w, "pace %s (%s)  mode=%s  pressure=%s", dash(s.Version), dash(s.Platform), dash(s.Mode), dash(s.Pressure))
	if s.KernelLevel > 0 {
		fmt.Fprintf(w, " (kernel level %d)", s.KernelLevel)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "memory    capacity %s  avail %s  reserved %s  reserve %s  headroom %s",
		units.FormatBytes(s.CapacityBytes), units.FormatBytes(s.AvailBytes), units.FormatBytes(s.ReservedBytes),
		units.FormatBytes(s.ReserveBytes), units.FormatBytes(s.HeadroomBytes))
	if s.SwapUsedBytes > 0 {
		fmt.Fprintf(w, "  swap %s", units.FormatBytes(s.SwapUsedBytes))
	}
	fmt.Fprintln(w)
	adm := yesNo(s.Admitting)
	if !s.Admitting && len(s.NotAdmittingReasons) > 0 {
		adm += " (" + strings.Join(s.NotAdmittingReasons, ", ") + ")"
	}
	fmt.Fprintf(w, "admitting %s\n", adm)
	sc := fmt.Sprintf("suggested concurrency %d", s.SuggestedConcurrency)
	if len(s.SuggestedByClass) > 0 {
		var parts []string
		for _, k := range sortedKeys(s.SuggestedByClass) {
			parts = append(parts, fmt.Sprintf("%s=%d", k, s.SuggestedByClass[k]))
		}
		sc += " (" + strings.Join(parts, " ") + ")"
	}
	fmt.Fprintln(w, sc)
	if s.NCPU > 0 || s.DiskFreeBytes > 0 {
		var parts []string
		if s.NCPU > 0 {
			parts = append(parts, fmt.Sprintf("load %.2f/%d cpus", float64(s.Load1Milli)/1000, s.NCPU))
		}
		if s.DiskFreeBytes > 0 {
			parts = append(parts, "disk free "+units.FormatBytes(s.DiskFreeBytes))
		}
		fmt.Fprintln(w, "system    "+strings.Join(parts, "  "))
	}

	fmt.Fprintf(w, "\nRUNNING (%d)\n", len(s.Running))
	if len(s.Running) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCLASS\tPRI\tUSED\tPEAK\tBUDGET\tRESERVED\tPROCS\tAGE\tSTAGE\tCOMMAND")
		for _, j := range s.Running {
			fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", j.ID, dash(j.Class), j.Priority,
				bytesOrDash(j.UsedBytes), bytesOrDash(j.PeakBytes), bytesOrDash(j.BudgetBytes), bytesOrDash(j.ReservedBytes),
				j.NProcs, age(now, j.StartTS), dash(j.Stage), cmdline(j.Argv, 50))
		}
		tw.Flush()
	}
	fmt.Fprintf(w, "\nQUEUED (%d)\n", len(s.Queued))
	if len(s.Queued) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "POS\tID\tCLASS\tPRI\tNEED\tWAITED\tCOMMAND")
		for _, j := range s.Queued {
			pos := "-"
			if j.Position > 0 {
				pos = fmt.Sprint(j.Position)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", pos, j.ID, dash(j.Class), j.Priority,
				bytesOrDash(j.EstimateBytes), age(now, j.SubmitTS), cmdline(j.Argv, 50))
		}
		tw.Flush()
	}
	if len(s.Unmanaged) > 0 {
		n := len(s.Unmanaged)
		if n > 5 {
			n = 5
		}
		fmt.Fprintf(w, "\nUNMANAGED (top %d of %d, started outside pace run)\n", n, len(s.Unmanaged))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PGID\tCOMM\tPROCS\tUSED\tPRI\tAGE\tSTAGE")
		for _, u := range s.Unmanaged[:n] {
			fmt.Fprintf(tw, "%d\t%s\t%d\t%s\t%d\t%s\t%s\n", u.PGID, dash(u.Comm), u.NProcs, bytesOrDash(u.UsedBytes),
				u.Priority, age(now, u.StartTS), dash(u.Stage))
		}
		tw.Flush()
	}
	if sm := s.UnmanagedBelowMinVictim; sm != nil && sm.Groups > 0 {
		g := "groups"
		if sm.Groups == 1 {
			g = "group"
		}
		fmt.Fprintf(w, "\nunmanaged below min_victim (%s): %d %s, %s in all\n", units.FormatBytes(sm.MinVictimBytes),
			sm.Groups, g, units.FormatBytes(sm.UsedBytes))
	}
	if len(s.Warnings) > 0 {
		fmt.Fprintf(w, "\nWARNINGS (%d)\n", len(s.Warnings))
		for _, e := range s.Warnings {
			for _, l := range e.Lines() {
				fmt.Fprintln(w, l)
			}
		}
	}
	if len(s.RecentAlerts) > 0 {
		fmt.Fprintf(w, "\nRECENT ALERTS (%d, last 15m)\n", len(s.RecentAlerts))
		for _, e := range s.RecentAlerts {
			fmt.Fprintf(w, "  %s ago  %s\n", age(now, e.TS), (&e).Line())
		}
	}
	if len(s.Faults) > 0 {
		fmt.Fprintf(w, "\nFAULTS (%d)\n", len(s.Faults))
		for _, f := range s.Faults {
			fmt.Fprintln(w, "  "+f)
		}
	}
	if len(s.Notes) > 0 {
		fmt.Fprintf(w, "\nNOTES (%d)\n", len(s.Notes))
		for _, n := range s.Notes {
			fmt.Fprintln(w, "  "+n)
		}
	}
}

// renderJob prints one job and its events.
func renderJob(w io.Writer, j proto.JobStatus, nowMS int64) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	row("job", j.ID)
	row("state", dash(j.State))
	if j.Stage != "" {
		row("stage", j.Stage)
	}
	row("class", dash(j.Class))
	row("priority", fmt.Sprint(j.Priority))
	row("command", dash(cmdline(j.Argv, 200)))
	if j.PID > 0 {
		row("pid", fmt.Sprintf("%d (pgid %d, %d procs)", j.PID, j.PGID, j.NProcs))
	}
	row("memory", fmt.Sprintf("used %s  peak %s  budget %s  estimate %s  reserved %s",
		bytesOrDash(j.UsedBytes), bytesOrDash(j.PeakBytes), bytesOrDash(j.BudgetBytes), bytesOrDash(j.EstimateBytes), bytesOrDash(j.ReservedBytes)))
	if j.Position > 0 {
		row("queue position", fmt.Sprint(j.Position))
	}
	row("submitted", fmtTS(j.SubmitTS))
	if j.StartTS > 0 {
		row("started", fmtTS(j.StartTS))
	}
	if j.EndTS > 0 {
		row("ended", fmt.Sprintf("%s (ran %s)", fmtTS(j.EndTS), age(j.EndTS, j.StartTS)))
	} else if j.StartTS > 0 {
		row("running for", age(nowMS, j.StartTS))
	}
	if j.ExitCode != nil {
		row("exit code", fmt.Sprint(*j.ExitCode))
	}
	if j.KilledBy != "" {
		row("killed by", j.KilledBy)
	}
	tw.Flush()
	if len(j.Events) > 0 {
		fmt.Fprintf(w, "\nEVENTS (%d)\n", len(j.Events))
		for _, e := range j.Events {
			ts := fmtTS(e.TS)
			for _, l := range e.Lines() {
				fmt.Fprintf(w, "%s  %s\n", ts, l)
			}
		}
	}
}

func fmtTS(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04:05")
}

const watchHelp = `
Usage: pace watch [--json] [--time] [--no-reconnect] [--config F]

Follow the governor's event feed: every QUEUED, WARN, THROTTLED, FROZEN,
KILLED... for governed jobs and for processes started outside 'pace run'.
Lines use the same 'pace: VERB key=value' grammar as 'pace run' stderr; --json
prints one JSON event per line instead. Reconnects across daemon restarts
unless --no-reconnect. Stop with Ctrl-C.
`

func (c *cli) cmdWatch(args []string) int {
	fs := c.newFlags("watch", watchHelp)
	asJSON := fs.Bool("json", false, "print one JSON event per line")
	withTime := fs.Bool("time", false, "prefix each line with the event time")
	noReconnect := fs.Bool("no-reconnect", false, "exit when the stream ends or the daemon is unreachable")
	cfg := fs.String("config", "", "config file (for the socket path)")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace watch: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return c.watch(ctx, c.client(*cfg), *asJSON, *withTime, !*noReconnect, time.Second)
}

func (c *cli) watch(ctx context.Context, cl *integrate.Client, asJSON, withTime, reconnect bool, backoff time.Duration) int {
	announced := false // an outage was reported; cleared once events flow again
	print := func(raw []byte, ev proto.Event) error {
		announced = false
		if asJSON {
			_, err := c.stdout.Write(append(trimNL(raw), '\n'))
			return err
		}
		prefix := ""
		if withTime {
			prefix = time.UnixMilli(ev.TS).Format("15:04:05.000") + " "
		}
		if ev.Verb == "" { // not an event we understand: show it raw
			_, err := fmt.Fprintf(c.stdout, "%s%s\n", prefix, trimNL(raw))
			return err
		}
		for _, l := range ev.Lines() {
			if _, err := fmt.Fprintln(c.stdout, prefix+l); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		err := cl.Follow(ctx, print)
		if ctx.Err() != nil {
			return exitOK
		}
		switch {
		case integrate.IsUnreachable(err):
			if !reconnect {
				return c.unreachable("watch", err)
			}
			if !announced {
				fmt.Fprintf(c.stderr, "pace watch: %v; retrying every %v\n", err, backoff)
				announced = true
			}
		case errors.Is(err, io.EOF):
			if !reconnect {
				return exitOK
			}
			fmt.Fprintln(c.stderr, "pace watch: stream closed; reconnecting")
			announced = false
		default:
			fmt.Fprintf(c.stderr, "pace watch: %v\n", err)
			if !reconnect {
				return exitFail
			}
		}
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(backoff):
		}
	}
}
