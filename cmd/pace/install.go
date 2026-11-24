package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/proto"
)

// newRunner is the command runner used by install/uninstall/doctor. Tests
// replace it so launchctl/systemctl are never executed.
var newRunner = func() integrate.Runner { return integrate.ExecRunner{} }

const installHelp = `
Usage: pace install [--mode observe|warn-only|enforce] [--config F] [--dry-run]

Install the daemon as a supervised per-user service that restarts by itself:
  macOS: ~/Library/LaunchAgents/com.pace.daemon.plist (launchctl bootstrap gui/<uid>)
  Linux: ~/.config/systemd/user/pace.service (systemctl --user enable --now)
The service runs '<this binary> daemon'. Re-running install upgrades in place
(restarts the daemon). --mode sets PACE_MODE for the daemon; roll out with
observe, then warn-only, then enforce. --dry-run prints what would be written
and run and changes nothing.
`

func (c *cli) installer(dryRun bool) *integrate.Installer {
	sock, _ := c.socketFor("")
	cl := &integrate.Client{Socket: sock, Timeout: 500 * time.Millisecond}
	return &integrate.Installer{
		GOOS:   runtime.GOOS,
		UID:    os.Getuid(),
		Home:   c.homeDir(),
		Env:    c.env,
		Runner: newRunner(),
		Out:    c.stdout,
		DryRun: dryRun,
		Health: func() error { return cl.Health(context.Background()) },
	}
}

func (c *cli) cmdInstall(args []string) int {
	fs := c.newFlags("install", installHelp)
	mode := fs.String("mode", "", "daemon mode: observe|warn-only|enforce (default: from config)")
	cfgPath := fs.String("config", "", "config file the daemon should use (sets PACE_CONFIG for the service)")
	dryRun := fs.Bool("dry-run", false, "print what would be done; change nothing")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace install: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *mode != "" && !integrate.ValidMode(*mode) {
		fmt.Fprintf(c.stderr, "pace install: invalid --mode %q (want observe, warn-only or enforce)\n", *mode)
		return exitUsage
	}
	bin, err := c.executable()
	if err != nil {
		fmt.Fprintf(c.stderr, "pace install: cannot locate the pace binary: %v\n", err)
		return exitFail
	}
	warns, err := integrate.CheckBinaryPath(bin)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace install: %v\n", err)
		return exitUsage
	}
	for _, w := range warns {
		fmt.Fprintf(c.stderr, "pace install: warning: %s\n", w)
	}
	env := map[string]string{}
	for _, k := range integrate.PassThroughEnv {
		if v := c.env(k); v != "" {
			env[k] = v
		}
	}
	if *cfgPath != "" {
		abs, err := filepath.Abs(*cfgPath)
		if err != nil {
			fmt.Fprintf(c.stderr, "pace install: --config: %v\n", err)
			return exitUsage
		}
		env["PACE_CONFIG"] = abs
	}
	cfg, err := c.loadConfig(env["PACE_CONFIG"])
	if err != nil {
		fmt.Fprintf(c.stderr, "pace install: %v\n", err)
		fmt.Fprintln(c.stderr, "pace: hint: fix it first ('pace config check'); the daemon would refuse to start")
		return exitUsage
	}
	if len(cfg.Socket) > integrate.MaxSocketPath {
		fmt.Fprintf(c.stderr, "pace install: socket path %s is longer than %d bytes; set PACE_SOCK or 'socket' in the config\n", cfg.Socket, integrate.MaxSocketPath)
		return exitUsage
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(c.stdout, "service environment: %s=%s\n", k, env[k])
	}
	spec := integrate.ServiceSpec{PaceBin: bin, Mode: *mode, LogDir: cfg.LogDir, Env: env}
	if err := c.installer(*dryRun).Install(spec); err != nil {
		fmt.Fprintf(c.stderr, "pace install: %v\n", err)
		return exitFail
	}
	if !*dryRun {
		fmt.Fprintln(c.stdout, "installed. Next: 'pace status', 'pace shims install' and 'pace integrate claude' to route agent commands through pace.")
	}
	return exitOK
}

