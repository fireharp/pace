// Command pacetest is the deterministic test tool for pace: it allocates a set
// amount of (really touched) memory, burns CPU, hangs, prints on a schedule,
// spawns children that escape the process group, ignores SIGTERM, spikes
// late like a linker, or exits with a given code or signal.
//
// Safety: any single allocation request above 8G is refused unless
// --i-know is given, and every process exits by itself after --max-life
// (default 1h) so a forgotten test never pins memory for good.
//
// Progress goes to stdout as "pacetest: <event> key=value..." lines.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fireharp/pace/internal/units"
)

// MaxAlloc is the largest allocation accepted without --i-know.
const MaxAlloc = 8 * units.GiB

// Exit codes of pacetest itself.
const (
	exitUsage   = 2
	exitMaxLife = 124 // like timeout(1)
)

// escapedArg is the hidden subcommand the escape parent re-execs itself with.
const escapedArg = "__escaped"

type env struct {
	stdout, stderr io.Writer
	self           string // path of this binary
}

func main() {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	e := &env{stdout: os.Stdout, stderr: os.Stderr, self: self}
	os.Exit(e.main(os.Args[1:]))
}

const usage = `pacetest: test tool for pace (deterministic, no dependencies)

Usage: pacetest <command> [flags]

Commands:
  alloc --mem SIZE [--step SIZE --every DUR] [--hold DUR] [--print-every DUR] [--fill random|page] [--ignore-term]
        allocate and touch SIZE bytes (in steps), then hold them; --ignore-term:
        only SIGKILL stops it (a shell's trap '' TERM does not reach a Go program)
  burn [--cpus N] [--for DUR]               spin N CPUs
  hang [--for DUR]                          sleep: no CPU, no output
  print [--every DUR] [--for DUR]           print a tick line every DUR
  escape --mem SIZE [--hold DUR] [--linger DUR] [--clear-env] [--keep-stdio]
        fork a child that setsid()s and allocates; parent exits at once (or after --linger)
  tree --n N --mem SIZE [--hold DUR]        N children in this process group, each allocating SIZE
  exit CODE | exit --signal SIG             exit with CODE, or die of signal SIG
  ignore-term [--for DUR]                   ignore SIGTERM (only SIGKILL stops it)
  spike --base SIZE --peak SIZE --at DUR [--hold DUR]
        hold SIZE base, then jump to peak at DUR (like a late link step)
  storm --depth D --fanout F --mem SIZE [--alloc-after DUR] [--setsid-leaves] [--hold DUR]
        a process tree: each node starts F children down to depth D, waits
        --alloc-after (so the whole tree exists first), then allocates SIZE
  probe [--every DUR] [--for DUR] [--report DUR]
        measure how late a DUR timer fires (a stall oracle); prints the worst
        lateness every --report and at the end

Every command accepts --max-life DUR (default 1h, 0 = none): exit 124 after
DUR whatever happens. Allocations above 8G need --i-know. Sizes: 512M, 2G.
Durations: 250ms, 40s, 2m; "forever" (or "inf") where a duration is optional.
`

func (e *env) main(args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, usage)
		return 0
	case "alloc":
		return e.cmdAlloc(rest)
	case "burn":
		return e.cmdBurn(rest)
	case "hang":
		return e.cmdHang(rest)
	case "print":
		return e.cmdPrint(rest)
	case "escape":
		return e.cmdEscape(rest)
	case escapedArg:
		return e.cmdEscaped(rest)
	case "tree":
		return e.cmdTree(rest)
	case "exit":
		return e.cmdExit(rest)
	case "ignore-term":
		return e.cmdIgnoreTerm(rest)
	case "spike":
		return e.cmdSpike(rest)
	case "storm":
		return e.cmdStorm(rest)
	case "probe":
		return e.cmdProbe(rest)
	}
	fmt.Fprintf(e.stderr, "pacetest: unknown command %q\n", cmd)
	return exitUsage
}

// ---- flag helpers ----

// sizeVal is a byte size flag.
type sizeVal struct {
	v   int64
	set bool
}

func (s *sizeVal) String() string {
	if s == nil || !s.set {
		return ""
	}
	return units.FormatBytes(s.v)
}

func (s *sizeVal) Set(x string) error {
	n, err := units.ParseBytes(x)
	if err != nil {
		return err
	}
	s.v, s.set = n, true
	return nil
}

