// Package fleet gives one view of the pace daemons on several machines.
//
// It is optional and local-first: every daemon governs its own machine and
// never depends on the fleet. A fleet view is a client-side read: the local
// daemon through its socket, every peer through ssh (`pace status --json`),
// which works over Tailscale or a LAN without opening a port or adding
// credentials. The last good snapshot of each peer is cached, so a peer
// that cannot be reached is shown as stale with its age (no connection is
// not "out of service"), and "pace is down there" is told apart from "the
// machine cannot be reached".
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/fireharp/pace/internal/proto"
)

// Peer is another machine running pace.
type Peer struct {
	Name string `toml:"name" json:"name"`
	// SSH is the ssh destination: a host alias, user@host, a Tailscale
	// MagicDNS name or IP.
	SSH string `toml:"ssh" json:"ssh"`
	// Pace is the pace binary on the peer ("" = pace on PATH, ~/.local/bin,
	// ~/bin, ~/go/bin, /opt/homebrew/bin, /usr/local/bin). A leading ~/ is
	// the remote home.
	Pace string `toml:"pace,omitempty" json:"pace,omitempty"`
	// SSHOptions are extra ssh arguments (e.g. ["-p", "2222"]).
	SSHOptions []string `toml:"ssh_options,omitempty" json:"ssh_options,omitempty"`
	// Container makes the peer a container on the SSH host: pace runs inside
	// it through `docker exec`. Pace is then the
	// path inside the container ("" = pace on the container's PATH).
	Container string `toml:"container,omitempty" json:"container,omitempty"`
	// ContainerUser is the user pace runs as inside the container (docker
	// exec -u), the one whose daemon it should reach ("" = the image's).
	ContainerUser string `toml:"container_user,omitempty" json:"container_user,omitempty"`
}

var containerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Command is the command line run on the peer's SSH host for `pace ARGS...`.
func (p Peer) Command(args ...string) string {
	if p.Container == "" {
		return RemoteCommand(p.Pace, args...)
	}
	cmd := []string{"docker", "exec"}
	if p.ContainerUser != "" {
		cmd = append(cmd, "-u", p.ContainerUser)
	}
	pace := p.Pace
	if pace == "" {
		pace = "pace"
	}
	cmd = append(cmd, p.Container, pace)
	q := make([]string, 0, len(cmd)+len(args))
	for _, a := range append(cmd, args...) {
		q = append(q, shellQuote(a))
	}
	// sshd's PATH is minimal: add where Docker Desktop, OrbStack, Homebrew
	// and rootless docker put the docker CLI, and say so if it is missing
	// (else "not found" would read as pace missing in the container).
	body := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.orbstack/bin:$HOME/.docker/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; export PATH; ` +
		`command -v docker >/dev/null 2>&1 || { echo "` + noDockerMsg + `" >&2; exit 127; }; exec ` + strings.Join(q, " ")
	return "sh -c " + shellQuote(body)
}

// noDockerMsg marks a container peer whose ssh host has no docker CLI.
const noDockerMsg = "pace-fleet: docker not found on the ssh host"

// File is the fleet file (fleet.toml).
type File struct {
	// Self names this machine in the view ("" = the short hostname). A
	// peer with the same name is this machine and is read via the socket.
	Self string `toml:"self,omitempty"`
	// Timeout bounds one peer's fetch ("" = 8s).
	Timeout string `toml:"timeout,omitempty"`
	Peers   []Peer `toml:"peer"`
}

// nameRE: letters, digits, '.', '_' and '-'; a leading '.' is fine when a
// letter or digit follows (".53"), so "." and ".." can never be names.
var nameRE = regexp.MustCompile(`^\.?[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// ValidName reports whether s can name a machine (also a cache file name).
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Validate checks names, destinations and the timeout.
func (f *File) Validate() error {
	seen := map[string]bool{}
	for _, p := range f.Peers {
		if !ValidName(p.Name) {
			return fmt.Errorf("peer name %q: use letters, digits, '.', '_' or '-'", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("peer %q is listed twice", p.Name)
		}
		seen[p.Name] = true
		if p.SSH == "" || strings.HasPrefix(p.SSH, "-") || strings.ContainsAny(p.SSH, " \t\n") {
			return fmt.Errorf("peer %q: ssh destination %q is not valid", p.Name, p.SSH)
		}
		if p.Container != "" {
			if !containerRE.MatchString(p.Container) {
				return fmt.Errorf("peer %q: container %q is not a container name", p.Name, p.Container)
			}
			if p.ContainerUser != "" && !containerRE.MatchString(p.ContainerUser) {
				return fmt.Errorf("peer %q: container_user %q is not a user name", p.Name, p.ContainerUser)
			}
			if p.Pace != "" && !strings.HasPrefix(p.Pace, "/") {
				return fmt.Errorf("peer %q: inside a container the pace path must be absolute (got %q)", p.Name, p.Pace)
			}
		} else if p.ContainerUser != "" {
			return fmt.Errorf("peer %q: container_user needs container", p.Name)
		}
		if strings.ContainsAny(p.Pace, "!\n") {
			// csh and tcsh expand ! even inside single quotes
			return fmt.Errorf("peer %q: pace path %q may not contain '!' or a newline", p.Name, p.Pace)
		}
	}
	if f.Self != "" && !ValidName(f.Self) {
		return fmt.Errorf("self name %q: use letters, digits, '.', '_' or '-'", f.Self)
	}
	if _, err := f.timeout(); err != nil {
		return err
	}
	return nil
}

func (f *File) timeout() (time.Duration, error) {
	if f.Timeout == "" {
		return 8 * time.Second, nil
	}
	d, err := time.ParseDuration(f.Timeout)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("timeout %q: want a positive duration such as \"8s\"", f.Timeout)
	}
	return d, nil
}

// Find returns the index of the peer named name, or -1.
func (f *File) Find(name string) int {
	for i, p := range f.Peers {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// Load reads the fleet file. A missing file is an empty fleet.
func Load(path string) (File, error) {
	var f File
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	md, err := toml.Decode(string(b), &f)
	if err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		return f, fmt.Errorf("%s: unknown key %q", path, und[0].String())
	}
	if err := f.Validate(); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

const fileHeader = `# pace fleet: the machines 'pace fleet' shows next to this one.
# Managed by 'pace fleet add/rm'; editing by hand is fine.
#   self    = "name"   this machine's name in the view (default: short hostname)
#   timeout = "8s"     how long one peer may take to answer
# [[peer]] name, ssh (destination: alias, user@host, Tailscale name or IP),
#          pace (pace path on the peer, optional), ssh_options (optional list)

`

// Save writes the fleet file atomically (0600).
func Save(path string, f File) error {
	if err := f.Validate(); err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString(fileHeader)
	if err := toml.NewEncoder(&b).Encode(f); err != nil {
		return err
	}
	return writeAtomic(path, b.Bytes(), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Chmod(mode)
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
	}
	return werr
}

// Env looks up environment variables.
type Env func(string) string

func home(env Env) string {
	if h := env("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// DefaultPath is $PACE_FLEET, else fleet.toml next to the default config
// ($XDG_CONFIG_HOME/pace or ~/.config/pace).
func DefaultPath(env Env) string {
	if p := env("PACE_FLEET"); p != "" {
		return p
	}
	if x := env("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "pace", "fleet.toml")
	}
	return filepath.Join(home(env), ".config", "pace", "fleet.toml")
}

// DefaultCacheDir holds the last good snapshot of each machine:
// $PACE_HOME/cache/fleet, else ~/Library/Caches/pace/fleet (macOS), else
// $XDG_CACHE_HOME/pace/fleet or ~/.cache/pace/fleet.
func DefaultCacheDir(env Env) string {
	if h := env("PACE_HOME"); h != "" {
		return filepath.Join(h, "cache", "fleet")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home(env), "Library", "Caches", "pace", "fleet")
	}
	if x := env("XDG_CACHE_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "pace", "fleet")
	}
	return filepath.Join(home(env), ".cache", "pace", "fleet")
}

// SelfName is f.Self, else the short hostname.
func SelfName(f File) string {
	if f.Self != "" {
		return f.Self
	}
	h, _ := os.Hostname()
	h = strings.TrimSuffix(h, ".local")
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	h = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' {
			return r
		}
		return '-'
	}, h)
	if !ValidName(h) {
		return "this-machine"
	}
	return h
}

// Machine states.
const (
	StateLive        = "live"        // answered this round
	StateStale       = "stale"       // not reachable now; last good snapshot shown
	StateUnreachable = "unreachable" // not reachable now and never seen
	StatePaceDown    = "pace-down"   // reachable, pace installed, daemon not answering
	StateNoPace      = "no-pace"     // reachable, no pace binary found
	StateError       = "error"       // reachable, unexpected answer
)

// Machine is one row of the fleet view.
type Machine struct {
	Name      string        `json:"name"`
	Self      bool          `json:"self,omitempty"`
	Via       string        `json:"via"` // socket|ssh
	Target    string        `json:"target,omitempty"`
	State     string        `json:"state"`
	Error     string        `json:"error,omitempty"`
	SeenMS    int64         `json:"seen_ms,omitempty"`    // when Status was taken (0 = never)
	LatencyMS int64         `json:"latency_ms,omitempty"` // this round's fetch time
	Status    *proto.Status `json:"status,omitempty"`     // live or last good snapshot
}

// Fresh reports whether Status was taken this round.
func (m Machine) Fresh() bool { return m.State == StateLive }

// Runner runs a command and returns its stdout, stderr and exit code
// (-1 when it did not exit normally, e.g. killed at the deadline).
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, code int, err error)

// ExecRunner runs commands with os/exec. WaitDelay keeps a child that
// leaves a pipe open (an ssh ControlPersist master) from blocking Wait.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.Stdin = nil
	cmd.WaitDelay = 500 * time.Millisecond
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee):
			code, err = ee.ExitCode(), nil
		case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
			// it exited; something it started still holds a pipe
			code, err = cmd.ProcessState.ExitCode(), nil
		default:
			code = -1
		}
	}
	if ctx.Err() != nil {
		code, err = -1, ctx.Err()
	}
	return out.Bytes(), errb.Bytes(), code, err
}

