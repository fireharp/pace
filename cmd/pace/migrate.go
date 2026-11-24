package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fireharp/pace/internal/integrate"
)

const migrateHelp = `
Usage: pace migrate [--apply]

Move an install of gov (pace's old name) over to pace. Without --apply it
prints the plan and changes nothing. With --apply, in order:
  1. stops and removes the old service (com.gov.daemon / gov.service),
     remembering the mode and config file it ran with
  2. copies config.toml and fleet.toml from ~/.config/gov to ~/.config/pace
     (a file that already exists under pace is left alone)
  3. replaces the shims in ~/.gov/shims with pace shims for the same tools in
     the pace shim directory
  4. replaces gov's hooks in the Claude Code and Codex settings files with
     pace's (each file is backed up first; other hooks are kept)
  5. installs the pace service with the old mode, if a gov service was there
Old state and logs (~/Library/Application Support/gov, ~/Library/Logs/gov,
~/.local/state/gov) are kept; delete them once pace runs well. GOV_* environment
variables still work as fallbacks for PACE_*, but rename them when you can.
`

func (c *cli) cmdMigrate(args []string) int {
	fs := c.newFlags("migrate", migrateHelp)
	apply := fs.Bool("apply", false, "do it (default: print the plan and change nothing)")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace migrate: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	m := migration{c: c, apply: *apply, env: integrate.Env(c.env), out: c.stdout}
	if !m.apply {
		fmt.Fprintln(m.out, "pace migrate: plan only; nothing is changed. Run 'pace migrate --apply' to do it.")
	}
	bin, err := c.executable()
	if err != nil {
		fmt.Fprintf(c.stderr, "pace migrate: cannot locate the pace binary: %v\n", err)
		return exitFail
	}
	if _, err := integrate.CheckBinaryPath(bin); err != nil {
		fmt.Fprintf(c.stderr, "pace migrate: %v\n", err)
		return exitUsage
	}
	m.bin = bin
	steps := []struct {
		name string
		run  func() error
	}{
		{"service", m.stopOldService},
		{"config", m.copyConfig},
		{"shims", m.moveShims},
		{"hooks", m.replaceHooks},
		{"install", m.installNew},
	}
	for _, s := range steps {
		fmt.Fprintf(m.out, "\n== %s\n", s.name)
		if err := s.run(); err != nil {
			fmt.Fprintf(c.stderr, "pace migrate: %s: %v\n", s.name, err)
			if m.apply {
				fmt.Fprintln(c.stderr, "pace migrate: stopped; the steps above are done, rerun 'pace migrate --apply' after fixing this")
			} else {
				fmt.Fprintln(c.stderr, "pace migrate: the plan fails at this step; fix it before 'pace migrate --apply'")
			}
			return exitFail
		}
	}
	m.leftovers()
	return exitOK
}

type migration struct {
	c     *cli
	apply bool
	env   integrate.Env
	out   io.Writer
	bin   string

	hadService bool
	oldEnv     map[string]string
}

func (m *migration) home() string { return m.c.homeDir() }

// stopOldService removes gov's launchd agent or systemd unit.
func (m *migration) stopOldService() error {
	file := integrate.LegacyServiceFile(runtime.GOOS, m.home(), m.env)
	m.oldEnv = integrate.ServiceFileEnv(file)
	inst := m.c.installer(!m.apply)
	if !fileExists(file) {
		fmt.Fprintf(m.out, "no gov service file at %s\n", file)
	} else {
		m.hadService = true
		fmt.Fprintf(m.out, "gov service: %s", file)
		if v := m.oldEnv["MODE"]; v != "" {
			fmt.Fprintf(m.out, " (mode %s)", v)
		}
		fmt.Fprintln(m.out)
	}
	return inst.UninstallLegacy()
}

