package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/fleet"
	"github.com/fireharp/pace/internal/proto"
)

const fleetHelp = `
Usage: pace fleet [status] [--json] [-v] [--watch] [--every 5s]
       pace fleet add <name> <ssh-destination> [--pace PATH] [--container C [--container-user U]] [--no-check]
       pace fleet rm <name>
       pace fleet rename <old-name> <new-name>
       pace fleet ls
       pace fleet self [<name>]
       pace fleet serve [--listen 127.0.0.1:7719] [--every 10s]

One view of the pace daemons on all your machines. Optional and local-first:
each daemon governs its own machine and never depends on the fleet. This
machine is read through its socket, every peer through ssh ('pace status
--json' on the peer), over Tailscale or a LAN: no port is opened and no new
credentials are needed, only ssh access that works without a prompt.

A peer that cannot be reached is shown as "stale" with its last snapshot and
its age (no connection is not "out of service"). "pace-down" means the machine
answered but its pace daemon did not; "no-pace" that pace is not installed there.

Peers live in ~/.config/pace/fleet.toml ($PACE_FLEET); snapshots are cached in
~/Library/Caches/pace/fleet (macOS) or ~/.cache/pace/fleet.

  status  print the table (the default); -v adds jobs, warnings and the
          biggest processes per machine; --watch redraws every --every
  add     add or update a peer and check it once (ssh destination: a host
          alias, user@host, a Tailscale name or IP). With --container the
          peer is a container on that host, read with docker exec (e.g. one
          station of an agent swarm, each with its own pace and memory limit)
  rm, ls  remove or list peers
  rename  rename a peer or this machine (its snapshot is kept)
  self    show or set this machine's name in the view (default: hostname)
  serve   serve the view as an auto-refreshing web page and /fleet.json; it
          listens on localhost unless --listen says otherwise (for example a
          Tailscale address, to open it from a phone on the tailnet). It
          answers requests for an IP, localhost, the --listen name, a short
          host name or a *.ts.net name, and refuses others (DNS rebinding).
`

func (c *cli) cmdFleet(args []string) int {
	sub := "status"
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			fmt.Fprint(c.stdout, fleetHelp[1:])
			return exitOK
		case "status", "add", "rm", "remove", "rename", "mv", "ls", "list", "serve", "self":
			sub, args = args[0], args[1:]
		default:
			if len(args[0]) > 0 && args[0][0] != '-' {
				fmt.Fprintf(c.stderr, "pace fleet: unknown subcommand %q\n", args[0])
				fmt.Fprintln(c.stderr, "pace: hint: run 'pace fleet -h' for usage")
				return exitUsage
			}
		}
	}
	switch sub {
	case "add":
		return c.fleetAdd(args)
	case "rm", "remove":
		return c.fleetRemove(args)
	case "ls", "list":
		return c.fleetList(args)
	case "serve":
		return c.fleetServe(args)
	case "self":
		return c.fleetSelf(args)
	case "rename", "mv":
		return c.fleetRename(args)
	}
	return c.fleetStatus(args)
}

func (c *cli) fleetPath() string { return fleet.DefaultPath(fleet.Env(c.env)) }

func (c *cli) loadFleet() (fleet.File, bool) {
	f, err := fleet.Load(c.fleetPath())
	if err != nil {
		fmt.Fprintf(c.stderr, "pace fleet: %v\n", err)
		return f, false
	}
	return f, true
}

func (c *cli) fetcher(f fleet.File, configPath string) *fleet.Fetcher {
	cl := c.client(configPath)
	cl.Timeout = 3 * time.Second
	return &fleet.Fetcher{
		File:     f,
		CacheDir: fleet.DefaultCacheDir(fleet.Env(c.env)),
		Local: func(ctx context.Context) (proto.Status, error) {
			s, _, err := cl.Status(ctx)
			return s, err
		},
		SSH: c.env("PACE_FLEET_SSH"),
	}
}

