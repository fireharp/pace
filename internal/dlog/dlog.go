// Package dlog is the decision log: one self-contained JSONL record per
// non-empty policy step (plus heartbeats), rotated by size, written by a
// background goroutine that never blocks the policy loop. Replay re-runs
// policy.Step on each record's (Params, State, Inputs) and diffs the result.
//
// Files: <dir>/decisions.jsonl is the active file. On rotation it becomes
// decisions.jsonl.1, older files shift up (.1 -> .2 ...), and rotated files
// are gzipped in the background (decisions.jsonl.N.gz). At most Keep rotated
// files are kept. LogFiles lists them oldest first, for Replay.
//
// Durability: records are buffered and flushed to the OS at most every
// FlushEvery (default 1s) and on Close; the file is fsynced on rotation and
// on Close. A crash loses at most the last flush interval. A record that
// cannot be enqueued (queue full) or written (I/O error) is counted and
// reported by a later KindGap record; Write never blocks.
package dlog

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fireharp/pace/internal/policy"
)

// Record kinds.
const (
	KindStart     = "start"     // daemon started (version, params)
	KindStep      = "step"      // a step that decided something or changed state
	KindHeartbeat = "heartbeat" // periodic, even if nothing happened
	KindConfig    = "config"    // config (re)loaded
	KindGap       = "gap"       // records dropped because the writer fell behind
	KindStop      = "stop"      // daemon stopping: throttles and freezes it released (State is after)
)

// FileName is the active log file name inside the log directory.
const FileName = "decisions.jsonl"

// Record is one log line.
type Record struct {
	V         int            `json:"v"`
	Kind      string         `json:"kind"`
	Seq       int64          `json:"seq"`
	TS        int64          `json:"ts"`
	Version   string         `json:"ver,omitempty"`
	CfgHash   string         `json:"cfg,omitempty"`
	Params    *policy.Params `json:"params,omitempty"`
	State     *policy.State  `json:"state,omitempty"` // state BEFORE the step
	In        *policy.Inputs `json:"in,omitempty"`
	Out       *policy.Output `json:"out,omitempty"`
	StateHash string         `json:"state_hash,omitempty"` // hash of state AFTER the step
	Dropped   int64          `json:"dropped,omitempty"`    // KindGap
	Msg       string         `json:"msg,omitempty"`
}

// Options control rotation and buffering. Zero values mean the default.
type Options struct {
	MaxBytes int64 // per file before rotation (default 64 MiB)
	Keep     int   // rotated files kept (default 8)
	Queue    int   // buffered records (default 1024)
	// FlushEvery bounds how long a record may sit in the write buffer
	// (default 1s).
	FlushEvery time.Duration
}

const (
	defMaxBytes   = 64 << 20
	defKeep       = 8
	defQueue      = 1024
	defFlushEvery = time.Second
)

func (o Options) withDefaults() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = defMaxBytes
	}
	if o.Keep <= 0 {
		o.Keep = defKeep
	}
	if o.Queue <= 0 {
		o.Queue = defQueue
	}
	if o.FlushEvery <= 0 {
		o.FlushEvery = defFlushEvery
	}
	return o
}

// Stats are cumulative writer counters.
type Stats struct {
	Written   int64 `json:"written"`   // records written (including gap records)
	Dropped   int64 `json:"dropped"`   // records lost (queue full, write error, or after Close)
	Errors    int64 `json:"errors"`    // I/O or encoding errors
	Rotations int64 `json:"rotations"` // completed rotations
}

// Writer appends records asynchronously. Write may be called from any
// goroutine; it never blocks on I/O.
type Writer struct {
	dir  string
	path string
	opts Options

	mu     sync.RWMutex // Write holds R; Close holds W to close ch exactly once
	closed bool
	ch     chan Record
	done   chan struct{}

	pending atomic.Int64 // drops not yet reported by a gap record
	dropped atomic.Int64
	written atomic.Int64
	errs    atomic.Int64
	rotated atomic.Int64

	openSeq int64 // seq of the last record in the log when opened

	// Owned by the background goroutine.
	f     *os.File
	bw    *bufio.Writer
	size  int64
	seq   int64
	dirty bool
	// retryRotateAt delays the next rotation attempt after a failed one.
	retryRotateAt int64

	compress sync.WaitGroup // background gzip of rotated files
	closeErr error

	beforeWrite func(Record) // test hook, called by the goroutine before writing a record
}

// Open creates dir if needed and opens dir/decisions.jsonl for append.
func Open(dir string, o Options) (*Writer, error) { return open(dir, o, nil) }

