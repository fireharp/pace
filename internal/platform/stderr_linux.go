//go:build linux

package platform

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// Governor lines on an unmanaged process's own stderr (StderrNotifier).
//
// A process group started outside pace run has no wrapper stream: when pace
// warns or kills it, the agent that ran it would see only "Terminated" (exit
// 143). So the daemon writes the WARN or KILLED line and its hint to the
// stderr of the unit's processes, right before it signals them, through
// /proc/<pid>/fd/2:
//
//   - each distinct target (device and inode: a pipe several members share
//     gets one copy) gets the whole note in one write, starting on a new
//     line (below);
//   - only a target the process has open for writing as fd 2 (fdinfo flags),
//     and only a pipe or FIFO, a regular file or an AF_UNIX socket. Nothing
//     else: never a character device, so never a terminal (a terminal may be
//     shared with a protected or interactive process, such as the agent's own
//     TUI; people in terminals see pace status or PaceBar), nor /dev/null, a
//     console or a serial port;
//   - a pipe or FIFO is opened anew (O_WRONLY|O_APPEND|O_NONBLOCK|O_NOCTTY|
//     O_CLOEXEC: a full pipe makes the write fail, never wait), so the
//     process's own file flags are never changed. The text gets a "\n" in
//     front (whatever the process wrote last may be a partial line), and it
//     fits PIPE_BUF with it, so a pipe takes it whole or not at all;
//   - a regular file only when the process opened it write-only (an O_RDWR
//     fd 2 may be a data file that landed on fd 2, for a process started
//     with stderr closed), and never on a pseudo file system (procfs, sysfs,
//     cgroupfs and the like, whose files parse what is written as commands:
//     fstatfs). It is written through the process's own open file
//     (pidfd_getfd), so the line lands at its current offset and the
//     process's next write follows it. Where that is not permitted (Docker's
//     default seccomp profile; Yama ptrace_scope >= 1 for a process that is
//     not the daemon's descendant) the file is opened anew with O_APPEND, but
//     only when the process opened it for appending too (">>", fdinfo
//     O_APPEND): otherwise its next write, at its own offset, would overwrite
//     the line and leave a broken piece of it, so such a file gets nothing.
//     The byte before the write position is read (pread on a read-only open
//     of the same file): only when it is not a newline does the text get a
//     "\n" in front; a byte that cannot be read counts as not a newline;
//   - a socket cannot be opened through /proc: it is written through the
//     process's own socket (pidfd_getfd) with MSG_DONTWAIT|MSG_NOSIGNAL, or
//     not at all, and only an AF_UNIX one (SO_DOMAIN: a Node harness's stdio
//     socketpair) that is not also the process's fd 0 (same device and
//     inode: an inetd-style two-way channel, where the text would land in
//     the peer's protocol). Like a pipe it gets a "\n" in front. A stream
//     socket whose buffer is nearly full may take only part of the text
//     (sendmsg returns fewer bytes): that counts as not written, and the part
//     already sent cannot be taken back;
//   - nothing is retried and every error is ignored. All of it runs on its
//     own goroutine, and the caller waits for it at most stderrNoteBudget: a
//     write stuck on a hung file system never delays the signal by more. The
//     goroutine has a hard deadline (stderrNoteDeadline from its start):
//     after it, no /proc/<pid> path or pid is used (each lookup checks it
//     first) and nothing more is written, and
//     every remaining pid and note is dropped, so a late write can never
//     land on an unrelated process that reused one of the pids. While such a
//     write is still stuck, later notes are dropped. Each drop (busy, or past
//     the deadline) is reported to the caller's dropped func.

const (
	stderrNoteBudget   = 5 * time.Millisecond
	stderrNoteDeadline = time.Second
	stderrNoteMaxPIDs  = 64
	stderrNoteMaxText  = 4096 // PIPE_BUF on Linux, the leading "\n" included
)

// skipDeadline is the outcome of a pid reached after the deadline.
const skipDeadline = "deadline passed"

// stderrNoteBusy is set while a NotifyStderr goroutine runs.
var stderrNoteBusy atomic.Bool

// NotifyStderr implements StderrNotifier.
func (b *linuxBackend) NotifyStderr(notes []StderrNote, dropped func(reason string)) {
	notifyStderr(procRoot, notes, stderrNoteBudget, stderrNoteDeadline, dropped)
}

