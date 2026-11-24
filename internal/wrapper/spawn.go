package wrapper

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fireharp/pace/internal/client"
	"github.com/fireharp/pace/internal/proto"
)

var errNotFound = errors.New("executable file not found in $PATH")

// lookPath resolves file like execvp(3), against pathEnv.
func lookPath(file, pathEnv string) (string, error) {
	if strings.Contains(file, "/") {
		if err := checkExec(file); err != nil {
			return "", err
		}
		return file, nil
	}
	if pathEnv == "" {
		pathEnv = "/usr/bin:/bin:/usr/sbin:/sbin"
	}
	var firstErr error
	for _, dir := range filepath.SplitList(pathEnv) {
		p := filepath.Join(dir, file)
		if dir == "" || dir == "." {
			p = "./" + file
		}
		err := checkExec(p)
		if err == nil {
			return p, nil
		}
		if firstErr == nil && !errors.Is(err, fs.ErrNotExist) {
			firstErr = err
		}
	}
	if firstErr != nil {
		return "", firstErr
	}
	return "", errNotFound
}

func checkExec(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return &fs.PathError{Op: "exec", Path: p, Err: syscall.EISDIR}
	}
	if fi.Mode()&0o111 == 0 {
		return &fs.PathError{Op: "exec", Path: p, Err: fs.ErrPermission}
	}
	return nil
}

// --- stdio -------------------------------------------------------------------

// stdioPlan says what the child gets as fds 0-2 and which pipes we relay.
//
// stdout that is a file of any kind (terminal, regular file, pipe, socket)
// is handed to the child as is: the child can fstat and seek it, and
// descendants that outlive the child keep writing to it after pace run has
// exited. stderr goes through a relay pipe unless it is a terminal, so that
// governor lines land at child line boundaries (the mux); so do stdout and
// stderr together when they are the same non-terminal file (2>&1), which
// keeps their relative order. A relay still held open by descendants when
// the child exits is handed to a cat(1) process after the drain timeout
// instead of being closed, so those descendants never see EPIPE because
// pace run finished.
type stdioPlan struct {
	child     [3]*os.File
	ours      []*os.File // child ends of our pipes, closed in the parent after start
	relays    []*relaySpec
	stdinCopy func()
	stdinW    *os.File
	// ctty is our controlling terminal: the first of fds 0-2 that is one,
	// else /dev/tty. fg means our process group is its foreground group:
	// the child then gets the foreground (cttyFD is ctty's descriptor in
	// this process, for SysProcAttr.Foreground).
	ctty      *os.File
	cttyOwned bool // opened by us (/dev/tty): close when done
	fg        bool
	cttyFD    int
	done      chan struct{} // closed when every relay has finished
}

type relaySpec struct {
	src   *os.File
	write func([]byte) error
	dst   *os.File // the real destination, when it is a file (hand-off target)

	mu       sync.Mutex
	finished bool // the relay goroutine is done with src
	hand     bool // src is to be handed off: the relay leaves it open
}

// finish ends the relay goroutine's use of src: closed, unless it is being
// handed off (and the destination did not fail).
func (rs *relaySpec) finish(failed bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.finished = true
	if failed || !rs.hand {
		rs.hand = false
		rs.src.Close()
	}
}

// requestHandOff marks a still-running relay for hand-off and unblocks its
// Read. It reports false if the relay already finished.
func (rs *relaySpec) requestHandOff() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.finished {
		return false
	}
	rs.hand = true
	_ = rs.src.SetReadDeadline(time.Now())
	return true
}

func (rs *relaySpec) handedOff() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.hand
}

func sameDest(a, b io.Writer) (same bool) {
	af, aok := a.(*os.File)
	bf, bok := b.(*os.File)
	if aok && bok {
		if af == bf {
			return true
		}
		sa, e1 := af.Stat()
		sb, e2 := bf.Stat()
		return e1 == nil && e2 == nil && os.SameFile(sa, sb)
	}
	if aok != bok {
		return false
	}
	defer func() {
		if recover() != nil {
			same = false // non-comparable writer types
		}
	}()
	return a == b
}

// openCtty opens the controlling terminal, if there is one.
func openCtty() *os.File {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	return f
}

// rawFD returns f's descriptor number without changing its mode.
func rawFD(f *os.File) int {
	n := -1
	_ = control(f, func(fd int) error { n = fd; return nil })
	return n
}