func open(dir string, o Options, hook func(Record)) (*Writer, error) {
	o = o.withDefaults()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("dlog: %w", err)
	}
	w := &Writer{
		dir:         dir,
		path:        filepath.Join(dir, FileName),
		opts:        o,
		ch:          make(chan Record, o.Queue),
		done:        make(chan struct{}),
		beforeWrite: hook,
	}
	if err := w.openActive(); err != nil {
		return nil, err
	}
	w.seq = lastSeq(w.path, w.size)
	if w.seq == 0 {
		// The active file is new or was just rotated: continue from the
		// newest rotated file.
		w.seq = lastRotatedSeq(w.rotatedPath(1))
	}
	w.openSeq = w.seq
	// Rotated files left uncompressed by a crash are compressed now.
	w.compress.Add(1)
	go func() {
		defer w.compress.Done()
		for i := 1; i <= o.Keep; i++ {
			p := w.rotatedPath(i)
			if _, err := os.Stat(p); err == nil {
				if err := gzipFile(p); err != nil {
					w.errs.Add(1)
				}
			}
		}
	}()
	go w.loop()
	return w, nil
}

func (w *Writer) openActive() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("dlog: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("dlog: %w", err)
	}
	w.f, w.size = f, st.Size()
	if w.bw == nil {
		w.bw = bufio.NewWriterSize(f, 64<<10)
	} else {
		w.bw.Reset(f)
	}
	return nil
}

// Write enqueues r without blocking; if the queue is full the record is
// counted as dropped and a KindGap record is written later. V defaults to 1,
// TS to the current time; Seq is assigned by the writer when zero, strictly
// increasing in file order (continuing from the last record in the file).
func (w *Writer) Write(r Record) {
	if r.V == 0 {
		r.V = 1
	}
	if r.TS == 0 {
		r.TS = time.Now().UnixMilli()
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		w.dropped.Add(1)
		return
	}
	select {
	case w.ch <- r:
	default:
		w.pending.Add(1)
		w.dropped.Add(1)
	}
}

// Close flushes, fsyncs and closes. It waits for background compression.
// Safe to call more than once.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	close(w.ch)
	w.mu.Unlock()
	<-w.done
	w.compress.Wait()
	return w.closeErr
}

// LastSeq returns the seq of the last record the log held when it was
// opened (0 if none), so a caller that assigns its own seqs can continue
// after it.
func (w *Writer) LastSeq() int64 { return w.openSeq }

// Path returns the active log file path.
func (w *Writer) Path() string { return w.path }

// Stats returns cumulative counters.
func (w *Writer) Stats() Stats {
	return Stats{Written: w.written.Load(), Dropped: w.dropped.Load(), Errors: w.errs.Load(), Rotations: w.rotated.Load()}
}

func (w *Writer) rotatedPath(i int) string { return w.path + "." + strconv.Itoa(i) }

func (w *Writer) loop() {
	defer close(w.done)
	t := time.NewTicker(w.opts.FlushEvery)
	defer t.Stop()
	for {
		select {
		case r, ok := <-w.ch:
			if !ok {
				w.writeGap()
				w.closeErr = w.closeActive()
				return
			}
			w.writeGap()
			w.put(r)
		case <-t.C:
			w.writeGap()
			if w.dirty {
				w.flush()
			}
		}
	}
}

func (w *Writer) flush() {
	w.dirty = false
	if w.bw == nil || w.f == nil {
		return
	}
	if err := w.bw.Flush(); err != nil {
		w.errs.Add(1)
	}
}

func (w *Writer) closeActive() error {
	if w.f == nil {
		return nil
	}
	err := w.bw.Flush()
	if e := w.f.Sync(); err == nil {
		err = e
	}
	if e := w.f.Close(); err == nil {
		err = e
	}
	w.f = nil
	if err != nil {
		w.errs.Add(1)
		return fmt.Errorf("dlog: %w", err)
	}
	return nil
}

// writeGap reports drops accumulated since the last gap record.
func (w *Writer) writeGap() {
	n := w.pending.Swap(0)
	if n == 0 {
		return
	}
	ok := w.writeRecord(Record{
		V: 1, Kind: KindGap, TS: time.Now().UnixMilli(), Dropped: n,
		Msg: "decision log writer fell behind; records dropped",
	})
	if !ok {
		w.pending.Add(n) // report them with the next gap record
	}
}

// put writes one caller record, counting it as dropped on failure.
func (w *Writer) put(r Record) {
	if !w.writeRecord(r) {
		w.pending.Add(1)
		w.dropped.Add(1)
	}
}

// writeRecord encodes and buffers r, rotating first when needed. It reports
// whether the record was written.
func (w *Writer) writeRecord(r Record) bool {
	if w.beforeWrite != nil {
		w.beforeWrite(r)
	}
	if r.Seq <= w.seq {
		// Unassigned, or a caller's seq that would repeat or go backwards
		// (its own counter restarted): seq is strictly increasing in file
		// order whatever callers pass.
		r.Seq = w.seq + 1
	}
	b, err := json.Marshal(r)
	if err != nil {
		w.errs.Add(1)
		return false
	}
	b = append(b, '\n')
	if w.f == nil {
		// A previous rotation or open failed: try again.
		if err := w.openActive(); err != nil {
			w.errs.Add(1)
			return false
		}
	}
	if w.size > 0 && w.size+int64(len(b)) > w.opts.MaxBytes && w.size >= w.retryRotateAt {
		if err := w.rotate(); err != nil {
			w.errs.Add(1)
			if w.f == nil {
				return false
			}
			// Keep appending; retry after another 1/8 of MaxBytes rather
			// than paying a close+fsync on every record.
			w.retryRotateAt = w.size + w.opts.MaxBytes/8
		} else {
			w.retryRotateAt = 0
		}
	}
	if _, err := w.bw.Write(b); err != nil {
		w.errs.Add(1)
		// Drop the broken buffer; reopen on the next record.
		w.bw.Reset(io.Discard)
		w.f.Close()
		w.f = nil
		return false
	}
	if r.Seq > w.seq {
		w.seq = r.Seq
	}
	w.size += int64(len(b))
	w.dirty = true
	w.written.Add(1)
	return true
}

