//go:build darwin

package wrapper

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY returns a pseudo-terminal pair (macOS ioctls).
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	fd := m.Fd()
	ioctl := func(req uintptr, arg uintptr) error {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, req, arg); e != 0 {
			return e
		}
		return nil
	}
	var name [128]byte
	if err := ioctl(unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatal(err)
	}
	if err := ioctl(unix.TIOCPTYUNLK, 0); err != nil {
		t.Fatal(err)
	}
	if err := ioctl(unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		t.Fatal(err)
	}
	n := strings.TrimRight(string(name[:]), "\x00")
	s, err := os.OpenFile(n, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); s.Close() })
	return m, s
}

type ptyReader struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (p *ptyReader) String() string { p.mu.Lock(); defer p.mu.Unlock(); return p.buf.String() }

func (p *ptyReader) waitFor(t *testing.T, sub string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(p.String(), sub) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; terminal shows %q", sub, p.String())
}

// startOnPTY runs the wrapper as the session leader of a new terminal, the
// way a login shell would run it in the foreground.
func startOnPTY(t *testing.T, e *env, argv []string) (*helper, *os.File, *ptyReader) {
	t.Helper()
	m, s := openPTY(t)
	h := startHelperIO(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000, Argv: argv}, []*os.File{s, s, s}, func(c *exec.Cmd) {
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	})
	s.Close()
	// Runs before the helper-kill cleanup (LIFO): a dying session leader
	// blocks in tty close while output is undrained, so close the master
	// first on failure paths.
	t.Cleanup(func() { m.Close() })
	pr := &ptyReader{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := m.Read(b)
			if n > 0 {
				pr.mu.Lock()
				pr.buf.Write(b[:n])
				pr.mu.Unlock()
			}
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	return h, m, pr
}

func TestPTYChildIsForeground(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	// The child reads the terminal: if it were not the foreground group it
	// would be stopped by SIGTTIN and this test would time out.
	h, m, pr := startOnPTY(t, e, sh(`echo "fg:$(ps -o pgid= -p $$ | tr -d ' '):$(ps -o tpgid= -p $$ | tr -d ' ')"; read line; echo "got:$line"`))
	pr.waitFor(t, "fg:", 10*time.Second)
	time.Sleep(50 * time.Millisecond)
	out := pr.String()
	i := strings.Index(out, "fg:")
	parts := strings.SplitN(strings.TrimSpace(strings.SplitN(out[i+3:], "\n", 2)[0]), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[0] != strings.TrimSpace(parts[1]) {
		t.Fatalf("child pgid/tpgid = %q; terminal: %q", parts, out)
	}
	m.Write([]byte("hello\n"))
	pr.waitFor(t, "got:hello", 10*time.Second)
	ws := h.wait(t, 10*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("status %v; terminal %q; stderr file %q", ws, pr.String(), h.stderr())
	}
}

func TestPTYCtrlCReachesChild(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	h, m, pr := startOnPTY(t, e, sh(`echo ready; exec sleep 30`))
	pr.waitFor(t, "ready", 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	m.Write([]byte{0x03}) // ^C: SIGINT to the terminal's foreground group
	ws := h.wait(t, 10*time.Second)
	if !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("wrapper: exited=%v code=%d sig=%v; want SIGINT; terminal %q", ws.Exited(), ws.ExitStatus(), ws.Signal(), pr.String())
	}
}

func TestPTYCtrlZDoesNotHang(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	// Our process group is orphaned here (no job-control shell), so the
	// wrapper's own SIGTSTP is discarded by the kernel; it must resume the
	// child instead of leaving it stopped forever.
	h, m, pr := startOnPTY(t, e, sh(`echo ready; sleep 0.5; echo after`))
	pr.waitFor(t, "ready", 10*time.Second)
	time.Sleep(100 * time.Millisecond)
	m.Write([]byte{0x1a}) // ^Z
	pr.waitFor(t, "after", 10*time.Second)
	ws := h.wait(t, 10*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("status %v; terminal %q", ws, pr.String())
	}
}