func (r *runner) planStdio() (*stdioPlan, error) {
	pl := &stdioPlan{cttyFD: -1, done: make(chan struct{})}
	pipe := func() (*os.File, *os.File, error) {
		pr, pw, err := os.Pipe()
		if err != nil {
			pl.closeAll()
			return nil, nil, err
		}
		return pr, pw, nil
	}
	// stdin
	if f, ok := r.o.Stdin.(*os.File); ok {
		pl.child[0] = f
	} else {
		pr, pw, err := pipe()
		if err != nil {
			return nil, err
		}
		pl.child[0] = pr
		pl.ours = append(pl.ours, pr)
		pl.stdinW = pw
		src := r.o.Stdin
		pl.stdinCopy = func() {
			_, _ = io.Copy(pw, src)
			pw.Close()
		}
	}

	outF, outIsF := r.o.Stdout.(*os.File)
	errF, errIsF := r.o.Stderr.(*os.File)
	outTTY := outIsF && isTTY(outF)
	errTTY := errIsF && isTTY(errF)
	same := sameDest(r.o.Stdout, r.o.Stderr)
	direct := (same && (outTTY || errTTY)) || (!same && errTTY)
	m := newMux(r.o.Stderr, r.t.MuxHold, direct)
	r.mux = m
	toMux := func(p []byte) error { return m.Child(p) }
	stdout := r.o.Stdout
	toStdout := func(p []byte) error { _, err := stdout.Write(p); return err }

	switch {
	case same && (outTTY || errTTY):
		pl.child[1], pl.child[2] = outF, errF
	case same:
		pr, pw, err := pipe()
		if err != nil {
			return nil, err
		}
		pl.child[1], pl.child[2] = pw, pw
		pl.ours = append(pl.ours, pw)
		pl.relays = append(pl.relays, &relaySpec{src: pr, write: toMux, dst: errF})
	default:
		if outIsF {
			pl.child[1] = outF
			if !outTTY {
				r.watchStdout(outF)
			}
		} else {
			pr, pw, err := pipe()
			if err != nil {
				return nil, err
			}
			pl.child[1] = pw
			pl.ours = append(pl.ours, pw)
			pl.relays = append(pl.relays, &relaySpec{src: pr, write: toStdout})
		}
		if errTTY {
			pl.child[2] = errF
		} else {
			pr, pw, err := pipe()
			if err != nil {
				return nil, err
			}
			pl.child[2] = pw
			pl.ours = append(pl.ours, pw)
			pl.relays = append(pl.relays, &relaySpec{src: pr, write: toMux, dst: errF})
		}
	}

	// Job control: our controlling terminal, from fds 0-2, else /dev/tty (a
	// child reading /dev/tty with stdio redirected, such as an ssh or sudo
	// prompt, must not stop forever). With /dev/tty the child gets the
	// foreground at once only when we lead our process group (a job-control
	// shell made us a job of our own). Otherwise whoever shares our group (a
	// script, a Makefile, an agent harness's TUI) keeps the terminal, and the
	// child gets it only once it actually needs it (onChildStop).
	pgrp := syscall.Getpgrp()
	for _, f := range pl.child {
		if f == nil || pl.isOurs(f) || !isTTY(f) {
			continue
		}
		if pg, err := tcgetpgrp(f); err == nil {
			pl.ctty, pl.fg = f, pg == pgrp
			break
		}
	}
	if pl.ctty == nil && r.o.h.openTTY != nil {
		if f := r.o.h.openTTY(); f != nil {
			if pg, err := tcgetpgrp(f); err == nil {
				pl.ctty, pl.cttyOwned, pl.fg = f, true, pg == pgrp && jobLeader()
			} else {
				f.Close()
			}
		}
	}
	if pl.fg {
		if pl.cttyFD = rawFD(pl.ctty); pl.cttyFD < 0 {
			pl.fg = false
		}
	}
	return pl, nil
}

func (pl *stdioPlan) isOurs(f *os.File) bool {
	for _, o := range pl.ours {
		if o == f {
			return true
		}
	}
	return false
}

// closeChildEnds closes the parent's copies of the child's pipe ends so that
// EOF arrives when the child (and its descendants) close theirs.
func (pl *stdioPlan) closeChildEnds() {
	for _, f := range pl.ours {
		f.Close()
	}
	pl.ours = nil
}

// closeAll closes everything (error paths).
func (pl *stdioPlan) closeAll() {
	pl.closeChildEnds()
	pl.closeRelays()
	if pl.stdinW != nil {
		pl.stdinW.Close()
	}
	pl.closeCtty()
}

func (pl *stdioPlan) closeCtty() {
	if pl.cttyOwned && pl.ctty != nil {
		pl.ctty.Close()
		pl.ctty = nil
	}
}

func (pl *stdioPlan) closeRelays() {
	for _, rs := range pl.relays {
		rs.src.Close()
	}
}

// endRelays ends the relays that descendants still hold open after the
// child exited. Normally they are handed off (handOff), so the descendants
// keep their output. When pace run is about to print an EXIT line, which
// must stay the last stderr line, they are cut instead: a descendant's
// next write then fails with EPIPE, as with no reader at all.
func (r *runner) endRelays(pl *stdioPlan, relaysDone <-chan struct{}, exitLine bool) {
	if !exitLine {
		r.handOff(pl, relaysDone)
		return
	}
	pl.closeRelays()
	if relaysDone != nil {
		select {
		case <-relaysDone:
		case <-time.After(time.Second):
		}
	}
}

// handOff ends our relaying of output that descendants still hold open
// after the child exited. Each relay with a real destination file is given
// to a cat(1) process that copies until the last writer is gone; we then
// exit without the descendants ever seeing EPIPE. Without a destination
// file (or cat), the read end is closed as a last resort.
func (r *runner) handOff(pl *stdioPlan, relaysDone <-chan struct{}) {
	cat, err := lookPath("cat", "/bin:/usr/bin")
	for _, rs := range pl.relays {
		if rs.dst == nil || err != nil || !rs.requestHandOff() {
			rs.src.Close()
		}
	}
	if relaysDone != nil {
		select {
		case <-relaysDone:
		case <-time.After(time.Second):
			// A relay is stuck writing to its destination: give up on order.
			pl.closeRelays()
			return
		}
	}
	devnull, _ := os.Open(os.DevNull)
	if devnull != nil {
		defer devnull.Close()
	}
	for _, rs := range pl.relays {
		if !rs.handedOff() {
			continue
		}
		_ = rs.src.SetReadDeadline(time.Time{})
		p, err := os.StartProcess(cat, []string{"cat"}, &os.ProcAttr{
			Env:   []string{"PATH=/usr/bin:/bin"},
			Files: []*os.File{rs.src, rs.dst, devnull},
		})
		if err == nil {
			_ = p.Release()
		}
		rs.src.Close()
	}
}

