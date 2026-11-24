package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
	"github.com/fireharp/pace/internal/wrapper"
)

const runHelp = `
Usage: pace run [flags] [--] command [args...]

Run command under the governor. The daemon admits it when its expected
footprint fits the headroom, queues it otherwise, and warns or kills it per
policy. While queued, 'pace: QUEUED' stderr lines say why and where: one at
once, another when the reason changes, and the latest position at most every
10s. The command's stdout, stderr and exit code pass through unchanged.

Exit codes: the command's own code; 200 refused (--no-wait, queue timeout or
too big); 201 killed by policy; 202 pace usage or config error. Whenever the
code is 200-202 the last stderr line is 'pace: EXIT code=N source=pace|child'.
If the daemon is unreachable the command still runs after a 'pace: DEGRADED'
line (bounded fail-open).

Sizes: 4G, 512M, 1.5G. Durations: 40s, 2m30s, 250ms (a bare number is seconds).

Examples:
  pace run -- go test ./...
  pace run --class build --mem 6G -- cargo build --release
  pace run --no-wait --class test -- npm test     # exit 200 instead of queueing
`

// priorityFlag records whether --priority was given.
type priorityFlag struct {
	v   int
	set bool
}

func (p *priorityFlag) String() string {
	if p == nil || !p.set {
		return ""
	}
	return strconv.Itoa(p.v)
}

func (p *priorityFlag) Set(s string) error {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > 9 {
		return fmt.Errorf("priority must be an integer 0..9, got %q", s)
	}
	p.v, p.set = n, true
	return nil
}

// sizeFlag parses a byte size.
type sizeFlag struct{ v int64 }

func (s *sizeFlag) String() string {
	if s == nil || s.v == 0 {
		return ""
	}
	return units.FormatBytes(s.v)
}

func (s *sizeFlag) Set(v string) error {
	n, err := units.ParseBytes(v)
	if err != nil {
		return err
	}
	s.v = n
	return nil
}

// durFlag parses a duration into milliseconds.
type durFlag struct{ ms int64 }

func (d *durFlag) String() string {
	if d == nil || d.ms == 0 {
		return ""
	}
	return units.FormatMS(d.ms)
}

func (d *durFlag) Set(v string) error {
	t, err := units.ParseDuration(v)
	if err != nil {
		return err
	}
	d.ms = t.Milliseconds()
	return nil
}

// runArgs is the parsed form of `pace run`.
type runArgs struct {
	class        string
	mem, budget  sizeFlag
	priority     priorityFlag
	timeout      durFlag
	noWait       bool
	queueTimeout durFlag
	statusFile   string
	configPath   string
	quiet        bool
	argv         []string
}

func (c *cli) runFlags(r *runArgs) *flagSet {
	fs := c.newFlags("run", runHelp)
	fs.StringVar(&r.class, "class", "", "job `CLASS`: build|test|browser|model|default (default: from config)")
	fs.Var(&r.mem, "mem", "expected peak memory footprint `SIZE`, e.g. 4G (default: learned or class default)")
	fs.Var(&r.budget, "budget", "memory budget `SIZE`; the job is warned near it and killed above it (default: class budget)")
	fs.Var(&r.priority, "priority", "priority `N` 0..9, higher survives longer under pressure (default: class priority)")
	fs.Var(&r.timeout, "timeout", "wall-clock limit `DUR`, e.g. 30m (default: class limit)")
	fs.BoolVar(&r.noWait, "no-wait", false, "exit 200 at once instead of queueing")
	fs.Var(&r.queueTimeout, "queue-timeout", "give up (exit 200) after queueing for `DUR` (default: config queue.timeout)")
	fs.StringVar(&r.statusFile, "status-file", "", "write the final JSON job status to `FILE` (default: $PACE_STATUS_FILE)")
	fs.StringVar(&r.configPath, "config", "", "config `FILE` (default: $PACE_CONFIG or ~/.config/pace/config.toml)")
	fs.BoolVar(&r.quiet, "quiet", false, "suppress non-essential pace lines")
	fs.BoolVar(&r.quiet, "q", false, "shorthand for --quiet")
	return fs
}

// paceExit reports a pace-side usage or config error: one human line with
// what to do, then the final EXIT line, and returns 202. The status file
// (--status-file, if parsed, else $PACE_STATUS_FILE) is written first, with
// outcome error, as for every other way pace run ends.
func (c *cli) paceExit(msg string, hint string) int { return c.paceExitFor(runArgs{}, msg, hint) }

