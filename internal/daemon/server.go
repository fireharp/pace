package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
)

func (d *daemon) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/jobs", d.hSubmit)
	mux.HandleFunc("POST /v1/jobs/{id}/started", d.hStarted)
	mux.HandleFunc("POST /v1/jobs/{id}/progress", d.hProgress)
	mux.HandleFunc("POST /v1/jobs/{id}/exit", d.hExit)
	mux.HandleFunc("POST /v1/jobs/{id}/attach", d.hAttach)
	mux.HandleFunc("GET /v1/jobs/{id}", d.hJob)
	mux.HandleFunc("GET /v1/status", d.hStatus)
	mux.HandleFunc("GET /v1/events", d.hEvents)
	mux.HandleFunc("POST /v1/reload", d.hReload)
	mux.HandleFunc("GET /metrics", d.hMetrics)
	mux.HandleFunc("GET /healthz", d.hHealthz)
	return mux
}

type errorBody struct {
	Error string `json:"error"`
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errorBody{Error: msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// decode reads exactly one JSON value (bounded) and reads the body to EOF,
// so the server starts watching the connection for the client going away.
// Anything but whitespace after the value is an error.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(body)
	err := dec.Decode(v)
	if err == nil {
		var extra json.RawMessage
		if e2 := dec.Decode(&extra); e2 == nil {
			err = errors.New("trailing data after the JSON value")
		} else if !errors.Is(e2, io.EOF) {
			err = e2
		}
	}
	if err == nil {
		_, err = io.Copy(io.Discard, body)
	}
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		}
		return false
	}
	return true
}

func pushErr(w http.ResponseWriter, err error) {
	writeErr(w, http.StatusServiceUnavailable, err.Error())
}

// validJobID accepts the ids the daemon issues and anything similar a
// re-attaching wrapper may carry: [A-Za-z0-9_-], 1..64 bytes.
func validJobID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func validateSubmit(s *proto.SubmitRequest) error {
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return errors.New("argv is required")
	}
	if s.Priority != nil && (*s.Priority < 0 || *s.Priority > 9) {
		return fmt.Errorf("priority %d out of range 0..9", *s.Priority)
	}
	if s.EstimateBytes < 0 || s.BudgetBytes < 0 || s.WallLimitMS < 0 || s.QueueTimeoutMS < 0 || s.WrapperPID < 0 {
		return errors.New("negative estimate_bytes, budget_bytes, wall_limit_ms, queue_timeout_ms or wrapper_pid")
	}
	return nil
}

// newJobID: 'j' + base36(unix ms) + '-' + base36(counter).
func (d *daemon) newJobID() string {
	n := d.idSeq.Add(1)
	return "j" + strconv.FormatInt(d.now(), 36) + "-" + strconv.FormatInt(n, 36)
}

func (d *daemon) accepted(id, class string) proto.Event {
	return proto.Event{V: 1, TS: d.now(), Verb: proto.VerbAccepted, Job: id, Class: class}
}