// startRelays copies child output; bytes are counted for progress. On a
// destination write error the read end is closed so the child sees EPIPE,
// as it would have without the wrapper.
func (r *runner) startRelays(pl *stdioPlan) {
	if pl.stdinCopy != nil {
		go pl.stdinCopy()
	}
	var wg sync.WaitGroup
	for _, rs := range pl.relays {
		wg.Add(1)
		go func(rs *relaySpec) {
			defer wg.Done()
			buf := make([]byte, 32<<10)
			failed := false
			for {
				n, err := rs.src.Read(buf)
				if n > 0 {
					r.outBytes.Add(int64(n))
					r.lastOutMS.Store(time.Now().UnixMilli())
					if werr := rs.write(buf[:n]); werr != nil {
						failed = true
						break
					}
				}
				if err != nil {
					break
				}
			}
			rs.finish(failed)
		}(rs)
	}
	go func() {
		wg.Wait()
		close(pl.done)
	}()
}

// watchStdout counts what the child writes to a regular-file stdout it
// owns directly, from the shared file offset (polled with each heartbeat).
func (r *runner) watchStdout(f *os.File) {
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return
	}
	off, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	r.outFile, r.outBase = f, off
}

// pollStdout updates the stdout byte count from the file offset.
func (r *runner) pollStdout() {
	if r.outFile == nil {
		return
	}
	off, err := r.outFile.Seek(0, io.SeekCurrent)
	if err != nil {
		return
	}
	if n := off - r.outBase; n > r.outFileBytes.Load() {
		r.outFileBytes.Store(n)
		r.lastOutMS.Store(time.Now().UnixMilli())
	}
}

// outputBytes is everything the job wrote to stdout and stderr so far.
func (r *runner) outputBytes() int64 {
	return r.outBytes.Load() + r.outFileBytes.Load()
}

// --- child ---------------------------------------------------------------------

type child struct {
	pid, pgid int
	startMS   int64
	tty       *os.File       // our controlling terminal (nil: none)
	fg        bool           // the child's group is the terminal's foreground
	stopped   bool           // seen stopped, not continued since
	stopSig   syscall.Signal // the signal of the last stop seen
	ttyWait   bool           // stopped by terminal I/O; left stopped until we can give it the terminal
	waitc     chan waitResult
	report    func(syscall.WaitStatus) // hooks.waitReport
}

type waitResult struct {
	ws  syscall.WaitStatus
	err error
}

func (r *runner) spawn(pl *stdioPlan, env []string) (*child, error) {
	sys := &syscall.SysProcAttr{Setpgid: true}
	if pl.fg {
		sys.Foreground = true
		sys.Ctty = pl.cttyFD // a descriptor in this process
	}
	attr := &os.ProcAttr{Env: env, Files: pl.child[:], Sys: sys}
	// A job container: the child is created inside it (CLONE_INTO_CGROUP),
	// so no descendant can start outside. If the kernel refuses that (too
	// old, or the placement is not permitted), spawn normally and move the
	// child in right after.
	inside := false
	if r.cg != nil && r.cg.fd >= 0 {
		withCG := *sys
		if useCgroupFD(&withCG, r.cg.fd) {
			attr.Sys = &withCG
			inside = true
		}
	}
	p, err := os.StartProcess(r.path, r.o.Argv, attr)
	if err != nil && inside {
		attr.Sys, inside = sys, false
		p, err = os.StartProcess(r.path, r.o.Argv, attr)
	}
	r.cg.closeFD()
	if err != nil {
		if r.cg != nil {
			_ = r.cg.c.Remove(r.cg.id)
			r.cg = nil
		}
		return nil, err
	}
	if r.cg != nil && !inside {
		if aerr := r.cg.c.Attach(r.cg.id, p.Pid); aerr != nil {
			r.dropContainer(aerr)
		}
	}
	ch := &child{pid: p.Pid, pgid: p.Pid, tty: pl.ctty, fg: pl.fg, waitc: make(chan waitResult, 8), report: r.o.h.waitReport}
	_ = p.Release() // we reap with wait4 ourselves
	if ch.startMS = procStartMS(ch.pid); ch.startMS == 0 {
		ch.startMS = time.Now().UnixMilli()
	}
	go ch.wait()
	return ch, nil
}

// wait reaps the child, reporting stops too (a stopped child must never
// leave the wrapper waiting forever) and continues, whoever sent them (the
// daemon's thaw, its release on shutdown, a kill -CONT). macOS reports a
// continue as a stop by SIGCONT (measured on Darwin 23); nothing relies on
// every continue being reported.
func (c *child) wait() {
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(c.pid, &ws, syscall.WUNTRACED|syscall.WCONTINUED, nil)
		if err == syscall.EINTR {
			continue
		}
		if c.report != nil && err == nil {
			c.report(ws)
		}
		c.waitc <- waitResult{ws: ws, err: err}
		if err != nil || ws.Exited() || ws.Signaled() {
			return
		}
	}
}

// stopSignal reports whether ws (from wait4 with WUNTRACED|WCONTINUED) is
// a stop, and by which signal. The raw status is decoded: Go's BSD
// WaitStatus.Stopped excludes SIGSTOP (it reads that status as Continued),
// so a daemon freeze would otherwise look like neither a stop nor an exit.
func stopSignal(ws syscall.WaitStatus) (syscall.Signal, bool) {
	if ws&0xff != 0x7f || continuedStatus(ws) {
		return 0, false
	}
	return syscall.Signal((ws >> 8) & 0xff), true
}