// Fetcher builds a fleet view.
type Fetcher struct {
	File     File
	CacheDir string // "" = no cache
	// Local returns this machine's status (the daemon socket).
	Local func(ctx context.Context) (proto.Status, error)
	Run   Runner // nil = ExecRunner
	Now   func() time.Time
	SSH   string // ssh binary ("" = "ssh")
}

func (f *Fetcher) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// remoteCommand is the command line run on a peer. sshd hands it to the
// user's login shell, which may be zsh, bash, fish or csh, so the actual
// work is one single-quoted argument to sh -c: every one of those shells
// passes a single-quoted word through unchanged.
func remoteCommand(pace string) string { return RemoteCommand(pace, "status", "--json") }

// RemoteCommand is the command line that runs `pace ARGS...` on a peer
// (pace = the peer's configured binary, "" = search the usual places).
func RemoteCommand(pace string, args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	rest := strings.Join(q, " ")
	var body string
	switch {
	case pace == "":
		body = `PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; export PATH; exec pace ` + rest
	case strings.HasPrefix(pace, "~/"):
		body = `exec "$HOME"/` + shellQuote(pace[2:]) + " " + rest
	default:
		body = "exec " + shellQuote(pace) + " " + rest
	}
	return "sh -c " + shellQuote(body)
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./+:@%=,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// SSHArgs returns the ssh arguments for fetching p's status.
func SSHArgs(p Peer, timeout time.Duration) []string {
	ct := int(timeout.Seconds() / 2)
	if ct < 1 {
		ct = 1
	}
	args := []string{
		"-o", "BatchMode=yes", // never prompt: no password, no host-key question
		"-o", fmt.Sprintf("ConnectTimeout=%d", ct),
		"-o", "ServerAliveInterval=2", "-o", "ServerAliveCountMax=2",
		"-o", "LogLevel=ERROR",
		"-T",
	}
	args = append(args, p.SSHOptions...)
	return append(args, "--", p.SSH, p.Command("status", "--json"))
}

type cacheEntry struct {
	V      int          `json:"v"`
	SeenMS int64        `json:"seen_ms"`
	Status proto.Status `json:"status"`
}

func (f *Fetcher) cachePath(name string) string {
	return filepath.Join(f.CacheDir, name+".json")
}

func (f *Fetcher) readCache(name string) (cacheEntry, bool) {
	if f.CacheDir == "" {
		return cacheEntry{}, false
	}
	b, err := os.ReadFile(f.cachePath(name))
	if err != nil {
		return cacheEntry{}, false
	}
	var e cacheEntry
	if json.Unmarshal(b, &e) != nil || e.SeenMS == 0 {
		return cacheEntry{}, false
	}
	return e, true
}

func (f *Fetcher) writeCache(name string, seen int64, s proto.Status) {
	if f.CacheDir == "" {
		return
	}
	b, err := json.Marshal(cacheEntry{V: 1, SeenMS: seen, Status: s})
	if err == nil {
		_ = writeAtomic(f.cachePath(name), b, 0o600)
	}
}

// Cached returns the view from the cache alone (no network): every
// machine with a snapshot is stale, the rest unreachable. It is what a
// watcher shows before the first round answers.
func (f *Fetcher) Cached() []Machine {
	self := SelfName(f.File)
	ms := []Machine{f.fromCache(Machine{Name: self, Self: true, Via: "socket"}, "not fetched yet")}
	for _, p := range f.File.Peers {
		if p.Name == self {
			continue
		}
		ms = append(ms, f.fromCache(Machine{Name: p.Name, Via: "ssh", Target: p.SSH}, "not fetched yet"))
	}
	return ms
}

func (f *Fetcher) fromCache(m Machine, why string) Machine {
	m.Error = why
	if e, ok := f.readCache(m.Name); ok {
		s := e.Status
		m.Status, m.SeenMS = &s, e.SeenMS
		if m.State == "" {
			m.State = StateStale
		}
		return m
	}
	if m.State == "" {
		m.State = StateUnreachable
	}
	return m
}

// Fetch builds the view: this machine and every peer, in parallel, each
// bounded by the fleet timeout. It never fails as a whole: a machine that
// does not answer is reported with its last good snapshot.
func (f *Fetcher) Fetch(ctx context.Context) []Machine {
	timeout, err := f.File.timeout()
	if err != nil {
		timeout = 8 * time.Second
	}
	self := SelfName(f.File)
	var peers []Peer
	for _, p := range f.File.Peers {
		if p.Name != self {
			peers = append(peers, p)
		}
	}
	out := make([]Machine, 1+len(peers))
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out[0] = f.fetchLocal(ctx, self, timeout)
	}()
	for i, p := range peers {
		wg.Add(1)
		go func(i int, p Peer) {
			defer wg.Done()
			out[i+1] = f.fetchPeer(ctx, p, timeout)
		}(i, p)
	}
	wg.Wait()
	return out
}