// durVal is a duration flag; forever=true means no limit.
type durVal struct {
	d       time.Duration
	forever bool
}

func (d *durVal) String() string {
	if d == nil {
		return ""
	}
	if d.forever {
		return "forever"
	}
	return d.d.String()
}

func (d *durVal) Set(x string) error {
	switch strings.ToLower(strings.TrimSpace(x)) {
	case "forever", "inf", "infinite", "-1":
		d.forever = true
		return nil
	}
	v, err := units.ParseDuration(x)
	if err != nil {
		return err
	}
	d.d, d.forever = v, false
	return nil
}

// common flags every command has.
type common struct {
	maxLife durVal
	iKnow   bool
}

func newFlags(name string, c *common) *flag.FlagSet {
	fs := flag.NewFlagSet("pacetest "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.maxLife = durVal{d: time.Hour}
	fs.Var(&c.maxLife, "max-life", "exit 124 after this long (0 = never)")
	fs.BoolVar(&c.iKnow, "i-know", false, "allow allocations above 8G")
	return fs
}

func (e *env) parse(fs *flag.FlagSet, args []string) (int, bool) {
	err := fs.Parse(args)
	if err == nil {
		if fs.NArg() > 0 {
			fmt.Fprintf(e.stderr, "%s: unexpected argument %q\n", fs.Name(), fs.Arg(0))
			return exitUsage, true
		}
		return 0, false
	}
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintf(e.stdout, "Usage of %s:\n", fs.Name())
		fs.SetOutput(e.stdout)
		fs.PrintDefaults()
		return 0, true
	}
	fmt.Fprintf(e.stderr, "%s: %v\n", fs.Name(), err)
	return exitUsage, true
}

func (e *env) checkCap(c *common, what string, n int64) bool {
	if n > MaxAlloc && !c.iKnow {
		fmt.Fprintf(e.stderr, "pacetest: %s %s exceeds the %s safety cap; pass --i-know to allow it\n", what, units.FormatBytes(n), units.FormatBytes(MaxAlloc))
		return false
	}
	return true
}

// startMaxLife arms the self-destruct timer.
func (e *env) startMaxLife(c *common) {
	if c.maxLife.forever || c.maxLife.d <= 0 {
		return
	}
	time.AfterFunc(c.maxLife.d, func() {
		fmt.Fprintf(e.stderr, "pacetest: max-life %v reached, exiting\n", c.maxLife.d)
		os.Exit(exitMaxLife)
	})
}

// wait sleeps for d (forever if d.forever), returning early on stop.
func wait(d durVal, stop <-chan struct{}) {
	if d.forever {
		<-stop
		return
	}
	if d.d <= 0 {
		return
	}
	t := time.NewTimer(d.d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-stop:
	}
}

var never = make(chan struct{})

func (e *env) say(format string, a ...any) {
	fmt.Fprintf(e.stdout, "pacetest: "+format+"\n", a...)
}

// ---- memory ----

const chunk = 16 * units.MiB

// arena holds allocated memory so the GC cannot free it.
type arena struct {
	mu     sync.Mutex
	chunks [][]byte
	total  int64
	seed   uint64
	fill   string
}

func newArena(fill string) *arena { return &arena{seed: 0x9E3779B97F4A7C15, fill: fill} }

func (a *arena) allocated() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.total
}

// grow allocates n more bytes and touches every page: "random" writes
// incompressible data to every byte (realistic for the macOS compressor),
// "page" writes one byte per 4 KiB page (faster, compresses well).
func (a *arena) grow(n int64) {
	for n > 0 {
		sz := chunk
		if n < sz {
			sz = n
		}
		b := make([]byte, sz)
		switch a.fill {
		case "page":
			for i := int64(0); i < sz; i += 4096 {
				b[i] = 1
			}
		default:
			x := a.seed
			i := int64(0)
			for ; i+8 <= sz; i += 8 {
				x ^= x << 13
				x ^= x >> 7
				x ^= x << 17
				b[i], b[i+1], b[i+2], b[i+3] = byte(x), byte(x>>8), byte(x>>16), byte(x>>24)
				b[i+4], b[i+5], b[i+6], b[i+7] = byte(x>>32), byte(x>>40), byte(x>>48), byte(x>>56)
			}
			for ; i < sz; i++ {
				b[i] = byte(x)
			}
			a.seed = x
		}
		a.mu.Lock()
		a.chunks = append(a.chunks, b)
		a.total += sz
		a.mu.Unlock()
		n -= sz
	}
}