const uninstallHelp = `
Usage: pace uninstall [--dry-run]

Stop the supervised daemon and remove its service file. Logs, state, config
and shims are kept ('pace shims uninstall' removes the shims). Jobs already
running under 'pace run' continue and fail open.
`

func (c *cli) cmdUninstall(args []string) int {
	fs := c.newFlags("uninstall", uninstallHelp)
	dryRun := fs.Bool("dry-run", false, "print what would be done; change nothing")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace uninstall: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if err := c.installer(*dryRun).Uninstall(); err != nil {
		fmt.Fprintf(c.stderr, "pace uninstall: %v\n", err)
		return exitFail
	}
	return exitOK
}

const shimsHelp = `
Usage: pace shims install [--dir D] [--tools go,npm,...] [--dry-run]
       pace shims uninstall [--dir D] [--dry-run]
       pace shims list [--dir D]

PATH shims route heavy commands (go build/test, cargo build, npm install,
pytest, make, ...) through 'pace run' without changing the command line. Each
shim is a tiny sh script that execs 'pace shim-exec NAME "$@"'; light commands
(go version, git, ls) exec the real binary directly (~2 ms). Inside a governed
job (PACE_JOB_ID set) or with PACE_SHIM_DISABLE=1, shims always pass through.

Put the directory first on PATH for agents only, e.g. with the Claude Code
SessionStart hook ('pace integrate claude'), or export PATH=D:$PATH.
Default directory: $PACE_SHIM_DIR, else $PACE_HOME/shims, else ~/.pace/shims.
`

func (c *cli) cmdShims(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, shimsHelp[1:])
		return exitUsage
	}
	action := args[0]
	if action == "-h" || action == "--help" || action == "help" {
		fmt.Fprint(c.stdout, shimsHelp[1:])
		return exitOK
	}
	fs := c.newFlags("shims "+action, shimsHelp)
	dir := fs.String("dir", integrate.DefaultShimDir(c.env), "shim directory")
	var tools *string
	var dryRun *bool
	switch action {
	case "install":
		tools = fs.String("tools", "", "comma-separated tool names (default: every tool in the heavy table)")
		dryRun = fs.Bool("dry-run", false, "print what would be done; change nothing")
	case "uninstall":
		dryRun = fs.Bool("dry-run", false, "print what would be done; change nothing")
	case "list":
	default:
		fmt.Fprintf(c.stderr, "pace shims: unknown action %q (want install, uninstall or list)\n", action)
		return exitUsage
	}
	if code, done := fs.parse(args[1:]); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace shims %s: unexpected argument %q\n", action, fs.Arg(0))
		return exitUsage
	}
	abs, err := filepath.Abs(*dir)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace shims: %v\n", err)
		return exitUsage
	}
	switch action {
	case "install":
		bin, err := c.executable()
		if err != nil {
			fmt.Fprintf(c.stderr, "pace shims: cannot locate the pace binary: %v\n", err)
			return exitFail
		}
		if _, err := integrate.CheckBinaryPath(bin); err != nil {
			fmt.Fprintf(c.stderr, "pace shims: %v\n", err)
			return exitUsage
		}
		_, err = integrate.InstallShims(integrate.ShimOptions{Dir: abs, PaceBin: bin, Tools: splitList(*tools), DryRun: *dryRun, Out: c.stdout})
		if err != nil {
			fmt.Fprintf(c.stderr, "pace shims: %v\n", err)
			return exitFail
		}
		if integrate.PathIndex(c.env("PATH"), abs) != 0 && !*dryRun {
			fmt.Fprintf(c.stdout, "shims are not first on PATH here. For agents: 'pace integrate claude', or export PATH=%s:\"$PATH\"\n", integrate.ShellQuote(abs))
		}
		return exitOK
	case "uninstall":
		if _, err := integrate.UninstallShims(abs, *dryRun, c.stdout); err != nil {
			fmt.Fprintf(c.stderr, "pace shims: %v\n", err)
			return exitFail
		}
		return exitOK
	}
	list, err := integrate.ListShims(abs, c.env("PATH"))
	if err != nil {
		fmt.Fprintf(c.stderr, "pace shims: %v\n", err)
		return exitFail
	}
	idx := integrate.PathIndex(c.env("PATH"), abs)
	switch {
	case len(list) == 0:
		fmt.Fprintf(c.stdout, "no shims in %s ('pace shims install' creates them)\n", abs)
		return exitOK
	case idx < 0:
		fmt.Fprintf(c.stdout, "%s: %d shims, NOT on PATH in this shell\n", abs, len(list))
	default:
		fmt.Fprintf(c.stdout, "%s: %d shims, PATH entry %d\n", abs, len(list), idx+1)
	}
	for _, s := range list {
		real := s.Real
		if real == "" {
			real = "(not installed)"
		}
		fmt.Fprintf(c.stdout, "  %-12s -> %s\n", s.Name, real)
	}
	return exitOK
}