func (c *cli) fleetStatus(args []string) int {
	fs := c.newFlags("fleet", fleetHelp)
	asJSON := fs.Bool("json", false, "print the view as JSON")
	verbose := fs.Bool("v", false, "add jobs, warnings and the biggest processes per machine")
	watch := fs.Bool("watch", false, "redraw until interrupted")
	every := fs.Duration("every", 5*time.Second, "redraw interval with --watch")
	cfg := fs.String("config", "", "config file (for the local socket path)")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace fleet: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *every < time.Second {
		fmt.Fprintln(c.stderr, "pace fleet: --every must be at least 1s")
		return exitUsage
	}
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	fe := c.fetcher(f, *cfg)
	show := func(ms []fleet.Machine) {
		if *asJSON {
			b, _ := json.Marshal(map[string]any{"v": 1, "ts": time.Now().UnixMilli(), "machines": ms})
			c.stdout.Write(append(b, '\n'))
			return
		}
		if *watch && isTerminal(c.stdout) {
			fmt.Fprint(c.stdout, "\033[H\033[2J")
		}
		fleet.RenderText(c.stdout, ms, time.Now(), *verbose)
		if len(f.Peers) == 0 && !*watch {
			fmt.Fprintf(c.stdout, "\nNo peers yet: add one with 'pace fleet add <name> <ssh-destination>' (stored in %s).\n", c.fleetPath())
		}
	}
	if !*watch {
		show(fe.Fetch(context.Background()))
		return exitOK
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if !*asJSON {
		show(fe.Cached()) // optimistic: last known state right away
	}
	for {
		ms := fe.Fetch(ctx)
		if ctx.Err() != nil {
			return exitOK // interrupted mid-round: the partial round is not the fleet's state
		}
		show(ms)
		select {
		case <-ctx.Done():
			return exitOK
		case <-time.After(*every):
		}
	}
}

func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (c *cli) fleetAdd(args []string) int {
	fs := c.newFlags("fleet add", fleetHelp)
	pace := fs.String("pace", "", "pace binary on the peer (default: PATH, ~/.local/bin, ~/bin, ~/go/bin, /opt/homebrew/bin, /usr/local/bin)")
	noCheck := fs.Bool("no-check", false, "do not contact the peer")
	container := fs.String("container", "", "the peer is this container on the ssh host (pace runs in it via docker exec)")
	containerUser := fs.String("container-user", "", "user to run pace as inside the container (docker exec -u)")
	pos, code, done := fs.parseInterspersed(args)
	if done {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr, "pace fleet add: want <name> <ssh-destination>")
		return exitUsage
	}
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	p := fleet.Peer{Name: pos[0], SSH: pos[1], Pace: *pace, Container: *container, ContainerUser: *containerUser}
	set := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	verb := "added"
	if i := f.Find(p.Name); i >= 0 {
		p.SSHOptions = f.Peers[i].SSHOptions
		// updating the destination keeps the settings not given again
		if !set["pace"] {
			p.Pace = f.Peers[i].Pace
		}
		if !set["container"] {
			p.Container = f.Peers[i].Container
		}
		if !set["container-user"] {
			p.ContainerUser = f.Peers[i].ContainerUser
		}
		f.Peers[i] = p
		verb = "updated"
	} else {
		f.Peers = append(f.Peers, p)
	}
	if err := f.Validate(); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet add: %v\n", err)
		return exitUsage
	}
	if p.Name == fleet.SelfName(f) {
		fmt.Fprintf(c.stderr, "pace fleet add: %q is this machine's name in the view; it is always shown (read via the socket)\n", p.Name)
		return exitUsage
	}
	if err := fleet.Save(c.fleetPath(), f); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet add: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(c.stdout, "%s %s (%s) in %s\n", verb, p.Name, p.SSH, c.fleetPath())
	if *noCheck {
		return exitOK
	}
	one := f
	one.Peers = []fleet.Peer{p}
	fe := c.fetcher(one, "")
	ms := fe.Fetch(context.Background())
	m := ms[len(ms)-1]
	switch m.State {
	case fleet.StateLive:
		fmt.Fprintf(c.stdout, "%s answers: pace %s, mode %s, pressure %s (%dms)\n", p.Name, m.Status.Version, m.Status.Mode, m.Status.Pressure, m.LatencyMS)
		return exitOK
	default:
		fmt.Fprintf(c.stdout, "%s does not answer yet (%s): %s\n", p.Name, m.State, m.Error)
		check := "pace status"
		if p.Container != "" {
			check = "docker exec"
			if p.ContainerUser != "" {
				check += " -u " + p.ContainerUser
			}
			check += " " + p.Container + " pace status"
		}
		fmt.Fprintf(c.stdout, "check: ssh -o BatchMode=yes %s %s  (ssh must work without a password or host-key prompt)\n", p.SSH, check)
		return exitFail
	}
}

