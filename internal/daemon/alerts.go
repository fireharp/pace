package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// alertWriter appends alert events to alerts.jsonl from its own goroutine
// (file I/O can stall under memory pressure; the loop must not). It keeps
// one rotated file (alerts.jsonl.1) past maxBytes. Overflow drops lines
// and writes a gap marker later.
type alertWriter struct {
	path     string
	maxBytes int64
	ch       chan []byte
	dropped  atomic.Int64
	done     chan struct{}
	fault    func(kind, msg string)
}

func newAlertWriter(path string, maxBytes int64, fault func(kind, msg string)) *alertWriter {
	a := &alertWriter{path: path, maxBytes: maxBytes, ch: make(chan []byte, 256), done: make(chan struct{}), fault: fault}
	go a.run()
	return a
}

func (a *alertWriter) write(e *proto.Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	select {
	case a.ch <- b:
	default:
		a.dropped.Add(1)
	}
}

func (a *alertWriter) run() {
	defer close(a.done)
	var f *os.File
	var size int64
	open := func() bool {
		if f != nil {
			return true
		}
		var err error
		f, err = os.OpenFile(a.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			a.fault("alerts", err.Error())
			f = nil
			return false
		}
		if fi, err := f.Stat(); err == nil {
			size = fi.Size()
		}
		return true
	}
	put := func(b []byte) {
		if !open() {
			return
		}
		if size+int64(len(b)) > a.maxBytes && size > 0 {
			f.Close()
			f = nil
			_ = os.Rename(a.path, a.path+".1")
			if !open() {
				return
			}
		}
		n, err := f.Write(b)
		size += int64(n)
		if err != nil {
			a.fault("alerts", err.Error())
		}
	}
	for b := range a.ch {
		if n := a.dropped.Swap(0); n > 0 {
			g, _ := json.Marshal(proto.Event{V: 1, TS: time.Now().UnixMilli(), Verb: proto.VerbInfo, Msg: "alerts gap dropped=" + strconv.FormatInt(n, 10)})
			put(append(g, '\n'))
		}
		put(b)
	}
	if f != nil {
		f.Close()
	}
}

func (a *alertWriter) close() {
	close(a.ch)
	<-a.done
}

// osascriptNotify shows a macOS notification. The message travels as an
// argv item, never inside the script text, so it cannot inject AppleScript.
func osascriptNotify(title, msg string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript",
		"-e", "on run argv",
		"-e", "display notification (item 1 of argv) with title (item 2 of argv)",
		"-e", "end run", msg, title)
	_ = cmd.Run()
}
