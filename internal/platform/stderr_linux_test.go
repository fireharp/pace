//go:build linux

package platform

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pipeText is what a pipe or socket gets: the note on a new line.
const pipeText = "\n" + noteText

const noteText = "pace: KILLED policy=pressure used=1.1G free=98M unit=pg:42 pid=42 comm=gen\npace: hint: retry when pace status shows headroom\n"

// startWithStderr starts argv with stderr set to f (stdin from stdin if
// non-nil) and kills it at the end of the test.
func startWithStderr(t *testing.T, f *os.File, stdin *os.File, argv ...string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath(argv[0]); err != nil {
		t.Skipf("no %s: %v", argv[0], err)
	}
	c := exec.Command(argv[0], argv[1:]...)
	c.Stderr = f
	if stdin != nil {
		c.Stdin = stdin
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = c.Process.Kill()
		_ = c.Wait()
	})
	waitFD2(t, c.Process.Pid)
	return c
}

// waitFD2 waits until the child has exec'd (its fd 2 is set up).
func waitFD2(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid) + "/fd/2"); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid %d has no fd 2", pid)
}

// readFor reads what arrives on r within d.
func readFor(t *testing.T, r *os.File, d time.Duration) []byte {
	t.Helper()
	if err := r.SetReadDeadline(time.Now().Add(d)); err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			return out
		}
	}
}

func note(pids ...int) *StderrNote { return &StderrNote{PIDs: pids, Text: noteText} }

// soon is a deadline no test reaches.
func soon() time.Time { return time.Now().Add(time.Minute) }

func outcomes(o []stderrOutcome) string {
	var sb strings.Builder
	for _, x := range o {
		sb.WriteString(strconv.Itoa(x.pid) + ":" + x.kind + ":" + strconv.FormatBool(x.wrote) + ":" + x.skip + " ")
	}
	return sb.String()
}

// pidfdGetfdAllowed: may this process duplicate pid's fd 2 (Yama, seccomp)?
func pidfdGetfdAllowed(pid int) bool {
	pfd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false
	}
	defer unix.Close(pfd)
	fd, err := unix.PidfdGetfd(pfd, 2, 0)
	if err != nil {
		return false
	}
	unix.Close(fd)
	return true
}

// Two processes share one stderr pipe: the reader gets the note once, whole.
func TestStderrNotePipeOnceForSharedTarget(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := startWithStderr(t, w, nil, "sleep", "30")
	b := startWithStderr(t, w, nil, "sleep", "30")
	w.Close()
	if !notifyStderr(procRoot, []StderrNote{*note(a.Process.Pid, b.Process.Pid)}, time.Second, time.Minute, nil) {
		t.Fatal("notifyStderr did not finish within its budget")
	}
	if got := readFor(t, r, 300*time.Millisecond); string(got) != pipeText {
		t.Fatalf("read %q, want the note once: %q", got, pipeText)
	}
	o := writeStderrNote(procRoot, note(a.Process.Pid, b.Process.Pid), soon())
	if len(o) != 2 || !o[0].wrote || o[0].kind != "pipe" || o[1].wrote || o[1].skip != "duplicate target" {
		t.Fatalf("outcomes %s", outcomes(o))
	}
	readFor(t, r, 100*time.Millisecond)
}

// A full pipe does not block: the write fails at once and nothing waits.
func TestStderrNoteFullPipeDoesNotBlock(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := startWithStderr(t, w, nil, "sleep", "30")
	fd := int(w.Fd())
	if err := syscall.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 4096)
	for {
		if _, err := syscall.Write(fd, chunk); err != nil {
			if !errors.Is(err, syscall.EAGAIN) {
				t.Fatal(err)
			}
			break
		}
	}
	for {
		if _, err := syscall.Write(fd, chunk[:1]); err != nil {
			break
		}
	}
	t0 := time.Now()
	o := writeStderrNote(procRoot, note(c.Process.Pid), soon())
	if el := time.Since(t0); el > 500*time.Millisecond {
		t.Fatalf("write to a full pipe took %v", el)
	}
	if len(o) != 1 || o[0].wrote || o[0].kind != "pipe" {
		t.Fatalf("outcomes %s", outcomes(o))
	}
	t0 = time.Now()
	if !notifyStderr(procRoot, []StderrNote{*note(c.Process.Pid)}, 2*time.Second, time.Minute, nil) {
		t.Fatal("notifyStderr on a full pipe did not finish")
	}
	if el := time.Since(t0); el > 500*time.Millisecond {
		t.Fatalf("notifyStderr on a full pipe took %v", el)
	}
	w.Close()
}