const hookHelp = `
Usage: pace hook claude-session-start [--quiet]
       pace hook claude-pre-tool-use
       pace hook codex-session-start [--quiet]
       pace hook codex-pre-tool-use

Hook handlers for Claude Code and Codex. They read the hook JSON on stdin,
always exit 0, never block, deny or rewrite a tool call, and finish within
~150 ms even when the daemon is down.
  claude-session-start  exports the shim directory via $CLAUDE_ENV_FILE (when
                        shims are installed) and prints one line of context
  claude-pre-tool-use   for Bash calls, adds the governor status as
                        additionalContext when memory pressure is warn or worse,
                        with a hint true to the mode (observe/warn-only/enforce)
  codex-session-start   prints one line of context: Codex cannot put the shims
                        on PATH, so it tells the agent to use 'pace run'
  codex-pre-tool-use    as claude-pre-tool-use, plus a reminder whenever a
                        heavy command is about to run outside 'pace run'
Wire them with 'pace integrate claude|codex --apply' (or paste the snippet).
`

func (c *cli) cmdHook(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, hookHelp[1:])
		return exitUsage
	}
	name := args[0]
	if name == "-h" || name == "--help" || name == "help" {
		fmt.Fprint(c.stdout, hookHelp[1:])
		return exitOK
	}
	fs := c.newFlags("hook "+name, hookHelp)
	quiet := fs.Bool("quiet", false, "session-start: print no context line")
	if code, done := fs.parse(args[1:]); done {
		return code
	}
	budget := 150 * time.Millisecond
	sock, _ := c.socketFor("") // a broken config must not break the hook
	cl := &integrate.Client{Socket: sock, Timeout: budget}
	bin, _ := c.executable()
	d := integrate.HookDeps{
		Stdin:   c.stdin,
		Stdout:  c.stdout,
		Env:     c.env,
		ShimDir: integrate.DefaultShimDir(c.env),
		Status: func(ctx context.Context) (proto.Status, error) {
			s, _, err := cl.Status(ctx)
			return s, err
		},
		Budget:  budget,
		Quiet:   *quiet,
		PaceBin: bin,
	}
	var err error
	switch name {
	case "claude-session-start":
		err = integrate.SessionStart(d)
	case "claude-pre-tool-use":
		err = integrate.PreToolUse(d)
	case "codex-session-start":
		err = integrate.CodexSessionStart(d)
	case "codex-pre-tool-use":
		err = integrate.CodexPreToolUse(d)
	default:
		fmt.Fprintf(c.stderr, "pace hook: unknown hook %q (want claude-session-start, claude-pre-tool-use, codex-session-start or codex-pre-tool-use)\n", name)
		return exitOK // a hook must never fail the harness
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "pace hook %s: %v\n", name, err)
	}
	return exitOK
}

const integrateHelp = `
Usage: pace integrate claude|codex [--print | --apply | --remove]

Wire pace into an agent harness with two hooks:
  SessionStart  tells the agent how pace reports (exit codes, 'pace:' lines) and
                the current headroom. Claude Code: also puts the pace shims first
                on PATH for the session's Bash commands ($CLAUDE_ENV_FILE).
                Codex has no such mechanism, so the agent is told to run heavy
                commands as 'pace run -- <cmd>'.
  PreToolUse    adds the governor status to the agent's context when memory
                pressure is warn or worse. Codex: also reminds the agent when a
                heavy command is about to run outside 'pace run'.
The hooks never block, deny or rewrite a command.

--print (default) prints the JSON snippet and changes nothing.
--apply merges the hooks into the harness' user file (~/.claude/settings.json,
        ~/.codex/hooks.json; $CLAUDE_CONFIG_DIR / $CODEX_HOME are honoured):
        other keys and hooks are kept in order, earlier pace hooks replaced, and
        the previous file saved as <file>.pace-backup-<time>.
--remove takes pace's hooks out again (same backup).
Codex asks you to review new hooks once ('/hooks' in the Codex CLI).
Install shims first with 'pace shims install' (Claude Code uses them).
`