// notifyStderr writes notes on a goroutine and waits for it at most budget;
// the goroutine stops writing deadline after it started. It reports whether
// the writes finished within budget (false also when an earlier call's
// goroutine is still running, and nothing was tried). dropped (may be nil)
// is called, from either goroutine, when notes or pids are dropped.
func notifyStderr(root string, notes []StderrNote, budget, deadline time.Duration, dropped func(string)) bool {
	if len(notes) == 0 {
		return true
	}
	if !stderrNoteBusy.CompareAndSwap(false, true) {
		if dropped != nil {
			dropped(fmt.Sprintf("%d stderr note(s) for %d process(es) dropped: the previous stderr write has not finished (stuck on a hung file system?)", len(notes), countNotePIDs(notes)))
		}
		return false
	}
	own := make([]StderrNote, len(notes))
	for i, n := range notes {
		own[i] = StderrNote{PIDs: append([]int(nil), n.PIDs...), Text: n.Text}
	}
	end := time.Now().Add(deadline)
	done := make(chan struct{})
	go func() {
		defer stderrNoteBusy.Store(false)
		defer close(done)
		late := 0
		for i := range own {
			for _, o := range writeStderrNote(root, &own[i], end) {
				if o.skip == skipDeadline {
					late++
				}
			}
		}
		if late > 0 && dropped != nil {
			dropped(fmt.Sprintf("stderr note dropped for %d process(es): the writes passed their %v deadline (a write was stuck)", late, deadline))
		}
	}()
	t := time.NewTimer(budget)
	defer t.Stop()
	select {
	case <-done:
		return true
	case <-t.C:
		return false
	}
}

func countNotePIDs(notes []StderrNote) int {
	n := 0
	for _, x := range notes {
		n += min(len(x.PIDs), stderrNoteMaxPIDs)
	}
	return n
}

// stderrOutcome is what writeStderrNote did for one pid (for tests).
type stderrOutcome struct {
	pid   int
	kind  string // "pipe", "file", "socket", "chr", "other", ""
	wrote bool
	skip  string // why nothing was written
	text  string // what was written (with any leading "\n")
}

// validStderrText: whole lines, each at most StderrNoteMaxLine bytes with
// its newline, all of it with a leading "\n" within PIPE_BUF.
func validStderrText(s string) bool {
	if s == "" || len(s)+1 > stderrNoteMaxText || s[len(s)-1] != '\n' {
		return false
	}
	for len(s) > 0 {
		i := strings.IndexByte(s, '\n')
		if i+1 > StderrNoteMaxLine {
			return false
		}
		s = s[i+1:]
	}
	return true
}

// writeStderrNote writes n to each of its pids' stderr; a pid reached at or
// after end gets nothing (skipDeadline), and neither do the rest.
func writeStderrNote(root string, n *StderrNote, end time.Time) []stderrOutcome {
	if !validStderrText(n.Text) {
		return nil
	}
	text := []byte(n.Text)
	var out []stderrOutcome
	seen := make(map[[2]uint64]bool, 2)
	for i, pid := range n.PIDs {
		if i >= stderrNoteMaxPIDs {
			break
		}
		o := stderrOutcome{pid: pid}
		writeStderrOne(root, pid, text, seen, end, &o)
		out = append(out, o)
	}
	return out
}

// late: the goroutine's deadline has passed.
func late(end time.Time) bool { return !time.Now().Before(end) }

func writeStderrOne(root string, pid int, text []byte, seen map[[2]uint64]bool, end time.Time, o *stderrOutcome) {
	if late(end) {
		o.skip = skipDeadline
		return
	}
	if pid <= 1 {
		o.skip = "pid <= 1"
		return
	}
	dir := root + "/" + strconv.Itoa(pid)
	path := dir + "/fd/2"
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		o.skip = "no fd 2: " + err.Error()
		return
	}
	id := [2]uint64{uint64(st.Dev), st.Ino}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFIFO:
		o.kind = "pipe"
	case unix.S_IFREG:
		o.kind = "file"
	case unix.S_IFSOCK:
		o.kind = "socket"
	case unix.S_IFCHR:
		o.kind = "chr"
	default:
		o.kind = "other"
	}
	switch o.kind {
	case "chr":
		o.skip = "a character device (a terminal, /dev/null, a console): never written to"
		return
	case "other":
		o.skip = "not a pipe, regular file or socket"
		return
	}
	if seen[id] {
		o.skip = "duplicate target"
		return
	}
	flags, ok := fdFlags(dir + "/fdinfo/2")
	acc := flags & unix.O_ACCMODE
	if !ok || (acc != unix.O_WRONLY && acc != unix.O_RDWR) {
		o.skip = "fd 2 not open for writing"
		return
	}
	switch o.kind {
	case "pipe":
		if acc != unix.O_WRONLY {
			o.skip = "FIFO open for reading too (O_RDWR): it may be the process's own input"
			return
		}
	case "file":
		if acc != unix.O_WRONLY {
			o.skip = "file open for reading too (O_RDWR): it may be the process's data, not its log"
			return
		}
		if fs, pseudo := pseudoFS(path); pseudo {
			o.skip = "file on a pseudo file system (" + fs + ")"
			return
		}
	case "socket":
		if sameAsFD0(dir, &st) {
			o.skip = "socket is fd 0 too (a two-way channel, inetd style)"
			return
		}
	}
	if late(end) { // the /proc lookups above may have been slow
		o.skip = skipDeadline
		return
	}
	seen[id] = true // tried once, whatever comes of it: never a second copy
	nlText := append([]byte{'\n'}, text...)
	switch o.kind {
	case "pipe":
		o.wrote, o.skip = writeReopened(path, &st, nlText, end)
		if o.wrote {
			o.text = string(nlText)
		}
	case "file":
		o.wrote, o.skip, o.text = writeFile(pid, path, &st, text, flags, end)
	case "socket":
		o.wrote, o.skip = writeSocket(pid, &st, nlText, end)
		if o.wrote {
			o.text = string(nlText)
		}
	}
	if !o.wrote && o.skip == "" {
		o.skip = "write failed"
		if late(end) {
			o.skip = skipDeadline
		}
	}
}

