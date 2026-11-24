//go:build darwin || linux

package wrapper

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// control runs fn on f's descriptor without switching it to blocking mode
// (as File.Fd would).
func control(f *os.File, fn func(fd int) error) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := rc.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}

// isTTY reports whether f is a terminal.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	return control(f, func(fd int) error {
		_, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
		return err
	}) == nil
}

// tcgetpgrp returns the foreground process group of the terminal f.
func tcgetpgrp(f *os.File) (int, error) {
	var pg int
	err := control(f, func(fd int) error {
		v, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		pg = v
		return err
	})
	return pg, err
}

// tcsetpgrp makes pgid the terminal's foreground group. SIGTTOU is ignored
// around the call: a background process changing the foreground group is
// otherwise stopped by it.
func tcsetpgrp(f *os.File, pgid int) error {
	if !signal.Ignored(syscall.SIGTTOU) {
		signal.Ignore(syscall.SIGTTOU)
		defer signal.Reset(syscall.SIGTTOU)
	}
	return control(f, func(fd int) error {
		return unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid)
	})
}