func validFill(f string) bool { return f == "random" || f == "page" }

// ---- alloc ----

type allocOpts struct {
	common
	mem, step  sizeVal
	every      durVal
	hold       durVal
	printEvery durVal
	fill       string
	ignoreTerm bool
}

func (e *env) parseAlloc(args []string) (allocOpts, int, bool) {
	var o allocOpts
	fs := newFlags("alloc", &o.common)
	o.hold = durVal{forever: true}
	o.printEvery = durVal{d: time.Second}
	fs.Var(&o.mem, "mem", "total memory to allocate and touch (required)")
	fs.Var(&o.step, "step", "allocate in steps of this size (default: all at once)")
	fs.Var(&o.every, "every", "pause between steps")
	fs.Var(&o.hold, "hold", "hold the memory this long after allocating (default forever; 0 = exit at once)")
	fs.Var(&o.printEvery, "print-every", "print a progress line this often (0 = only start/done)")
	fs.StringVar(&o.fill, "fill", "random", "how pages are touched: random (incompressible) | page (1 byte/page)")
	fs.BoolVar(&o.ignoreTerm, "ignore-term", false, "ignore SIGTERM (only SIGKILL stops it)")
	if code, done := e.parse(fs, args); done {
		return o, code, true
	}
	switch {
	case !o.mem.set:
		fmt.Fprintln(e.stderr, "pacetest alloc: --mem is required")
		return o, exitUsage, true
	case o.step.set && o.step.v <= 0:
		fmt.Fprintln(e.stderr, "pacetest alloc: --step must be > 0")
		return o, exitUsage, true
	case !validFill(o.fill):
		fmt.Fprintf(e.stderr, "pacetest alloc: --fill must be random or page, got %q\n", o.fill)
		return o, exitUsage, true
	case !e.checkCap(&o.common, "--mem", o.mem.v):
		return o, exitUsage, true
	}
	if !o.step.set || o.step.v > o.mem.v {
		o.step = sizeVal{v: o.mem.v, set: true}
	}
	return o, 0, false
}

func (e *env) cmdAlloc(args []string) int {
	o, code, done := e.parseAlloc(args)
	if done {
		return code
	}
	e.startMaxLife(&o.common)
	if o.ignoreTerm {
		signal.Ignore(syscall.SIGTERM)
		e.say("alloc ignoring SIGTERM pid=%d", os.Getpid())
	}
	a := newArena(o.fill)
	e.allocate(a, o.mem.v, o.step.v, o.every, o.printEvery, "alloc")
	e.holdArena(a, o.hold, o.printEvery)
	return 0
}

// allocate grows a to target in steps, printing progress.
func (e *env) allocate(a *arena, target, step int64, every, printEvery durVal, label string) {
	start := time.Now()
	e.say("%s start pid=%d target=%s step=%s", label, os.Getpid(), units.FormatBytes(target), units.FormatBytes(step))
	lastPrint := start
	for a.allocated() < target {
		n := step
		if rem := target - a.allocated(); n > rem {
			n = rem
		}
		// grow in chunks so progress can be printed during a big step
		for n > 0 {
			c := chunk * 4
			if c > n {
				c = n
			}
			a.grow(c)
			n -= c
			if !printEvery.forever && printEvery.d > 0 && time.Since(lastPrint) >= printEvery.d {
				e.say("%s progress allocated=%s target=%s", label, units.FormatBytes(a.allocated()), units.FormatBytes(target))
				lastPrint = time.Now()
			}
		}
		if a.allocated() < target {
			wait(every, never)
		}
	}
	e.say("%s done allocated=%s elapsed=%s", label, units.FormatBytes(a.allocated()), units.FormatMS(time.Since(start).Milliseconds()))
}

func (e *env) holdArena(a *arena, hold, printEvery durVal) {
	if !hold.forever && hold.d <= 0 {
		runtime.KeepAlive(a)
		return
	}
	stop := make(chan struct{})
	if !printEvery.forever && printEvery.d > 0 {
		t := time.NewTicker(printEvery.d)
		defer t.Stop()
		go func() {
			for {
				select {
				case <-t.C:
					e.say("hold allocated=%s", units.FormatBytes(a.allocated()))
				case <-stop:
					return
				}
			}
		}()
	}
	wait(hold, never)
	close(stop)
	runtime.KeepAlive(a)
}

