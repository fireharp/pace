package integrate

import (
	"bufio"
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

// Client talks to the daemon's HTTP/1.1 + JSON API on its unix socket.
type Client struct {
	Socket string
	// Timeout bounds non-streaming requests (0 = 2s). Streams are bounded
	// only by their context.
	Timeout time.Duration
}

// UnreachableError means no daemon answered on the socket.
type UnreachableError struct {
	Socket string
	Err    error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("daemon unreachable at %s: %v", e.Socket, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

// IsUnreachable reports whether err means the daemon is not answering.
func IsUnreachable(err error) bool {
	var u *UnreachableError
	return errors.As(err, &u)
}

// ErrJobNotFound is returned by Job for an unknown id.
var ErrJobNotFound = errors.New("job not found")

// MaxSocketPath is the longest unix socket path the kernel accepts
// (sun_path is 104 bytes on macOS including the NUL).
const MaxSocketPath = 103

func (c *Client) httpClient(stream bool) *http.Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, "unix", c.Socket)
			if err != nil {
				return nil, &UnreachableError{Socket: c.Socket, Err: err}
			}
			return conn, nil
		},
		DisableKeepAlives: true,
	}
	hc := &http.Client{Transport: tr}
	if !stream {
		to := c.Timeout
		if to <= 0 {
			to = 2 * time.Second
		}
		hc.Timeout = to
	}
	return hc
}

func (c *Client) do(ctx context.Context, method, path string, stream bool) (*http.Response, error) {
	if c.Socket == "" {
		return nil, &UnreachableError{Socket: c.Socket, Err: errors.New("no socket path")}
	}
	if len(c.Socket) > MaxSocketPath {
		return nil, &UnreachableError{Socket: c.Socket, Err: fmt.Errorf("socket path is %d bytes, longer than the %d-byte limit", len(c.Socket), MaxSocketPath)}
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://pace"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient(stream).Do(req)
	if err != nil {
		var u *UnreachableError
		if errors.As(err, &u) {
			return nil, u
		}
		// Deadline while connecting or before any response: treat as unreachable,
		// the daemon is not answering in time.
		return nil, &UnreachableError{Socket: c.Socket, Err: err}
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, v any) ([]byte, error) {
	resp, err := c.do(ctx, http.MethodGet, path, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return body, ErrJobNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return body, fmt.Errorf("GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, v); err != nil {
		return body, fmt.Errorf("GET %s: bad JSON: %w", path, err)
	}
	return body, nil
}

// Status returns GET /v1/status, decoded and raw.
func (c *Client) Status(ctx context.Context) (proto.Status, []byte, error) {
	var s proto.Status
	raw, err := c.getJSON(ctx, "/v1/status", &s)
	return s, raw, err
}

// Job returns GET /v1/jobs/{id}, decoded and raw.
func (c *Client) Job(ctx context.Context, id string) (proto.JobStatus, []byte, error) {
	var j proto.JobStatus
	raw, err := c.getJSON(ctx, "/v1/jobs/"+url.PathEscape(id), &j)
	return j, raw, err
}

// Health checks GET /healthz.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, "/healthz", false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
	return nil
}

// Reload asks the daemon to re-read its config (POST /v1/reload).
func (c *Client) Reload(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodPost, "/v1/reload", false)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("reload: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// Follow streams GET /v1/events?follow=1 and calls fn for each event line
// (raw bytes and decoded form) until ctx ends, the stream closes, or fn
// returns an error. Lines that fail to decode are passed with a zero Event.
// A clean end of stream returns io.EOF.
func (c *Client) Follow(ctx context.Context, fn func(raw []byte, ev proto.Event) error) error {
	resp, err := c.do(ctx, http.MethodGet, "/v1/events?follow=1", true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("events: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var ev proto.Event
		_ = json.Unmarshal(line, &ev)
		if err := fn(append([]byte(nil), line...), ev); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return io.EOF
}
