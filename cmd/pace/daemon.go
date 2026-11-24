package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/fireharp/pace/internal/config"
	"github.com/fireharp/pace/internal/daemon"
	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/platform"
)

const daemonHelp = `
Usage: pace daemon [--config F]

Run the governor daemon in the foreground until SIGTERM or SIGINT. SIGHUP
(or 'POST /v1/reload') re-reads the config. Normally started by the
supervisor installed with 'pace install'; run it by hand for debugging.
`

func (c *cli) cmdDaemon(args []string) int {
	fs := c.newFlags("daemon", daemonHelp)
	cfgPath := fs.String("config", "", "config file (default: $PACE_CONFIG or ~/.config/pace/config.toml)")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace daemon: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	be, err := platform.New()
	if err != nil {
		fmt.Fprintf(c.stderr, "pace daemon: %v\n", err)
		return exitFail
	}
	m, err := be.SampleMachine(nil)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace daemon: first machine sample: %v\n", err)
		return exitFail
	}
	cfg, err := config.LoadResolved(*cfgPath, m.TotalBytes, runtime.GOOS)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace daemon: %v\n", err)
		return exitUsage
	}
	cfg.Socket = integrate.SocketPath(cfg.Socket, c.env)
	if cfg.LogDir == "" {
		cfg.LogDir = integrate.DefaultLogDir(c.env, runtime.GOOS)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	fmt.Fprintf(c.stderr, "pace daemon: starting version=%s pid=%d platform=%s mode=%s config=%s socket=%s\n",
		versionString(), os.Getpid(), be.Name(), dash(cfg.Params.Mode), dash(cfg.Source), cfg.Socket)
	err = daemon.Run(ctx, daemon.Options{
		Config:     cfg,
		Backend:    be,
		UID:        os.Getuid(),
		Version:    versionString(),
		ConfigPath: *cfgPath,
	})
	if err != nil {
		fmt.Fprintf(c.stderr, "pace daemon: %v\n", err)
		return exitFail
	}
	fmt.Fprintln(c.stderr, "pace daemon: stopped")
	return exitOK
}

const replayHelp = `
Usage: pace replay FILE... [--config F]

Re-run every decision-log record in FILE... through the policy and diff the
result against what was logged. With --config, the records are replayed
with that configuration instead (what-if). Exit 0 when every decision
matches, 1 on any mismatch, 202 when the input cannot be read.
`

func (c *cli) cmdReplay(args []string) int {
	fs := c.newFlags("replay", replayHelp)
	cfgPath := fs.String("config", "", "replay with this config instead of each record's params (what-if)")
	files, code, done := fs.parseInterspersed(args)
	if done {
		return code
	}
	if len(files) == 0 {
		fmt.Fprintln(c.stderr, "pace replay: no log files given")
		fmt.Fprintln(c.stderr, "pace: hint: decision logs are decisions.jsonl* in the daemon log directory ('pace config print' shows it)")
		return exitUsage
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			fmt.Fprintf(c.stderr, "pace replay: %v\n", err)
			return exitUsage
		}
	}
	var override *config.Resolved
	if *cfgPath != "" {
		r, err := c.loadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintf(c.stderr, "pace replay: %v\n", err)
			return exitUsage
		}
		override = &r
	}
	var rep dlog.ReplayReport
	var err error
	if override != nil {
		p := override.Params
		rep, err = dlog.Replay(files, &p, c.stdout)
	} else {
		rep, err = dlog.Replay(files, nil, c.stdout)
	}
	if err != nil {
		fmt.Fprintf(c.stderr, "pace replay: %v\n", err)
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "replay: records=%d replayed=%d mismatches=%d\n", rep.Records, rep.Replayed, rep.Mismatches)
	if rep.Mismatches > 0 {
		return exitFail
	}
	return exitOK
}

const configHelp = `
Usage: pace config print|check|example [--config F]

  print    print the effective, resolved configuration as JSON
  check    validate the configuration; exit 0 if valid, 202 if not
  example  print a commented example config.toml (all defaults)

The config file is $PACE_CONFIG, else ~/.config/pace/config.toml; it is
optional and unknown keys are refused. Environment overrides: PACE_HOME,
PACE_SOCK, PACE_CAPACITY, PACE_MODE.
`

func (c *cli) cmdConfig(args []string) int {
	fs := c.newFlags("config", configHelp)
	cfgPath := fs.String("config", "", "config file")
	pos, code, done := fs.parseInterspersed(args)
	if done {
		return code
	}
	if len(pos) != 1 {
		fs.printUsage(c.stderr)
		return exitUsage
	}
	switch pos[0] {
	case "example":
		fmt.Fprint(c.stdout, integrate.ExampleConfig)
		return exitOK
	case "print":
		r, err := c.loadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintf(c.stderr, "pace config: %v\n", err)
			return exitUsage
		}
		b, _ := json.MarshalIndent(r, "", "  ")
		fmt.Fprintln(c.stdout, string(b))
		return exitOK
	case "check":
		r, err := c.loadConfig(*cfgPath)
		if err != nil {
			fmt.Fprintf(c.stderr, "pace config: invalid: %v\n", err)
			return exitUsage
		}
		if len(r.Socket) > integrate.MaxSocketPath {
			fmt.Fprintf(c.stderr, "pace config: invalid: socket path %s is %d bytes; the limit is %d (set socket or PACE_SOCK to a shorter path)\n",
				r.Socket, len(r.Socket), integrate.MaxSocketPath)
			return exitUsage
		}
		fmt.Fprintf(c.stdout, "ok source=%s hash=%s mode=%s socket=%s\n", dash(r.Source), dash(r.Hash), dash(r.Params.Mode), r.Socket)
		return exitOK
	}
	fmt.Fprintf(c.stderr, "pace config: unknown action %q (want print, check or example)\n", pos[0])
	return exitUsage
}
