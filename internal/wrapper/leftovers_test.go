package wrapper

import (
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fireharp/pace/internal/platform"
	"github.com/fireharp/pace/internal/proto"
)

// Finding 14 (d): the daemon dies while it has the job frozen. Nobody is
// left to thaw it, so pace run continues it when its stream is lost (also
// when the FROZEN line never arrived), and the job finishes ungoverned
// instead of staying stopped forever. The daemon dies right after its
// freeze: pace run must not depend on having handled wait4's report of the
// stop before the stream error (with that report held back past the loss,
// the job used to be left stopped).
func TestLostDaemonThawsFrozenJob(t *testing.T) {
	for _, c := range []struct {
		name        string
		withLine    bool
		reportDelay time.Duration // hold wait4's report of the SIGSTOP back this long
	}{
		{"FROZEN relayed", true, 0},
		{"SIGSTOP without FROZEN", false, 0},
		{"stop reported after the stream loss", false, 300 * time.Millisecond},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			f := newFake(t, e.sock)
			frozen := make(chan struct{})
			var pgid atomic.Int64
			f.onSubmit = func(s *jobStream) {
				s.send(proto.Event{Verb: proto.VerbAccepted})
				s.send(proto.Event{Verb: proto.VerbAdmitted})
				st := s.waitStarted(t)
				if st.PGID <= 1 {
					return
				}
				pgid.Store(int64(st.PGID))
				if !freezeSteadyJob(t, st.PGID) {
					return
				}
				if c.withLine {
					s.send(proto.Event{Verb: proto.VerbFrozen, Policy: proto.PolicyPressure})
				}
				close(frozen)
				s.hold()
			}
			f.start()
			o := Options{Argv: sh("sleep 0.5; echo done")}
			if c.reportDelay > 0 {
				o.h.waitReport = func(ws syscall.WaitStatus) {
					if sig, ok := stopSignal(ws); ok && sig == syscall.SIGSTOP {
						time.Sleep(c.reportDelay)
					}
				}
			}
			done := make(chan result, 1)
			go func() { done <- e.run(o) }()
			select {
			case <-frozen:
			case <-time.After(10 * time.Second):
				t.Fatal("job never frozen")
			}
			f.stop() // the daemon dies right after its freeze
			var res result
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				if pg := int(pgid.Load()); pg > 1 {
					syscall.Kill(-pg, syscall.SIGKILL) // our own test job
				}
				t.Fatal("job left stopped after the daemon was lost")
			}
			if res.code != 0 || res.stdout != "done\n" {
				t.Fatalf("code %d stdout %q stderr %q", res.code, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, "DEGRADED reason=daemon_lost") || !strings.Contains(res.stderr, "lost daemon had frozen") {
				t.Fatalf("stderr %q", res.stderr)
			}
		})
	}
}

