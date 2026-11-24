package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/units"
)

const testAlertHelp = `
Usage: pace test-alert [--mem 256M] [--hold 45s] [--on <machine>]

Checks that alerts reach you, end to end. It runs one small job under
'pace run' with a 64M memory budget, and the job allocates --mem (default
256M) and holds it for --hold (default 45s). Being over its budget, the job
gets a real governor warning within a few seconds:
  - a 'pace: WARN policy=mem_budget ...' line here,
  - the event in 'pace watch' and 'pace status' (warnings),
  - PaceBar: the alert in the menu bar and a notification,
  - on every machine whose fleet includes this one: the same, within ~10s.
In observe and warn-only mode the job is not killed (the line says
mode=observe). In enforce mode it is killed after the warning period (exit
201), which tests the whole ladder. Only this job's own memory is used.

--on <machine> runs the test on a fleet peer over ssh instead, so an alert
there can be checked on this machine's PaceBar.
`

// maxTestAlloc caps the helper: the test must never strain a machine.
const maxTestAlloc = 1 << 30

func (c *cli) cmdTestAlert(args []string) int {
	fs := c.newFlags("test-alert", testAlertHelp)
	memf := sizeFlag{v: 256 << 20}
	fs.Var(&memf, "mem", "memory the test job allocates (`SIZE`, at most 1G)")
	hold := fs.Duration("hold", 45*time.Second, "how long the job holds it")
	on := fs.String("on", "", "run the test on this fleet `machine` over ssh")
	if code, done := fs.parse(args); done {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(c.stderr, "pace test-alert: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	mem := memf.v
	if mem < 128<<20 || mem > maxTestAlloc {
		fmt.Fprintln(c.stderr, "pace test-alert: --mem must be between 128M and 1G")
		return exitUsage
	}
	if *hold < 10*time.Second || *hold > 10*time.Minute {
		fmt.Fprintln(c.stderr, "pace test-alert: --hold must be between 10s and 10m")
		return exitUsage
	}
	if *on != "" {
		return c.testAlertOn(*on, mem, *hold)
	}
	exe, err := c.executable()
	if err != nil {
		fmt.Fprintf(c.stderr, "pace test-alert: %v\n", err)
		return exitFail
	}
	fmt.Fprintf(c.stderr, "pace test-alert: a test job will use %s against a 64M budget for %s; expect a WARN line here within a few seconds, and an alert in PaceBar and a notification.\n",
		units.FormatBytes(mem), hold.Round(time.Second))
	code := c.cmdRun([]string{"--class", "test", "--mem", "64M", "--budget", "64M", "--",
		exe, "test-alert-alloc", "--mem", fmt.Sprint(mem), "--hold", hold.String()})
	fmt.Fprintf(c.stderr, "pace test-alert: done (exit %d). Did PaceBar show the alert and a notification? 'pace watch' shows the event feed.\n", code)
	return code
}

func (c *cli) testAlertOn(name string, mem int64, hold time.Duration) int {
	f, ok := c.loadFleet()
	if !ok {
		return exitUsage
	}
	i := f.Find(name)
	if i < 0 {
		fmt.Fprintf(c.stderr, "pace test-alert: no peer named %q ('pace fleet ls')\n", name)
		return exitUsage
	}
	p := f.Peers[i]
	ssh := c.env("PACE_FLEET_SSH")
	if ssh == "" {
		ssh = "ssh"
	}
	args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=8"}
	args = append(args, p.SSHOptions...)
	args = append(args, "--", p.SSH, p.Command("test-alert", "--mem", fmt.Sprint(mem), "--hold", hold.String()))
	cmd := exec.Command(ssh, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, c.stdout, c.stderr
	fmt.Fprintf(c.stderr, "pace test-alert: running on %s (%s); its alert should reach this machine's PaceBar within ~10s\n", p.Name, p.SSH)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintf(c.stderr, "pace test-alert: %v\n", err)
		return exitFail
	}
	return exitOK
}

// cmdTestAlertAlloc is the test job: it allocates and touches mem bytes in
// 16M steps with incompressible data, holds them, then exits 0.
func (c *cli) cmdTestAlertAlloc(args []string) int {
	fs := c.newFlags("test-alert-alloc", "\nUsage: pace test-alert-alloc --mem BYTES --hold DUR\n\nInternal: the job 'pace test-alert' runs under 'pace run'.\n")
	mem := fs.Int64("mem", 256<<20, "bytes")
	hold := fs.Duration("hold", 45*time.Second, "")
	if code, done := fs.parse(args); done {
		return code
	}
	if *mem <= 0 || *mem > maxTestAlloc || *hold <= 0 || *hold > 10*time.Minute {
		fmt.Fprintln(c.stderr, "test-alert-alloc: out of range")
		return exitUsage
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	const step = 16 << 20
	var keep [][]byte
	x := uint64(0x9E3779B97F4A7C15)
	for got := int64(0); got < *mem; got += step {
		n := int64(step)
		if *mem-got < n {
			n = *mem - got
		}
		b := make([]byte, n)
		for i := 0; i+8 <= len(b); i += 8 { // xorshift: the memory compressor cannot shrink it
			x ^= x << 13
			x ^= x >> 7
			x ^= x << 17
			b[i], b[i+1], b[i+2], b[i+3], b[i+4], b[i+5], b[i+6], b[i+7] = byte(x), byte(x>>8), byte(x>>16), byte(x>>24), byte(x>>32), byte(x>>40), byte(x>>48), byte(x>>56)
		}
		keep = append(keep, b)
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Fprintf(c.stdout, "test job: holding %s for %s\n", units.FormatBytes(*mem), hold.Round(time.Second))
	select {
	case <-time.After(*hold):
	case <-sig:
	}
	if len(keep) == 0 {
		return exitFail
	}
	return exitOK
}
