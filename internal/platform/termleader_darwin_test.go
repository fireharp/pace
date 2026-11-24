package platform

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openTestPTY returns the slave side of a new pseudo-terminal.
func openTestPTY(t *testing.T) *os.File {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	ioctl := func(req uintptr, arg uintptr) error {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, m.Fd(), req, arg); e != 0 {
			return e
		}
		return nil
	}
	var name [128]byte
	for _, req := range []uintptr{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err := ioctl(req, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := ioctl(unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		t.Fatal(err)
	}
	s, err := os.OpenFile(strings.TrimRight(string(name[:]), "\x00"), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func startSleep(t *testing.T, attr *syscall.SysProcAttr, stdin *os.File) int {
	t.Helper()
	c := exec.Command("/bin/sleep", "30")
	c.Stdin, c.SysProcAttr = stdin, attr
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c.Process.Pid
}

// Finding (e): a terminal's session leader (a login shell) is recognised,
// and neither a session leader without a terminal (a launchd-started app)
// nor an ordinary process is.
func TestTermLeaderDarwin(t *testing.T) {
	b := newTestBackend(t)
	tty := startSleep(t, &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}, openTestPTY(t))
	noTTY := startSleep(t, &syscall.SysProcAttr{Setsid: true}, nil)
	plain := startSleep(t, &syscall.SysProcAttr{Setpgid: true}, nil)
	for _, c := range []struct {
		name string
		pid  int
		want bool
	}{{"session leader with a terminal", tty, true}, {"session leader without one", noTTY, false}, {"group leader", plain, false}} {
		p, ok := findProc(t, b, c.pid)
		if !ok {
			t.Fatalf("%s: pid %d not listed", c.name, c.pid)
		}
		if p.TermLeader != c.want {
			t.Errorf("%s: TermLeader=%v want %v", c.name, p.TermLeader, c.want)
		}
	}
}

// Terminal.app and iTerm2 start the login shell through root's
// /usr/bin/login, which leads the session: the user's -zsh only leads its
// own group, as login's child, and login is not in the user's listing. It is
// a terminal shell too. A job of a user's shell (its own group on the same
// terminal, child of the listed shell) is not.
func TestTermShellUnderLogin(t *testing.T) {
	sid := func(s int) func(int) (int, error) { return func(int) (int, error) { return s, nil } }
	for _, c := range []struct {
		name                       string
		pid, ppid, pgid            int
		sleader, tty, parentListed bool
		getsid                     func(int) (int, error)
		want                       bool
	}{
		{"session leader with a terminal", 10, 5, 10, true, true, true, sid(10), true},
		{"session leader without one", 10, 1, 10, true, false, false, sid(10), false},
		{"-zsh under root login", 57536, 57527, 57536, false, true, false, sid(57527), true},
		{"job of a session-leader shell", 70, 60, 70, false, true, true, sid(60), false},
		{"job of the -zsh under login", 71, 57536, 71, false, true, true, sid(57527), false},
		{"group member, not leader", 72, 57527, 57536, false, true, false, sid(57527), false},
		{"parent not the session leader", 73, 40, 73, false, true, false, sid(39), false},
		{"getsid fails", 74, 40, 74, false, true, false, func(int) (int, error) { return 0, unix.ESRCH }, false},
		{"no terminal", 75, 57527, 75, false, false, false, sid(57527), false},
	} {
		if got := termShell(c.pid, c.ppid, c.pgid, c.sleader, c.tty, c.parentListed, c.getsid); got != c.want {
			t.Errorf("%s: termShell=%v want %v", c.name, got, c.want)
		}
	}

	// Live: a shell leading a pty session, with a job of its own group on
	// that terminal (set -m). The real getsid sees the job in the shell's
	// session: unlisted parent (login) makes it a terminal shell, a listed
	// one (the user's shell) does not; the listing marks only the shell.
	c := exec.Command("/bin/sh", "-c", "set -m; /bin/sleep 30 & echo $!; wait")
	c.Stdin = openTestPTY(t)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	out, err := c.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	var job int
	t.Cleanup(func() {
		if job > 0 {
			_ = syscall.Kill(job, syscall.SIGKILL)
		}
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	line := make([]byte, 32)
	n, _ := out.Read(line)
	job, err = strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil || job <= 1 {
		t.Fatalf("job pid %q: %v", line[:n], err)
	}
	if pg, err := syscall.Getpgid(job); err != nil || pg != job {
		t.Skipf("sh -m did not give the job its own group (pgid %d, %v)", pg, err)
	}
	sh := c.Process.Pid
	if !termShell(job, sh, job, false, true, false, unix.Getsid) {
		t.Error("group leader whose unlisted parent leads the session: not a terminal shell")
	}
	if termShell(job, sh, job, false, true, true, unix.Getsid) {
		t.Error("job of a listed shell taken for a terminal shell")
	}
	b := newTestBackend(t)
	if p, ok := findProc(t, b, sh); !ok || !p.TermLeader {
		t.Errorf("session-leader shell: %+v listed=%v", p, ok)
	}
	if p, ok := findProc(t, b, job); !ok || p.TermLeader {
		t.Errorf("job of the shell: %+v listed=%v", p, ok)
	}
}