func (c *cli) fleetRemove(args []string) int {
	fs := c.newFlags("fleet rm", fleetHelp)
	pos, code, done := fs.parseInterspersed(args)
	if done {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(c.stderr, "pace fleet rm: want <name>")
		return exitUsage
	}
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	i := f.Find(pos[0])
	if i < 0 {
		fmt.Fprintf(c.stderr, "pace fleet rm: no peer named %q\n", pos[0])
		return exitFail
	}
	f.Peers = append(f.Peers[:i], f.Peers[i+1:]...)
	if err := fleet.Save(c.fleetPath(), f); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet rm: %v\n", err)
		return exitFail
	}
	os.Remove(filepath.Join(fleet.DefaultCacheDir(fleet.Env(c.env)), pos[0]+".json"))
	fmt.Fprintf(c.stdout, "removed %s\n", pos[0])
	return exitOK
}

// fleetRename renames a peer (or this machine, when old is its name) and
// keeps its cached snapshot.
func (c *cli) fleetRename(args []string) int {
	fs := c.newFlags("fleet rename", fleetHelp)
	pos, code, done := fs.parseInterspersed(args)
	if done {
		return code
	}
	if len(pos) != 2 {
		fmt.Fprintln(c.stderr, "pace fleet rename: want <old-name> <new-name>")
		return exitUsage
	}
	oldName, newName := pos[0], pos[1]
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	if !fleet.ValidName(newName) {
		fmt.Fprintf(c.stderr, "pace fleet rename: %q: use letters, digits, '.', '_' or '-' (a leading '.' needs a letter or digit after it)\n", newName)
		return exitUsage
	}
	if f.Find(newName) >= 0 || newName == fleet.SelfName(f) {
		fmt.Fprintf(c.stderr, "pace fleet rename: %q is already in the fleet\n", newName)
		return exitUsage
	}
	switch i := f.Find(oldName); {
	case i >= 0:
		f.Peers[i].Name = newName
	case oldName == fleet.SelfName(f):
		f.Self = newName
	default:
		fmt.Fprintf(c.stderr, "pace fleet rename: no machine named %q ('pace fleet ls')\n", oldName)
		return exitFail
	}
	if err := f.Validate(); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet rename: %v\n", err)
		return exitUsage
	}
	if err := fleet.Save(c.fleetPath(), f); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet rename: %v\n", err)
		return exitFail
	}
	dir := fleet.DefaultCacheDir(fleet.Env(c.env))
	_ = os.Rename(filepath.Join(dir, oldName+".json"), filepath.Join(dir, newName+".json"))
	fmt.Fprintf(c.stdout, "renamed %s to %s\n", oldName, newName)
	return exitOK
}

func (c *cli) fleetSelf(args []string) int {
	fs := c.newFlags("fleet self", fleetHelp)
	pos, code, done := fs.parseInterspersed(args)
	if done {
		return code
	}
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	switch len(pos) {
	case 0:
		fmt.Fprintln(c.stdout, fleet.SelfName(f))
		return exitOK
	case 1:
	default:
		fmt.Fprintln(c.stderr, "pace fleet self: want at most one <name>")
		return exitUsage
	}
	if f.Find(pos[0]) >= 0 {
		fmt.Fprintf(c.stderr, "pace fleet self: %q is a peer here; remove it first ('pace fleet rm %s')\n", pos[0], pos[0])
		return exitUsage
	}
	old := fleet.SelfName(f)
	f.Self = pos[0]
	if err := f.Validate(); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet self: %v\n", err)
		return exitUsage
	}
	if err := fleet.Save(c.fleetPath(), f); err != nil {
		fmt.Fprintf(c.stderr, "pace fleet self: %v\n", err)
		return exitFail
	}
	if old != f.Self {
		os.Remove(filepath.Join(fleet.DefaultCacheDir(fleet.Env(c.env)), old+".json"))
	}
	fmt.Fprintf(c.stdout, "this machine is %s in the fleet view\n", f.Self)
	return exitOK
}

