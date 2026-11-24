// Package client is a small HTTP-over-unix-socket client for the daemon API
// documented in internal/proto (api.go). It is used by `pace run` (the
// wrapper) and by the CLI subcommands (status, watch, job, reload).
//
// Requests are plain HTTP/1.1 + JSON; streams are NDJSON (one proto.Event per
// line). Only the standard library is used.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// Defaults.
const (
	// DefaultTimeout bounds every non-streaming request.
	DefaultTimeout = 2 * time.Second
	// DefaultHeaderTimeout bounds the wait for a stream's response headers.
	// The daemon writes the first event (ACCEPTED) immediately.
	DefaultHeaderTimeout = 5 * time.Second
	// MaxSocketPath is the longest usable sun_path on macOS (measured: 103).
	MaxSocketPath = 103
	// MaxLine is the longest NDJSON line a Stream accepts.
	MaxLine = 1 << 20

	minBackoff = 10 * time.Millisecond
	maxBackoff = 250 * time.Millisecond
)

// ErrSocketPath is returned for an empty or over-long socket path.
var ErrSocketPath = errors.New("client: invalid socket path")

// APIError is a non-2xx HTTP response: the daemon is reachable but refused
// the request.
type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	b := strings.TrimSpace(e.Body)
	if b == "" {
		return fmt.Sprintf("pace daemon: %s %s: HTTP %d", e.Method, e.Path, e.Status)
	}
	return fmt.Sprintf("pace daemon: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, b)
}

// DecodeError reports one malformed stream line. It is not fatal: the
// stream may be read further.
type DecodeError struct {
	Line string
	Err  error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("client: malformed event line %q: %v", truncate(e.Line, 120), e.Err)
}

func (e *DecodeError) Unwrap() error { return e.Err }

// IsUnreachable reports whether err means the daemon could not be talked to
// at all (no socket, connection refused, reset, timeout), as opposed to the
// daemon answering with an error status.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return false
	}
	var de *DecodeError
	return !errors.As(err, &de)
}

// IsDown reports whether err means the daemon is not usable: unreachable
// (IsUnreachable) or answering with a server error (5xx), such as /healthz
// reporting "loop stalled". A 4xx answer is the request's fault: the daemon
// is up.
func IsDown(err error) bool {
	if err == nil {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500
	}
	return IsUnreachable(err)
}

// IsStatus reports whether err is an APIError with the given HTTP status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Client talks to one daemon socket. It is safe for concurrent use. A
// Client survives daemon restarts: every request re-dials when needed.
type Client struct {
	socket string
	// Timeout bounds non-streaming requests (0 = DefaultTimeout).
	Timeout time.Duration
	// HeaderTimeout bounds the wait for stream response headers
	// (0 = DefaultHeaderTimeout).
	HeaderTimeout time.Duration

	hc *http.Client
}

// New returns a client for socket without contacting the daemon.
func New(socket string) *Client {
	c := &Client{socket: socket}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			if err := checkSocket(socket); err != nil {
				return nil, err
			}
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableCompression:  true,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
	}
	c.hc = &http.Client{Transport: tr}
	return c
}

// Socket returns the socket path.
func (c *Client) Socket() string { return c.socket }

// CloseIdle closes idle keep-alive connections.
func (c *Client) CloseIdle() { c.hc.CloseIdleConnections() }

func checkSocket(p string) error {
	if p == "" {
		return fmt.Errorf("%w: empty", ErrSocketPath)
	}
	if len(p) > MaxSocketPath {
		return fmt.Errorf("%w: %d bytes > %d: %s", ErrSocketPath, len(p), MaxSocketPath, p)
	}
	return nil
}

