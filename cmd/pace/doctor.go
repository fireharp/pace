package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fireharp/pace/internal/integrate"
	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/units"
)

const doctorHelp = `
Usage: pace doctor [--config F]

Check the installation: binary path, config, socket, daemon (pid, mode),
launchd/systemd service, PATH shims, Claude Code hooks, log directory, and
whether this platform's memory signals are readable. Exit 0 when nothing
FAILed (WARN lines are advice), 1 otherwise.
`

type doctorReport struct {
	w     *cli
	fails int
	warns int
}

func (r *doctorReport) line(level, check, msg string) {
	switch level {
	case "FAIL":
		r.fails++
	case "WARN":
		r.warns++
	}
	fmt.Fprintf(r.w.stdout, "%-4s  %-9s %s\n", level, check, msg)
}

func (c *cli) cmdDoctor(args []string) int {
	fs := c.newFlags("doctor", doctorHelp)
	cfgPath := fs.String("config", "", "config file")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace doctor: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	r := &doctorReport{w: c}

	// Binary.
	bin, err := c.executable()
	if err != nil {
		r.line("FAIL", "binary", err.Error())
	} else if warns, err := integrate.CheckBinaryPath(bin); err != nil {
		r.line("FAIL", "binary", err.Error())
	} else {
		r.line("ok", "binary", bin+" ("+versionString()+")")
		for _, w := range warns {
			r.line("WARN", "binary", w)
		}
	}

	// Config.
	cfg, err := c.loadConfig(*cfgPath)
	cfgOK := err == nil
	if err != nil {
		r.line("FAIL", "config", err.Error())
		cfg.Socket = integrate.SocketPath("", c.env)
		cfg.LogDir = integrate.DefaultLogDir(c.env, runtime.GOOS)
	} else {
		r.line("ok", "config", fmt.Sprintf("source=%s mode=%s hash=%s", dash(cfg.Source), dash(cfg.Params.Mode), dash(cfg.Hash)))
	}

	// Socket and daemon.
	if len(cfg.Socket) > integrate.MaxSocketPath {
		r.line("FAIL", "socket", fmt.Sprintf("%s is %d bytes; the limit is %d", cfg.Socket, len(cfg.Socket), integrate.MaxSocketPath))
	} else {
		cl := &integrate.Client{Socket: cfg.Socket, Timeout: time.Second}
		ctx := context.Background()
		if err := cl.Health(ctx); err != nil {
			r.line("FAIL", "daemon", err.Error()+" (start it: 'pace install' or 'pace daemon')")
		} else {
			pid := "?"
			if p, err := daemonPID(cfg.Socket); err == nil {
				pid = fmt.Sprint(p)
			}
			s, _, err := cl.Status(ctx)
			if err != nil {
				r.line("WARN", "daemon", fmt.Sprintf("pid %s answers /healthz but /v1/status failed: %v", pid, err))
			} else {
				r.line("ok", "daemon", fmt.Sprintf("pid %s version=%s mode=%s pressure=%s headroom=%s socket=%s",
					pid, dash(s.Version), dash(s.Mode), dash(s.Pressure), units.FormatBytes(s.HeadroomBytes), cfg.Socket))
				override := ""
				if cfgOK && s.Mode != "" && cfg.Params.Mode != "" && s.Mode != cfg.Params.Mode {
					// pace install --mode sets PACE_MODE in the service, which wins over the file
					override = " (the daemon's environment overrides the config's mode=" + cfg.Params.Mode + ")"
				}
				if s.Mode == "observe" || s.Mode == "warn-only" {
					r.line("WARN", "mode", s.Mode+": enforcement is off"+override+" (fine for rollout; switch with 'pace install --mode enforce')")
				} else if override != "" {
					r.line("info", "mode", s.Mode+override)
				}
				for _, f := range s.Faults {
					r.line("WARN", "fault", f)
				}
				for _, n := range s.Notes {
					r.line("info", "platform", n)
				}
			}
		}
	}

	// Supervisor.
	inst := c.installer(true)
	if st, err := inst.Status(); err != nil {
		r.line("WARN", "service", err.Error())
	} else {
		switch {
		case !st.Installed && !st.Loaded:
			r.line("WARN", "service", st.Manager+": not installed; the daemon will not restart by itself ('pace install')")
		case !st.Loaded:
			r.line("FAIL", "service", st.Manager+": "+st.File+" exists but is not loaded ('pace install' reloads it)")
		case !st.Running:
			r.line("FAIL", "service", fmt.Sprintf("%s: loaded but not running (%s); see the daemon log", st.Manager, dash(st.Detail)))
		default:
			r.line("ok", "service", fmt.Sprintf("%s: running pid %d (%s)", st.Manager, st.PID, st.File))
		}
	}

	// Shims.
	shimDir := integrate.DefaultShimDir(c.env)
	shims, _ := integrate.ListShims(shimDir, c.env("PATH"))
	if len(shims) == 0 {
		r.line("WARN", "shims", "none in "+shimDir+" (agents bypass pace unless they call 'pace run'; 'pace shims install')")
	} else {
		resolved := 0
		for _, s := range shims {
			if s.Real != "" {
				resolved++
			}
		}
		msg := fmt.Sprintf("%d in %s, %d resolve to a real tool", len(shims), shimDir, resolved)
		if idx := integrate.PathIndex(c.env("PATH"), shimDir); idx < 0 {
			r.line("ok", "shims", msg+"; not on this shell's PATH (expected: the Claude hook adds it per session)")
		} else {
			r.line("ok", "shims", fmt.Sprintf("%s; PATH entry %d", msg, idx+1))
		}
	}

	// Harness hooks (read-only).
	for _, h := range []integrate.Harness{integrate.HarnessClaude, integrate.HarnessCodex} {
		path := integrate.HooksFile(h, c.env)
		found, err := integrate.HooksInstalled(path)
		want := []string{"hook " + string(h) + "-session-start", "hook " + string(h) + "-pre-tool-use"}
		missing := []string{}
		for i, ev := range []string{"SessionStart", "PreToolUse"} {
			ok := false
			for _, cmd := range found[ev] {
				ok = ok || strings.Contains(cmd, want[i])
			}
			if !ok {
				missing = append(missing, ev)
			}
		}
		switch {
		case err != nil && os.IsNotExist(err) && h == integrate.HarnessCodex:
			if _, e := os.Stat(filepath.Dir(path)); e == nil {
				r.line("WARN", string(h), "pace hooks not found ("+path+" does not exist; 'pace integrate codex --apply')")
			} // no ~/.codex: Codex is not used here
		case err != nil && !os.IsNotExist(err):
			r.line("WARN", string(h), path+": "+err.Error())
		case len(missing) == 0:
			r.line("ok", string(h), "hooks found in "+path)
		case len(missing) == 2:
			r.line("WARN", string(h), "pace hooks not found in "+path+" ('pace integrate "+string(h)+" --apply')")
		default:
			r.line("WARN", string(h), "pace hook missing in "+path+": "+strings.Join(missing, ", ")+" ('pace integrate "+string(h)+" --apply')")
		}
	}

	// Log directory.
	if fi, err := os.Stat(cfg.LogDir); err != nil {
		r.line("WARN", "logs", cfg.LogDir+" does not exist yet (created by 'pace install' / the daemon)")
	} else if !fi.IsDir() {
		r.line("FAIL", "logs", cfg.LogDir+" is not a directory")
	} else if f, err := os.CreateTemp(cfg.LogDir, ".doctor-*"); err != nil {
		r.line("FAIL", "logs", cfg.LogDir+" not writable: "+err.Error())
	} else {
		f.Close()
		os.Remove(f.Name())
		r.line("ok", "logs", cfg.LogDir+" writable")
	}

	// Platform signals.
	be, err := platform.New()
	if err != nil {
		r.line("FAIL", "platform", err.Error())
	} else {
		t0 := time.Now()
		m, err := be.SampleMachine(nil)
		if err != nil {
			r.line("FAIL", "platform", "SampleMachine: "+err.Error())
		} else {
			procs, perr := be.ListProcs(os.Getuid())
			el := time.Since(t0)
			if perr != nil {
				r.line("FAIL", "platform", "ListProcs: "+perr.Error())
			} else {
				r.line("ok", "platform", fmt.Sprintf("%s: total %s avail %s kernel level %d swap %s, %d processes, sample %v",
					be.Name(), units.FormatBytes(m.TotalBytes), units.FormatBytes(m.AvailBytes), m.KernelLevel,
					units.FormatBytes(m.SwapUsed), len(procs), el.Round(10*time.Microsecond)))
			}
		}
	}

	fmt.Fprintf(c.stdout, "\n%d failed, %d warnings\n", r.fails, r.warns)
	if r.fails > 0 {
		return exitFail
	}
	return exitOK
}

// daemonPID asks the kernel who is listening on the socket.
func daemonPID(sock string) (int, error) {
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix connection")
	}
	return peerPID(uc)
}