// ---- burn / hang / print ----

func (e *env) cmdBurn(args []string) int {
	var c common
	fs := newFlags("burn", &c)
	cpus := fs.Int("cpus", 1, "number of CPUs to spin")
	dur := durVal{forever: true}
	fs.Var(&dur, "for", "how long (default forever)")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if *cpus < 1 || *cpus > 4*runtime.NumCPU() {
		fmt.Fprintf(e.stderr, "pacetest burn: --cpus must be 1..%d\n", 4*runtime.NumCPU())
		return exitUsage
	}
	e.startMaxLife(&c)
	runtime.GOMAXPROCS(*cpus + 1)
	e.say("burn start pid=%d cpus=%d", os.Getpid(), *cpus)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *cpus; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			x := uint64(i + 1)
			for n := 0; ; n++ {
				x = x*6364136223846793005 + 1442695040888963407
				if n&0xffff == 0 {
					select {
					case <-stop:
						sink.Store(x)
						return
					default:
					}
				}
			}
		}()
	}
	wait(dur, never)
	close(stop)
	wg.Wait()
	e.say("burn done")
	return 0
}

// sink keeps the spin loops' results observable so they are not optimised away.
var sink atomic.Uint64

func (e *env) cmdHang(args []string) int {
	var c common
	fs := newFlags("hang", &c)
	dur := durVal{forever: true}
	fs.Var(&dur, "for", "how long (default forever)")
	if code, done := e.parse(fs, args); done {
		return code
	}
	e.startMaxLife(&c)
	wait(dur, never)
	return 0
}

func (e *env) cmdPrint(args []string) int {
	var c common
	fs := newFlags("print", &c)
	every := durVal{d: time.Second}
	dur := durVal{forever: true}
	fs.Var(&every, "every", "interval between lines (default 1s)")
	fs.Var(&dur, "for", "how long (default forever)")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if every.forever || every.d <= 0 {
		fmt.Fprintln(e.stderr, "pacetest print: --every must be > 0")
		return exitUsage
	}
	e.startMaxLife(&c)
	t := time.NewTicker(every.d)
	defer t.Stop()
	var end <-chan time.Time
	if !dur.forever {
		end = time.After(dur.d)
	}
	for n := 1; ; n++ {
		select {
		case <-t.C:
			e.say("tick n=%d", n)
		case <-end:
			return 0
		}
	}
}

// ---- escape ----