// hSubmit: POST /v1/jobs -> NDJSON stream for the job's life.
func (d *daemon) hSubmit(w http.ResponseWriter, r *http.Request) {
	var req proto.SubmitRequest
	if !decode(w, r, &req) {
		return
	}
	if err := validateSubmit(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := d.newJobID()
	acc := d.accepted(id, req.Class)
	st := d.reg.create(id, req, acc)
	d.feed.add(acc) // before the push: the loop's events for this job follow it
	d.metrics.countEvent(&acc)
	sub := cloneSubmit(req)
	if err := d.push(r.Context(), policy.InEvent{Kind: policy.EvSubmit, Job: id, Submit: &sub}); err != nil {
		d.reg.remove(id)
		pushErr(w, err)
		return
	}
	d.serveStream(w, r, st)
}

// hAttach: POST /v1/jobs/{id}/attach -> stream, after a daemon restart.
func (d *daemon) hAttach(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	var req proto.AttachRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Submit.Argv) == 0 {
		if s, ok := d.reg.submitOf(id); ok {
			req.Submit = s
		}
	}
	if err := validateSubmit(&req.Submit); err != nil {
		writeErr(w, http.StatusBadRequest, "submit: "+err.Error())
		return
	}
	if req.PID != 0 && (req.PID <= 1 || req.PGID <= 1) {
		writeErr(w, http.StatusBadRequest, "pid and pgid must be > 1")
		return
	}
	if len(req.Cgroup) > 4096 {
		writeErr(w, http.StatusBadRequest, "cgroup path too long")
		return
	}
	acc := d.accepted(id, req.Submit.Class)
	st, _, undo := d.reg.attach(id, req, acc)
	d.feed.add(acc)
	d.metrics.countEvent(&acc)
	// The policy gets the request's full argv (as on submit): the registry
	// keeps a bounded copy, and a key derived from that could be its
	// truncation marker.
	sub := cloneSubmit(req.Submit)
	ev := policy.InEvent{Kind: policy.EvAttach, Job: id, Submit: &sub, PID: req.PID, PGID: req.PGID, StartMS: req.StartMS,
		OutputBytes: req.OutputBytes, Cgroup: req.Cgroup}
	if err := d.push(r.Context(), ev); err != nil {
		// The loop never saw this attach: undo it (an unknown id is
		// forgotten, a known one gets its previous record back), so a
		// wrapper that never retries leaves no live record that protects
		// its pid forever. The stream ends and the wrapper retries.
		d.reg.undoAttach(st, undo)
		pushErr(w, err)
		return
	}
	d.serveStream(w, r, st)
}

// serveStream writes a job's events as NDJSON until the job is terminal,
// the client goes away (-> EvLost) or the daemon shuts down (no EvLost:
// the wrapper re-attaches to the next daemon).
func (d *daemon) serveStream(w http.ResponseWriter, r *http.Request, st *stream) {
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	enc := json.NewEncoder(w)
	var buf []proto.Event
	disconnected := false
	defer func() {
		if d.reg.streamClosed(st, disconnected, d.stopping.Load()) {
			ctx, cancel := context.WithTimeout(context.Background(), d.pushTimeout)
			defer cancel()
			if err := d.push(ctx, policy.InEvent{Kind: policy.EvLost, Job: st.id}); err != nil && !errors.Is(err, errStopping) {
				d.fault("event_queue", "wrapper_lost for "+st.id+" not delivered: "+err.Error())
			}
		}
	}()
	for {
		closed := isClosed(st.done)
		var dropped int64
		buf, dropped = d.reg.read(st, buf[:0])
		if dropped > 0 {
			gap := proto.Event{V: 1, TS: d.now(), Verb: proto.VerbInfo, Job: st.id, Msg: "gap dropped=" + strconv.FormatInt(dropped, 10)}
			buf = append([]proto.Event{gap}, buf...)
		}
		if len(buf) > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			for i := range buf {
				if err := enc.Encode(&buf[i]); err != nil {
					disconnected = true
					return
				}
			}
			if err := rc.Flush(); err != nil {
				disconnected = true
				return
			}
		}
		if closed {
			return
		}
		select {
		case <-st.notify:
		case <-st.done:
		case <-r.Context().Done():
			disconnected = !d.stopping.Load()
			return
		case <-d.stopCh:
			return
		}
	}
}