func (c *cli) fleetList(args []string) int {
	fs := c.newFlags("fleet ls", fleetHelp)
	if code, done := fs.parse(args); done {
		return code
	}
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	fmt.Fprintf(c.stdout, "%s (this machine, socket)\n", fleet.SelfName(f))
	for _, p := range f.Peers {
		line := p.Name + "  ssh " + p.SSH
		if p.Container != "" {
			line += "  container " + p.Container
			if p.ContainerUser != "" {
				line += " (user " + p.ContainerUser + ")"
			}
		}
		if p.Pace != "" {
			line += "  pace " + p.Pace
		}
		fmt.Fprintln(c.stdout, line)
	}
	return exitOK
}

func (c *cli) fleetServe(args []string) int {
	fs := c.newFlags("fleet serve", fleetHelp)
	listen := fs.String("listen", "127.0.0.1:7719", "address to serve on")
	every := fs.Duration("every", 10*time.Second, "how often to refresh the view")
	cfg := fs.String("config", "", "config file (for the local socket path)")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace fleet serve: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if *every < time.Second {
		fmt.Fprintln(c.stderr, "pace fleet serve: --every must be at least 1s")
		return exitUsage
	}
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace fleet serve: --listen %q: %v\n", *listen, err)
		return exitUsage
	}
	allInterfaces := "pace fleet serve: refusing to listen on all interfaces; give a loopback or Tailscale address"
	if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
		fmt.Fprintln(c.stderr, allInterfaces)
		return exitUsage
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(c.stderr, "pace fleet serve: %v\n", err)
		return exitFail
	}
	if ta, ok := ln.Addr().(*net.TCPAddr); !ok || ta.IP.IsUnspecified() {
		ln.Close() // a name such as "0" or "0x0" resolved to the wildcard address
		fmt.Fprintln(c.stderr, allInterfaces)
		return exitUsage
	}
	fe := c.fetcher(f, *cfg)
	var mu sync.Mutex
	view := fe.Cached()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		for {
			ms := fe.Fetch(ctx)
			if ctx.Err() != nil {
				return
			}
			mu.Lock()
			view = ms
			mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-time.After(*every):
			}
		}
	}()
	current := func() []fleet.Machine {
		mu.Lock()
		defer mu.Unlock()
		return view
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = fleet.RenderHTML(w, current(), time.Now(), *every)
	})
	mux.HandleFunc("GET /fleet.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"v": 1, "ts": time.Now().UnixMilli(), "machines": current()})
	})
	srv := &http.Server{Handler: hostGuard(host, mux), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Fprintf(c.stdout, "pace fleet: serving http://%s/ (and /fleet.json); Ctrl-C stops\n", ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(c.stderr, "pace fleet serve: %v\n", err)
		return exitFail
	}
	return exitOK
}

// hostGuard answers only requests addressed to this server by a name a
// DNS-rebinding page cannot control: an IP literal, localhost, the --listen
// host, a single-label name (a Tailscale MagicDNS short name such as "mini")
// or a *.ts.net name. A page that rebinds its own domain to 127.0.0.1 is
// refused, so it cannot read the fleet view through the user's browser. The
// port is not checked, so ssh -L forwarding to another port works.
func hostGuard(listenHost string, next http.Handler) http.Handler {
	listenHost = strings.Trim(listenHost, "[]")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			h = r.Host
		}
		h = strings.TrimSuffix(strings.Trim(h, "[]"), ".")
		ok := h != "" && (net.ParseIP(h) != nil || strings.EqualFold(h, "localhost") || strings.EqualFold(h, listenHost) ||
			!strings.Contains(h, ".") || strings.HasSuffix(strings.ToLower(h), ".ts.net"))
		if !ok {
			http.Error(w, "pace fleet: unexpected Host header", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}
