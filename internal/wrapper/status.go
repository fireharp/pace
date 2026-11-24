package wrapper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// maxStatusEvents caps the events kept for the status file.
const maxStatusEvents = 32

// statusRecord is the final JSON written to the status file
// (--status-file or $PACE_STATUS_FILE): the run's final proto.JobStatus
// (same keys: id, class, state, killed_by, peak_bytes, ...) plus what only
// the wrapper knows (outcome, source, signal, ...). JobStatus fields the
// wrapper cannot know itself (class defaults, peak, budget) come from the
// daemon's view of the job when it is reachable at the end.
type statusRecord struct {
	V int `json:"v"`
	proto.JobStatus
	// ExitCode and Events shadow the JobStatus fields so that they are
	// always present: the exit code pace run returned, and the governor
	// events this run printed (oldest first).
	ExitCode int           `json:"exit_code"`
	Events   []proto.Event `json:"events"`

	Outcome      string `json:"outcome"`          // admitted|refused|killed|degraded|error|interrupted
	Source       string `json:"source"`           // child|pace
	Signal       int    `json:"signal,omitempty"` // child (or wrapper) died of this signal
	Reason       string `json:"reason,omitempty"` // REFUSED reason
	AdmitTS      int64  `json:"admit_ts,omitempty"`
	OutputBytes  int64  `json:"output_bytes"`
	FailOpenSlot string `json:"failopen_slot,omitempty"`
	Forwarded    []int  `json:"forwarded_signals,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (s *statusRecord) addEvent(ev proto.Event) {
	if len(s.Events) >= maxStatusEvents {
		copy(s.Events, s.Events[1:])
		s.Events = s.Events[:maxStatusEvents-1]
	}
	s.Events = append(s.Events, ev)
}

func (r *runner) statusPath() string {
	if r.o.StatusFile != "" {
		return r.o.StatusFile
	}
	return getenv(r.env, EnvStatusFile)
}

// daemonJob asks the daemon for its view of the job (class defaults,
// budget, peak, stage), bounded by PostTimeout. ok is false when there is
// no governed job or the daemon does not answer.
func (r *runner) daemonJob() (proto.JobStatus, bool) {
	if r.c == nil || r.id == "" || r.failOpen {
		return proto.JobStatus{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.t.PostTimeout)
	defer cancel()
	js, err := r.c.Job(ctx, r.id)
	if err != nil || js.ID != r.id {
		return proto.JobStatus{}, false
	}
	return js, true
}

// writeStatus writes the status file atomically (temp file + rename in the
// same directory). Failure is reported as an INFO line, never fatal. It is
// called before the final EXIT line is printed, so that line stays last.
func (r *runner) writeStatus(outcome string, code int, source string) {
	path := r.statusPath()
	if path == "" {
		return
	}
	rec := r.rec
	if js, ok := r.daemonJob(); ok {
		rec.JobStatus = js
	} else {
		rec.JobStatus = proto.JobStatus{Class: r.o.Class, EstimateBytes: r.o.EstimateBytes, BudgetBytes: r.o.BudgetBytes}
		if rec.Class == "" {
			rec.Class = r.o.Config.Params.DefaultClass
		}
		if r.o.Priority != nil {
			rec.Priority = *r.o.Priority
		}
	}
	rec.JobStatus.Events = nil // shadowed by rec.Events
	rec.JobStatus.ExitCode = nil
	rec.ID = r.id
	rec.State = policy.JobDone
	rec.Argv = r.o.Argv
	rec.PID, rec.PGID = r.rec.PID, r.rec.PGID
	rec.ReservedBytes, rec.Position = 0, 0
	rec.SubmitTS = r.submitMS
	rec.StartTS = r.rec.StartTS
	rec.EndTS = time.Now().UnixMilli()
	rec.KilledBy = ""
	if r.killed != nil {
		rec.KilledBy = r.killed.Policy
		rec.Stage = policy.StageKilled
	} else if rec.Stage == policy.StageTerminating || rec.Stage == policy.StageKilled {
		rec.Stage = "" // a daemon-side kill this run did not end by
	}
	rec.ExitCode = code
	rec.Outcome = outcome
	rec.Source = source
	rec.AdmitTS = r.admitMS
	rec.OutputBytes = r.outputBytes()
	rec.FailOpenSlot = r.failOpenSlot
	if r.refused != nil {
		rec.Reason = r.refused.Reason
	}
	if rec.Events == nil {
		rec.Events = []proto.Event{}
	}
	if err := writeFileAtomic(path, rec); err != nil {
		r.mux.Pace((&proto.Event{Verb: proto.VerbInfo, Job: r.id, Msg: "status file: " + err.Error()}).Line())
	}
}

// WriteErrorStatus writes the status file for a pace run that ended with a
// usage or configuration error (exit 202) before a runner existed (bad
// flag, bad size, invalid config): outcome error, source pace, the error,
// and the EXIT event its stderr ends with. path "" writes nothing.
func WriteErrorStatus(path string, argv []string, exit proto.Event) error {
	if path == "" {
		return nil
	}
	code := proto.ExitPaceError
	if exit.Code != nil {
		code = *exit.Code
	}
	rec := statusRecord{V: 1, ExitCode: code, Events: []proto.Event{exit}, Outcome: OutcomeError, Source: "pace", Error: exit.Msg}
	rec.State = policy.JobDone
	rec.Argv = argv
	rec.EndTS = time.Now().UnixMilli()
	return writeFileAtomic(path, rec)
}

func writeFileAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, "."+base+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(b)
	if werr == nil {
		werr = tmp.Chmod(0o600)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		os.Remove(name)
		return fmt.Errorf("write %s: %w", path, werr)
	}
	return nil
}