// continuedStatus reports whether ws (from wait4 with WCONTINUED) says the
// child was continued: 0xffff on Linux; on the BSDs a stop status whose
// signal is SIGCONT (decoded by hand: Go's BSD WaitStatus.Continued tests
// for SIGSTOP).
func continuedStatus(ws syscall.WaitStatus) bool {
	return ws == 0xffff || (ws&0xff == 0x7f && syscall.Signal((ws>>8)&0xff) == syscall.SIGCONT)
}

func isTTYStop(s syscall.Signal) bool {
	return s == syscall.SIGTSTP || s == syscall.SIGTTIN || s == syscall.SIGTTOU
}

// restoreTTY takes the terminal back after the child is gone, unless the
// foreground already moved to a live group that is not the child's.
func (r *runner) restoreTTY(ch *child) {
	if !ch.fg || ch.tty == nil {
		return
	}
	self := syscall.Getpgrp()
	pg, err := tcgetpgrp(ch.tty)
	if err != nil || pg == self {
		return
	}
	if pg == ch.pgid || syscall.Kill(-pg, 0) == syscall.ESRCH {
		_ = tcsetpgrp(ch.tty, self)
	}
}

// becomeFg records that the child now owns the terminal. Ignoring SIGTTOU
// lets us write pace lines and restore the terminal from the background (the
// child has exec'd, so this cannot leak into it).
func (r *runner) becomeFg(ch *child) {
	if ch.fg {
		return
	}
	ch.fg = true
	signal.Ignore(syscall.SIGTTOU)
}

// stopSelf stops the wrapper, so that the shell sees the job stopped, and
// returns once it is continued. If our process group is orphaned the kernel
// discards the stop; we then return after ResumeWait instead of hanging.
func (r *runner) stopSelf() {
	for len(r.contc) > 0 {
		<-r.contc
	}
	sig := syscall.SIGTSTP
	if r.tstpc != nil {
		// SIGTSTP is caught (forwarded to the job), so sending it to
		// ourselves would only come back to us. SIGTTIN stops by default
		// too, and is discarded the same way in an orphaned group.
		sig = syscall.SIGTTIN
	}
	_ = syscall.Kill(os.Getpid(), sig)
	t := time.NewTimer(r.t.ResumeWait)
	select {
	case <-r.contc:
	case <-t.C:
	}
	t.Stop()
}

// jobLeader reports whether we lead our process group: a job-control
// shell runs us as a job of our own, and it alone shares the terminal
// decisions with us.
func jobLeader() bool { return syscall.Getpgrp() == os.Getpid() }

// cooked reports whether the terminal is in canonical (line) mode. An
// interactive program that drives the terminal itself (an agent harness's
// TUI, an editor) runs it in raw mode; a shell runs its foreground jobs in
// canonical mode.
func cooked(f *os.File) bool {
	var on bool
	_ = control(f, func(fd int) error {
		t, err := unix.IoctlGetTermios(fd, ioctlReadTermios)
		if err == nil {
			on = t.Lflag&unix.ICANON != 0
		}
		return err
	})
	return on
}

// tryForeground gives the child the terminal if we hold its foreground.
func (r *runner) tryForeground(ch *child) bool {
	if ch.tty == nil {
		return false
	}
	pg, err := tcgetpgrp(ch.tty)
	if err != nil || pg != syscall.Getpgrp() || tcsetpgrp(ch.tty, ch.pgid) != nil {
		return false
	}
	r.becomeFg(ch)
	return true
}

// continueChild sends SIGCONT, unless the daemon has the job frozen
// (THAWED will continue it).
func (r *runner) continueChild(ch *child) {
	if !r.frozen {
		_ = syscall.Kill(-ch.pgid, syscall.SIGCONT)
		ch.stopped = false
	}
	ch.ttyWait = false
}

// thawLost continues a job its lost daemon left frozen: no governor is
// there to thaw it, and it runs ungoverned until a daemon takes it back
// (fail-open). A job seen stopped by SIGSTOP counts as frozen even when the
// FROZEN line never arrived (the daemon died between the signal and the
// line). A job continued since (THAWED, a continue reported by wait4, or
// one seen stopped of whose process group the kernel has no member stopped
// now) is not frozen. The freeze is on the whole group, so the leader
// running again is not enough: it may have been continued alone, or (on
// macOS) escaped the SIGSTOP in execve, which wait4 still reports as a
// stop. A terminal stop (Ctrl-Z, terminal I/O) is job control, not the
// daemon's, and is left alone.
func (r *runner) thawLost(ch *child) {
	if r.cg != nil {
		// Only pace freezes a job container, and SIGCONT does not wake one:
		// thaw it whether or not the FROZEN line arrived. The job runs
		// ungoverned now (fail-open): nor does it stay capped by the
		// throttle's cpu.max quota and weights, or by memory.max, with no
		// governor to lift them. A daemon that takes it back re-applies
		// what its state wants.
		_ = r.cg.c.Freeze(r.cg.id, false)
		_, _ = r.cg.c.SetThrottle(r.cg.id, false)
		_ = r.cg.c.SetMemMax(r.cg.id, 0)
	}
	if !r.frozen && !(ch.stopped && ch.stopSig == syscall.SIGSTOP) {
		return
	}
	if live, stopped := groupProcs(ch.pgid); live > 0 && stopped == 0 && (ch.stopped || r.cg == nil) {
		// Nothing of the group is stopped now: continued since (by the
		// daemon without its THAWED line, or a kill -CONT), so not frozen.
		// That holds even when wait4 never reported the stop: a leader in
		// vfork-wait (dash runs a command with vfork) does not stop while
		// its stopped child is mid-exec, so neither the stop nor the
		// continue is reported for it. A container's freeze (cgroup.freeze,
		// state S) is not visible here: it is thawed above, and counts as
		// frozen unless seen stopped and continued.
		r.frozen, ch.stopped = false, false
		return
	}
	_ = syscall.Kill(-ch.pgid, syscall.SIGCONT)
	r.frozen, ch.stopped = false, false
	r.emit(proto.Event{Verb: proto.VerbInfo, Job: r.id, Msg: "continued the job its lost daemon had frozen"})
}

