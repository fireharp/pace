package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"

	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// cloneState deep-copies a policy state. The loop passes a clone to Step
// so the state logged as "before" can never be changed by the policy.
func cloneState(s policy.State) policy.State {
	c := s
	if s.Jobs != nil {
		c.Jobs = make([]policy.Job, len(s.Jobs))
		copy(c.Jobs, s.Jobs)
		for i := range c.Jobs {
			c.Jobs[i].Enf = cloneEnf(c.Jobs[i].Enf)
		}
	}
	if s.Units != nil {
		c.Units = make([]policy.Unit, len(s.Units))
		copy(c.Units, s.Units)
		for i := range c.Units {
			c.Units[i].Enf = cloneEnf(c.Units[i].Enf)
		}
	}
	if s.History != nil {
		c.History = make([]policy.History, len(s.History))
		for i, h := range s.History {
			h.Peaks, h.DurMS = cloneInts64(h.Peaks), cloneInts64(h.DurMS) // other fields are values
			c.History[i] = h
		}
	}
	return c
}

func cloneEnf(e policy.Enforcement) policy.Enforcement {
	if e.Warnings != nil {
		w := make([]policy.Warning, len(e.Warnings))
		copy(w, e.Warnings)
		e.Warnings = w
	}
	return e
}

func cloneInts64(v []int64) []int64 {
	if v == nil {
		return nil
	}
	return append(make([]int64, 0, len(v)), v...)
}

func cloneInts(v []int) []int {
	if v == nil {
		return nil
	}
	return append(make([]int, 0, len(v)), v...)
}

// cloneInputs deep-copies inputs for the asynchronous log writer (the
// tracker may reuse its sample buffers on the next tick).
func cloneInputs(in policy.Inputs) policy.Inputs {
	c := in
	if in.Jobs != nil {
		c.Jobs = make([]policy.JobSample, len(in.Jobs))
		copy(c.Jobs, in.Jobs)
		for i := range c.Jobs {
			c.Jobs[i].ExtraPIDs = cloneInts(c.Jobs[i].ExtraPIDs)
		}
	}
	if in.Units != nil {
		c.Units = make([]policy.UnitSample, len(in.Units))
		copy(c.Units, in.Units)
		for i := range c.Units {
			c.Units[i].PIDs = cloneInts(c.Units[i].PIDs)
		}
	}
	if in.Events != nil {
		c.Events = make([]policy.InEvent, len(in.Events))
		copy(c.Events, in.Events)
		for i := range c.Events {
			if s := c.Events[i].Submit; s != nil {
				sc := cloneSubmit(*s)
				c.Events[i].Submit = &sc
			}
		}
	}
	return c
}

func cloneSubmit(s proto.SubmitRequest) proto.SubmitRequest {
	if s.Argv != nil {
		s.Argv = append(make([]string, 0, len(s.Argv)), s.Argv...)
	}
	if s.Priority != nil {
		p := *s.Priority
		s.Priority = &p
	}
	return s
}

// materialHash hashes the state without the clock and the fields that move
// on every sample (usage, CPU, output progress). A step is logged when its
// output is non-empty or this hash changes; every record is self-contained,
// so skipping telemetry-only steps never breaks replay.
func materialHash(s policy.State) uint64 {
	m := cloneState(s)
	m.LastMS = 0
	for i := range m.Jobs {
		j := &m.Jobs[i]
		j.UsedBytes, j.PeakBytes, j.ReservedBytes, j.NProcs = 0, 0, 0, 0
		j.CPUMS, j.LastProgressMS, j.OutputBytes = 0, 0, 0
	}
	for i := range m.Units {
		m.Units[i].UsedBytes, m.Units[i].NProcs = 0, 0
	}
	b, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	h := fnv.New64a()
	h.Write(b)
	return h.Sum64()
}

// stateHash is the decision log's canonical state hash; it falls back to
// sha256 of the JSON encoding when dlog provides none.
func stateHash(s policy.State) string {
	if h := dlog.StateHash(s); h != "" {
		return h
	}
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
