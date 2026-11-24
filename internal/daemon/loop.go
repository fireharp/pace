package daemon

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"time"

	"github.com/fireharp/pace/internal/dlog"
	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

// loop is the only goroutine that touches policy state.
func (d *daemon) loop(ctx context.Context) {
	defer close(d.loopDone)
	defer d.finalPersist()
	defer d.closeContainers()
	defer d.stopRelease()
	timer := time.NewTimer(0) // first step at once (re-injected EvLost etc.)
	defer timer.Stop()
	for {
		var evc chan policy.InEvent
		// Without any machine sample the policy must not run (it would see
		// zero available memory); hold wrapper events in the channel until
		// sampling works. Handlers time out and wrappers fail open.
		if d.haveMach && len(d.pending) < maxPending {
			evc = d.evCh
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			d.runStep(true)
		case ev := <-evc:
			urgent := d.enqueue(ev)
		drain:
			for len(d.pending) < maxPending {
				select {
				case ev := <-d.evCh:
					urgent = d.enqueue(ev) || urgent
				default:
					break drain
				}
			}
			if !urgent {
				continue // started/progress wait for the next tick
			}
			d.runStep(false)
		case rr := <-d.reloadCh:
			if rr.hook != nil {
				rr.hook(d)
			} else {
				d.applyReload(rr)
			}
			close(rr.done)
			continue
		}
		timer.Reset(d.nextDelay())
	}
}

// enqueue appends a wrapper event; it reports whether it warrants an
// immediate step (admission latency: submit/attach/exit/lost).
func (d *daemon) enqueue(ev policy.InEvent) bool {
	d.pending = append(d.pending, ev)
	d.noteCgroup(ev)
	switch ev.Kind {
	case policy.EvSubmit, policy.EvAttach, policy.EvExit, policy.EvLost:
		return true
	}
	return false
}

func (d *daemon) nextDelay() time.Duration {
	iv := time.Duration(d.curIvMS.Load()) * time.Millisecond
	delay := iv - time.Since(d.lastSampleAt)
	if delay < time.Millisecond {
		delay = time.Millisecond
	}
	return delay
}

// nowMS returns the step clock, never going backwards.
func (d *daemon) nowMS() int64 {
	n := d.now()
	if n < d.lastNowMS {
		n = d.lastNowMS
	}
	if n < d.st.LastMS {
		n = d.st.LastMS
	}
	d.lastNowMS = n
	return n
}

// sample reads the machine and the user's processes. On error the last
// good sample is kept and a fault recorded.
func (d *daemon) sample() {
	t0 := time.Now()
	d.sampleStale = false
	m, err := d.backend.SampleMachine(d.cfg.DiskPaths)
	if err != nil {
		d.fault("sample_machine", err.Error())
		d.sampleStale = true
	} else {
		d.lastMach = m
		d.haveMach = true
		d.lastMachOK.Store(time.Now().UnixMilli())
	}
	procs, err := d.backend.ListProcs(d.o.UID)
	d.procsFresh = err == nil
	if err != nil {
		d.fault("list_procs", err.Error())
		d.sampleStale = true
	} else {
		prot := d.reg.protectedPIDs(nil) // fresh: the tracker may keep it
		d.tracker.SetProtectedPIDs(prot)
		d.feedContainers()
		d.setAttribution(d.tracker.Attribute(procs, jobsWithPendingStarts(d.st.Jobs, d.pending), d.st.Units), procs)
		d.lastProcsOK.Store(time.Now().UnixMilli())
	}
	d.metrics.sampleNS.Store(int64(time.Since(t0)))
	d.lastSampleAt = t0
}

// jobsWithPendingStarts returns jobs with the pid and group of every
// "started" event still waiting for the next step filled in, so that the
// sample taken for that step already attributes the new process tree to its
// job. Without this, a job's first sample after its start shows its memory
// but no job to charge it to: the job is no candidate for the pressure
// ladder in that step, and a lower-ranked job is picked as the victim
// instead. The policy applies the same events itself; jobs is not modified.
func jobsWithPendingStarts(jobs []policy.Job, pending []policy.InEvent) []policy.Job {
	var out []policy.Job
	for _, ev := range pending {
		if ev.Kind != policy.EvStarted || ev.PID <= 1 {
			continue
		}
		for i := range jobs {
			j := &jobs[i]
			if j.ID != ev.Job || j.State != policy.JobRunning || j.PID > 0 || j.PGID > 0 {
				continue
			}
			if out == nil {
				out = append([]policy.Job(nil), jobs...)
			}
			out[i].PID, out[i].PGID = ev.PID, ev.PGID
			if ev.StartMS > 0 {
				out[i].StartMS = ev.StartMS
			}
		}
	}
	if out == nil {
		return jobs
	}
	return out
}