// resumeChild continues a job-control-stopped child: it gets the terminal
// if we hold the foreground now, then SIGCONT.
func (r *runner) resumeChild(ch *child) {
	r.tryForeground(ch)
	r.continueChild(ch)
}

// giveTerminal hands the terminal to a child stopped by terminal I/O from
// the background, as a shell's fg would, and continues it. That needs our
// group to hold the foreground. When we do not lead our group, it also
// needs a terminal in canonical mode: a raw-mode terminal is driven by
// another program in our group (an agent's TUI), which must not be put in
// the background for the child's sake.
func (r *runner) giveTerminal(ch *child) bool {
	if ch.tty == nil || (!jobLeader() && !cooked(ch.tty)) || !r.tryForeground(ch) {
		return false
	}
	r.continueChild(ch)
	return true
}

// onChildStop handles a stop of the child reported by wait4.
//
//   - SIGSTOP (a daemon freeze, or someone's kill -STOP): left alone.
//   - A terminal stop of a foreground child (Ctrl-Z): take the terminal
//     back, stop ourselves, and on resume give it back and continue.
//   - A SIGTSTP of a background child (one we forwarded): stop with it, so
//     the shell sees the job stopped, and continue it when we are continued.
//   - Terminal I/O of a background child (SIGTTIN/SIGTTOU): hand it the
//     terminal and continue it, as a shell's fg would (giveTerminal). If we
//     cannot, and we are a job of our own, stop with it so the shell shows
//     the job stopped and its fg gives us the terminal. Otherwise it stays
//     stopped (continuing it would only stop it again): a pace INFO line says
//     why, a later SIGCONT of pace run retries, and a forwarded signal still
//     reaches it.
func (r *runner) onChildStop(ch *child, sig syscall.Signal) {
	ch.stopped, ch.stopSig = true, sig
	if !isTTYStop(sig) {
		return
	}
	if ch.fg {
		if pg, err := tcgetpgrp(ch.tty); err == nil && pg == ch.pgid {
			_ = tcsetpgrp(ch.tty, syscall.Getpgrp())
		}
		r.stopSelf()
		r.resumeChild(ch)
		return
	}
	if sig == syscall.SIGTSTP {
		r.stopSelf()
		r.resumeChild(ch)
		return
	}
	if r.giveTerminal(ch) {
		return
	}
	if jobLeader() {
		r.stopSelf()
		if r.giveTerminal(ch) {
			return
		}
	}
	ch.ttyWait = true
	if !r.ttyWaitShown {
		r.ttyWaitShown = true
		what := "input (SIGTTIN)"
		if sig == syscall.SIGTTOU {
			what = "output (SIGTTOU)"
		}
		r.emit(proto.Event{Verb: proto.VerbInfo, Job: r.id,
			Msg: "the command is stopped waiting for terminal " + what + ", which pace run cannot hand it: " +
				"another program drives the terminal, or pace run is not in the foreground; give the command its input on stdin, or bring pace run to the foreground"})
	}
}

// --- posting -------------------------------------------------------------------

func (r *runner) startPoster() {
	if r.postc != nil {
		return
	}
	c := make(chan postReq, 16)
	r.postc = c
	timeout := r.t.PostTimeout
	go func() {
		for p := range c {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			err := p.fn(ctx)
			cancel()
			if p.done != nil {
				p.done <- err
			}
		}
	}()
}

var errNoPoster = errors.New("wrapper: not connected")

// post queues fn on the serial poster. Without done it never blocks (a
// heartbeat is dropped when the queue is full).
// It reports whether fn was queued.
func (r *runner) post(fn func(ctx context.Context) error, done chan error) bool {
	if r.postc == nil {
		if done != nil {
			done <- errNoPoster
		}
		return false
	}
	req := postReq{fn: fn, done: done}
	if done == nil {
		select {
		case r.postc <- req:
			return true
		default:
			return false
		}
	}
	r.postc <- req
	return true
}

// postProgress queues one heartbeat unless one is still waiting: a slow
// daemon must never pile heartbeats up in front of the exit POST. The
// counters are read when the heartbeat is sent, so it is never stale.
func (r *runner) postProgress() {
	if !r.progressQueued.CompareAndSwap(false, true) {
		return
	}
	c, id := r.c, r.id
	queued := r.post(func(ctx context.Context) error {
		r.progressQueued.Store(false)
		req := proto.ProgressRequest{OutputBytes: r.outputBytes(), LastOutputMS: r.lastOutMS.Load()}
		return c.Progress(ctx, id, req)
	}, nil)
	if !queued {
		r.progressQueued.Store(false)
	}
}

// exitRequest is the exit report for the daemon. A governor kill is
// reported as exit code 201 (with the signal kept for diagnostics), so that
// the daemon's job status agrees with what pace run returns.
func exitRequest(ws syscall.WaitStatus, killedByPace bool) proto.ExitRequest {
	if killedByPace {
		x := proto.ExitRequest{Code: proto.ExitKilled}
		if ws.Signaled() {
			x.Signal = int(ws.Signal())
		}
		return x
	}
	if ws.Signaled() {
		return proto.ExitRequest{Code: 128 + int(ws.Signal()), Signaled: true, Signal: int(ws.Signal())}
	}
	return proto.ExitRequest{Code: ws.ExitStatus()}
}

