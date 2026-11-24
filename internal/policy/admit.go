package policy

import (
	"sort"

	"github.com/fireharp/pace/internal/proto"
)

// ---- 10. admission ---------------------------------------------------------

// queueOrder returns indices of queued jobs: priority desc, then Seq asc.
func queueOrder(s *State) []int {
	var idx []int
	for i := range s.Jobs {
		if s.Jobs[i].State == JobQueued {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ja, jb := &s.Jobs[idx[a]], &s.Jobs[idx[b]]
		if ja.Priority != jb.Priority {
			return ja.Priority > jb.Priority
		}
		return ja.Seq < jb.Seq
	})
	return idx
}

// etaTable lists, ascending, the predicted remaining run time of every
// running job whose duration history is trusted (p50 of its key, else its
// class). Jobs without trusted history are not predicted.
func (c *stepCtx) etaTable() []int64 {
	var rem []int64
	for i := range c.s.Jobs {
		j := &c.s.Jobs[i]
		if j.State != JobRunning {
			continue
		}
		d, ok := int64(0), false
		if j.Key != "" {
			d, ok = learnedDuration(c.p, c.s, histKeyPrefix+j.Key)
		}
		if !ok {
			d, ok = learnedDuration(c.p, c.s, histClassPrefix+j.Class)
		}
		if !ok {
			continue
		}
		el := int64(0)
		if j.StartMS > 0 {
			el = c.now - j.StartMS
		}
		r := d - el
		if r < 0 {
			r = 0
		}
		rem = append(rem, r)
	}
	sort.Slice(rem, func(a, b int) bool { return rem[a] < rem[b] })
	return rem
}

// etaFor is the ETA for queue position pos (1-based): the pos-th earliest
// predicted end among running jobs; beyond the predictions (or without
// any), DefaultETAPerPositionMS per remaining position. Never below MinETAMS.
func etaFor(rem []int64, pos int) int64 {
	if pos < 1 {
		pos = 1
	}
	var eta int64
	if pos <= len(rem) {
		eta = rem[pos-1]
	} else {
		if len(rem) > 0 {
			eta = rem[len(rem)-1]
		}
		eta += int64(pos-len(rem)) * DefaultETAPerPositionMS
	}
	if eta < MinETAMS {
		eta = MinETAMS
	}
	return eta
}

func (c *stepCtx) admit(j *Job, free int64) {
	j.State = JobRunning
	j.AdmitMS = c.now
	if j.ReservedBytes < j.EstimateBytes {
		j.ReservedBytes = j.EstimateBytes
	}
	ev := c.jobEvent(proto.VerbAdmitted, j)
	ev.WaitedMS = proto.I64(c.now - j.SubmitMS)
	ev.NeedBytes = proto.I64(j.EstimateBytes)
	ev.FreeBytes = proto.I64(nonNeg(free))
	if j.BudgetBytes > 0 {
		ev.BudgetBytes = proto.I64(j.BudgetBytes)
	}
	c.emit(ev)
}

func (c *stepCtx) refuse(j *Job, reason string, free, retry int64, hint string) {
	ev := c.jobEvent(proto.VerbRefused, j)
	ev.Reason = reason
	c.setFreeNeed(&ev, reason, free, j.EstimateBytes)
	if retry > 0 {
		ev.RetryAfterMS = proto.I64(retry)
	}
	ev.Hint = hint
	c.emit(ev)
	j.State = JobDone // dropped below
}

// setFreeNeed sets free= and need= of a QUEUED or REFUSED line. They are
// the memory headroom and the estimate, except for reason=disk: memory is
// not what stops the job then, so free= is the free disk space (as on the
// WARN policy=disk line; omitted when no disk is watched) and need= is
// left out.
func (c *stepCtx) setFreeNeed(ev *proto.Event, reason string, free, need int64) {
	if reason == proto.ReasonDisk {
		if df := c.in.Machine.DiskFreeBytes; df >= 0 {
			ev.FreeBytes = proto.I64(df)
		}
		return
	}
	ev.FreeBytes = proto.I64(nonNeg(free))
	ev.NeedBytes = proto.I64(need)
}

func refuseHint(reason string) string {
	switch reason {
	case proto.ReasonHeadroom:
		return HintHeadroom
	case proto.ReasonPressure:
		return HintPressureWait
	case proto.ReasonSlots:
		return HintSlots
	case proto.ReasonDisk:
		return HintDisk
	case ReasonJobWarning:
		return HintJobWarning
	}
	return ""
}

// HintQueuedAhead is the REFUSED hint for a job that would fit the
// headroom but is held behind a queued job that does not (no backfill).
const HintQueuedAhead = "retry after retry_after; larger jobs queued ahead must start first"

