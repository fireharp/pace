package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// listen takes the daemon locks, handles a stale socket and binds.
//
//   - the socket path must fit sun_path (<= 103 bytes, measured on macOS);
//   - a missing parent dir is created 0700; an existing one is never
//     chmodded but must be ours and not group/world-writable (privateDir);
//   - <home>/pace.lock and <socket dir>/pace.lock are flock'ed for the
//     daemon's life: a second daemon fails even if it races the first
//     one's bind, and two daemons never share one home (state.json, logs)
//     whatever sockets they use;
//   - an existing socket that answers a dial means another daemon is
//     running; one that does not is stale and removed (a non-socket file at
//     the path is never removed);
//   - the socket is chmod 0600 and every accepted connection's peer uid is
//     checked against ours.
func (d *daemon) listen() (net.Listener, error) {
	p := d.sockPath
	if len(p) > maxSocketPath {
		return nil, fmt.Errorf("pace daemon: socket path is %d bytes, the limit is %d: %s (set PACE_SOCK to a shorter path)", len(p), maxSocketPath, p)
	}
	dir := filepath.Dir(p)
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	if d.home != "" {
		if err := d.lockDir(d.home); err != nil {
			return nil, err
		}
	}
	if err := d.lockDir(dir); err != nil {
		d.unlock()
		return nil, err
	}

	if err := removeStaleSocket(p); err != nil {
		d.unlock()
		return nil, err
	}
	ln, err := net.Listen("unix", p)
	if err != nil {
		d.unlock()
		return nil, fmt.Errorf("pace daemon: bind %s: %w", p, err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		ln.Close()
		d.unlock()
		return nil, fmt.Errorf("pace daemon: chmod socket: %w", err)
	}
	return &credListener{Listener: ln, uid: d.allowUID, reject: d.fault}, nil
}

// privateDir creates dir 0700 if missing. An existing dir is checked, never
// changed (it may be $HOME or another directory pace does not own): it must
// be ours and not writable by group or others, or another user could
// replace the socket. Reading it is harmless: the socket is 0600 and peer
// uids are checked.
func privateDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("pace daemon: socket dir: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("pace daemon: socket dir: %w", err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("pace daemon: socket dir %s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("pace daemon: socket dir %s is owned by uid %d, not by us; set PACE_SOCK to a path in a private directory", dir, st.Uid)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return fmt.Errorf("pace daemon: socket dir %s is writable by group or others (mode %04o); set PACE_SOCK to a path in a private directory", dir, perm)
	}
	return nil
}

// lockDir takes an exclusive flock on <dir>/pace.lock for the daemon's life
// (skipped when this daemon already holds it).
func (d *daemon) lockDir(dir string) error {
	path := filepath.Join(dir, "pace.lock")
	for _, lf := range d.lockFiles {
		if lf.Name() == path {
			return nil
		}
	}
	lf, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("pace daemon: lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("pace daemon: another pace daemon is running (lock %s held)", path)
		}
		return fmt.Errorf("pace daemon: lock: %w", err)
	}
	d.lockFiles = append(d.lockFiles, lf)
	return nil
}

func (d *daemon) unlock() {
	for _, lf := range d.lockFiles {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	}
	d.lockFiles = nil
}

// removeStaleSocket returns an error if a live daemon answers on p, removes
// a dead socket file, and refuses to touch anything that is not a socket.
func removeStaleSocket(p string) error {
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pace daemon: stat socket: %w", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("pace daemon: %s exists and is not a socket; refusing to remove it", p)
	}
	c, err := net.DialTimeout("unix", p, 300*time.Millisecond)
	if err == nil {
		c.Close()
		return fmt.Errorf("pace daemon: another pace daemon is running (socket %s answers)", p)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("pace daemon: remove stale socket: %w", err)
	}
	return nil
}

// peerCred is the credential of the process on the other end of a conn.
type peerCred struct {
	Known bool // false when the OS gives no peer credentials
	UID   int
	PID   int
}

type ctxKey int

const credKey ctxKey = 1

// credListener rejects connections from other users at accept time.
type credListener struct {
	net.Listener
	uid    int
	reject func(kind, msg string)
}

type credConn struct {
	net.Conn
	cred peerCred
}

func (l *credListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		pc, err := connPeerCred(c)
		if err != nil {
			l.reject("peer_cred", "peer credentials unavailable: "+err.Error())
			c.Close()
			continue
		}
		if pc.Known && pc.UID != l.uid {
			l.reject("peer_cred", fmt.Sprintf("rejected connection from uid %d pid %d", pc.UID, pc.PID))
			c.Close()
			continue
		}
		return &credConn{Conn: c, cred: pc}, nil
	}
}

// connPeerCred reads the peer credentials of a unix conn.
func connPeerCred(c net.Conn) (peerCred, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peerCred{}, fmt.Errorf("not a unix connection (%T)", c)
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return peerCred{}, err
	}
	var pc peerCred
	var cerr error
	if err := rc.Control(func(fd uintptr) { pc, cerr = getPeerCred(int(fd)) }); err != nil {
		return peerCred{}, err
	}
	return pc, cerr
}

func connContext(ctx context.Context, c net.Conn) context.Context {
	if cc, ok := c.(*credConn); ok {
		return context.WithValue(ctx, credKey, cc.cred)
	}
	return ctx
}

// peerFrom returns the peer credentials stashed by ConnContext.
func peerFrom(ctx context.Context) (peerCred, bool) {
	pc, ok := ctx.Value(credKey).(peerCred)
	return pc, ok
}