func (f *Fetcher) fetchLocal(ctx context.Context, name string, timeout time.Duration) Machine {
	m := Machine{Name: name, Self: true, Via: "socket"}
	if f.Local == nil {
		m.State = StatePaceDown
		return f.fromCache(m, "no local daemon client")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t0 := f.now()
	s, err := f.Local(cctx)
	m.LatencyMS = f.now().Sub(t0).Milliseconds()
	if err != nil {
		m.State = StatePaceDown
		return f.fromCache(m, "daemon not answering: "+err.Error())
	}
	seen := f.now().UnixMilli()
	m.State, m.Status, m.SeenMS = StateLive, &s, seen
	f.writeCache(name, seen, s)
	return m
}

func (f *Fetcher) fetchPeer(ctx context.Context, p Peer, timeout time.Duration) Machine {
	m := Machine{Name: p.Name, Via: "ssh", Target: p.SSH}
	run := f.Run
	if run == nil {
		run = ExecRunner
	}
	ssh := f.SSH
	if ssh == "" {
		ssh = "ssh"
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t0 := f.now()
	stdout, stderr, code, err := run(cctx, ssh, SSHArgs(p, timeout)...)
	m.LatencyMS = f.now().Sub(t0).Milliseconds()
	msg := lastLine(stderr)
	switch {
	case err != nil && cctx.Err() != nil:
		return f.fromCache(m, fmt.Sprintf("no answer within %s", timeout))
	case err != nil:
		return f.fromCache(m, err.Error())
	case code == 255:
		if msg == "" {
			msg = "ssh failed"
		}
		if !strings.HasPrefix(msg, "ssh:") {
			msg = "ssh: " + msg
		}
		return f.fromCache(m, msg)
	case code == 127 && p.Container != "" && strings.Contains(string(stderr), noDockerMsg):
		m.State = StateError
		return f.fromCache(m, "docker not found on "+p.SSH+" (the container peer runs docker exec there)")
	case code == 127 && p.Container != "":
		m.State = StateNoPace
		return f.fromCache(m, "pace not found in container "+p.Container+" (install it there, or set pace = \"/path/in/container\")")
	case code == 127:
		m.State = StateNoPace
		return f.fromCache(m, "pace not found on the peer (install it, or set pace = \"/path/to/pace\")")
	case code == 3:
		m.State = StatePaceDown
		if msg == "" {
			msg = "daemon unreachable"
		}
		return f.fromCache(m, "daemon not answering: "+msg)
	case code != 0:
		m.State = StateError
		what := "pace status"
		if p.Container != "" {
			what = "docker exec " + p.Container
		}
		return f.fromCache(m, fmt.Sprintf("%s exited %d: %s", what, code, msg))
	}
	s, ok := parseStatus(stdout)
	if !ok {
		m.State = StateError
		return f.fromCache(m, "unexpected output from pace status --json")
	}
	seen := f.now().UnixMilli()
	m.State, m.Status, m.SeenMS = StateLive, &s, seen
	f.writeCache(p.Name, seen, s)
	return m
}

// parseStatus reads pace status --json output. A login shell may print a
// banner (motd, rc-file echo) before it, so the last line that holds a JSON
// object is used when the whole output is not one.
func parseStatus(out []byte) (proto.Status, bool) {
	var s proto.Status
	if json.Unmarshal(bytes.TrimSpace(out), &s) == nil && s.V != 0 {
		return s, true
	}
	lines := bytes.Split(out, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		l := bytes.TrimSpace(lines[i])
		if len(l) == 0 || l[0] != '{' {
			continue
		}
		s = proto.Status{}
		if json.Unmarshal(l, &s) == nil && s.V != 0 {
			return s, true
		}
		break
	}
	return proto.Status{}, false
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			if len(l) > 200 {
				l = l[:200] + "..."
			}
			return strings.TrimRight(l, ".")
		}
	}
	return ""
}

// Summary is the fleet in one line per state.
func Summary(ms []Machine) string {
	n := map[string]int{}
	for _, m := range ms {
		n[m.State]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n[k]))
	}
	return strings.Join(parts, " ")
}