// pseudoFS: path is on a pseudo file system whose files are interfaces to
// the kernel (or to a daemon), not storage; a statfs that fails counts as
// one.
func pseudoFS(path string) (string, bool) {
	var sf unix.Statfs_t
	if err := unix.Statfs(path, &sf); err != nil {
		return "statfs: " + err.Error(), true
	}
	switch uint64(sf.Type) {
	case unix.PROC_SUPER_MAGIC:
		return "procfs", true
	case unix.SYSFS_MAGIC:
		return "sysfs", true
	case unix.CGROUP_SUPER_MAGIC, unix.CGROUP2_SUPER_MAGIC:
		return "cgroupfs", true
	case unix.DEBUGFS_MAGIC, unix.TRACEFS_MAGIC:
		return "debugfs", true
	case unix.SECURITYFS_MAGIC, unix.SELINUX_MAGIC, unix.SMACK_MAGIC:
		return "securityfs", true
	case unix.BPF_FS_MAGIC, unix.PSTOREFS_MAGIC, unix.EFIVARFS_MAGIC, unix.BINFMTFS_MAGIC,
		unix.RDTGROUP_SUPER_MAGIC, unix.USBDEVICE_SUPER_MAGIC, unix.XENFS_SUPER_MAGIC,
		unix.OPENPROM_SUPER_MAGIC, unix.BINDERFS_SUPER_MAGIC, unix.NSFS_MAGIC,
		unix.ANON_INODE_FS_MAGIC, unix.PID_FS_MAGIC,
		0x62656570, // configfs
		0x65735543: // fusectl
		return fmt.Sprintf("magic %#x", uint64(sf.Type)), true
	}
	return "", false
}

// sameAsFD0: the process's fd 0 is the object st describes.
func sameAsFD0(dir string, st *unix.Stat_t) bool {
	var s0 unix.Stat_t
	return unix.Stat(dir+"/fd/0", &s0) == nil && s0.Dev == st.Dev && s0.Ino == st.Ino
}

// fdFlags reads the (octal) flags of an fdinfo file.
func fdFlags(fdinfo string) (uint64, bool) {
	b, ok := readSmall(fdinfo)
	if !ok {
		return 0, false
	}
	i := bytes.Index(b, []byte("flags:"))
	if i < 0 {
		return 0, false
	}
	b = bytes.TrimLeft(b[i+len("flags:"):], " \t")
	j := 0
	for j < len(b) && b[j] >= '0' && b[j] <= '7' {
		j++
	}
	if j == 0 {
		return 0, false
	}
	flags, err := strconv.ParseUint(string(b[:j]), 8, 64)
	return flags, err == nil
}

// readSmall reads the first KiB of a proc file.
func readSmall(path string) ([]byte, bool) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, false
	}
	defer unix.Close(fd)
	buf := make([]byte, 1024)
	n, err := unix.Read(fd, buf)
	if err != nil || n <= 0 {
		return nil, false
	}
	return buf[:n], true
}

// sameTarget: fd refers to the object stat'ed through /proc.
func sameTarget(fd int, st *unix.Stat_t) bool {
	var f unix.Stat_t
	return unix.Fstat(fd, &f) == nil && f.Dev == st.Dev && f.Ino == st.Ino && f.Mode&unix.S_IFMT == st.Mode&unix.S_IFMT
}