// --- supervision ---------------------------------------------------------------

type phase int

const (
	phRunning  phase = iota // child alive
	phDraining              // child exited; reading leftover output
	phPosting               // POST exit in flight
	phKillWait              // waiting briefly for a KILLED attribution
	phDone
)

type attachResult struct {
	st  *client.Stream
	err error
}

// runChild spawns the admitted (governed) or fail-open (ungoverned) command
// and supervises it to the end.
func (r *runner) runChild(governed bool) int {
	pl, err := r.planStdio()
	if err != nil {
		r.mux = newMux(r.o.Stderr, r.t.MuxHold, true)
		if governed {
			r.syncExit(proto.ExitRequest{Code: proto.ExitPaceError})
		}
		return r.paceError("cannot set up stdio: " + err.Error())
	}
	defer pl.closeCtty()
	env := setenv(r.env, EnvJobID, r.envJobID())
	if governed {
		r.cg = r.openContainer()
	}
	ch, err := r.spawn(pl, env)
	pl.closeChildEnds()
	if err != nil {
		pl.closeAll()
		r.mux = newMux(r.o.Stderr, r.t.MuxHold, true)
		code := r.execFailed(err)
		if governed {
			r.syncExit(proto.ExitRequest{Code: code})
		}
		r.writeStatus(OutcomeError, code, "child")
		return code
	}
	r.contc = make(chan os.Signal, 4)
	signal.Notify(r.contc, syscall.SIGCONT)
	if ch.fg {
		// The child has exec'd: ignoring SIGTTOU now cannot leak into it, and
		// lets us write pace lines and restore the terminal from the background.
		signal.Ignore(syscall.SIGTTOU)
	} else if !signal.Ignored(syscall.SIGTSTP) {
		// The terminal's Ctrl-Z (or a kill -TSTP) reaches only us: pass it on
		// to the job, and stop with it (onChildStop).
		r.tstpc = make(chan os.Signal, 2)
		signal.Notify(r.tstpc, syscall.SIGTSTP)
	}
	r.rec.PID, r.rec.PGID, r.rec.StartTS = ch.pid, ch.pgid, ch.startMS
	if r.slot != nil {
		writeSlotInfo(r.slot, r.o.Argv, ch.pgid, ch.startMS)
	}
	if governed {
		r.startPoster()
	}
	r.startRelays(pl)
	return r.supervise(ch, pl, governed)
}

// stopReportWait bounds how long a lost stream's thaw waits for wait4's
// report of a stop the kernel already shows (normally at once).
const stopReportWait = 2 * time.Second

// settleLost runs when the child ended while the daemon stream was lost.
// A daemon that answers for the job (restarted, not yet re-attached to)
// may have killed it: its record then names the policy and holds the
// KILLED event, which is printed here (exit 201 follows, as with a live
// stream). Either way the daemon gets the exit report, so its record
// carries the exit code. A child that died of SIGTERM or SIGKILL is asked
// about a few times (the record lags the signal by a step's bookkeeping).
func (r *runner) settleLost(ws syscall.WaitStatus) {
	if r.c == nil || r.id == "" {
		return
	}
	tries := 1
	if ws.Signaled() && (ws.Signal() == syscall.SIGTERM || ws.Signal() == syscall.SIGKILL) {
		tries = 4
	}
	var js proto.JobStatus
	for i := 0; ; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), r.t.PostTimeout)
		var err error
		js, err = r.c.Job(ctx, r.id)
		cancel()
		if err != nil {
			return // no daemon, or it does not know the job: nothing to settle
		}
		if js.KilledBy != "" || i+1 >= tries {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if js.KilledBy != "" && r.killed == nil {
		ev := proto.Event{Verb: proto.VerbKilled, Policy: js.KilledBy, Job: r.id}
		for i := len(js.Events) - 1; i >= 0; i-- {
			if e := js.Events[i]; e.Verb == proto.VerbKilled {
				ev = e
				break
			}
		}
		if ev.Job == "" {
			ev.Job = r.id
		}
		r.killed = &ev
		r.emit(ev)
	}
	req := exitRequest(ws, r.killed != nil)
	if r.cg != nil && r.cg.oom && r.killed == nil {
		req.OOMKill, req.PeakBytes = true, r.cg.peak
	}
	r.syncExit(req)
}

// syncExit posts an exit report and waits for it (bounded).
func (r *runner) syncExit(req proto.ExitRequest) {
	if r.c == nil || r.id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.t.PostTimeout)
	defer cancel()
	_ = r.c.Exit(ctx, r.id, req)
}

type session struct {
	lost         bool // stream gone while the child runs
	lostPrinted  bool
	noReattach   bool
	attachBusy   bool
	attachVerify bool // attach stream opened; not yet confirmed by an event
	streamGone   bool
}