func (c *cli) cmdIntegrate(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(c.stderr, integrateHelp[1:])
		return exitUsage
	}
	target := args[0]
	if target == "-h" || target == "--help" || target == "help" {
		fmt.Fprint(c.stdout, integrateHelp[1:])
		return exitOK
	}
	fs := c.newFlags("integrate "+target, integrateHelp)
	fs.Bool("print", true, "print the snippet and change nothing (the default)")
	apply := fs.Bool("apply", false, "merge the hooks into the harness' settings file (with a backup)")
	remove := fs.Bool("remove", false, "remove pace's hooks from the harness' settings file (with a backup)")
	file := fs.String("file", "", "settings file to change instead of the harness default")
	if code, done := fs.parse(args[1:]); done {
		return code
	}
	h := integrate.Harness(target)
	if h != integrate.HarnessClaude && h != integrate.HarnessCodex {
		fmt.Fprintf(c.stderr, "pace integrate: unknown target %q (supported: claude, codex)\n", target)
		return exitUsage
	}
	if *apply && *remove {
		fmt.Fprintln(c.stderr, "pace integrate: --apply and --remove are exclusive")
		return exitUsage
	}
	bin, err := c.executable()
	if err != nil {
		fmt.Fprintf(c.stderr, "pace integrate: cannot locate the pace binary: %v\n", err)
		return exitFail
	}
	path := *file
	if path == "" {
		path = integrate.HooksFile(h, c.env)
	}
	if !*apply && !*remove {
		fmt.Fprint(c.stdout, integrate.HooksSnippet(h, bin))
		fmt.Fprintf(c.stderr, "pace: merge the snippet above into %s yourself, or run 'pace integrate %s --apply'.\n", path, h)
		if h == integrate.HarnessClaude {
			if l, _ := integrate.ListShims(integrate.DefaultShimDir(c.env), ""); len(l) == 0 {
				fmt.Fprintln(c.stderr, "pace: no shims installed yet; run 'pace shims install' so SessionStart can route heavy commands.")
			}
		}
		return exitOK
	}
	if *apply {
		if warns, err := integrate.CheckBinaryPath(bin); err != nil {
			fmt.Fprintf(c.stderr, "pace integrate: %v\n", err)
			return exitFail
		} else {
			for _, w := range warns {
				fmt.Fprintln(c.stderr, "pace: warning: "+w)
			}
		}
	}
	res, err := integrate.UpdateHooksFile(path, h, bin, *remove, time.Now())
	if err != nil {
		fmt.Fprintf(c.stderr, "pace integrate: %v (nothing changed)\n", err)
		return exitFail
	}
	verb := "installed"
	if *remove {
		verb = "removed"
	}
	switch {
	case !res.Changed:
		fmt.Fprintf(c.stdout, "%s: pace hooks already %s; nothing changed\n", path, verb)
	case res.Backup != "":
		fmt.Fprintf(c.stdout, "%s: pace hooks %s (previous file saved as %s)\n", path, verb, res.Backup)
	default:
		fmt.Fprintf(c.stdout, "%s: pace hooks %s\n", path, verb)
	}
	if *apply {
		switch h {
		case integrate.HarnessCodex:
			fmt.Fprintln(c.stdout, "Codex runs new hooks only after you review them once: open the Codex CLI and run /hooks. New sessions pick them up.")
		case integrate.HarnessClaude:
			fmt.Fprintln(c.stdout, "New Claude Code sessions pick the hooks up.")
			if l, _ := integrate.ListShims(integrate.DefaultShimDir(c.env), ""); len(l) == 0 {
				fmt.Fprintln(c.stdout, "No shims installed yet: run 'pace shims install' so heavy commands are routed through pace run.")
			}
		}
	}
	return exitOK
}
