package wrapper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fireharp/pace/internal/client"
	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
)

// Fail open: the daemon is unreachable after Config.FailOpenWaitMS. The
// command still runs (the prime directive treats daemon absence as a fault,
// not a reason to stop work) but at most Config.FailOpenSlots such commands
// run at once, enforced with flock(2) on <home>/failopen/slot-<n>: the
// kernel drops a lock when its holder dies, so slots never leak. The slot
// file also records the job's process group, and a slot stays taken while
// that group is alive, even after its wrapper is gone (SIGKILLed by a
// harness timeout, or exited leaving background descendants): the bound is
// on ungoverned work, not on wrappers. Nothing is started into
// kernel-critical memory pressure.

const startDaemonHint = "the governor daemon is not running: start it with pace install (or run pace daemon)"

const quickPressureCritical = 4

// failOpenRetryAfterMS is the retry_after of a fail-open refusal: a slot
// frees when another ungoverned job ends, which nothing predicts.
const failOpenRetryAfterMS = 5000

func (r *runner) failOpenSlots() int {
	if n := r.o.Config.FailOpenSlots; n > 0 {
		return n
	}
	return 2
}

// slotDirs lists where slot files may live, preferred first.
func (r *runner) slotDirs() []string {
	var dirs []string
	switch {
	case r.o.Config.Home != "":
		dirs = append(dirs, filepath.Join(r.o.Config.Home, "failopen"))
	case r.o.Config.Socket != "":
		dirs = append(dirs, filepath.Join(filepath.Dir(r.o.Config.Socket), "failopen"))
	}
	return append(dirs, filepath.Join(os.TempDir(), fmt.Sprintf("pace-%d", os.Getuid()), "failopen"))
}

// acquireSlot tries slot-1..slot-n without blocking. It returns the locked
// file and its 1-based number, or (nil, 0, nil) when every slot is held.
// An error means no slot directory is usable at all.
func acquireSlot(dirs []string, n int) (*os.File, int, error) {
	var lastErr error
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			lastErr = err
			continue
		}
		usable := false
		for k := 1; k <= n; k++ {
			f, err := os.OpenFile(filepath.Join(dir, fmt.Sprintf("slot-%d", k)), os.O_RDWR|os.O_CREATE, 0o600)
			if err != nil {
				lastErr = err
				continue
			}
			usable = true
			var lerr error
			cerr := control(f, func(fd int) error {
				lerr = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
				return nil
			})
			if cerr == nil && lerr == nil && !slotJobAlive(f) {
				return f, k, nil
			}
			f.Close() // drops our lock, if we got it
		}
		if usable {
			return nil, 0, nil
		}
	}
	return nil, 0, lastErr
}