// admission decides every queued job, in queue order.
//
//   - observe: everything is admitted at once (too_big only logged as INFO).
//   - too_big: estimate > capacity - reserve -> REFUSED, no retry_after.
//   - queue deadline passed -> REFUSED reason=queue_timeout.
//   - paused (disk critical / pressure / optional job warnings): nothing is
//     admitted; new submissions are refused outright at disk critical.
//   - class slots full -> skipped (does not block jobs behind it).
//   - estimate > headroom -> blocked, and so is every job behind it
//     (no backfill, so big jobs are not starved by small ones). The
//     blocked job keeps its claim on the headroom: free= reported to the
//     jobs behind it is what is left after it (so free < need always holds
//     for reason=headroom).
//   - not admitted and no_wait -> REFUSED reason=<why> retry_after=<eta>;
//     a refused job leaves the queue and blocks nobody.
//   - otherwise QUEUED, re-emitted when the position or the reason changes.
//
// An undeclared job is too_big only when its own command's observed peak is
// above the limit; a class value never makes it too_big (undeclaredEstimate).
func (c *stepCtx) admission() {
	order := queueOrder(c.s)
	if len(order) == 0 {
		return
	}
	hr := headroom(c.p, c.s, c.in)
	if c.observe {
		for _, i := range order {
			j := &c.s.Jobs[i]
			if m := maxAdmissible(c.p, c.capacity); m > 0 && j.EstimateBytes > m {
				ev := c.jobEvent(proto.VerbInfo, j)
				ev.Reason = proto.ReasonTooBig
				ev.NeedBytes = proto.I64(j.EstimateBytes)
				ev.Msg = "would refuse"
				c.emit(ev)
			}
			c.admit(j, hr)
			hr -= j.EstimateBytes
		}
		return
	}
	rem := c.etaTable()
	reasons := pauseReasons(c.p, c.s, c.level, c.disk)
	pause := ""
	if len(reasons) > 0 {
		pause = reasons[0]
	}
	running := map[string]int{}
	for i := range c.s.Jobs {
		if c.s.Jobs[i].State == JobRunning {
			running[c.s.Jobs[i].Class]++
		}
	}

	// Pass 1: refusals that do not depend on the rest of the queue.
	limit := maxAdmissible(c.p, c.capacity)
	var live []int
	for qpos, i := range order {
		j := &c.s.Jobs[i]
		switch {
		case limit > 0 && j.EstimateBytes > limit:
			hint := HintTooBig
			if j.Learned {
				hint = HintTooBigLearned
			}
			c.refuse(j, proto.ReasonTooBig, hr, 0, hint)
		case j.QueueDeadlineMS > 0 && c.now >= j.QueueDeadlineMS:
			c.refuse(j, proto.ReasonQueueTimeout, hr, etaFor(rem, qpos+1), HintQueueTimeout)
		default:
			live = append(live, i)
		}
	}

	// Pass 2: admit in order.
	blocked := false
	var claimed int64 // estimates of headroom-blocked jobs still queued
	pos := 0
	for _, i := range live {
		j := &c.s.Jobs[i]
		cls := classFor(c.p, j.Class)
		reason := ""
		behind := false // fits, but held behind a blocked job
		switch {
		case pause != "":
			reason = pause
		case cls.Slots > 0 && running[j.Class] >= cls.Slots:
			reason = proto.ReasonSlots
		case blocked:
			reason = proto.ReasonHeadroom
			behind = j.EstimateBytes <= hr
		case j.EstimateBytes > hr:
			reason = proto.ReasonHeadroom
		}
		if reason == "" {
			c.admit(j, hr)
			hr -= j.EstimateBytes
			running[j.Class]++
			continue
		}
		free := hr - claimed // what is left for it after the blocked jobs ahead
		if reason == proto.ReasonDisk && c.newJobs[j.ID] {
			c.refuse(j, reason, free, etaFor(rem, pos+1), HintDisk)
			continue
		}
		if j.NoWait {
			hint := refuseHint(reason)
			if behind {
				hint = HintQueuedAhead
			}
			c.refuse(j, reason, free, etaFor(rem, pos+1), hint)
			continue
		}
		// It stays queued (only now may it block the jobs behind it).
		if reason == proto.ReasonHeadroom {
			blocked = true
			claimed += j.EstimateBytes
		}
		pos++
		if !j.WasQueued || j.LastPosition != pos || j.LastQueueReason != reason {
			ev := c.jobEvent(proto.VerbQueued, j)
			ev.Reason = reason
			c.setFreeNeed(&ev, reason, free, j.EstimateBytes)
			ev.Position = proto.Int(pos)
			ev.ETAMS = proto.I64(etaFor(rem, pos))
			c.emit(ev)
			j.WasQueued = true
			j.LastPosition = pos
			j.LastQueueReason = reason
		}
	}

	// Drop refused jobs.
	jobs := c.s.Jobs[:0]
	for _, j := range c.s.Jobs {
		if j.State != JobDone {
			jobs = append(jobs, j)
		}
	}
	c.s.Jobs = jobs
}