func (e *env) cmdEscape(args []string) int {
	var c common
	fs := newFlags("escape", &c)
	var mem sizeVal
	hold := durVal{forever: true}
	fs.Var(&mem, "mem", "memory the escaped child allocates (required)")
	fs.Var(&hold, "hold", "how long the child holds it (default forever, bounded by --max-life)")
	keepStdio := fs.Bool("keep-stdio", false, "child keeps this process' stdout/stderr (default: /dev/null, like a daemon)")
	clearEnv := fs.Bool("clear-env", false, "remove PACE_JOB_ID from the child's environment")
	var linger durVal
	fs.Var(&linger, "linger", "parent waits this long before exiting (a launcher that outlives its daemon's start)")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if !mem.set {
		fmt.Fprintln(e.stderr, "pacetest escape: --mem is required")
		return exitUsage
	}
	if !e.checkCap(&c, "--mem", mem.v) {
		return exitUsage
	}
	childArgs := []string{escapedArg, "--mem", strconv.FormatInt(mem.v, 10), "--hold", hold.String(), "--max-life", c.maxLife.String()}
	if c.iKnow {
		childArgs = append(childArgs, "--i-know")
	}
	cmd := exec.Command(e.self, childArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if *keepStdio {
		cmd.Stdout, cmd.Stderr = e.stdout, e.stderr
	}
	if *clearEnv {
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "PACE_JOB_ID=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(e.stderr, "pacetest escape: %v\n", err)
		return 1
	}
	e.say("escape child=%d mem=%s", cmd.Process.Pid, units.FormatBytes(mem.v))
	if linger.forever || linger.d > 0 {
		e.startMaxLife(&c)
		wait(linger, never)
	}
	_ = cmd.Process.Release() // do not wait: the child is reparented to init
	return 0
}

func (e *env) cmdEscaped(args []string) int {
	var c common
	fs := newFlags(escapedArg, &c)
	var mem sizeVal
	hold := durVal{forever: true}
	fs.Var(&mem, "mem", "")
	fs.Var(&hold, "hold", "")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if !e.checkCap(&c, "--mem", mem.v) {
		return exitUsage
	}
	e.startMaxLife(&c)
	a := newArena("random")
	e.allocate(a, mem.v, mem.v, durVal{}, durVal{}, "escaped")
	e.holdArena(a, hold, durVal{})
	return 0
}

// ---- tree ----

func (e *env) cmdTree(args []string) int {
	var c common
	fs := newFlags("tree", &c)
	n := fs.Int("n", 2, "number of children")
	var mem sizeVal
	hold := durVal{forever: true}
	fs.Var(&mem, "mem", "memory each child allocates (required)")
	fs.Var(&hold, "hold", "how long children hold it (default forever)")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if !mem.set {
		fmt.Fprintln(e.stderr, "pacetest tree: --mem is required")
		return exitUsage
	}
	if *n < 1 || *n > 256 {
		fmt.Fprintln(e.stderr, "pacetest tree: --n must be 1..256")
		return exitUsage
	}
	if !e.checkCap(&c, "total (--n x --mem)", int64(*n)*mem.v) {
		return exitUsage
	}
	e.startMaxLife(&c)
	childArgs := []string{"alloc", "--mem", strconv.FormatInt(mem.v, 10), "--hold", hold.String(), "--print-every", "0", "--max-life", c.maxLife.String()}
	if c.iKnow {
		childArgs = append(childArgs, "--i-know")
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	var cmds []*exec.Cmd
	for i := 0; i < *n; i++ {
		cmd := exec.Command(e.self, childArgs...)
		cmd.Stdout, cmd.Stderr = e.stdout, e.stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(e.stderr, "pacetest tree: %v\n", err)
			for _, c := range cmds {
				c.Process.Kill()
				c.Wait()
			}
			return 1
		}
		cmds = append(cmds, cmd)
		e.say("tree child=%d i=%d", cmd.Process.Pid, i)
	}
	done := make(chan int, 1)
	go func() {
		code := 0
		for _, cmd := range cmds {
			if err := cmd.Wait(); err != nil && code == 0 {
				code = 1
				var ee *exec.ExitError
				if errors.As(err, &ee) && ee.ExitCode() > 0 {
					code = ee.ExitCode()
				}
			}
		}
		done <- code
	}()
	var received os.Signal
	select {
	case code := <-done:
		// All children may exit before the parent reads a group signal.
		// Stop delivery, then check for a signal already queued for us.
		signal.Stop(sigs)
		select {
		case received = <-sigs:
		default:
			return code
		}
	case received = <-sigs:
		for _, cmd := range cmds {
			cmd.Process.Signal(received)
		}
		<-done
	}
	signal.Reset(received)
	syscall.Kill(os.Getpid(), received.(syscall.Signal))
	time.Sleep(time.Second)
	return 1
}

// ---- exit / ignore-term ----

var signalNames = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL,
	"TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "SEGV": syscall.SIGSEGV,
	"ABRT": syscall.SIGABRT, "PIPE": syscall.SIGPIPE,
}

func parseSignal(s string) (syscall.Signal, error) {
	u := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "SIG")
	if sig, ok := signalNames[u]; ok {
		return sig, nil
	}
	if n, err := strconv.Atoi(u); err == nil && n > 0 && n < 32 {
		return syscall.Signal(n), nil
	}
	return 0, fmt.Errorf("unknown signal %q", s)
}