// rotate closes the active file, shifts rotated files, renames the active
// file to .1, starts compressing it and opens a fresh active file.
func (w *Writer) rotate() error {
	if err := w.closeActive(); err != nil {
		return err
	}
	// Never shift while the previous rotated file is still being compressed.
	w.compress.Wait()
	k := w.opts.Keep
	for _, p := range []string{w.rotatedPath(k) + ".gz", w.rotatedPath(k)} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			w.errs.Add(1)
		}
	}
	for i := k - 1; i >= 1; i-- {
		for _, sfx := range []string{".gz", ""} {
			from, to := w.rotatedPath(i)+sfx, w.rotatedPath(i+1)+sfx
			if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
				w.errs.Add(1)
			}
		}
	}
	first := w.rotatedPath(1)
	renameErr := os.Rename(w.path, first)
	if err := w.openActive(); err != nil {
		return err
	}
	if renameErr != nil {
		// Keep appending to the old file rather than losing records.
		return fmt.Errorf("dlog: rotate: %w", renameErr)
	}
	w.rotated.Add(1)
	w.compress.Add(1)
	go func() {
		defer w.compress.Done()
		if err := gzipFile(first); err != nil {
			w.errs.Add(1)
		}
	}()
	return nil
}

// gzipFile compresses p to p.gz (via a temp file and rename) and removes p.
func gzipFile(p string) error {
	in, err := os.Open(p)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := p + ".gz.tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// BestSpeed: rotation happens under load; JSONL still compresses ~10x.
	zw, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	_, err = io.Copy(zw, in)
	if e := zw.Close(); err == nil {
		err = e
	}
	if e := out.Sync(); err == nil {
		err = e
	}
	if e := out.Close(); err == nil {
		err = e
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p+".gz"); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Remove(p)
}

// lastRotatedSeq returns the last seq in a rotated file, plain or already
// compressed (path + ".gz"); 0 if neither is readable.
func lastRotatedSeq(path string) int64 {
	if fi, err := os.Stat(path); err == nil {
		return lastSeq(path, fi.Size())
	}
	var last int64
	_ = ReadRecords(path+".gz", func(r Record) error {
		if r.Seq > last {
			last = r.Seq
		}
		return nil
	})
	return last
}

// lastSeq returns the seq of the last complete record in path (0 if none).
// It reads at most the final 1 MiB.
func lastSeq(path string, size int64) int64 {
	if size <= 0 {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	const tail = 1 << 20
	off := size - tail
	if off < 0 {
		off = 0
	}
	buf := make([]byte, size-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return 0
	}
	for len(buf) > 0 {
		buf = bytes.TrimRight(buf, "\n")
		i := bytes.LastIndexByte(buf, '\n')
		line := buf[i+1:]
		var r struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(line, &r) == nil && r.Seq > 0 {
			return r.Seq
		}
		if i < 0 {
			break
		}
		buf = buf[:i]
	}
	return 0
}

// LogFiles returns the decision log files in dir, oldest first (rotated
// files from the highest number down, then the active file). Only files that
// exist are returned.
func LogFiles(dir string) []string {
	base := filepath.Join(dir, FileName)
	var rotated []int
	ents, _ := os.ReadDir(dir)
	seen := map[int]bool{}
	for _, e := range ents {
		name := e.Name()
		if len(name) <= len(FileName)+1 || name[:len(FileName)+1] != FileName+"." {
			continue
		}
		rest := name[len(FileName)+1:]
		if len(rest) > 3 && rest[len(rest)-3:] == ".gz" {
			rest = rest[:len(rest)-3]
		}
		n, err := strconv.Atoi(rest)
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		rotated = append(rotated, n)
	}
	// Highest number is oldest.
	sort.Sort(sort.Reverse(sort.IntSlice(rotated)))
	var out []string
	for _, n := range rotated {
		p := base + "." + strconv.Itoa(n)
		// Prefer the compressed file; a plain one exists only mid-compression or after a crash.
		if _, err := os.Stat(p + ".gz"); err == nil {
			out = append(out, p+".gz")
		} else if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	if _, err := os.Stat(base); err == nil {
		out = append(out, base)
	}
	return out
}

// StateHash is the canonical hash of a policy state ("sha256:<hex>"):
// sha256 over encoding/json's encoding. State contains only structs, slices
// and scalars (no maps, no floats), so the encoding is canonical and
// survives a JSON round trip unchanged.
func StateHash(s policy.State) string {
	b, err := json.Marshal(s)
	if err != nil {
		return "sha256:error"
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