// A regular file gets the note after what the process wrote, and the
// process's next write follows the note: through the process's own file
// (pidfd_getfd, where permitted), or, for a file the process opened for
// appending, through a new O_APPEND open. A file not opened for appending
// whose own file cannot be shared gets nothing: the process's next write,
// at its own offset, would overwrite the note.
func TestStderrNoteRegularFile(t *testing.T) {
	for _, c := range []struct {
		appendMode bool
		before     string // what the process wrote before the note
	}{{false, "before\n"}, {true, "before\n"}, {false, "partial"}, {true, "partial"}} {
		t.Run("append="+strconv.FormatBool(c.appendMode)+"/"+strings.TrimSpace(c.before), func(t *testing.T) {
			dir := t.TempDir()
			fl := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			if c.appendMode {
				fl = os.O_WRONLY | os.O_CREATE | os.O_APPEND
			}
			f, err := os.OpenFile(dir+"/err.log", fl, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			inR, inW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer inW.Close()
			cmd := "printf '" + strings.ReplaceAll(c.before, "\n", "\\n") + "' >&2; read x; echo after >&2; exec sleep 30"
			p := startWithStderr(t, f, inR, "sh", "-c", cmd)
			f.Close()
			inR.Close()
			for i := 0; i < 200; i++ {
				if b, _ := os.ReadFile(dir + "/err.log"); string(b) == c.before {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			shared := pidfdGetfdAllowed(p.Process.Pid)
			o := writeStderrNote(procRoot, note(p.Process.Pid), soon())
			wantWrote := shared || c.appendMode
			if len(o) != 1 || o[0].kind != "file" || o[0].wrote != wantWrote {
				t.Fatalf("outcomes %s (pidfd_getfd permitted: %v)", outcomes(o), shared)
			}
			want := c.before
			if wantWrote {
				if !strings.HasSuffix(c.before, "\n") {
					want += "\n" // the line starts on a new line
				}
				want += noteText
			}
			if b, _ := os.ReadFile(dir + "/err.log"); string(b) != want {
				t.Fatalf("file %q, want %q", b, want)
			}
			if _, err := inW.Write([]byte("go\n")); err != nil {
				t.Fatal(err)
			}
			want += "after\n"
			var b []byte
			for i := 0; i < 200; i++ {
				if b, _ = os.ReadFile(dir + "/err.log"); bytes.Contains(b, []byte("after")) {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if string(b) != want {
				t.Fatalf("after the process's next write the file is %q, want %q", b, want)
			}
			if !wantWrote {
				t.Logf("pidfd_getfd not permitted here: a file not opened for appending gets no note")
			}
		})
	}
}

// A regular file the process has open for reading too (O_RDWR: it may be
// a data file that landed on fd 2) gets nothing, nor does a file on a
// pseudo file system (procfs here: /proc/self/comm would rename us).
func TestStderrNoteFileSkips(t *testing.T) {
	dir := t.TempDir()
	rw, err := os.OpenFile(dir+"/data.db", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	c := startWithStderr(t, rw, nil, "sleep", "30")
	rw.Close()
	o := writeStderrNote(procRoot, note(c.Process.Pid), soon())
	if len(o) != 1 || o[0].kind != "file" || o[0].wrote || !strings.HasPrefix(o[0].skip, "file open for reading too") {
		t.Fatalf("O_RDWR file: %s", outcomes(o))
	}
	if b, _ := os.ReadFile(dir + "/data.db"); len(b) != 0 {
		t.Fatalf("O_RDWR file written: %q", b)
	}
	fifo := dir + "/ctl.fifo"
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	ff, err := os.OpenFile(fifo, os.O_RDWR, 0) // O_RDWR never blocks on a FIFO
	if err != nil {
		t.Fatal(err)
	}
	cf := startWithStderr(t, ff, nil, "sleep", "30")
	ff.Close()
	o = writeStderrNote(procRoot, note(cf.Process.Pid), soon())
	if len(o) != 1 || o[0].kind != "pipe" || o[0].wrote || !strings.HasPrefix(o[0].skip, "FIFO open for reading too") {
		t.Fatalf("O_RDWR FIFO: %s", outcomes(o))
	}
	before, _ := os.ReadFile("/proc/self/comm")
	comm, err := os.OpenFile("/proc/self/comm", os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("open /proc/self/comm: %v", err)
	}
	cp := startWithStderr(t, comm, nil, "sleep", "30")
	comm.Close()
	o = writeStderrNote(procRoot, note(cp.Process.Pid), soon())
	if len(o) != 1 || o[0].kind != "file" || o[0].wrote || o[0].skip != "file on a pseudo file system (procfs)" {
		t.Fatalf("procfs file: %s", outcomes(o))
	}
	if after, _ := os.ReadFile("/proc/self/comm"); string(after) != string(before) {
		t.Fatalf("comm changed from %q to %q", before, after)
	}
}

// What is not written to: a character device (/dev/null), a missing pid, pid 1 and below, a fd 2 open only for
// reading (a pipe's read end), and a bad text.
func TestStderrNoteSkips(t *testing.T) {
	null, err := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	cn := startWithStderr(t, null, nil, "sleep", "30")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cr := startWithStderr(t, r, nil, "sleep", "30")
	missing := 1 << 30 // above pid_max
	o := writeStderrNote(procRoot, note(cn.Process.Pid, missing, 1, 0, cr.Process.Pid), soon())
	if len(o) != 5 {
		t.Fatalf("outcomes %s", outcomes(o))
	}
	if o[0].wrote || o[0].kind != "chr" {
		t.Fatalf("/dev/null: %s", outcomes(o[:1]))
	}
	for _, x := range o[1:4] {
		if x.wrote || x.kind != "" || x.skip == "" {
			t.Fatalf("missing or low pid written: %s", outcomes(o))
		}
	}
	if o[4].wrote || o[4].skip != "fd 2 not open for writing" {
		t.Fatalf("read end: %s", outcomes(o[4:]))
	}
	w.Close()
	if got := readFor(t, r, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("a note reached the pipe the process reads its fd 2 from: %q", got)
	}
	if got := writeStderrNote(procRoot, &StderrNote{PIDs: []int{cn.Process.Pid}, Text: "no newline"}, soon()); got != nil {
		t.Fatalf("a text without its newline was tried: %s", outcomes(got))
	}
	if got := writeStderrNote(procRoot, &StderrNote{PIDs: []int{cn.Process.Pid}, Text: strings.Repeat("x", StderrNoteMaxLine) + "\n"}, soon()); got != nil {
		t.Fatalf("an over-long line was tried: %s", outcomes(got))
	}
	if !notifyStderr(procRoot, []StderrNote{*note(missing)}, time.Second, time.Minute, nil) {
		t.Fatal("a missing pid did not finish at once")
	}
}

// While an earlier note is still being written (stuck), a new one is
// dropped at once, and the drop is reported.
func TestStderrNoteBusyDrops(t *testing.T) {
	if !stderrNoteBusy.CompareAndSwap(false, true) {
		t.Fatal("busy at start")
	}
	var why []string
	t0 := time.Now()
	ok := notifyStderr(procRoot, []StderrNote{*note(os.Getpid())}, time.Second, time.Minute, func(r string) { why = append(why, r) })
	stderrNoteBusy.Store(false)
	if ok || time.Since(t0) > 100*time.Millisecond {
		t.Fatalf("busy notify ok=%v after %v", ok, time.Since(t0))
	}
	if len(why) != 1 || !strings.Contains(why[0], "previous stderr write has not finished") {
		t.Fatalf("drop reported as %q", why)
	}
}

// Past the goroutine's deadline nothing more is written: every remaining
// pid is dropped (a late write must never land on a reused pid), and the
// drop is reported.
func TestStderrNoteDeadlineDrops(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a := startWithStderr(t, w, nil, "sleep", "30")
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	b := startWithStderr(t, w2, nil, "sleep", "30")
	w.Close()
	w2.Close()
	o := writeStderrNote(procRoot, note(a.Process.Pid, b.Process.Pid), time.Now())
	if len(o) != 2 || o[0].wrote || o[0].skip != skipDeadline || o[1].wrote || o[1].skip != skipDeadline {
		t.Fatalf("outcomes past the deadline %s", outcomes(o))
	}
	var why []string
	done := make(chan struct{})
	if !notifyStderr(procRoot, []StderrNote{*note(a.Process.Pid), *note(b.Process.Pid)}, time.Second, 0, func(r string) { why = append(why, r); close(done) }) {
		t.Fatal("notifyStderr past its deadline did not finish at once")
	}
	<-done
	if len(why) != 1 || !strings.Contains(why[0], "deadline") || !strings.Contains(why[0], "2 process(es)") {
		t.Fatalf("drop reported as %q", why)
	}
	if got := readFor(t, r, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("written past the deadline: %q", got)
	}
	if got := readFor(t, r2, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("written past the deadline: %q", got)
	}
}

// readFD reads what arrives on the non-blocking fd within d.
func readFD(t *testing.T, fd int, d time.Duration) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, 8192)
	end := time.Now().Add(d)
	for {
		left := time.Until(end)
		if left <= 0 {
			return out
		}
		pfd := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, err := unix.Poll(pfd, int(left/time.Millisecond)+1)
		if err != nil && !errors.Is(err, unix.EINTR) {
			t.Fatal(err)
		}
		if n <= 0 {
			continue
		}
		m, err := unix.Read(fd, buf)
		if m > 0 {
			out = append(out, buf[:m]...)
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) || m == 0 && err == nil {
			return out
		}
	}
}

// A socket (what a node harness such as Claude Code may give a child as its
// stderr) is written through the process's own socket, where pidfd_getfd
// is permitted.
func TestStderrNoteSocket(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	mine := fds[0]
	defer unix.Close(mine)
	if err := unix.SetNonblock(mine, true); err != nil {
		t.Fatal(err)
	}
	theirs := os.NewFile(uintptr(fds[1]), "theirs")
	c := startWithStderr(t, theirs, nil, "sleep", "30")
	theirs.Close()
	o := writeStderrNote(procRoot, note(c.Process.Pid), soon())
	if len(o) != 1 || o[0].kind != "socket" {
		t.Fatalf("outcomes %s", outcomes(o))
	}
	if !o[0].wrote {
		if pidfdGetfdAllowed(c.Process.Pid) {
			t.Fatalf("pidfd_getfd permitted, yet not written: %s", outcomes(o))
		}
		t.Skipf("pidfd_getfd not permitted here (Yama, seccomp): a socket stderr gets no note")
	}
	if got := readFD(t, mine, 300*time.Millisecond); string(got) != pipeText {
		t.Fatalf("socket got %q", got)
	}
}

// A socket that is also the process's fd 0 (an inetd-style two-way
// channel) gets nothing, and neither does a socket that is not AF_UNIX (a
// TCP connection: the text would land in the peer's protocol).
func TestStderrNoteSocketSkips(t *testing.T) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	mine := fds[0]
	defer unix.Close(mine)
	if err := unix.SetNonblock(mine, true); err != nil {
		t.Fatal(err)
	}
	theirs := os.NewFile(uintptr(fds[1]), "theirs")
	c := startWithStderr(t, theirs, theirs, "sleep", "30")
	theirs.Close()
	o := writeStderrNote(procRoot, note(c.Process.Pid), soon())
	if len(o) != 1 || o[0].kind != "socket" || o[0].wrote || o[0].skip != "socket is fd 0 too (a two-way channel, inetd style)" {
		t.Fatalf("fd 0 = fd 2 socket: %s", outcomes(o))
	}
	if got := readFD(t, mine, 100*time.Millisecond); len(got) != 0 {
		t.Fatalf("two-way socket got %q", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback tcp: %v", err)
	}
	defer ln.Close()
	acc := make(chan net.Conn, 1)
	go func() {
		cn, err := ln.Accept()
		if err == nil {
			acc <- cn
		}
		close(acc)
	}()
	dc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	peer := <-acc
	if peer == nil {
		t.Fatal("no accepted connection")
	}
	defer peer.Close()
	tf, err := dc.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	ct := startWithStderr(t, tf, nil, "sleep", "30")
	tf.Close()
	o = writeStderrNote(procRoot, note(ct.Process.Pid), soon())
	if len(o) != 1 || o[0].kind != "socket" || o[0].wrote {
		t.Fatalf("tcp socket: %s", outcomes(o))
	}
	if pidfdGetfdAllowed(ct.Process.Pid) && o[0].skip != "not an AF_UNIX socket" {
		t.Fatalf("tcp socket skipped for another reason: %s", outcomes(o))
	}
	_ = peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, _ := peer.Read(make([]byte, 64)); n != 0 {
		t.Fatalf("tcp peer got %d bytes", n)
	}
}

func TestValidStderrText(t *testing.T) {
	for _, c := range []struct {
		s  string
		ok bool
	}{
		{noteText, true},
		{"", false},
		{"pace: X", false},
		{strings.Repeat("x", StderrNoteMaxLine-1) + "\n", true},
		{strings.Repeat("x", StderrNoteMaxLine) + "\n", false},
		{strings.Repeat(strings.Repeat("x", 500)+"\n", 9), false}, // over PIPE_BUF
		{strings.Repeat(strings.Repeat("x", 511)+"\n", 8), false}, // PIPE_BUF, but not with its leading newline
		{strings.Repeat(strings.Repeat("x", 511)+"\n", 7) + strings.Repeat("x", 510) + "\n", true},
	} {
		if got := validStderrText(c.s); got != c.ok {
			t.Errorf("validStderrText(%.20q...) = %v, want %v", c.s, got, c.ok)
		}
	}
}

// The backend's own budget bounds what a step waits.
func TestNotifyStderrBackendBudget(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	c := startWithStderr(t, w, nil, "sleep", "30")
	w.Close()
	b := &linuxBackend{}
	t0 := time.Now()
	b.NotifyStderr([]StderrNote{*note(c.Process.Pid)}, nil)
	if el := time.Since(t0); el > stderrNoteBudget+50*time.Millisecond {
		t.Fatalf("NotifyStderr took %v", el)
	}
	if got := readFor(t, r, 300*time.Millisecond); string(got) != pipeText {
		t.Fatalf("read %q", got)
	}
}

// A pipe's read and write ends are one inode: a member reading its fd 2
// from the pipe does not use up the copy that a member writing to it gets.
func TestStderrNoteReadEndDoesNotHideWriter(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cr := startWithStderr(t, r, nil, "sleep", "30")
	cw := startWithStderr(t, w, nil, "sleep", "30")
	w.Close()
	o := writeStderrNote(procRoot, note(cr.Process.Pid, cw.Process.Pid), soon())
	if len(o) != 2 || o[0].wrote || o[0].skip != "fd 2 not open for writing" || !o[1].wrote {
		t.Fatalf("outcomes %s", outcomes(o))
	}
	// Handing r to the child made it blocking (Fd): poll it instead.
	rfd := int(r.Fd())
	if err := unix.SetNonblock(rfd, true); err != nil {
		t.Fatal(err)
	}
	if got := readFD(t, rfd, 300*time.Millisecond); string(got) != pipeText {
		t.Fatalf("read %q", got)
	}
}