func (e *env) cmdExit(args []string) int {
	var c common
	fs := newFlags("exit", &c)
	sigName := fs.String("signal", "", "die of this signal instead (TERM, KILL, 9, ...)")
	// Accept the code before or after flags: `exit 3`, `exit --signal TERM`.
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				fmt.Fprintln(e.stdout, "Usage: pacetest exit CODE | pacetest exit --signal SIG")
				return 0
			}
			fmt.Fprintf(e.stderr, "pacetest exit: %v\n", err)
			return exitUsage
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if *sigName != "" {
		if len(pos) > 0 {
			fmt.Fprintln(e.stderr, "pacetest exit: give a CODE or --signal, not both")
			return exitUsage
		}
		sig, err := parseSignal(*sigName)
		if err != nil {
			fmt.Fprintf(e.stderr, "pacetest exit: %v\n", err)
			return exitUsage
		}
		signal.Reset(sig)
		syscall.Kill(os.Getpid(), sig)
		time.Sleep(2 * time.Second) // the signal is delivered asynchronously
		fmt.Fprintf(e.stderr, "pacetest exit: signal %v did not terminate the process\n", sig)
		return 1
	}
	if len(pos) != 1 {
		fmt.Fprintln(e.stderr, "pacetest exit: want exactly one CODE (0..255)")
		return exitUsage
	}
	code, err := strconv.Atoi(pos[0])
	if err != nil || code < 0 || code > 255 {
		fmt.Fprintf(e.stderr, "pacetest exit: CODE must be 0..255, got %q\n", pos[0])
		return exitUsage
	}
	return code
}

func (e *env) cmdIgnoreTerm(args []string) int {
	var c common
	fs := newFlags("ignore-term", &c)
	dur := durVal{forever: true}
	fs.Var(&dur, "for", "how long (default forever)")
	if code, done := e.parse(fs, args); done {
		return code
	}
	e.startMaxLife(&c)
	signal.Ignore(syscall.SIGTERM)
	e.say("ignoring SIGTERM pid=%d", os.Getpid())
	wait(dur, never)
	return 0
}

// ---- spike ----

type spikeOpts struct {
	common
	base, peak sizeVal
	at         durVal
	hold       durVal
}

func (e *env) parseSpike(args []string) (spikeOpts, int, bool) {
	var o spikeOpts
	fs := newFlags("spike", &o.common)
	o.hold = durVal{forever: true}
	fs.Var(&o.base, "base", "memory held from the start (required)")
	fs.Var(&o.peak, "peak", "total memory after the spike (required, >= base)")
	fs.Var(&o.at, "at", "when the spike happens, from start (required)")
	fs.Var(&o.hold, "hold", "how long to hold the peak (default forever)")
	if code, done := e.parse(fs, args); done {
		return o, code, true
	}
	switch {
	case !o.base.set || !o.peak.set:
		fmt.Fprintln(e.stderr, "pacetest spike: --base and --peak are required")
		return o, exitUsage, true
	case o.peak.v < o.base.v:
		fmt.Fprintln(e.stderr, "pacetest spike: --peak must be >= --base")
		return o, exitUsage, true
	case o.at.forever:
		fmt.Fprintln(e.stderr, "pacetest spike: --at must be a duration")
		return o, exitUsage, true
	case !e.checkCap(&o.common, "--peak", o.peak.v):
		return o, exitUsage, true
	}
	return o, 0, false
}

func (e *env) cmdSpike(args []string) int {
	o, code, done := e.parseSpike(args)
	if done {
		return code
	}
	e.startMaxLife(&o.common)
	start := time.Now()
	a := newArena("random")
	e.allocate(a, o.base.v, o.base.v, durVal{}, durVal{}, "base")
	if rem := o.at.d - time.Since(start); rem > 0 {
		time.Sleep(rem)
	}
	e.allocate(a, o.peak.v, o.peak.v, durVal{}, durVal{}, "spike")
	e.holdArena(a, o.hold, durVal{})
	return 0
}

// ---- storm ----