func (r *runner) supervise(ch *child, pl *stdioPlan, governed bool) int {
	var (
		ws            syscall.WaitStatus
		waitErr       error
		ph            = phRunning
		s             = session{streamGone: !governed}
		relaysDone    = pl.done
		relaysOver    bool
		drainC        <-chan time.Time
		killC         <-chan time.Time
		exitDone      chan error
		exitPosted    *proto.ExitRequest
		progressC     <-chan time.Time
		reattachC     <-chan time.Time
		attachRes     = make(chan attachResult, 1)
		reattachTmr   = time.NewTimer(time.Hour)
		startedDone   chan error
		startedRetryC <-chan time.Time
		startedWait   = r.t.StartedRetry
		thawC         <-chan time.Time // a lost stream's thaw, waiting for wait4's report of a stop
	)
	reattachTmr.Stop()
	defer reattachTmr.Stop()
	if governed {
		tk := time.NewTicker(r.t.Progress)
		defer tk.Stop()
		progressC = tk.C
	}
	// The started report is what lets the daemon attribute the job's
	// processes: retry it (with backoff) until it lands or the child is gone.
	sreq := proto.StartedRequest{PID: ch.pid, PGID: ch.pgid, StartMS: ch.startMS}
	if r.cg != nil {
		sreq.Cgroup = r.cg.path
	}
	postStarted := func() {
		startedDone = make(chan error, 1)
		c, id := r.c, r.id
		r.post(func(ctx context.Context) error { return c.Started(ctx, id, sreq) }, startedDone)
	}
	if governed {
		postStarted()
	}
	scheduleReattach := func() {
		if s.noReattach {
			reattachC = nil
			return
		}
		reattachTmr.Reset(r.t.Reattach)
		reattachC = reattachTmr.C
	}
	enterKillWait := func() {
		ph = phKillWait
		if r.killed != nil || s.streamGone || s.lost {
			ph = phDone
			return
		}
		d := r.t.KillWait
		if ws.Exited() && ws.ExitStatus() == 0 {
			d = r.t.KillWaitClean
		}
		killC = time.After(d)
	}
	enterPost := func() {
		ph = phPosting
		r.mux.Close()
		if governed && !s.lost && waitErr == nil {
			exitDone = make(chan error, 1)
			c, id, req := r.c, r.id, exitRequest(ws, r.killed != nil)
			if r.cg != nil && r.cg.oom && r.killed == nil {
				req.OOMKill, req.PeakBytes = true, r.cg.peak
			}
			exitPosted = &req
			r.post(func(ctx context.Context) error { return c.Exit(ctx, id, req) }, exitDone)
			return
		}
		enterKillWait()
	}
	childGone := func() {
		if waitErr == nil {
			r.checkOOM(ws)
		}
		r.restoreTTY(ch)
		ph = phDraining
		reattachC = nil
		startedRetryC = nil
		thawC = nil
		if relaysOver {
			enterPost()
			return
		}
		drainC = time.After(r.t.Drain)
	}

	for ph != phDone {
		select {
		case it := <-r.evc:
			if it.gen != r.gen {
				continue
			}
			if it.err != nil {
				s.streamGone = true
				s.attachVerify = false
				r.closeStream()
				switch ph {
				case phRunning:
					s.lost = true
					r.everLost = true
					if !s.lostPrinted {
						s.lostPrinted = true
						r.emit(proto.Event{Verb: proto.VerbDegraded, Reason: proto.ReasonDaemonLost, Job: r.id})
					}
					if _, stopped := groupProcs(ch.pgid); !r.frozen && !ch.stopped && stopped > 0 {
						// The kernel has the job stopped, but wait4's report
						// of the stop has not been handled yet (the daemon
						// died right after its SIGSTOP): thaw once the
						// report says by which signal, bounded.
						thawC = time.After(stopReportWait)
					} else {
						r.thawLost(ch)
					}
					scheduleReattach()
				case phKillWait:
					ph = phDone
				}
				continue
			}
			r.onRunningEvent(it.ev, &s)
			if it.ev.Verb == proto.VerbThawed && (it.ev.Job == "" || it.ev.Job == r.id) && ch.stopSig == syscall.SIGSTOP {
				ch.stopped = false // the daemon continued it before the line (wait4's report may come later)
			}
			if ph == phKillWait && r.killed != nil {
				ph = phDone
			}

		case w := <-ch.waitc:
			if w.err == nil && continuedStatus(w.ws) {
				// Continued, by whoever: neither stopped nor frozen now (a
				// daemon that freezes it again says FROZEN again).
				ch.stopped, ch.ttyWait = false, false
				r.frozen = false
				continue
			}
			if sig, stopped := stopSignal(w.ws); w.err == nil && stopped {
				if ph == phRunning {
					r.onChildStop(ch, sig)
					if thawC != nil && s.lost {
						thawC = nil
						r.thawLost(ch)
					}
				}
				continue
			}
			ws, waitErr = w.ws, w.err
			childGone()

		case <-thawC:
			thawC = nil
			if ph == phRunning && s.lost {
				r.thawLost(ch)
			}

		case <-r.tstpc:
			if ph == phRunning {
				_ = syscall.Kill(-ch.pgid, syscall.SIGTSTP)
			}

		case <-r.contc:
			// Continued (a shell's fg or bg): a job left waiting for the
			// terminal may get it now.
			if ph == phRunning && ch.ttyWait {
				r.giveTerminal(ch)
			}

		case <-relaysDone:
			relaysDone = nil
			relaysOver = true
			if ph == phDraining {
				drainC = nil
				enterPost()
			}

		case <-drainC:
			// Output still open after the child exited (a descendant holds
			// the pipe): go on without waiting for it. The relays keep
			// running (through the mux, so pace lines stay at line
			// boundaries) until the exit code is known; see endRelays.
			drainC = nil
			enterPost()

		case <-exitDone:
			exitDone = nil
			enterKillWait()

		case <-killC:
			ph = phDone

		case err := <-startedDone:
			startedDone = nil
			if err != nil && retryable(err) && ph == phRunning && !s.lost {
				startedRetryC = time.After(startedWait)
				startedWait = min(2*startedWait, 2*time.Second)
			}

		case <-startedRetryC:
			startedRetryC = nil
			if ph == phRunning && !s.lost {
				postStarted()
			}

		case sig := <-r.sigc:
			if ph <= phDraining {
				if s, ok := sig.(syscall.Signal); ok {
					_ = syscall.Kill(-ch.pgid, s)
					// A stopped (frozen) job keeps the signal pending until
					// it is continued: continue it so the signal acts now
					// (a job container frozen with cgroup.freeze is thawed:
					// SIGCONT does not wake it).
					if r.frozen || ch.stopped {
						if r.frozen && r.cg != nil {
							_ = r.cg.c.Freeze(r.cg.id, false)
						}
						_ = syscall.Kill(-ch.pgid, syscall.SIGCONT)
						ch.stopped, ch.ttyWait = false, false
					}
					r.rec.Forwarded = append(r.rec.Forwarded, int(s))
				}
			}

		case <-progressC:
			if governed && !s.lost && ph <= phDraining {
				r.pollStdout()
				r.postProgress()
			}

		case <-reattachC:
			reattachC = nil
			if ph == phRunning && s.lost && !s.attachBusy && !s.noReattach {
				s.attachBusy = true
				startedRetryC = nil
				c, id := r.c, r.id
				r.pollStdout()
				req := proto.AttachRequest{
					Submit: r.submit, PID: ch.pid, PGID: ch.pgid, StartMS: ch.startMS,
					SubmitMS: r.submitMS, OutputBytes: r.outputBytes(), LastEventTS: r.lastEvTS.Load(),
				}
				if r.cg != nil {
					req.Cgroup = r.cg.path
				}
				go func() {
					st, err := c.Attach(context.Background(), id, req)
					attachRes <- attachResult{st, err}
				}()
			}

		case res := <-attachRes:
			s.attachBusy = false
			if ph != phRunning || !s.lost {
				if res.st != nil {
					res.st.Close()
				}
				continue
			}
			if res.err != nil {
				var ae *client.APIError
				if errors.As(res.err, &ae) && ae.Status >= 400 && ae.Status < 500 {
					s.noReattach = true
					r.emit(proto.Event{Verb: proto.VerbInfo, Job: r.id,
						Msg: fmt.Sprintf("daemon refused to re-attach (HTTP %d); job continues ungoverned", ae.Status)})
					continue
				}
				scheduleReattach()
				continue
			}
			r.startStream(res.st)
			s.streamGone = false
			s.attachVerify = true
		}
	}

	if governed && s.lost && !s.noReattach && waitErr == nil {
		// The child ended while no daemon stream was open: a restarted
		// daemon may have killed it before we re-attached (its KILLED never
		// reached us). Ask it, and report the exit to it.
		r.settleLost(ws)
	}
	if r.killed == nil && r.cg != nil && r.cg.oom {
		// The kernel killed the job at the hard limit the daemon set, and
		// the daemon's KILLED did not arrive (lost, or late): say so here.
		// The exit record is corrected to 201 below.
		ev := r.kernelKilled()
		r.killed = &ev
		r.emit(ev)
	}
	r.removeContainer()
	code, source, sig := exitCode(ws, r.killed != nil)
	exitLine := waitErr != nil || (sig == 0 && proto.IsReserved(code))
	if !relaysOver {
		r.endRelays(pl, relaysDone, exitLine)
	}
	r.pollStdout()
	r.rec.OutputBytes = r.outputBytes()
	if waitErr != nil {
		return r.paceError("lost track of the child: " + waitErr.Error())
	}
	if governed && r.killed != nil && exitPosted != nil && exitPosted.Code != proto.ExitKilled && !s.lost {
		// KILLED arrived after the exit report: correct the daemon's record.
		r.syncExit(exitRequest(ws, true))
	}
	if ws.Signaled() {
		r.rec.Signal = int(ws.Signal())
	}
	outcome := OutcomeAdmitted
	switch {
	case r.killed != nil:
		outcome = OutcomeKilled
	case r.failOpen || r.everLost:
		outcome = OutcomeDegraded
	}
	if exitLine {
		r.exitLine(proto.Event{Verb: proto.VerbExit, Code: proto.Int(code), Source: source, Job: r.id}, outcome, code, source)
	} else {
		r.writeStatus(outcome, code, source)
	}
	r.closeStream()
	if sig != 0 {
		return r.reraise(sig)
	}
	return code
}