// Dial returns a client once the daemon answers on socket, retrying with
// exponential backoff (10ms doubling to 250ms) for up to wait. Any HTTP
// answer to GET /healthz except a server error counts as reachable: a 5xx
// (the daemon's own health check reporting its loop stalled) is retried
// like no answer, and returned as the error once wait is over. An invalid
// socket path fails at once.
func Dial(ctx context.Context, socket string, wait time.Duration) (*Client, error) {
	if err := checkSocket(socket); err != nil {
		return nil, err
	}
	c := New(socket)
	deadline := time.Now().Add(wait)
	backoff := minBackoff
	for {
		attempt := c.Timeout
		if attempt == 0 {
			attempt = DefaultTimeout
		}
		if rem := time.Until(deadline); rem < attempt {
			attempt = rem
		}
		if attempt < minBackoff {
			attempt = minBackoff
		}
		actx, cancel := context.WithTimeout(ctx, attempt)
		err := c.Ping(actx)
		cancel()
		if !IsDown(err) {
			return c, nil
		}
		if ctx.Err() != nil {
			c.CloseIdle()
			return nil, ctx.Err()
		}
		rem := time.Until(deadline)
		if rem <= 0 {
			c.CloseIdle()
			return nil, err
		}
		sl := backoff
		if sl > rem {
			sl = rem
		}
		t := time.NewTimer(sl)
		select {
		case <-ctx.Done():
			t.Stop()
			c.CloseIdle()
			return nil, ctx.Err()
		case <-t.C:
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c *Client) headerTimeout() time.Duration {
	if c.HeaderTimeout > 0 {
		return c.HeaderTimeout
	}
	return DefaultHeaderTimeout
}

func jobPath(id, suffix string) string {
	return "/v1/jobs/" + url.PathEscape(id) + suffix
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("client: encode %s: %w", path, err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pace"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func apiError(req *http.Request, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &APIError{Method: req.Method, Path: req.URL.Path, Status: resp.StatusCode, Body: string(b)}
}

// do runs a bounded request and decodes a JSON answer into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return c.doWithin(ctx, c.timeout(), method, path, body, out)
}

// doWithin is do bounded by d instead of Client.Timeout.
func (c *Client) doWithin(ctx context.Context, d time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(req, resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	if s, ok := out.(*string); ok {
		b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return err
		}
		*s = string(b)
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<20))
	if err := dec.Decode(out); err != nil {
		return &DecodeError{Line: path, Err: err}
	}
	return nil
}

// stream opens a streaming request. ctx governs the whole stream's life;
// the response headers must arrive within HeaderTimeout.
func (c *Client) stream(ctx context.Context, method, path string, body any) (*Stream, error) {
	sctx, cancel := context.WithCancel(ctx)
	req, err := c.newRequest(sctx, method, path, body)
	if err != nil {
		cancel()
		return nil, err
	}
	timer := time.AfterFunc(c.headerTimeout(), cancel)
	resp, err := c.hc.Do(req)
	if !timer.Stop() && err == nil {
		// headers arrived exactly as the timer fired: the context is dead.
		resp.Body.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		err := apiError(req, resp)
		resp.Body.Close()
		cancel()
		return nil, err
	}
	return newStream(resp.Body, cancel), nil
}

// Ping checks GET /healthz.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

// Probe is Ping bounded by d instead of Client.Timeout: a liveness check
// that must tolerate a daemon that is slow, not stalled.
func (c *Client) Probe(ctx context.Context, d time.Duration) error {
	return c.doWithin(ctx, d, http.MethodGet, "/healthz", nil, nil)
}

// Submit asks for admission (POST /v1/jobs) and returns the job's event
// stream: ACCEPTED, QUEUED*, ADMITTED|REFUSED, then live events.
func (c *Client) Submit(ctx context.Context, req proto.SubmitRequest) (*Stream, error) {
	return c.stream(ctx, http.MethodPost, "/v1/jobs", req)
}

// Attach re-registers a running job with a (restarted) daemon and returns
// the job's event stream.
func (c *Client) Attach(ctx context.Context, id string, req proto.AttachRequest) (*Stream, error) {
	return c.stream(ctx, http.MethodPost, jobPath(id, "/attach"), req)
}

// Started reports the spawned job process.
func (c *Client) Started(ctx context.Context, id string, req proto.StartedRequest) error {
	return c.do(ctx, http.MethodPost, jobPath(id, "/started"), req, nil)
}

// Progress is the ~1s heartbeat with output progress.
func (c *Client) Progress(ctx context.Context, id string, req proto.ProgressRequest) error {
	return c.do(ctx, http.MethodPost, jobPath(id, "/progress"), req, nil)
}

// Exit reports how the job ended.
func (c *Client) Exit(ctx context.Context, id string, req proto.ExitRequest) error {
	return c.do(ctx, http.MethodPost, jobPath(id, "/exit"), req, nil)
}

// Status returns the machine-wide view (GET /v1/status).
func (c *Client) Status(ctx context.Context) (proto.Status, error) {
	var s proto.Status
	err := c.do(ctx, http.MethodGet, "/v1/status", nil, &s)
	return s, err
}

// Job returns one job (GET /v1/jobs/{id}), kept 24h after it ends.
func (c *Client) Job(ctx context.Context, id string) (proto.JobStatus, error) {
	var j proto.JobStatus
	err := c.do(ctx, http.MethodGet, jobPath(id, ""), nil, &j)
	return j, err
}

// Follow streams every event (GET /v1/events?follow=1), the alert channel.
func (c *Client) Follow(ctx context.Context) (*Stream, error) {
	return c.stream(ctx, http.MethodGet, "/v1/events?follow=1", nil)
}

// Reload asks the daemon to re-read its config (POST /v1/reload).
func (c *Client) Reload(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/reload", nil, nil)
}

// Metrics returns the Prometheus text exposition (GET /metrics).
func (c *Client) Metrics(ctx context.Context) (string, error) {
	var s string
	err := c.do(ctx, http.MethodGet, "/metrics", nil, &s)
	return s, err
}

// Stream reads NDJSON events. It is not safe for concurrent Next calls;
// Close may be called from any goroutine.
type Stream struct {
	body   io.ReadCloser
	r      *bufio.Reader
	cancel context.CancelFunc
}

func newStream(body io.ReadCloser, cancel context.CancelFunc) *Stream {
	return &Stream{body: body, r: bufio.NewReaderSize(body, 16<<10), cancel: cancel}
}

// NewStream wraps any NDJSON reader (tests, files).
func NewStream(r io.ReadCloser) *Stream { return newStream(r, func() {}) }

// Next returns the next event. Blank lines are skipped. A malformed line
// yields a *DecodeError and the stream stays usable. The end of the stream
// is io.EOF; anything else is a transport error.
func (s *Stream) Next() (proto.Event, error) {
	for {
		line, err := s.readLine()
		if len(bytes.TrimSpace(line)) > 0 {
			var ev proto.Event
			if derr := json.Unmarshal(line, &ev); derr != nil {
				return proto.Event{}, &DecodeError{Line: string(line), Err: derr}
			}
			return ev, nil
		}
		if err != nil {
			return proto.Event{}, err
		}
	}
}

// readLine returns one line without its terminator. A final unterminated
// line is returned together with the read error (usually io.EOF).
func (s *Stream) readLine() ([]byte, error) {
	var buf []byte
	for {
		frag, err := s.r.ReadSlice('\n')
		if len(buf)+len(frag) > MaxLine {
			// discard the rest of the over-long line
			for err == bufio.ErrBufferFull {
				_, err = s.r.ReadSlice('\n')
			}
			if err != nil {
				return nil, err
			}
			head := buf
			if len(head) == 0 {
				head = frag[:min(len(frag), 64)]
			}
			return nil, &DecodeError{Line: truncate(string(head), 64), Err: errors.New("line too long")}
		}
		buf = append(buf, frag...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == nil {
			return bytes.TrimRight(buf, "\r\n"), nil
		}
		return buf, err
	}
}

// Close ends the stream (and the underlying HTTP request).
func (s *Stream) Close() error {
	s.cancel()
	return s.body.Close()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
