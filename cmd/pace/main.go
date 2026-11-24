// Command pace is the resource governor: daemon and CLI in one binary.
//
// Agents launch heavy work with `pace run -- <cmd>`; the daemon admits,
// queues, warns and enforces, and every decision reaches the agent as a
// `pace:` stderr line, an exit code and JSON. See docs/CONTRACT.md.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/envcompat"
	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
)

// version is set at build time: go build -ldflags "-X main.version=v0.1.0".
var version = "dev"

// Exit codes of the non-run subcommands (run passes the child's code through
// and uses proto.Exit* for its own outcomes).
const (
	exitOK          = 0
	exitFail        = 1 // check failed (doctor, replay mismatches, config check)
	exitUnreachable = 3 // daemon unreachable (status, watch)
	exitUsage       = proto.ExitPaceError
)

// cli carries the process' standard streams and environment so that every
// subcommand can run in-process under test.
type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	env            integrate.Env
	// exe is the absolute path of this binary ("" = os.Executable).
	exe string
}

func newCLI() *cli {
	return &cli{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr, env: envcompat.Getenv}
}

func main() { os.Exit(newCLI().main(os.Args[1:])) }

type command struct {
	name    string
	summary string
	run     func(c *cli, args []string) int
	hidden  bool
}

var commands []command

func init() {
	commands = []command{
		{"run", "run a command under the governor (admission, budget, feedback)", (*cli).cmdRun, false},
		{"status", "show headroom, pressure, jobs and suggested concurrency", (*cli).cmdStatus, false},
		{"watch", "follow the governor's event feed (the alert channel)", (*cli).cmdWatch, false},
		{"daemon", "run the governor daemon in the foreground", (*cli).cmdDaemon, false},
		{"install", "install the daemon as a supervised per-user service", (*cli).cmdInstall, false},
		{"uninstall", "stop and remove the supervised service", (*cli).cmdUninstall, false},
		{"migrate", "move an install of gov (the old name) over to pace", (*cli).cmdMigrate, false},
		{"shims", "manage PATH shims that route heavy commands through pace run", (*cli).cmdShims, false},
		{"fleet", "one view of pace on all your machines (over ssh; optional)", (*cli).cmdFleet, false},
		{"hook", "Claude Code and Codex hook handlers", (*cli).cmdHook, false},
		{"integrate", "wire pace into Claude Code or Codex (hooks)", (*cli).cmdIntegrate, false},
		{"replay", "re-run decision log records and diff the decisions", (*cli).cmdReplay, false},
		{"config", "print, check or show an example configuration", (*cli).cmdConfig, false},
		{"doctor", "check the installation and the daemon", (*cli).cmdDoctor, false},
		{"version", "print the version", (*cli).cmdVersion, false},
		{"help", "show help for pace or a subcommand", (*cli).cmdHelp, false},
		{"test-alert", "check that alerts reach you (a small job over its budget)", (*cli).cmdTestAlert, false},
		{"shim-exec", "internal: entry point of PATH shims", (*cli).cmdShimExec, true},
		{"test-alert-alloc", "internal: the test-alert job", (*cli).cmdTestAlertAlloc, true},
	}
}

func lookup(name string) *command {
	for i := range commands {
		if commands[i].name == name {
			return &commands[i]
		}
	}
	return nil
}

func (c *cli) main(args []string) int {
	if len(args) == 0 {
		c.usage(c.stderr)
		return exitUsage
	}
	name := args[0]
	switch name {
	case "-h", "--help", "-help":
		c.usage(c.stdout)
		return exitOK
	case "-v", "--version", "-version":
		return c.cmdVersion(nil)
	}
	cmd := lookup(name)
	if cmd == nil {
		fmt.Fprintf(c.stderr, "pace: unknown command %q\n", name)
		fmt.Fprintln(c.stderr, "pace: hint: run 'pace help' for the list of commands")
		return exitUsage
	}
	return cmd.run(c, args[1:])
}

func (c *cli) usage(w io.Writer) {
	fmt.Fprint(w, `pace is a resource governor for agent workloads: it admits, queues, throttles
and kills heavy processes so the machine never gets stuck, and tells agents why.

Usage:
  pace <command> [flags] [arguments]

Commands:
`)
	for _, cmd := range commands {
		if !cmd.hidden {
			fmt.Fprintf(w, "  %-10s %s\n", cmd.name, cmd.summary)
		}
	}
	fmt.Fprint(w, `
Examples:
  pace run --class build -- go build ./...
  pace run --mem 6G --no-wait -- npm test
  pace status
  pace install --mode observe

Exit codes of 'pace run': the command's own code, or 200 refused, 201 killed by
policy, 202 pace usage/config error (followed by a final 'pace: EXIT' line).
Run 'pace help <command>' or 'pace <command> -h' for details.
`)
}