// runStep samples (unless a fresh sample exists and force is false), runs
// one policy step over the pending events and carries out its output.
func (d *daemon) runStep(force bool) {
	if force || !d.haveMach || time.Since(d.lastSampleAt) >= minResampleMS*time.Millisecond {
		d.sample()
	}
	if !d.haveMach {
		return
	}
	now := d.nowMS()
	events := d.pending
	d.pending = nil
	d.reg.markInLoop(events)

	in := policy.Inputs{
		NowMS:           now,
		Machine:         d.lastMach,
		ScopedUsedBytes: d.lastAttr.ScopedUsedBytes,
		Jobs:            d.lastAttr.Jobs,
		Units:           d.lastAttr.Units,
		Events:          events,
		Stale:           d.sampleStale,
	}
	params := d.cfg.Params
	in.Units = withSmallUnits(params, in, d.lastAttr.SmallUnits)
	before := d.st
	next, out, ok := d.safeStep(params, before, in)
	if !ok {
		d.reg.abandon(events, now)
		d.lastStep.Store(time.Now().UnixMilli())
		return
	}
	d.st = next

	d.actProt = d.reg.protectedPIDs(d.actProt[:0])
	d.notifyUnitStderr(out.Events) // before the signals: the reader still reads
	if len(out.Actions) > 0 {
		d.execute(out.Actions, now)
	}
	refrozen := d.reconcileHeld()
	refrozen = append(refrozen, d.syncContainers()...)

	evs := out.Events
	if len(evs) > 0 || len(refrozen) > 0 {
		evs = make([]proto.Event, 0, len(out.Events)+len(refrozen))
		evs = append(evs, out.Events...)
		evs = append(evs, refrozen...)
		for i := range evs {
			if evs[i].V == 0 {
				evs[i].V = 1
			}
			if evs[i].TS == 0 {
				evs[i].TS = now
			}
			if evs[i].Verb == proto.VerbThrottled && evs[i].Msg == "" {
				evs[i].Msg = d.throttleCaveat(evs[i].Job) // weak or one-way here (the decision log keeps the policy's event)
			}
		}
		d.decorateAdmitted(evs)
	}
	as := d.assess(params, next, in)
	jobsChanged := d.reg.apply(stepResult{nowMS: now, state: &d.st, events: evs, processed: events})
	d.fanout(evs, as.Level)

	mat := materialHash(next)
	changed := mat != d.lastMaterial
	d.lastMaterial = mat
	switch {
	case !out.Empty() || changed:
		d.writeRecord(dlog.KindStep, params, &before, &in, &out, next)
	case time.Since(d.lastLogAt) >= time.Duration(d.cfg.HeartbeatMS)*time.Millisecond:
		d.writeRecord(dlog.KindHeartbeat, params, &before, &in, &out, next)
	}
	if changed || jobsChanged || !out.Empty() {
		d.dirty = true
	}
	d.telemetry = true
	d.maybePersist(false)

	if time.Since(d.lastPrune) >= time.Minute {
		d.reg.prune(now)
		d.lastPrune = time.Now()
	}
	d.publish(now, params, next, in, as)
	d.curIvMS.Store(d.interval(as, next))
	d.lastStep.Store(time.Now().UnixMilli())
}