// slotJobAlive reports whether the job recorded in a slot file still has a
// live process group of ours (its wrapper may be gone).
func slotJobAlive(f *os.File) bool {
	b := make([]byte, 512)
	n, _ := f.ReadAt(b, 0)
	pgid, startMS := 0, int64(0)
	for _, kv := range strings.Fields(string(b[:n])) {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "pgid":
			pgid, _ = strconv.Atoi(v)
		case "pgid_start_ms":
			startMS, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if pgid <= 1 || syscall.Kill(-pgid, 0) != nil {
		return false // gone, or not ours (EPERM: the id was reused)
	}
	// The leader still running with another start time: the id was reused.
	if startMS > 0 {
		if s := procStartMS(pgid); s > 0 && absDiff(s, startMS) > 1000 {
			return false
		}
	}
	return true
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

func (r *runner) releaseSlot() {
	if r.slot != nil {
		// The job's group is gone: forget it, so that a later group reusing
		// its id never holds the slot.
		if pg := r.rec.PGID; pg <= 0 || syscall.Kill(-pg, 0) == syscall.ESRCH {
			_ = r.slot.Truncate(0)
		}
		r.slot.Close() // closing the last descriptor drops the flock
		r.slot = nil
	}
}

// writeSlotInfo records the holder in the slot file: the wrapper, and once
// spawned, the job's process group (which keeps the slot taken).
func writeSlotInfo(f *os.File, argv []string, pgid int, startMS int64) {
	info := fmt.Sprintf("pid=%d start_ms=%d", os.Getpid(), time.Now().UnixMilli())
	if pgid > 0 {
		info += fmt.Sprintf(" pgid=%d pgid_start_ms=%d", pgid, startMS)
	}
	info += " argv=" + strings.Join(argv, " ") + "\n"
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt([]byte(info), 0)
	}
}

// probeDaemon makes one quick attempt to reach the daemon. A daemon that
// answers its health check with a server error (loop stalled) does not
// count.
func (r *runner) probeDaemon() *client.Client {
	c := client.New(r.o.Config.Socket)
	c.Timeout = 250 * time.Millisecond
	if err := c.Ping(context.Background()); !client.IsDown(err) {
		return c
	}
	c.CloseIdle()
	return nil
}

func (r *runner) runFailOpen(dialErr error) int {
	r.failOpen = true
	r.closeStream()
	r.id = "" // no daemon knows this run: its lines carry no job id
	if dialErr != nil {
		r.rec.Error = dialErr.Error()
	}
	n := r.failOpenSlots()
	start := time.Now()
	deadline := start.Add(r.queueTimeout())
	var announcedFull, announcedCritical bool
	waitReason := proto.ReasonFailOpenFull

	refuse := func(reason string, level string, hint string) int {
		w := time.Since(start).Milliseconds()
		ev := proto.Event{Verb: proto.VerbRefused, Reason: reason, Level: level, WaitedMS: &w,
			RetryAfterMS: proto.I64(failOpenRetryAfterMS), Hint: hint}
		r.refused = &ev
		r.emit(ev)
		return r.finishPace(proto.ExitRefused, OutcomeRefused)
	}

	for {
		f, k, err := acquireSlot(r.slotDirs(), n)
		if err != nil {
			// No usable slot directory anywhere: run rather than block all work.
			r.rec.Error = strings.TrimSpace(r.rec.Error + "; failopen slots: " + err.Error())
		}
		if f != nil || err != nil {
			if r.o.h.quickPressure() >= quickPressureCritical {
				if f != nil {
					f.Close()
				}
				waitReason = proto.ReasonPressure
				if !announcedCritical {
					announcedCritical = true
					r.emit(proto.Event{Verb: proto.VerbDegraded, Reason: proto.ReasonUnreachable, Level: proto.LevelCritical,
						Msg: "memory pressure is critical; waiting before running ungoverned", Hint: startDaemonHint})
				}
				if r.o.NoWait {
					return refuse(proto.ReasonPressure, proto.LevelCritical, "")
				}
			} else {
				r.slot = f
				r.failOpenSlot = fmt.Sprintf("%d/%d", k, n)
				if f != nil {
					writeSlotInfo(f, r.o.Argv, 0, 0)
				}
				r.emitExtra(proto.Event{Verb: proto.VerbDegraded, Reason: proto.ReasonUnreachable, Hint: startDaemonHint},
					"failopen_slot="+r.failOpenSlot)
				return r.runChild(false)
			}
		} else {
			waitReason = proto.ReasonFailOpenFull
			if !announcedFull {
				announcedFull = true
				if !announcedCritical {
					r.emit(proto.Event{Verb: proto.VerbDegraded, Reason: proto.ReasonUnreachable, Hint: startDaemonHint})
				}
				if !r.o.NoWait { // --no-wait refuses at once: nothing waits
					r.emit(proto.Event{Verb: proto.VerbQueued, Reason: proto.ReasonFailOpenFull})
				}
			}
			if r.o.NoWait {
				return refuse(proto.ReasonFailOpenFull, "", "")
			}
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			lvl, why := "", "the fail-open slots stayed taken"
			if waitReason == proto.ReasonPressure {
				lvl, why = proto.LevelCritical, "memory pressure stayed critical"
			}
			return refuse(proto.ReasonQueueTimeout, lvl, "the governor daemon is down and "+why+
				" for the whole queue timeout: start the daemon (pace install, or pace daemon), retry in "+
				units.FormatMS(failOpenRetryAfterMS)+" or raise the queue timeout")
		}
		t := time.NewTimer(min(r.t.FailOpenPoll, rem))
		select {
		case <-t.C:
		case sig := <-r.sigc:
			t.Stop()
			s, _ := sig.(syscall.Signal)
			return r.interrupted(s)
		}
		if c := r.probeDaemon(); c != nil {
			r.failOpen = false
			r.emit(proto.Event{Verb: proto.VerbInfo, Msg: "daemon reachable again; submitting"})
			return r.governed(c, time.Now().Add(r.failOpenWait()))
		}
	}
}