// onRunningEvent relays one live event while the child runs (or just ended).
func (r *runner) onRunningEvent(ev proto.Event, s *session) {
	if ev.Job != "" && r.id != "" && ev.Job != r.id && ev.Verb != proto.VerbAccepted {
		return // not ours
	}
	if s.attachVerify {
		s.attachVerify = false
		if ev.Verb == proto.VerbRefused {
			s.noReattach = true
			msg := "daemon refused to re-attach; job continues ungoverned"
			r.emit(proto.Event{Verb: proto.VerbInfo, Job: r.id, Reason: ev.Reason, Msg: msg})
			r.closeStream()
			s.streamGone = true
			return
		}
		if ev.Verb == proto.VerbAccepted && ev.Job != "" {
			r.id = ev.Job
		}
		s.lost = false
		s.lostPrinted = false
		r.emit(proto.Event{Verb: proto.VerbInfo, Job: r.id, Msg: "reattached"})
	}
	switch ev.Verb {
	case proto.VerbAccepted:
		if ev.Job != "" {
			r.id = ev.Job
		}
	case proto.VerbQueued, proto.VerbAdmitted, proto.VerbExit, proto.VerbRefused:
		// not meaningful once running
	case proto.VerbKilled:
		if ev.Job == "" {
			ev.Job = r.id
		}
		e := ev
		r.killed = &e
		r.emit(ev)
	case proto.VerbFrozen:
		r.frozen = true
		r.emit(ev)
	case proto.VerbThawed:
		r.frozen = false
		r.emit(ev)
	default:
		r.emit(ev)
	}
}