// withSmallUnits returns in.Units, plus the unit candidates below min_victim
// (small, from attribution) while memory pressure is above normal: a swarm
// of small groups outside pace may then be what holds the memory, and the
// policy may pick them as victims (policy.candidates decides whether they
// matter). At normal pressure they stay out of the inputs (and the decision
// log): nothing could act on them.
func withSmallUnits(p policy.Params, in policy.Inputs, small []policy.UnitSample) []policy.UnitSample {
	if len(small) == 0 || policy.MemLevel(p, in) == proto.LevelNormal {
		return in.Units
	}
	out := make([]policy.UnitSample, 0, len(in.Units)+len(small))
	out = append(append(out, in.Units...), small...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// safeStep runs the policy on a clone of the state (so the logged "before"
// state can never be mutated) and survives a policy panic.
func (d *daemon) safeStep(p policy.Params, s policy.State, in policy.Inputs) (next policy.State, out policy.Output, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 2048)
			buf = buf[:runtime.Stack(buf, false)]
			d.fault("step_panic", fmt.Sprintf("%v: %s", r, buf))
			ok = false
		}
	}()
	next, out = d.step(p, cloneState(s), in)
	return next, out, true
}

// interval picks the sampling interval: fast when pressure is at warn or
// above, or any running job uses >= 80% of its budget.
func (d *daemon) interval(as Assessment, s policy.State) int64 {
	if levelNum(as.Level) >= 1 || levelNum(s.Pressure.Level) >= 1 {
		return d.cfg.FastMS
	}
	for i := range s.Jobs {
		j := &s.Jobs[i]
		if j.State == policy.JobRunning && j.BudgetBytes > 0 && j.UsedBytes*10 >= j.BudgetBytes*8 {
			return d.cfg.FastMS
		}
	}
	return d.cfg.SampleMS
}

func levelNum(l string) int {
	switch l {
	case proto.LevelWarn:
		return 1
	case proto.LevelCritical:
		return 2
	}
	return 0
}

// alertVerb: verbs that go to alerts.jsonl even when they name a job.
func alertVerb(v string) bool {
	switch v {
	case proto.VerbWarn, proto.VerbKilled, proto.VerbFrozen, proto.VerbThrottled, proto.VerbInfo:
		return true
	}
	return false
}

// fanout sends step events to the global feed, alerts.jsonl, metrics and
// notifications (job streams were fed by registry.apply).
func (d *daemon) fanout(evs []proto.Event, level string) {
	for i := range evs {
		e := evs[i].WithoutStreamFields() // ADMITTED's container fields are the job stream's alone
		d.metrics.countEvent(&e)
		d.feed.add(e)
		if e.Job == "" || alertVerb(e.Verb) {
			d.alerts.write(&e)
		}
		d.noteAlert(e)
		switch e.Verb {
		case proto.VerbWarn:
			d.lastWarn[warnKey(e.Job, e.Unit, e.Policy)] = e
		case proto.VerbOK:
			delete(d.lastWarn, warnKey(e.Job, e.Unit, e.Policy))
		case proto.VerbKilled:
			d.maybeNotify(&e, level)
		}
	}
}

func warnKey(job, unit, pol string) string { return job + "|" + unit + "|" + pol }

const (
	recentAlertWindowMS = 15 * 60 * 1000
	maxRecentAlerts     = 32
)

// isAlert: the events a person should hear about.
func isAlert(e *proto.Event) bool {
	switch e.Verb {
	case proto.VerbWarn, proto.VerbKilled, proto.VerbFrozen, proto.VerbThrottled:
		return true
	case proto.VerbInfo:
		return e.Policy != ""
	}
	return false
}

// noteAlert keeps alert events for status.recent_alerts.
func (d *daemon) noteAlert(e proto.Event) {
	if !isAlert(&e) {
		return
	}
	d.recentAlerts = append(d.recentAlerts, e)
	if n := len(d.recentAlerts); n > maxRecentAlerts {
		d.recentAlerts = append([]proto.Event(nil), d.recentAlerts[n-maxRecentAlerts:]...)
	}
}

// recentAlertsAt returns the kept alerts no older than the window at now.
func (d *daemon) recentAlertsAt(now int64) []proto.Event {
	var out []proto.Event
	for _, e := range d.recentAlerts {
		if e.TS == 0 || now-e.TS <= recentAlertWindowMS {
			out = append(out, e)
		}
	}
	return out
}

// writeRecord appends one self-contained decision record.
func (d *daemon) writeRecord(kind string, p policy.Params, before *policy.State, in *policy.Inputs, out *policy.Output, after policy.State) {
	d.logSeq++
	pc := p
	inc := cloneInputs(*in)
	oc := *out
	d.log.Write(dlog.Record{
		V: 1, Kind: kind, Seq: d.logSeq, TS: in.NowMS, Version: d.o.Version, CfgHash: d.cfg.Hash,
		Params: &pc, State: before, In: &inc, Out: &oc, StateHash: stateHash(after),
	})
	d.lastLogAt = time.Now()
}