// openAnew opens path (a /proc/<pid>/fd link) with flags and checks it is
// still the object st describes; never at or after end.
func openAnew(path string, st *unix.Stat_t, flags int, end time.Time) (int, bool) {
	if late(end) {
		return -1, false
	}
	fd, err := unix.Open(path, flags|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, false
	}
	if !sameTarget(fd, st) {
		unix.Close(fd)
		return -1, false
	}
	return fd, true
}

// sharedFD duplicates the process's own fd 2 (pidfd_getfd: the same open
// file, offset and flags), checked to be the object st describes; never
// at or after end.
func sharedFD(pid int, st *unix.Stat_t, end time.Time) (int, bool) {
	if late(end) {
		return -1, false
	}
	pfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return -1, false
	}
	fd, err := unix.PidfdGetfd(pfd, 2, 0)
	unix.Close(pfd)
	if err != nil {
		return -1, false
	}
	if !sameTarget(fd, st) {
		unix.Close(fd)
		return -1, false
	}
	return fd, true
}

// writeReopened opens a pipe or FIFO anew through /proc and writes text
// once (it fits PIPE_BUF: whole or not at all).
func writeReopened(path string, st *unix.Stat_t, text []byte, end time.Time) (bool, string) {
	fd, ok := openAnew(path, st, unix.O_WRONLY|unix.O_APPEND, end)
	if !ok {
		return false, ""
	}
	defer unix.Close(fd)
	if late(end) {
		return false, skipDeadline
	}
	n, err := unix.Write(fd, text)
	return err == nil && n == len(text), ""
}

// writeFile writes text to a regular file: through the process's own open
// file where pidfd_getfd is permitted, else, when the process appends to
// it, through a new O_APPEND open. The text gets a "\n" in front when the
// byte before the write position is not a newline. It returns what was
// written.
func writeFile(pid int, path string, st *unix.Stat_t, text []byte, flags uint64, end time.Time) (bool, string, string) {
	fd, shared := sharedFD(pid, st, end)
	appending := true
	if shared {
		fl, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		appending = err == nil && fl&unix.O_APPEND != 0
	} else {
		if late(end) {
			return false, skipDeadline, ""
		}
		if flags&unix.O_APPEND == 0 {
			return false, "file not open for appending, and its own file cannot be shared", ""
		}
		var ok bool
		if fd, ok = openAnew(path, st, unix.O_WRONLY|unix.O_APPEND, end); !ok {
			return false, "", ""
		}
	}
	defer unix.Close(fd)
	var pos int64
	if appending {
		var f unix.Stat_t
		if unix.Fstat(fd, &f) == nil {
			pos = f.Size
		}
	} else if p, err := unix.Seek(fd, 0, unix.SEEK_CUR); err == nil {
		pos = p
	}
	if pos > 0 && !newlineAt(path, st, pos-1, end) {
		text = append([]byte{'\n'}, text...)
	}
	if late(end) {
		return false, skipDeadline, ""
	}
	n, err := unix.Write(fd, text)
	if err != nil || n != len(text) {
		return false, "", ""
	}
	return true, "", string(text)
}

// newlineAt: the byte at off of the file st describes is a newline (read
// through a read-only open of it; a byte that cannot be read is not).
func newlineAt(path string, st *unix.Stat_t, off int64, end time.Time) bool {
	fd, ok := openAnew(path, st, unix.O_RDONLY, end)
	if !ok {
		return false
	}
	defer unix.Close(fd)
	var b [1]byte
	n, err := unix.Pread(fd, b[:], off)
	return err == nil && n == 1 && b[0] == '\n'
}

// writeSocket sends text through the process's own socket (pidfd_getfd)
// with MSG_DONTWAIT|MSG_NOSIGNAL, only when it is an AF_UNIX socket. A
// partial send counts as not written.
func writeSocket(pid int, st *unix.Stat_t, text []byte, end time.Time) (bool, string) {
	fd, ok := sharedFD(pid, st, end)
	if !ok {
		if late(end) {
			return false, skipDeadline
		}
		return false, "its own socket cannot be shared (pidfd_getfd not permitted)"
	}
	defer unix.Close(fd)
	if d, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_DOMAIN); err != nil || d != unix.AF_UNIX {
		return false, "not an AF_UNIX socket"
	}
	if late(end) {
		return false, skipDeadline
	}
	n, err := unix.SendmsgN(fd, text, nil, nil, unix.MSG_DONTWAIT|unix.MSG_NOSIGNAL)
	if err != nil {
		return false, ""
	}
	if n != len(text) {
		return false, "partial send (" + strconv.Itoa(n) + " of " + strconv.Itoa(len(text)) + " bytes): counted as not written"
	}
	return true, ""
}
