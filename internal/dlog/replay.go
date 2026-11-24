package dlog

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/fireharp/pace/internal/policy"
)

// MaxLineBytes is the longest record line the reader accepts; longer lines
// are reported as corrupt and skipped.
const MaxLineBytes = 64 << 20

var maxLine = MaxLineBytes // tests lower it

// ErrNotRecord marks a line that is valid JSON but not a log record.
var ErrNotRecord = errors.New("not a decision log record")

// ReadRecords calls fn for every record in path (plain JSONL or gzip,
// detected by content). Corrupt lines are skipped silently; use ScanRecords
// to observe them. An error from fn stops the scan and is returned.
func ReadRecords(path string, fn func(Record) error) error {
	return ScanRecords(path, fn, nil)
}

// ScanRecords is ReadRecords with a callback for corrupt lines (1-based line
// number). A truncated or damaged gzip stream is reported as a corrupt line
// and ends the file without error; failing to open or read the file is an
// error.
func ScanRecords(path string, fn func(Record) error, bad func(line int, err error)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	lr := br
	if magic, _ := br.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(br)
		if err != nil {
			if bad != nil {
				bad(1, fmt.Errorf("gzip: %w", err))
			}
			return nil
		}
		defer zr.Close()
		lr = bufio.NewReaderSize(zr, 64<<10)
	}
	line := 0
	for {
		b, tooLong, err := readLine(lr, maxLine)
		if len(b) > 0 || tooLong {
			line++
			switch {
			case tooLong:
				if bad != nil {
					bad(line, fmt.Errorf("line longer than %d bytes", maxLine))
				}
			default:
				rec, derr := decodeRecord(b)
				if derr != nil {
					if bad != nil {
						bad(line, derr)
					}
				} else if ferr := fn(rec); ferr != nil {
					return ferr
				}
			}
		} else if err == nil {
			line++ // empty line
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if isCorruptStream(err) {
				if bad != nil {
					bad(line+1, fmt.Errorf("damaged compressed stream: %w", err))
				}
				return nil
			}
			return err
		}
	}
}

func isCorruptStream(err error) bool {
	var ce flate.CorruptInputError
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, gzip.ErrChecksum) ||
		errors.Is(err, gzip.ErrHeader) || errors.As(err, &ce)
}