func (c *cli) paceExitFor(r runArgs, msg string, hint string) int {
	human := "pace run: " + msg
	if hint != "" {
		human += " (" + hint + ")"
	}
	fmt.Fprintln(c.stderr, human)
	e := proto.Event{V: 1, TS: time.Now().UnixMilli(), Verb: proto.VerbExit, Code: proto.Int(proto.ExitPaceError), Source: "pace", Msg: msg}
	path := r.statusFile
	if path == "" {
		path = c.env(wrapper.EnvStatusFile)
	}
	if err := wrapper.WriteErrorStatus(path, r.argv, e); err != nil {
		fmt.Fprintln(c.stderr, (&proto.Event{Verb: proto.VerbInfo, Msg: "status file: " + err.Error()}).Line())
	}
	fmt.Fprintln(c.stderr, e.Line())
	return proto.ExitPaceError
}

func (c *cli) parseRun(args []string) (runArgs, int, bool) {
	var r runArgs
	fs := c.runFlags(&r)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.printUsage(c.stdout)
			return r, exitOK, true
		}
		return r, c.paceExitFor(r, err.Error(), "see 'pace run -h'"), true
	}
	r.argv = fs.Args()
	if len(r.argv) == 0 {
		return r, c.paceExitFor(r, "no command given", "usage: pace run [flags] -- command [args...]"), true
	}
	if r.argv[0] == "" {
		return r, c.paceExitFor(r, "empty command name", "usage: pace run [flags] -- command [args...]"), true
	}
	return r, 0, false
}

func (c *cli) cmdRun(args []string) int {
	r, code, done := c.parseRun(args)
	if done {
		return code
	}
	return c.execRun(r)
}

// wrapperRun is wrapper.Run; tests replace it to observe the options.
var wrapperRun = wrapper.Run

// runOptions maps parsed flags and the resolved config to wrapper options.
func (c *cli) runOptions(r runArgs) (wrapper.Options, error) {
	cfg, err := c.loadConfig(r.configPath)
	if err != nil {
		return wrapper.Options{}, err
	}
	statusFile := r.statusFile
	if statusFile == "" {
		statusFile = c.env("PACE_STATUS_FILE")
	}
	o := wrapper.Options{
		Config:         cfg,
		Class:          r.class,
		EstimateBytes:  r.mem.v,
		BudgetBytes:    r.budget.v,
		WallLimitMS:    r.timeout.ms,
		NoWait:         r.noWait,
		QueueTimeoutMS: r.queueTimeout.ms,
		StatusFile:     statusFile,
		Quiet:          r.quiet,
		Argv:           r.argv,
		Stdin:          c.stdin,
		Stdout:         c.stdout,
		Stderr:         c.stderr,
	}
	if r.priority.set {
		p := r.priority.v
		o.Priority = &p
	}
	return o, nil
}

func (c *cli) execRun(r runArgs) int {
	o, err := c.runOptions(r)
	if err != nil {
		return c.paceExitFor(r, err.Error(), "fix it ('pace config check') or point --config/PACE_CONFIG at a valid file")
	}
	return wrapperRun(o)
}

// cmdShimExec is what every PATH shim execs: `pace shim-exec NAME args...`.
// It must be fast for light commands: no config load, no daemon contact.
func (c *cli) cmdShimExec(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(c.stderr, "Usage: pace shim-exec NAME [args...]\n\nInternal entry point of the PATH shims ('pace shims install').")
		if len(args) == 0 {
			return exitUsage
		}
		return exitOK
	}
	name, rest := args[0], args[1:]
	self, _ := c.executable()
	shimDir := integrate.DefaultShimDir(c.env)
	real, err := integrate.LookPathExcluding(name, c.env("PATH"), []string{shimDir}, self)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace: shim: %s: command not found in PATH (outside the shim directory %s)\n", name, shimDir)
		return 127 // the shell's "command not found"
	}
	argv := append([]string{name}, rest...)
	if c.env("PACE_JOB_ID") != "" || c.env("PACE_SHIM_DISABLE") != "" {
		return c.execReal(real, argv)
	}
	class, heavy := integrate.Classify(argv)
	if !heavy {
		return c.execReal(real, argv)
	}
	return c.execRun(runArgs{class: class, argv: append([]string{real}, rest...)})
}

// execRealHook lets tests observe exec instead of replacing the process.
var execRealHook func(path string, argv []string) int

func (c *cli) execReal(path string, argv []string) int {
	if execRealHook != nil {
		return execRealHook(path, argv)
	}
	err := syscall.Exec(path, argv, os.Environ())
	fmt.Fprintf(c.stderr, "pace: shim: exec %s: %v\n", path, err)
	return 126 // the shell's "found but not executable"
}

// splitList splits a comma/space separated list.
func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' })
}
