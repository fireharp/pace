package wrapper

import (
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// raiseSignal makes the wrapper die of sig, so that its caller sees exactly
// the status it would have seen without the wrapper (measured: bash/zsh then
// report the same $? and scripts stop on SIGINT as usual).
//
// The Go runtime turns only SIGHUP, SIGINT and SIGTERM (and the uncatchable
// SIGKILL) into a plain death-by-signal once the handler is reset. For every
// other signal it keeps its own handler (SIGQUIT/SIGABRT would dump
// goroutines and exit 2; SIGPIPE/SIGUSR1 would be ignored), so we exec
// kill(1) on our own pid: exec(2) resets caught signals to the default, and
// the pid (hence the parent's wait status) stays the same. Measured: a shell
// is no substitute, bash ignores a SIGQUIT it sends itself. If everything
// fails the caller returns 128+sig.
//
// A signal that was ignored when we started (nohup ignores SIGHUP, `cmd &`
// in a non-interactive shell ignores SIGINT and SIGQUIT) cannot be
// re-raised: the Go runtime keeps an inherited SIG_IGN for SIGHUP and
// SIGINT (signal.Reset restores it), and exec(2) keeps ignored signals
// ignored, so kill(1) would ignore its own signal and exit 0, turning the
// child's death into success. We then return at once, and the caller exits
// 128+sig: the same $? a shell shows for a death by that signal.
func raiseSignal(sig syscall.Signal) {
	if signal.Ignored(sig) {
		return
	}
	switch sig {
	case syscall.SIGSTOP, syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU, syscall.SIGCONT:
		return // not a cause of death
	case syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL:
		signal.Reset(sig)
		_ = syscall.Kill(os.Getpid(), sig)
		// Delivery may land on another thread; give it a moment.
		time.Sleep(50 * time.Millisecond)
	}
	n, pid := strconv.Itoa(int(sig)), strconv.Itoa(os.Getpid())
	env := []string{"PATH=/usr/bin:/bin"}
	for _, k := range []string{"/bin/kill", "/usr/bin/kill"} {
		_ = syscall.Exec(k, []string{"kill", "-" + n, pid}, env)
	}
	_ = syscall.Exec("/bin/sh", []string{"sh", "-c", "kill -" + n + " $$"}, env)
}