// freezeSteadyJob SIGSTOPs the group of a `sh -c "sleep ...; ..."` job, as
// the daemon's freeze does, once the shell's sleep has exec'd, and waits
// until the kernel has the shell stopped. Freezing earlier would not test
// pace run: a SIGSTOP that lands while a process is in execve is reported by
// wait4, but the new image runs on (measured on macOS: the /bin/sh shim
// exec'ing bash, and bash exec'ing sleep), so the job would not be frozen
// at all and there would be nothing to continue. Past the fork the shell
// only waits, so it stays stopped (a sleep that escapes in its own execve
// leaves the shell, and so the job, frozen).
func freezeSteadyJob(t *testing.T, pgid int) bool {
	deadline := time.Now().Add(10 * time.Second)
	for {
		// The sleep has exec'd: the shell waits for it in wait(), where it
		// stops. Before that, the child may be mid-exec while its parent sits
		// in vfork-wait (dash uses vfork), which a SIGSTOP never stops.
		if groupHasComm(pgid, "sleep") {
			break
		}
		if time.Now().After(deadline) {
			t.Errorf("job group %d: the shell never started its sleep", pgid)
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
	syscall.Kill(-pgid, syscall.SIGSTOP) // the daemon's freeze (of our own test job)
	for !procStopped(pgid) {
		if time.Now().After(deadline) {
			t.Errorf("job group %d: the shell never stopped", pgid)
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
	return true
}

// A lost daemon's freeze is on the whole process group: a leader that runs
// again (continued alone, or one that escaped the SIGSTOP in execve) does
// not mean the job was continued. Members still stopped are continued, and
// the INFO line says so (they used to be left stopped, with no line).
func TestLostDaemonThawsGroupWhoseLeaderRuns(t *testing.T) {
	for _, withLine := range []bool{true, false} {
		// Both processes (and the group signalled) are sleeps of our own.
		lead := exec.Command("/bin/sleep", "5")
		lead.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := lead.Start(); err != nil {
			t.Fatal(err)
		}
		pg := lead.Process.Pid
		member := exec.Command("/bin/sleep", "5")
		member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pg}
		if err := member.Start(); err != nil {
			_ = lead.Process.Kill()
			_ = lead.Wait()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-pg, syscall.SIGKILL)
			_ = syscall.Kill(-pg, syscall.SIGCONT)
			_ = lead.Wait()
			_ = member.Wait()
		})
		syscall.Kill(-pg, syscall.SIGSTOP) // the daemon's freeze
		waitProc(t, member.Process.Pid, true)
		waitProc(t, pg, true)
		syscall.Kill(pg, syscall.SIGCONT) // the leader alone runs again
		waitProc(t, pg, false)

		r := &runner{o: Options{Quiet: true}, id: "j1", frozen: withLine}
		ch := &child{pid: pg, pgid: pg, stopped: true, stopSig: syscall.SIGSTOP}
		r.thawLost(ch)
		stuck := false
		for end := time.Now().Add(2 * time.Second); procStopped(member.Process.Pid); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(end) {
				stuck = true
				break
			}
		}
		info := false
		for _, ev := range r.rec.Events {
			info = info || (ev.Verb == proto.VerbInfo && strings.Contains(ev.Msg, "lost daemon had frozen"))
		}
		if stuck || !info {
			t.Fatalf("FROZEN line %v: member left stopped %v, INFO %v (events %+v)", withLine, stuck, info, r.rec.Events)
		}
		if r.frozen || ch.stopped {
			t.Fatalf("FROZEN line %v: frozen %v, stopped %v after the thaw", withLine, r.frozen, ch.stopped)
		}
	}
}

// waitProc waits until the kernel has pid stopped (or running).
func waitProc(t *testing.T, pid int, stopped bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); procStopped(pid) != stopped; time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("pid %d: stopped never became %v", pid, stopped)
		}
	}
}

// A job the user stopped with job control is not the daemon's to thaw: a
// lost stream leaves a terminal stop alone.
func TestLostDaemonLeavesJobControlStop(t *testing.T) {
	// The group signalled (if the check failed) is a sleep of our own.
	c := exec.Command("/bin/sleep", "5")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	r := &runner{}
	ch := &child{pid: c.Process.Pid, pgid: c.Process.Pid, stopped: true, stopSig: syscall.SIGTSTP}
	r.thawLost(ch)
	if !ch.stopped {
		t.Fatal("terminal stop continued on daemon loss")
	}
}

// Merge of round 2: a job frozen with cgroup.freeze is not woken by SIGCONT,
// so a lost daemon's freeze is undone on the container itself.
func TestLostDaemonThawsFrozenContainer(t *testing.T) {
	c := exec.Command("/bin/sleep", "5") // our own test process
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	fc := &platform.FakeContainers{Base: "/cg/pace", CG: map[string]*platform.FakeCgroup{"j1": {Frozen: true}}}
	r := &runner{o: Options{Quiet: true}, id: "j1", frozen: true, cg: &jobCgroup{c: fc, id: "j1", fd: -1}}
	ch := &child{pid: c.Process.Pid, pgid: c.Process.Pid}
	r.thawLost(ch)
	if g, _ := fc.Get("j1"); g.Frozen {
		t.Fatal("job container left frozen after the daemon was lost")
	}
	if r.frozen {
		t.Fatal("runner still frozen")
	}
}

// A FROZEN line whose freeze is over (continued without THAWED, and wait4
// never reported it: a leader in vfork-wait does not stop) is not claimed
// as continued on daemon loss: the kernel shows nothing of the group
// stopped.
func TestLostDaemonFrozenLineButGroupRunning(t *testing.T) {
	c := exec.Command("/bin/sleep", "5") // our own test process, running
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	r := &runner{o: Options{Quiet: true}, id: "j1", frozen: true}
	ch := &child{pid: c.Process.Pid, pgid: c.Process.Pid}
	r.thawLost(ch)
	for _, ev := range r.rec.Events {
		if strings.Contains(ev.Msg, "lost daemon had frozen") {
			t.Fatal("claims to continue a job that was running")
		}
	}
	if r.frozen {
		t.Fatal("runner still frozen")
	}
}