// cmdStorm builds a process tree: every node starts --fanout children down to
// --depth (re-exec of this binary), waits --alloc-after so the whole tree
// exists before anything grows, allocates --mem, then holds it. Leaves can
// setsid() out of the process group (--setsid-leaves), as daemonizing tools do.
func (e *env) cmdStorm(args []string) int {
	var c common
	fs := newFlags("storm", &c)
	depth := fs.Int("depth", 2, "levels of children below this node")
	fanout := fs.Int("fanout", 2, "children per node")
	var mem sizeVal
	fs.Var(&mem, "mem", "memory each node allocates (required)")
	allocAfter := durVal{d: 2 * time.Second}
	fs.Var(&allocAfter, "alloc-after", "wait this long after starting children before allocating")
	hold := durVal{forever: true}
	fs.Var(&hold, "hold", "how long each node holds its memory (default forever)")
	setsidLeaves := fs.Bool("setsid-leaves", false, "leaves start their own session")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if !mem.set {
		fmt.Fprintln(e.stderr, "pacetest storm: --mem is required")
		return exitUsage
	}
	if *depth < 0 || *depth > 6 || *fanout < 1 || *fanout > 8 {
		fmt.Fprintln(e.stderr, "pacetest storm: --depth must be 0..6 and --fanout 1..8")
		return exitUsage
	}
	nodes := int64(1)
	for i, w := 0, int64(1); i < *depth; i++ {
		w *= int64(*fanout)
		nodes += w
	}
	if nodes > 512 {
		fmt.Fprintf(e.stderr, "pacetest storm: %d nodes is more than 512\n", nodes)
		return exitUsage
	}
	if !e.checkCap(&c, "total (nodes x --mem)", nodes*mem.v) {
		return exitUsage
	}
	e.startMaxLife(&c)
	e.say("storm node pid=%d pgid=%d depth=%d", os.Getpid(), syscall.Getpgrp(), *depth)
	var cmds []*exec.Cmd
	if *depth > 0 {
		for i := 0; i < *fanout; i++ {
			a := []string{"storm", "--depth", strconv.Itoa(*depth - 1), "--fanout", strconv.Itoa(*fanout),
				"--mem", strconv.FormatInt(mem.v, 10), "--alloc-after", allocAfter.String(), "--hold", hold.String(),
				"--max-life", c.maxLife.String()}
			if *setsidLeaves {
				a = append(a, "--setsid-leaves")
			}
			if c.iKnow {
				a = append(a, "--i-know")
			}
			cmd := exec.Command(e.self, a...)
			cmd.Stdout, cmd.Stderr = e.stdout, e.stderr
			if *setsidLeaves && *depth == 1 {
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			}
			if err := cmd.Start(); err != nil {
				fmt.Fprintf(e.stderr, "pacetest storm: %v\n", err)
				break
			}
			cmds = append(cmds, cmd)
		}
	}
	wait(allocAfter, never)
	a := newArena("random")
	if mem.v > 0 {
		e.allocate(a, mem.v, mem.v, durVal{}, durVal{}, "storm")
	}
	go func() {
		for _, cmd := range cmds {
			cmd.Wait()
		}
	}()
	e.holdArena(a, hold, durVal{})
	return 0
}

// ---- probe ----

// cmdProbe measures scheduling stalls: it sleeps --every in a loop and records
// how late each wakeup is. A machine that stalls (swap storm, OOM thrash,
// frozen cgroup) shows up as large lateness.
func (e *env) cmdProbe(args []string) int {
	var c common
	fs := newFlags("probe", &c)
	every := durVal{d: 100 * time.Millisecond}
	fs.Var(&every, "every", "timer interval")
	dur := durVal{forever: true}
	fs.Var(&dur, "for", "how long (default forever)")
	report := durVal{d: 10 * time.Second}
	fs.Var(&report, "report", "print the worst lateness this often")
	if code, done := e.parse(fs, args); done {
		return code
	}
	if every.forever || every.d <= 0 || report.forever || report.d <= 0 {
		fmt.Fprintln(e.stderr, "pacetest probe: --every and --report must be > 0")
		return exitUsage
	}
	e.startMaxLife(&c)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	var end <-chan time.Time
	if !dur.forever {
		end = time.After(dur.d)
	}
	start := time.Now()
	var worst, worstWin time.Duration
	n := 0
	lastReport := start
	e.say("probe start pid=%d every=%s", os.Getpid(), every.d)
	for {
		t0 := time.Now()
		select {
		case <-time.After(every.d):
		case <-end:
			e.say("probe done samples=%d worst_late_ms=%d", n, worst.Milliseconds())
			return 0
		case <-sigs:
			e.say("probe done samples=%d worst_late_ms=%d", n, worst.Milliseconds())
			return 0
		}
		late := time.Since(t0) - every.d
		n++
		if late > worst {
			worst = late
		}
		if late > worstWin {
			worstWin = late
		}
		if time.Since(lastReport) >= report.d {
			e.say("probe window_late_ms=%d worst_late_ms=%d samples=%d", worstWin.Milliseconds(), worst.Milliseconds(), n)
			worstWin, lastReport = 0, time.Now()
		}
	}
}