// stopRelease runs on the loop goroutine as it exits: it releases every
// throttle and freeze (see releaseAll) and logs what it released.
func (d *daemon) stopRelease() {
	if !d.haveMach && len(d.held) == 0 {
		return // never stepped: nothing can have been enforced by us
	}
	msg := d.releaseAll()
	if msg == "" {
		return
	}
	now := d.nowMS()
	d.reg.apply(stepResult{nowMS: now, state: &d.st}) // job statuses: stage released
	d.logSeq++
	st := cloneState(d.st)
	d.log.Write(dlog.Record{
		V: 1, Kind: dlog.KindStop, Seq: d.logSeq, TS: now, Version: d.o.Version, CfgHash: d.cfg.Hash,
		State: &st, StateHash: stateHash(st), Msg: msg,
	})
}

func (d *daemon) logStart() {
	d.logSeq++
	pc := d.cfg.Params
	st := cloneState(d.st)
	d.log.Write(dlog.Record{
		V: 1, Kind: dlog.KindStart, Seq: d.logSeq, TS: d.now(), Version: d.o.Version, CfgHash: d.cfg.Hash,
		Params: &pc, State: &st, StateHash: stateHash(st),
		Msg: fmt.Sprintf("pid=%d platform=%s config=%s jobs=%d", d.selfPID, d.backend.Name(), d.cfg.Source, len(st.Jobs)),
	})
	d.lastLogAt = time.Now()
}

// applyReload swaps in a new config (loop goroutine). Paths that need a
// restart (home, socket, log dir) keep their running values.
func (d *daemon) applyReload(rr reloadReq) {
	d.logSeq++
	rec := dlog.Record{V: 1, Kind: dlog.KindConfig, Seq: d.logSeq, TS: d.now(), Version: d.o.Version}
	if rr.err != nil {
		rec.CfgHash = d.cfg.Hash
		rec.Msg = "reload failed, keeping previous config: " + rr.err.Error()
		d.fault("reload", rr.err.Error())
		d.log.Write(rec)
		return
	}
	nc := normalizeConfig(rr.cfg)
	var notes []string
	if nc.Home != "" && nc.Home != d.cfg.Home {
		notes = append(notes, "home")
	}
	if nc.Socket != "" && nc.Socket != d.cfg.Socket {
		notes = append(notes, "socket")
	}
	if nc.LogDir != "" && nc.LogDir != d.cfg.LogDir {
		notes = append(notes, "log_dir")
	}
	nc.Home, nc.Socket, nc.LogDir = d.cfg.Home, d.cfg.Socket, d.cfg.LogDir
	d.cfg = nc
	d.tracker.SetOptions(d.attribOptions())
	pc := nc.Params
	rec.Params, rec.CfgHash = &pc, nc.Hash
	rec.Msg = "reloaded from " + nc.Source
	if len(notes) > 0 {
		rec.Msg += fmt.Sprintf("; changes to %v need a restart", notes)
	}
	d.log.Write(rec)
	d.lastLogAt = time.Now()
	d.curIvMS.Store(d.cfg.SampleMS)
}

// maybeNotify shows a desktop notification for a kill: opt-in, at most one
// per 10s, asynchronous, one at a time, and never at critical pressure
// (spawning a process then is a hazard).
func (d *daemon) maybeNotify(e *proto.Event, level string) {
	if !d.cfg.NotifyMacOS || level == proto.LevelCritical {
		return
	}
	fn := d.o.Notify
	if fn == nil {
		if runtime.GOOS != "darwin" {
			return
		}
		fn = osascriptNotify
	}
	if !d.lastNotify.IsZero() && time.Since(d.lastNotify) < notifyEvery {
		return
	}
	if !d.notifyIn.CompareAndSwap(false, true) {
		return
	}
	d.lastNotify = time.Now()
	msg := e.Line()
	go func() {
		defer d.notifyIn.Store(false)
		fn("pace: job killed", msg)
	}()
}
