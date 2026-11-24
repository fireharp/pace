//go:build darwin

package wrapper

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// #23: with a controlling terminal but stdio redirected (`pace run -- git
// push >log 2>&1 </dev/null` where ssh prompts on /dev/tty), the child gets
// the terminal's foreground and its /dev/tty read works instead of being
// stopped by SIGTTIN with the wrapper waiting forever.
func TestDevTTYWithRedirectedStdio(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	m, s := openPTY(t)
	got := filepath.Join(e.dir, "got")
	h := startHelperIO(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000, Ctty: true,
		Argv: sh(`read x </dev/tty; echo "got:$x" > ` + got)}, nil, func(c *exec.Cmd) {
		// Session leader with the pty as controlling terminal on fd 3 only:
		// fds 0-2 are /dev/null and files.
		c.ExtraFiles = []*os.File{s}
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	})
	s.Close()
	t.Cleanup(func() { m.Close() }) // before the helper-kill cleanup (LIFO)
	go func() {
		b := make([]byte, 4096)
		for {
			_, err := m.Read(b) // keep the terminal drained
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	m.Write([]byte("hello\n"))
	waitContent(t, got, "got:hello", 10*time.Second)
	ws := h.wait(t, 10*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("status %v; stderr %q", ws, h.stderr())
	}
}

// startNonLeader runs the wrapper the way a script or Makefile recipe
// does: as a child of a non-job-control sh, sharing its process group,
// which is the foreground group of a pty that is reachable only as
// /dev/tty (stdio is redirected). It returns the helper (waiting on sh)
// and the pty master, kept drained.
func startNonLeader(t *testing.T, e *env, argv []string, raw bool) (*helper, *os.File) {
	t.Helper()
	m, s := openPTY(t)
	if raw {
		tio, err := unix.IoctlGetTermios(int(s.Fd()), unix.TIOCGETA)
		if err != nil {
			t.Fatal(err)
		}
		tio.Lflag &^= unix.ICANON | unix.ECHO
		if err := unix.IoctlSetTermios(int(s.Fd()), unix.TIOCSETA, tio); err != nil {
			t.Fatal(err)
		}
	}
	h := startHelperIO(t, e.dir, helperSpec{Socket: e.sock, WaitMS: 2000, Ctty: true, Argv: argv}, nil, func(c *exec.Cmd) {
		bin := c.Path
		c.Path = "/bin/sh"
		// "; exit" keeps sh from exec'ing the wrapper in its place.
		c.Args = []string{"sh", "-c", `"$0" '-test.run=^$'; exit $?`, bin}
		c.ExtraFiles = []*os.File{s}
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 3}
	})
	s.Close()
	t.Cleanup(func() { m.Close() })
	go func() {
		b := make([]byte, 4096)
		for {
			_, err := m.Read(b)
			if errors.Is(err, syscall.EAGAIN) {
				time.Sleep(5 * time.Millisecond)
				continue
			}
			if err != nil {
				return
			}
		}
	}()
	return h, m
}

// #23, wrapper not leading its process group (run from a script): a
// /dev/tty prompt of the child still gets the terminal when it needs it.
func TestDevTTYNotGroupLeader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	got, info := filepath.Join(e.dir, "got"), filepath.Join(e.dir, "info")
	h, m := startNonLeader(t, e, sh(`echo "$(ps -o pgid= -p $PPID) $PPID" > `+info+
		`; read x </dev/tty; echo "got:$x" > `+got), false)
	waitContent(t, info, " ", 10*time.Second)
	b, _ := os.ReadFile(info)
	if f := strings.Fields(string(b)); len(f) != 2 || f[0] == f[1] {
		t.Fatalf("wrapper leads its group (%q): the test does not test the case", b)
	}
	time.Sleep(500 * time.Millisecond)
	m.Write([]byte("hello\n"))
	waitContent(t, got, "got:hello", 10*time.Second)
	ws := h.wait(t, 10*time.Second)
	if !ws.Exited() || ws.ExitStatus() != 0 {
		t.Fatalf("status %v; stderr %q", ws, h.stderr())
	}
	if strings.Contains(h.stderr(), "pace: ") {
		t.Fatalf("unexpected pace lines: %q", h.stderr())
	}
}

// #23: when another program drives the terminal in raw mode (an agent's
// TUI sharing our group), the child is not handed the terminal; the stop
// is reported, the wrapper does not stop itself, and a signal still ends
// the job.
func TestDevTTYRawTerminalReported(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	newFake(t, e.sock).start()
	ppid := filepath.Join(e.dir, "ppid")
	h, _ := startNonLeader(t, e, sh(`echo $PPID > `+ppid+`; read x </dev/tty; echo must-not-read`), true)
	wpid := readPID(t, ppid)
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(h.stderr(), "waiting for terminal input") {
		if time.Now().After(deadline) {
			t.Fatalf("no report of the stop; stderr %q", h.stderr())
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if procStopped(wpid) {
		t.Fatal("the wrapper stopped itself")
	}
	if n := strings.Count(h.stderr(), "waiting for terminal"); n != 1 {
		t.Fatalf("reported %d times: %q", n, h.stderr())
	}
	syscall.Kill(wpid, syscall.SIGTERM)
	ws := h.wait(t, 10*time.Second)
	if ws.Exited() && ws.ExitStatus() == 0 {
		t.Fatalf("status %v; stderr %q", ws, h.stderr())
	}
}
