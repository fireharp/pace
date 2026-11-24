package daemon

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// maxNoteLines: the event line and its hint.
const maxNoteLines = 2

// noteFaultEveryMS: at most one stderr_note fault per this many ms; the
// drops in between are counted into the next one.
const noteFaultEveryMS = 10_000

// notifyUnitStderr writes a unit's WARN and KILLED lines, with their hints,
// to the stderr of the unit's processes (platform.StderrNotifier; Linux
// only), before the step's signals are sent. A process group started
// outside pace run has no wrapper to print them, and an agent whose command
// is killed would otherwise see only "Terminated".
//
// Only real lines (no mode=: in observe and warn-only nothing is carried
// out) and only processes an action for that unit may reach: members that
// attribution placed in the unit this tick and that pass the actuator's own
// checks (never the daemon, a wrapper, an operating-system process or one
// outside scope_root_pid). Nothing on a stale sample, nothing with
// unmanaged.notify_stderr = false. The backend bounds the time it takes;
// a note it drops (an earlier write still stuck, or past its deadline) is
// a stderr_note fault, so a stalled feature shows in status.
func (d *daemon) notifyUnitStderr(evs []proto.Event) {
	if !d.cfg.NotifyStderr || d.sampleStale || len(evs) == 0 {
		return
	}
	n, ok := d.backend.(platform.StderrNotifier)
	if !ok {
		return
	}
	var notes []platform.StderrNote
	var keys map[string]string // public unit ref -> ownership key
	for i := range evs {
		e := &evs[i]
		if e.Unit == "" || e.Job != "" || e.Mode != "" || (e.Verb != proto.VerbWarn && e.Verb != proto.VerbKilled) {
			continue
		}
		if keys == nil {
			keys = make(map[string]string)
			for k := range d.owned {
				if strings.HasPrefix(k, "u:") {
					keys[proto.UnitRef(k[2:])] = k
				}
			}
		}
		key, ok := keys[e.Unit]
		if !ok {
			continue
		}
		pids := d.notePIDs(key, e.PID)
		text, ok := stderrNoteText(e)
		if len(pids) == 0 || !ok {
			continue
		}
		notes = append(notes, platform.StderrNote{PIDs: pids, Text: text})
	}
	if len(notes) > 0 {
		n.NotifyStderr(notes, d.noteDrops.report(d.fault))
	}
}

// noteDropLimiter rate-limits the stderr_note fault: each drop the backend
// reports is one occurrence, recorded at most once per noteFaultEveryMS
// (the ones held back are counted into the next).
type noteDropLimiter struct {
	mu     sync.Mutex
	lastMS int64
	held   int
}

// report returns the dropped func for one NotifyStderr call (safe from
// any goroutine).
func (l *noteDropLimiter) report(fault func(kind, msg string)) func(string) {
	return func(reason string) {
		now := time.Now().UnixMilli()
		l.mu.Lock()
		if l.lastMS != 0 && now-l.lastMS < noteFaultEveryMS {
			l.held++
			l.mu.Unlock()
			return
		}
		held := l.held
		l.lastMS, l.held = now, 0
		l.mu.Unlock()
		if held > 0 {
			reason += fmt.Sprintf(" (and %d more drop(s) since the last report)", held)
		}
		fault("stderr_note", reason)
	}
}

// notePIDs: the unit's current members an action for key may reach, the
// leader first, then by pid.
func (d *daemon) notePIDs(key string, leader int) []int {
	var out []int
	for _, pid := range d.owned[key] {
		if p, ok := d.proc(pid); !ok || p.State == 'Z' || d.unsafeFor(pid, key) != "" {
			continue
		}
		out = append(out, pid)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i] == leader) != (out[j] == leader) {
			return out[i] == leader
		}
		return out[i] < out[j]
	})
	return out
}

// stderrNoteText renders e as its stderr lines, each with its newline; ok
// is false when a line is longer than a note may carry (never cut: a cut
// line would break the grammar).
func stderrNoteText(e *proto.Event) (string, bool) {
	var sb strings.Builder
	for i, l := range e.Lines() {
		if i >= maxNoteLines || len(l)+1 > platform.StderrNoteMaxLine {
			return "", false
		}
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	return sb.String(), sb.Len() > 0
}
