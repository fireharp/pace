package wrapper

import (
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/proto"
)

// A job that was frozen and then continued (the daemon's THAWED, or a
// continue without the line: the daemon's release on shutdown, a kill
// -CONT) is not frozen any more: losing the daemon later must not claim to
// continue a frozen job, nor send it SIGCONT.
func TestLostDaemonAfterThawDoesNotClaimFrozen(t *testing.T) {
	for _, withLine := range []bool{true, false} {
		name := "THAWED relayed"
		if !withLine {
			name = "SIGCONT without THAWED"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			f := newFake(t, e.sock)
			thawed := make(chan struct{})
			f.onSubmit = func(s *jobStream) {
				s.send(proto.Event{Verb: proto.VerbAccepted})
				s.send(proto.Event{Verb: proto.VerbAdmitted})
				st := s.waitStarted(t)
				if st.PGID <= 1 {
					return
				}
				syscall.Kill(-st.PGID, syscall.SIGSTOP)
				s.send(proto.Event{Verb: proto.VerbFrozen, Policy: proto.PolicyPressure})
				time.Sleep(300 * time.Millisecond)
				syscall.Kill(-st.PGID, syscall.SIGCONT)
				if withLine {
					s.send(proto.Event{Verb: proto.VerbThawed, Policy: proto.PolicyPressure})
				}
				close(thawed)
				s.hold()
			}
			f.start()
			done := make(chan result, 1)
			go func() { done <- e.run(Options{Argv: sh("sleep 1.5; echo done")}) }()
			select {
			case <-thawed:
			case <-time.After(10 * time.Second):
				t.Fatal("job never thawed")
			}
			time.Sleep(300 * time.Millisecond) // the wrapper has seen the continue
			f.stop()                           // the daemon goes away
			res := <-done
			if res.code != 0 || res.stdout != "done\n" {
				t.Fatalf("code %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, "DEGRADED reason=daemon_lost") {
				t.Fatalf("stderr %q", res.stderr)
			}
			if strings.Contains(res.stderr, "lost daemon had frozen") {
				t.Fatalf("claims to continue a running job: %q", res.stderr)
			}
		})
	}
}

func TestContinuedStatusDecoding(t *testing.T) {
	for _, ws := range []syscall.WaitStatus{0xffff, syscall.WaitStatus(int(syscall.SIGCONT)<<8 | 0x7f)} {
		if !continuedStatus(ws) {
			t.Errorf("continuedStatus(%#x) = false", uint32(ws))
		}
		if _, ok := stopSignal(ws); ok {
			t.Errorf("stopSignal(%#x) reports a continue as a stop", uint32(ws))
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGSTOP, syscall.SIGTSTP} {
		if continuedStatus(syscall.WaitStatus(int(sig)<<8 | 0x7f)) {
			t.Errorf("continuedStatus(stop by %v) = true", sig)
		}
	}
}
