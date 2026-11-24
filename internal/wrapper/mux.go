package wrapper

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// mux is the single writer of the wrapper's stderr. Child bytes are written
// through immediately, never buffered. Governor lines are emitted only at a
// child line boundary: when the child is mid-line, a pace line is held until
// the child's next newline (and spliced in right after it, inside the same
// write), or for at most hold, after which a newline is forced first.
//
// In direct mode (the child writes to stderr itself, e.g. a TTY) the mux
// cannot see child bytes and writes pace lines at once.
type mux struct {
	mu      sync.Mutex
	w       io.Writer
	hold    time.Duration
	direct  bool
	closed  bool // child output finished: pace lines flush at once
	bol     bool // at beginning of line (last child byte was '\n', or none yet)
	pending []byte
	timer   *time.Timer
	werr    error // first error writing child bytes
}

func newMux(w io.Writer, hold time.Duration, direct bool) *mux {
	return &mux{w: w, hold: hold, direct: direct, bol: true}
}

// Child writes child bytes. It returns the error of the underlying write so
// the caller can stop relaying (and let the child see EPIPE) when the
// destination is gone.
func (m *mux) Child(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.pending) > 0 {
		if i := bytes.IndexByte(p, '\n'); i >= 0 {
			if err := m.writeChild(p[:i+1]); err != nil {
				return err
			}
			m.bol = true
			m.flushLocked()
			p = p[i+1:]
			if len(p) == 0 {
				return nil
			}
		}
	}
	if err := m.writeChild(p); err != nil {
		return err
	}
	m.bol = p[len(p)-1] == '\n'
	return nil
}

func (m *mux) writeChild(p []byte) error {
	_, err := m.w.Write(p)
	if err != nil && m.werr == nil {
		m.werr = err
	}
	return err
}

// Pace emits complete governor lines (each without trailing newline).
func (m *mux) Pace(lines ...string) {
	if len(lines) == 0 {
		return
	}
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pending = append(m.pending, b...)
	if m.direct || m.bol {
		m.flushLocked()
		return
	}
	if m.closed {
		m.forceLocked()
		return
	}
	if m.timer == nil {
		m.timer = time.AfterFunc(m.hold, m.expire)
	}
}

// expire forces a line break before held pace lines.
func (m *mux) expire() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.timer = nil
	if len(m.pending) == 0 {
		return
	}
	m.forceLocked()
}

func (m *mux) forceLocked() {
	if !m.bol && !m.direct {
		_, _ = m.w.Write([]byte{'\n'})
		m.bol = true
	}
	m.flushLocked()
}

func (m *mux) flushLocked() {
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	if len(m.pending) > 0 {
		// pace line write errors are ignored: nothing useful can be done.
		_, _ = m.w.Write(m.pending)
		m.pending = m.pending[:0]
	}
}

// Close marks the end of child output: held lines are flushed now (with a
// forced newline if the child ended mid-line) and later lines go straight
// out. Close is idempotent.
func (m *mux) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if len(m.pending) > 0 {
		m.forceLocked()
	}
}