// copyConfig copies gov's config.toml and fleet.toml to pace's config dir.
func (m *migration) copyConfig() error {
	from, to := integrate.LegacyConfigDir(m.env), integrate.ConfigDir(m.env)
	n := 0
	for _, name := range []string{"config.toml", "fleet.toml"} {
		src, dst := filepath.Join(from, name), filepath.Join(to, name)
		fi, err := os.Stat(src)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		n++
		if fileExists(dst) {
			fmt.Fprintf(m.out, "keep %s (already there; %s not copied)\n", dst, src)
			continue
		}
		if !m.apply {
			fmt.Fprintf(m.out, "would copy %s -> %s\n", src, dst)
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(to, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, fi.Mode().Perm()); err != nil {
			return err
		}
		fmt.Fprintf(m.out, "copied %s -> %s\n", src, dst)
	}
	if n == 0 {
		fmt.Fprintf(m.out, "no gov config in %s\n", from)
	}
	return nil
}

// moveShims replaces gov's shims with pace shims for the same tools.
func (m *migration) moveShims() error {
	oldDir, newDir := integrate.LegacyShimDir(m.env), integrate.DefaultShimDir(m.env)
	list, err := integrate.ListShims(oldDir, "")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if len(list) == 0 && oldDir != newDir {
		fmt.Fprintf(m.out, "no gov shims in %s\n", oldDir)
		return nil
	}
	tools := make([]string, 0, len(list))
	for _, s := range list {
		tools = append(tools, s.Name)
	}
	if oldDir != newDir {
		if _, err := integrate.UninstallShims(oldDir, !m.apply, m.out); err != nil {
			return err
		}
		if m.apply {
			_ = os.Remove(oldDir) // only if now empty
			_ = os.Remove(filepath.Dir(oldDir))
		}
	}
	if len(tools) == 0 {
		return nil
	}
	_, err = integrate.InstallShims(integrate.ShimOptions{Dir: newDir, PaceBin: m.bin, Tools: tools, DryRun: !m.apply, Out: m.out})
	return err
}

// replaceHooks swaps gov's hooks for pace's in each harness settings file.
// Hook commands are matched by shape, so the old 'gov hook ...' commands are
// recognised and replaced; other hooks in the file are kept.
func (m *migration) replaceHooks() error {
	for _, h := range []integrate.Harness{integrate.HarnessClaude, integrate.HarnessCodex} {
		path := integrate.HooksFile(h, m.env)
		got, err := integrate.HooksInstalled(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w", path, err)
		}
		if len(got) == 0 {
			fmt.Fprintf(m.out, "%s: no gov hooks\n", path)
			continue
		}
		if !m.apply {
			fmt.Fprintf(m.out, "would replace the hooks in %s with 'pace hook %s-...' (backup first)\n", path, h)
			continue
		}
		res, err := integrate.UpdateHooksFile(path, h, m.bin, false, time.Now())
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		switch {
		case !res.Changed:
			fmt.Fprintf(m.out, "%s: pace hooks already installed\n", path)
		case res.Backup != "":
			fmt.Fprintf(m.out, "%s: hooks now run pace (previous file saved as %s)\n", path, res.Backup)
		default:
			fmt.Fprintf(m.out, "%s: hooks now run pace\n", path)
		}
		if h == integrate.HarnessCodex {
			fmt.Fprintln(m.out, "Codex runs changed hooks only after you review them once: run /hooks in the Codex CLI.")
		}
	}
	return nil
}

// installNew installs pace's service with the mode and config gov ran with.
func (m *migration) installNew() error {
	if !m.hadService {
		fmt.Fprintln(m.out, "no gov service was installed; run 'pace install' when you want the daemon supervised")
		return nil
	}
	var args []string
	if v := m.oldEnv["MODE"]; integrate.ValidMode(v) {
		args = append(args, "--mode", v)
	}
	if v := m.oldEnv["CONFIG"]; v != "" {
		// A config inside gov's config dir was copied to pace's.
		if rel, err := filepath.Rel(integrate.LegacyConfigDir(m.env), v); err == nil && !strings.HasPrefix(rel, "..") {
			v = filepath.Join(integrate.ConfigDir(m.env), rel)
		}
		args = append(args, "--config", v)
	}
	for _, k := range []string{"HOME", "SOCK"} {
		if v := m.oldEnv[k]; v != "" && m.c.env("PACE_"+k) != v {
			fmt.Fprintf(m.out, "note: the gov service set GOV_%s=%s; to keep it, export PACE_%s=%s and run 'pace install' again\n", k, v, k, v)
		}
	}
	if !m.apply {
		args = append(args, "--dry-run")
	}
	if code := m.c.cmdInstall(args); code != exitOK {
		return fmt.Errorf("pace install %s failed (exit %d)", strings.Join(args, " "), code)
	}
	return nil
}

func (m *migration) leftovers() {
	fmt.Fprintln(m.out, "\n== left in place")
	home := m.home()
	var dirs []string
	if runtime.GOOS == "darwin" {
		dirs = []string{
			filepath.Join(home, "Library", "Application Support", integrate.LegacyName),
			filepath.Join(home, "Library", "Logs", integrate.LegacyName),
			filepath.Join(home, "Library", "Caches", integrate.LegacyName),
		}
	} else {
		dirs = []string{filepath.Join(home, ".local", "state", integrate.LegacyName)}
	}
	dirs = append(dirs, integrate.LegacyConfigDir(m.env))
	n := 0
	for _, d := range dirs {
		if fileExists(d) {
			fmt.Fprintf(m.out, "%s (gov state, logs or config: delete it once pace runs well)\n", d)
			n++
		}
	}
	if integrate.PathIndex(m.c.env("PATH"), integrate.LegacyShimDir(m.env)) >= 0 {
		fmt.Fprintf(m.out, "PATH still names %s: drop it from your shell profile\n", integrate.LegacyShimDir(m.env))
		n++
	}
	if n == 0 {
		fmt.Fprintln(m.out, "nothing")
	}
	fmt.Fprintln(m.out, "The old 'gov' binary is no longer used; remove it from PATH when you are done.")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