func (c *cli) cmdHelp(args []string) int {
	if len(args) == 0 || args[0] == "help" || strings.HasPrefix(args[0], "-") {
		c.usage(c.stdout)
		return exitOK
	}
	cmd := lookup(args[0])
	if cmd == nil {
		fmt.Fprintf(c.stderr, "pace help: unknown command %q\n", args[0])
		return exitUsage
	}
	return cmd.run(c, append(args[1:], "-h"))
}

func (c *cli) cmdVersion(args []string) int {
	if len(args) > 0 && args[0] != "-h" && args[0] != "--help" {
		fmt.Fprintln(c.stderr, "pace version: takes no arguments")
		return exitUsage
	}
	if len(args) > 0 {
		fmt.Fprintln(c.stdout, "Usage: pace version\n\nPrint the pace version, platform and Go version.")
		return exitOK
	}
	fmt.Fprintf(c.stdout, "pace %s %s/%s %s\n", versionString(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	return exitOK
}

// versionString is the ldflags version, or "dev+<vcs revision>" for a dev build.
func versionString() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		rev, dirty := "", false
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if len(rev) > 12 {
			rev = rev[:12]
		}
		if rev != "" {
			if dirty {
				rev += "-dirty"
			}
			return "dev+" + rev
		}
	}
	return version
}

// flagSet builds a FlagSet whose -h prints usage (header + flags) to stdout.
type flagSet struct {
	*flag.FlagSet
	c      *cli
	header string
}

func (c *cli) newFlags(name, header string) *flagSet {
	fs := flag.NewFlagSet("pace "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &flagSet{FlagSet: fs, c: c, header: header}
}

func (f *flagSet) printUsage(w io.Writer) {
	fmt.Fprint(w, strings.TrimLeft(f.header, "\n"))
	has := false
	f.VisitAll(func(*flag.Flag) { has = true })
	if has {
		fmt.Fprintln(w, "\nFlags:")
		f.SetOutput(w)
		f.PrintDefaults()
		f.SetOutput(io.Discard)
	}
}

// parse parses args; done=true means the caller should return code.
func (f *flagSet) parse(args []string) (code int, done bool) {
	err := f.Parse(args)
	if err == nil {
		return 0, false
	}
	if errors.Is(err, flag.ErrHelp) {
		f.printUsage(f.c.stdout)
		return exitOK, true
	}
	fmt.Fprintf(f.c.stderr, "%s: %v\n", f.Name(), err)
	fmt.Fprintf(f.c.stderr, "pace: hint: run '%s -h' for usage\n", f.Name())
	return exitUsage, true
}

// parseInterspersed parses flags that may appear after positional arguments
// (`pace replay a.jsonl --config x`). A literal "--" ends flag parsing.
func (f *flagSet) parseInterspersed(args []string) (pos []string, code int, done bool) {
	for {
		if code, done := f.parse(args); done {
			return nil, code, true
		}
		rest := f.Args()
		if len(rest) == 0 {
			return pos, 0, false
		}
		if rest[0] == "--" {
			return append(pos, rest[1:]...), 0, false
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

// executable returns the absolute, symlink-resolved path of this binary.
func (c *cli) executable() (string, error) {
	if c.exe != "" {
		return c.exe, nil
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Abs(p)
}

// loadConfig loads and resolves the configuration for this machine and
// fills paths the resolver left empty with the documented defaults.
func (c *cli) loadConfig(path string) (config.Resolved, error) {
	r, err := config.LoadResolved(path, totalMemory(), runtime.GOOS)
	if err != nil {
		return r, err
	}
	r.Socket = integrate.SocketPath(r.Socket, c.env)
	if r.LogDir == "" {
		r.LogDir = integrate.DefaultLogDir(c.env, runtime.GOOS)
	}
	return r, nil
}

// socketFor resolves the daemon socket, tolerating a broken config (status
// and hooks must still reach the daemon).
func (c *cli) socketFor(configPath string) (string, error) {
	r, err := c.loadConfig(configPath)
	if err != nil {
		return integrate.SocketPath("", c.env), err
	}
	return r.Socket, nil
}

func (c *cli) client(configPath string) *integrate.Client {
	sock, err := c.socketFor(configPath)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace: warning: %v (using socket %s)\n", err, sock)
	}
	return &integrate.Client{Socket: sock}
}

// homeDir returns $HOME (from the cli env) or the OS home.
func (c *cli) homeDir() string {
	if h := c.env("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