// readLine returns the next line without its newline. A line over max bytes
// is consumed and reported with tooLong=true (and no data).
func readLine(r *bufio.Reader, max int) (line []byte, tooLong bool, err error) {
	var buf []byte
	for {
		chunk, e := r.ReadSlice('\n')
		if !tooLong {
			if len(buf)+len(chunk) > max+1 {
				tooLong, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if e == bufio.ErrBufferFull {
			continue
		}
		if e != nil && len(buf) == 0 && !tooLong {
			return nil, false, e
		}
		buf = bytes.TrimRight(buf, "\r\n")
		if tooLong {
			buf = nil
		}
		return buf, tooLong, e
	}
}

func decodeRecord(b []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, err
	}
	if rec.V <= 0 || rec.Kind == "" {
		return Record{}, ErrNotRecord
	}
	return rec, nil
}

// ReplayReport summarises a replay.
type ReplayReport struct {
	Records    int `json:"records"`
	Replayed   int `json:"replayed"`
	Mismatches int `json:"mismatches"`
	// Skipped step records had no Params (and none earlier in the log) and
	// no override was given.
	Skipped int `json:"skipped"`
	// Corrupt lines could not be decoded and were skipped.
	Corrupt int `json:"corrupt"`
	// Gaps counts KindGap records; Dropped sums their dropped counts.
	Gaps    int   `json:"gaps"`
	Dropped int64 `json:"dropped"`
}

// maxDiffs bounds the differences printed per record.
const maxDiffs = 4

// Replay re-runs every step record in files (in order). If override is
// non-nil it is used instead of each record's Params (what-if). A step
// record without Params uses the most recent Params seen earlier in the log
// (start/config records carry them). Mismatches are described on out, one
// line per record, followed by one summary line. Returns an error only for
// unreadable input.
func Replay(files []string, override *policy.Params, out io.Writer) (ReplayReport, error) {
	var rep ReplayReport
	var last *policy.Params
	for _, file := range files {
		err := ScanRecords(file, func(rec Record) error {
			rep.Records++
			if rec.Params != nil {
				last = rec.Params
			}
			if rec.Kind == KindGap {
				rep.Gaps++
				rep.Dropped += rec.Dropped
				fmt.Fprintf(out, "%s seq=%d: gap: %d records dropped by the writer\n", file, rec.Seq, rec.Dropped)
				return nil
			}
			if rec.Kind != KindStep || rec.State == nil || rec.In == nil {
				return nil
			}
			params := override
			if params == nil {
				params = last
			}
			if params == nil {
				rep.Skipped++
				return nil
			}
			rep.Replayed++
			if diffs := replayOne(*params, rec); len(diffs) > 0 {
				rep.Mismatches++
				fmt.Fprintf(out, "%s seq=%d ts=%d: MISMATCH %s\n", file, rec.Seq, rec.TS, strings.Join(diffs, "; "))
			}
			return nil
		}, func(line int, err error) {
			rep.Corrupt++
			fmt.Fprintf(out, "%s:%d: warning: skipped corrupt line: %v\n", file, line, err)
		})
		if err != nil {
			return rep, err
		}
	}
	fmt.Fprintf(out, "replay: records=%d replayed=%d mismatches=%d skipped=%d corrupt=%d gaps=%d\n",
		rep.Records, rep.Replayed, rep.Mismatches, rep.Skipped, rep.Corrupt, rep.Gaps)
	return rep, nil
}

// replayOne runs Step for one record and returns the differences.
func replayOne(p policy.Params, rec Record) (diffs []string) {
	defer func() {
		if r := recover(); r != nil {
			diffs = []string{fmt.Sprintf("step panicked: %v", r)}
		}
	}()
	ns, o := policy.Step(p, *rec.State, *rec.In)
	want := rec.Out
	if want == nil {
		want = &policy.Output{}
	}
	gotJ, err1 := json.Marshal(o)
	wantJ, err2 := json.Marshal(want)
	if err1 != nil || err2 != nil {
		return []string{fmt.Sprintf("cannot encode output: %v %v", err1, err2)}
	}
	if !bytes.Equal(gotJ, wantJ) {
		for _, d := range JSONDiff(wantJ, gotJ, maxDiffs) {
			diffs = append(diffs, "out"+d)
		}
	}
	if rec.StateHash != "" {
		if h := StateHash(ns); h != rec.StateHash {
			diffs = append(diffs, fmt.Sprintf("state_hash: logged %s, replay %s", short(rec.StateHash), short(h)))
		}
	}
	return diffs
}

func short(h string) string {
	if len(h) > 19 {
		return h[:19]
	}
	return h
}

// JSONDiff compares two JSON documents structurally and returns up to max
// differences as "<path>: want X, got Y" (paths like ".actions[0].signal").
// Numbers are compared exactly (no float conversion).
func JSONDiff(want, got []byte, max int) []string {
	var a, b any
	da := json.NewDecoder(bytes.NewReader(want))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(got))
	db.UseNumber()
	if err := da.Decode(&a); err != nil {
		return []string{fmt.Sprintf(": cannot decode logged value: %v", err)}
	}
	if err := db.Decode(&b); err != nil {
		return []string{fmt.Sprintf(": cannot decode replayed value: %v", err)}
	}
	var out []string
	diffValue("", a, b, &out, max)
	if len(out) == 0 && !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(got)) {
		out = append(out, ": encodings differ")
	}
	return out
}

func diffValue(path string, a, b any, out *[]string, max int) {
	if len(*out) >= max {
		return
	}
	switch av := a.(type) {
	case map[string]any:
		bv, ok := b.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(av)+len(bv))
		for k := range av {
			keys = append(keys, k)
		}
		for k := range bv {
			if _, ok := av[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			x, okA := av[k]
			y, okB := bv[k]
			p := path + "." + k
			switch {
			case !okA:
				add(out, max, fmt.Sprintf("%s: want absent, got %s", p, compact(y)))
			case !okB:
				add(out, max, fmt.Sprintf("%s: want %s, got absent", p, compact(x)))
			default:
				diffValue(p, x, y, out, max)
			}
		}
		return
	case []any:
		bv, ok := b.([]any)
		if !ok {
			break
		}
		n := len(av)
		if len(bv) < n {
			n = len(bv)
		}
		for i := 0; i < n; i++ {
			diffValue(path+"["+strconv.Itoa(i)+"]", av[i], bv[i], out, max)
		}
		if len(av) != len(bv) {
			add(out, max, fmt.Sprintf("%s: want %d elements, got %d", path, len(av), len(bv)))
		}
		return
	default:
		if fmt.Sprint(a) == fmt.Sprint(b) && sameKind(a, b) {
			return
		}
	}
	add(out, max, fmt.Sprintf("%s: want %s, got %s", path, compact(a), compact(b)))
}

func sameKind(a, b any) bool { return fmt.Sprintf("%T", a) == fmt.Sprintf("%T", b) }

func add(out *[]string, max int, s string) {
	if len(*out) < max {
		*out = append(*out, s)
	}
}

func compact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	s := string(b)
	if len(s) > 80 {
		s = s[:77] + "..."
	}
	return s
}