func isClosed(c chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// jobEvent handles started/progress/exit: 404 for unknown jobs, else push.
func (d *daemon) jobEvent(w http.ResponseWriter, r *http.Request, ev policy.InEvent) {
	if err := d.push(r.Context(), ev); err != nil {
		pushErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) hStarted(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req proto.StartedRequest
	if !decode(w, r, &req) {
		return
	}
	if !d.reg.known(id) {
		writeErr(w, http.StatusNotFound, "unknown job "+strconv.Quote(id))
		return
	}
	if req.PID <= 1 || req.PGID <= 1 {
		writeErr(w, http.StatusBadRequest, "pid and pgid must be > 1")
		return
	}
	if len(req.Cgroup) > 4096 {
		writeErr(w, http.StatusBadRequest, "cgroup path too long")
		return
	}
	d.jobEvent(w, r, policy.InEvent{Kind: policy.EvStarted, Job: id, PID: req.PID, PGID: req.PGID, StartMS: req.StartMS, Cgroup: req.Cgroup})
}

func (d *daemon) hProgress(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req proto.ProgressRequest
	if !decode(w, r, &req) {
		return
	}
	if !d.reg.known(id) {
		writeErr(w, http.StatusNotFound, "unknown job "+strconv.Quote(id))
		return
	}
	d.jobEvent(w, r, policy.InEvent{Kind: policy.EvProgress, Job: id, OutputBytes: req.OutputBytes, LastOutputMS: req.LastOutputMS})
}

func (d *daemon) hExit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req proto.ExitRequest
	if !decode(w, r, &req) {
		return
	}
	if !d.reg.markExit(id, req) {
		writeErr(w, http.StatusNotFound, "unknown job "+strconv.Quote(id))
		return
	}
	d.jobEvent(w, r, policy.InEvent{Kind: policy.EvExit, Job: id, Code: req.Code, Signaled: req.Signaled, Signal: req.Signal,
		OOMKill: req.OOMKill, PeakBytes: max(req.PeakBytes, 0)})
}

func (d *daemon) hJob(w http.ResponseWriter, r *http.Request) {
	js, ok := d.reg.jobStatus(r.PathValue("id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown job "+strconv.Quote(r.PathValue("id")))
		return
	}
	writeJSON(w, js)
}

func (d *daemon) hStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, d.snap.Load().status)
}

// hEvents: GET /v1/events -> the last 200 events as NDJSON; with
// follow=1 a live stream (backlog=N first prepends up to 256 old events).
func (d *daemon) hEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	follow := q.Get("follow")
	if follow != "1" && follow != "true" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, e := range d.feed.last(recentEvents) {
			_ = enc.Encode(&e)
		}
		return
	}
	backlog, _ := strconv.Atoi(q.Get("backlog"))
	backlog = max(0, min(backlog, feedRingSize))
	f := d.feed.subscribe(backlog)
	defer d.feed.unsubscribe(f)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	enc := json.NewEncoder(w)
	var buf []proto.Event
	for {
		var dropped int64
		buf, dropped = d.feed.read(f, buf[:0])
		if dropped > 0 {
			gap := proto.Event{V: 1, TS: d.now(), Verb: proto.VerbInfo, Msg: "gap dropped=" + strconv.FormatInt(dropped, 10)}
			buf = append([]proto.Event{gap}, buf...)
		}
		if len(buf) > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
			for i := range buf {
				if enc.Encode(&buf[i]) != nil {
					return
				}
			}
			if rc.Flush() != nil {
				return
			}
		}
		select {
		case <-f.notify:
		case <-r.Context().Done():
			return
		case <-d.stopCh:
			return
		}
	}
}

func (d *daemon) hReload(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := d.reload(); err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errStopping) {
			code = http.StatusServiceUnavailable
		}
		writeErr(w, code, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *daemon) hMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_ = d.writeMetrics(w)
}

// hHealthz: 200 "ok" while the loop steps on fresh samples; 503 when it
// has stalled, or when sampling the machine or the process list has been
// failing for as long (enforcement is then held: supervisors and pace run
// can tell a blind or wedged daemon from a slow one).
func (d *daemon) hHealthz(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UnixMilli()
	limit := max(10*d.curIvMS.Load(), 10_000)
	for _, c := range []struct {
		what string
		last int64
	}{
		{"loop stalled: last step", d.lastStep.Load()},
		{"machine sampling failing: last good sample", d.lastMachOK.Load()},
		{"process listing failing: last good listing", d.lastProcsOK.Load()},
	} {
		if now-c.last > limit {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "%s %dms ago\n", c.what, now-c.last)
			return
		}
	}
	_, _ = io.WriteString(w, "ok\n")
}
