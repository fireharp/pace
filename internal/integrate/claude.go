package integrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fireharp/pace/internal/policy"
	"github.com/fireharp/pace/internal/proto"
	"github.com/fireharp/pace/internal/units"
)

// HookInput is the subset of the Claude Code hook payload pace reads.
type HookInput struct {
	SessionID     string          `json:"session_id"`
	HookEventName string          `json:"hook_event_name"`
	Source        string          `json:"source,omitempty"` // SessionStart: startup|resume|clear|compact
	Cwd           string          `json:"cwd,omitempty"`
	ToolName      string          `json:"tool_name,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
}

// BashToolInput is tool_input for the Bash tool.
type BashToolInput struct {
	Command string `json:"command"`
}

// HookDeps are the hook handlers' inputs and side channels.
type HookDeps struct {
	Stdin  io.Reader
	Stdout io.Writer
	Env    Env
	// ShimDir is the PATH shim directory (SessionStart exports it).
	ShimDir string
	// Status queries the daemon; nil or an error means unreachable.
	Status func(ctx context.Context) (proto.Status, error)
	// Budget bounds the whole handler (0 = 150ms). Hooks must finish well
	// under Claude Code's patience and never block a tool call.
	Budget time.Duration
	// Quiet suppresses the SessionStart context line.
	Quiet bool
	// PaceBin is the absolute path of the pace binary, named in the context
	// a harness without PATH shims (Codex) is given. "" = "pace".
	PaceBin string
}

func (d HookDeps) paceCmd() string {
	if d.PaceBin == "" {
		return "pace"
	}
	return ShellQuote(d.PaceBin)
}

func (d HookDeps) budget() time.Duration {
	if d.Budget <= 0 {
		return 150 * time.Millisecond
	}
	return d.Budget
}

// readHookInput reads and decodes stdin, giving up at the deadline. A
// malformed or missing payload yields a zero HookInput, never an error that
// could block the tool call.
func readHookInput(r io.Reader, deadline time.Time) HookInput {
	var in HookInput
	if r == nil {
		return in
	}
	type res struct{ b []byte }
	ch := make(chan res, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, 4<<20))
		ch <- res{b}
	}()
	select {
	case got := <-ch:
		_ = json.Unmarshal(got.b, &in)
	case <-time.After(time.Until(deadline)):
	}
	return in
}

// envFileLine is the line SessionStart appends to $CLAUDE_ENV_FILE. It is
// idempotent when sourced repeatedly.
func envFileLine(shimDir string) string {
	q := ShellQuote(shimDir)
	return fmt.Sprintf(`case ":$PATH:" in *:%s:*) ;; *) export PATH=%s:"$PATH" ;; esac # pace shims`, q, q)
}

func hasShims(dir string) bool {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range ents {
		if e.Type().IsRegular() && IsShim(dir+"/"+e.Name()) {
			return true
		}
	}
	return false
}

// appendEnvFile adds line to path unless it is already there.
func appendEnvFile(path, line string) error {
	old, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, l := range strings.Split(string(old), "\n") {
		if l == line {
			return nil
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	prefix := ""
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d HookDeps) status(ctx context.Context) (proto.Status, bool) {
	if d.Status == nil {
		return proto.Status{}, false
	}
	type res struct {
		s   proto.Status
		err error
	}
	ch := make(chan res, 1)
	go func() {
		s, err := d.Status(ctx)
		ch <- res{s, err}
	}()
	select {
	case r := <-ch:
		return r.s, r.err == nil
	case <-ctx.Done():
		return proto.Status{}, false
	}
}

// summary renders the key=value status fields agents act on.
func summary(s proto.Status) string {
	parts := []string{
		"mode=" + proto.QuoteValue(orDash(s.Mode)),
		"pressure=" + proto.QuoteValue(orDash(s.Pressure)),
		"headroom=" + units.FormatBytes(s.HeadroomBytes),
		"suggested_concurrency=" + fmt.Sprint(s.SuggestedConcurrency),
		"admitting=" + fmt.Sprint(s.Admitting),
	}
	if len(s.NotAdmittingReasons) > 0 {
		parts = append(parts, "reasons="+proto.QuoteValue(strings.Join(s.NotAdmittingReasons, ",")))
	}
	parts = append(parts, fmt.Sprintf("running=%d queued=%d", len(s.Running), len(s.Queued)))
	return strings.Join(parts, " ")
}

// exitCodesText is the SessionStart sentence on pace run's exit codes. A
// policy kill is not always about memory: the KILLED line names the policy
// and its hint says what to do, so the sentence sends the agent there and
// gives each policy's remedy (the hints of internal/policy).
const exitCodesText = "Exit 200 = refused (retry after retry_after), " +
	"201 = killed by policy: the KILLED line's policy= says why and its hint what to do " +
	"(mem_budget: lower parallelism or ask for a larger --budget; wall_clock: split the work or raise --timeout; " +
	"stuck: check why the command hangs, e.g. waiting for input; pressure: retry when pace status shows headroom), " +
	"202 = pace usage error. "

// modeNote is the SessionStart sentence on what the daemon's mode does,
// for the modes that do less than enforce ("" for enforce or unknown).
func modeNote(mode string) string {
	switch mode {
	case policy.ModeObserve:
		return "Mode observe: pace only reports and warns; every command starts at once, and nothing is queued, refused, throttled, frozen or killed."
	case policy.ModeWarnOnly:
		return "Mode warn-only: pace queues and refuses new commands under pressure, but never throttles, freezes or kills a running one (it only logs that it would)."
	}
	return ""
}

// pressureHint is the PreToolUse hint line at warn or critical pressure. It
// says only what pace really does in mode: observe admits every job at once
// and enforces nothing, warn-only queues and refuses new jobs but never
// stops a running one, enforce (or an unknown mode) does both.
func pressureHint(mode, level string) string {
	crit := level == proto.LevelCritical
	switch mode {
	case policy.ModeObserve:
		if crit {
			return "pace: hint: memory is critical; this mode (observe) only reports: heavy commands start at once and nothing is queued, refused or stopped; do not start heavy commands until pressure is normal, and run at most one at a time"
		}
		return "pace: hint: memory pressure is high; this mode (observe) only reports: heavy commands start at once and nothing is queued, refused or stopped; run them one at a time and avoid parallel builds or test runs"
	case policy.ModeWarnOnly:
		if crit {
			return "pace: hint: memory is critical; heavy commands will queue or be refused, but this mode (warn-only) stops no running job; do not start heavy commands until pressure is normal, and run at most one at a time"
		}
		return "pace: hint: memory pressure is high; heavy commands will queue or be refused (this mode, warn-only, stops no running job); run them one at a time and avoid parallel builds or test runs"
	}
	if crit {
		return "pace: hint: memory is critical and pace is stopping jobs now; do not start heavy commands until pressure is normal, and run at most one at a time"
	}
	return "pace: hint: memory pressure is high; heavy commands will queue or be refused; run them one at a time and avoid parallel builds or test runs"
}

// heavyLine is the PreToolUse line for a heavy command that runs under
// pace at warn or critical pressure. Outside observe mode admission is
// paused at warn and above, so the command waits; observe starts it at once.
func heavyLine(mode, class string) string {
	if mode == policy.ModeObserve {
		return fmt.Sprintf("pace: this command is heavy (class=%s); this mode (observe) starts it at once, so it adds to the pressure", class)
	}
	return fmt.Sprintf("pace: this command is heavy (class=%s) and will wait for headroom", class)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// SessionStart handles the Claude Code SessionStart hook: it exports the
// shim directory through $CLAUDE_ENV_FILE (when shims are installed) and
// prints one line of context for the agent. It never fails the session:
// errors are returned for logging only and the caller exits 0.
func SessionStart(d HookDeps) error {
	deadline := time.Now().Add(d.budget())
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_ = readHookInput(d.Stdin, deadline)

	var werr error
	shims := d.ShimDir != "" && hasShims(d.ShimDir)
	if envFile := d.Env.get("CLAUDE_ENV_FILE"); envFile != "" && shims {
		werr = appendEnvFile(envFile, envFileLine(d.ShimDir))
	}
	if d.Quiet || d.Stdout == nil {
		return werr
	}
	var b strings.Builder
	b.WriteString("pace resource governor is active on this machine. ")
	if shims {
		b.WriteString("Heavy commands (go build/test, cargo, npm/pnpm/yarn install|test|run, npx playwright/vitest, pytest, uv run, make, tsc, xcodebuild, docker build) are routed through 'pace run' automatically. ")
	} else {
		b.WriteString("Run heavy commands as 'pace run [--class build|test|browser] [--mem 4G] -- <cmd>'. ")
	}
	b.WriteString("Lines starting with 'pace:' on stderr explain queueing, warnings and kills; follow their 'pace: hint:' lines. ")
	b.WriteString(exitCodesText)
	b.WriteString("'pace status --json' shows headroom and suggested concurrency. ")
	if s, ok := d.status(ctx); ok {
		b.WriteString("Now: " + summary(s) + ".")
		if n := modeNote(s.Mode); n != "" {
			b.WriteString(" " + n)
		}
	} else {
		b.WriteString("Now: daemon unreachable (pace run fails open with a DEGRADED line).")
	}
	fmt.Fprintln(d.Stdout, b.String())
	return werr
}

// CodexSessionStart handles the Codex SessionStart hook: Codex has no
// equivalent of $CLAUDE_ENV_FILE, so instead of routing commands through the
// shims it tells the agent to run heavy commands with 'pace run' (plain
// stdout becomes developer context in Codex). It never fails the session.
func CodexSessionStart(d HookDeps) error {
	deadline := time.Now().Add(d.budget())
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	_ = readHookInput(d.Stdin, deadline)
	if d.Quiet || d.Stdout == nil {
		return nil
	}
	pace := d.paceCmd()
	var b strings.Builder
	b.WriteString("pace resource governor is active on this machine. ")
	b.WriteString("Run heavy commands (go build/test, cargo build/test, npm/pnpm/yarn install|test|build, npx playwright/vitest, pytest, uv run, make, tsc, xcodebuild, docker build) through it: ")
	b.WriteString(pace + " run [--class build|test|browser] [--mem 4G] -- <command>. Light commands (ls, git, rg, cat, go version) need no wrapper. ")
	b.WriteString("Lines starting with 'pace:' on stderr explain queueing, warnings and kills; follow their 'pace: hint:' lines. ")
	b.WriteString(exitCodesText)
	b.WriteString("'" + pace + " status --json' shows headroom and suggested concurrency. ")
	if s, ok := d.status(ctx); ok {
		b.WriteString("Now: " + summary(s) + ".")
		if n := modeNote(s.Mode); n != "" {
			b.WriteString(" " + n)
		}
	} else {
		b.WriteString("Now: daemon unreachable (pace run fails open with a DEGRADED line).")
	}
	fmt.Fprintln(d.Stdout, b.String())
	return nil
}

// PreToolUseOutput is the JSON PreToolUse prints when it has context.
type PreToolUseOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

// PreToolUse handles the Claude Code PreToolUse hook for Bash: when the
// daemon reports pressure at warn or above it adds the status as
// additionalContext, with a hint that says what the daemon's mode does. It never blocks, denies or rewrites a command, and
// prints nothing when pressure is normal or the daemon is down.
func PreToolUse(d HookDeps) error { return preToolUse(d, false) }

// CodexPreToolUse handles the Codex PreToolUse hook for shell commands. It
// adds what PreToolUse adds and, because Codex has no way to put the pace
// shims on the PATH of its commands, a reminder whenever a heavy command is
// about to run outside 'pace run' (also at normal pressure and with the
// daemon down). It never blocks, denies or rewrites a command.
func CodexPreToolUse(d HookDeps) error { return preToolUse(d, true) }

// shellTools are the tool names harnesses use for shell commands.
var shellTools = map[string]bool{"Bash": true, "shell": true, "exec_command": true, "local_shell": true, "exec": true, "container.exec": true}

func preToolUse(d HookDeps, nudge bool) error {
	deadline := time.Now().Add(d.budget())
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	in := readHookInput(d.Stdin, deadline)
	if !shellTools[in.ToolName] {
		return nil
	}
	cmd := CommandFromToolInput(in.ToolInput)
	class, heavy, routed := ClassifyShell(cmd)
	var lines []string
	if s, ok := d.status(ctx); ok && (s.Pressure == proto.LevelWarn || s.Pressure == proto.LevelCritical) {
		lines = append(lines, "pace: "+summary(s), pressureHint(s.Mode, s.Pressure))
		if heavy && (routed || !nudge) {
			lines = append(lines, heavyLine(s.Mode, class))
		}
	}
	if nudge && heavy && !routed {
		lines = append(lines, fmt.Sprintf("pace: this command is heavy (class=%s) but runs outside pace, so it gets no admission, warnings or fair share of memory; run heavy commands as: %s run --class %s -- <command>", class, d.paceCmd(), class))
	}
	if len(lines) == 0 {
		return nil
	}
	var out PreToolUseOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.AdditionalContext = strings.Join(lines, "\n")
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(d.Stdout, string(b))
	return err
}

// CommandFromToolInput extracts the shell command from a tool_input object:
// {"command": "..."} (Claude Code, Codex), {"cmd": "..."}, or an argv array
// such as ["bash", "-lc", "..."] in either field.
func CommandFromToolInput(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "cmd"} {
		v, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		var argv []string
		if json.Unmarshal(v, &argv) == nil && len(argv) > 0 {
			if script, ok := shellScript(argv); ok {
				return script // sh [-opts] -c SCRIPT [arg0 ...]
			}
			return strings.Join(argv, " ")
		}
	}
	return ""
}

// shellSegments splits a shell command line into simple commands (lists of
// words) at the operators ; & && || | |& ( ) and newlines. It understands
// single and double quotes, backslash escapes, comments, redirections such
// as 2>&1 or &>file (not separators), and skips heredoc bodies. It is not a
// full shell parser: expansions are left as they are.
func shellSegments(line string) [][]string {
	var segs [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	var heredocs []string // delimiters whose bodies start at the next newline
	flushWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	flushSeg := func() {
		flushWord()
		if len(words) > 0 {
			segs = append(segs, words)
			words = nil
		}
	}
	rs := []rune(line)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			i++
			if rs[i] != '\n' {
				cur.WriteRune(rs[i])
				inWord = true
			}
		case r == '$' && i+1 < len(rs) && rs[i+1] == '\'':
			// $'...' (ANSI-C quoting): backslash escapes, \' included
			inWord = true
			for i += 2; i < len(rs) && rs[i] != '\''; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				cur.WriteRune(rs[i])
			}
		case r == '$' && i+2 < len(rs) && rs[i+1] == '(' && rs[i+2] == '(':
			// $((arithmetic)): one word; its << and >> are shifts
			inWord = true
			depth := 0
			for ; i < len(rs); i++ {
				cur.WriteRune(rs[i])
				if rs[i] == '(' {
					depth++
				} else if rs[i] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
		case r == '<' && i+2 < len(rs) && rs[i+1] == '<' && rs[i+2] == '<':
			// <<< here-string: a redirection, not a heredoc
			if inWord && !allDigits(cur.String()) {
				flushWord()
			}
			cur.WriteString("<<<")
			inWord = true
			i += 2
		case r == '\'':
			inWord = true
			for i++; i < len(rs) && rs[i] != '\''; i++ {
				cur.WriteRune(rs[i])
			}
		case r == '"':
			inWord = true
			for i++; i < len(rs) && rs[i] != '"'; i++ {
				if rs[i] == '\\' && i+1 < len(rs) {
					i++
				}
				cur.WriteRune(rs[i])
			}
		case r == '#' && !inWord:
			for i < len(rs) && rs[i] != '\n' {
				i++
			}
			i-- // let the newline end the segment
		case r == '<' && i+1 < len(rs) && rs[i+1] == '<' && !(i+2 < len(rs) && rs[i+2] == '<'):
			// heredoc: <<WORD, <<-WORD, <<'WORD', <<"WORD"
			flushWord()
			j := i + 2
			if j < len(rs) && rs[j] == '-' {
				j++
			}
			for j < len(rs) && (rs[j] == ' ' || rs[j] == '\t') {
				j++
			}
			var d strings.Builder
			for j < len(rs) && !strings.ContainsRune(" \t\n;&|()<>", rs[j]) {
				if rs[j] != '\'' && rs[j] != '"' && rs[j] != '\\' {
					d.WriteRune(rs[j])
				}
				j++
			}
			if d.Len() > 0 {
				heredocs = append(heredocs, d.String())
			}
			i = j - 1
		case r == '\n':
			flushSeg()
			for _, delim := range heredocs { // skip each body up to its delimiter line
				for i+1 < len(rs) {
					k := i + 1
					for k < len(rs) && rs[k] != '\n' {
						k++
					}
					l := strings.TrimLeft(string(rs[i+1:k]), "\t")
					i = k
					if l == delim {
						break
					}
				}
			}
			heredocs = nil
		case r == '&' && i > 0 && (rs[i-1] == '>' || rs[i-1] == '<'):
			cur.WriteRune(r) // 2>&1, >&2, <&0: part of the redirection word
			inWord = true
		case r == '&' && i+1 < len(rs) && rs[i+1] == '>':
			flushWord() // cmd&>file: the redirection starts a new word
			cur.WriteRune(r)
			inWord = true
		case (r == '>' || r == '<') && inWord && !allDigits(cur.String()) && !strings.HasSuffix(cur.String(), ">") && !strings.HasSuffix(cur.String(), "<") && !strings.HasSuffix(cur.String(), "&"):
			flushWord() // ci>/dev/null: the redirection is not part of the argument
			cur.WriteRune(r)
			inWord = true
		case strings.ContainsRune(";&|()", r):
			flushSeg()
		case r == ' ' || r == '\t' || r == '\r':
			flushWord()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	flushSeg()
	return segs
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// shellScript returns SCRIPT from a shell invocation argv such as
// [bash -lc SCRIPT arg0 ...] or [sh -e -c SCRIPT].
func shellScript(argv []string) (string, bool) {
	if len(argv) < 3 || !shells[filepath.Base(argv[0])] {
		return "", false
	}
	for i := 1; i+1 < len(argv); i++ {
		a := argv[i]
		if a == "--" || !strings.HasPrefix(a, "-") {
			break
		}
		if !strings.HasPrefix(a, "--") && strings.ContainsRune(a, 'c') {
			return argv[i+1], true
		}
	}
	return "", false
}

// wrappers are prefixes that run the rest of the words as the command.
var wrappers = map[string]bool{"env": true, "time": true, "nice": true, "nohup": true, "command": true, "exec": true, "caffeinate": true}

// ClassifyShell classifies a shell command line: heavy reports whether any
// simple command in it is heavy (class is the first heavy one's class), and
// routed whether every heavy command already runs under 'pace run'. It is a
// heuristic over words: quotes, comments and heredoc bodies are understood,
// expansions and functions are not.
func ClassifyShell(line string) (class string, heavy, routed bool) {
	routed = true
	for _, words := range shellSegments(line) {
		for len(words) > 0 && (strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "=") || wrappers[words[0]] || strings.HasPrefix(words[0], "-")) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		if filepath.Base(words[0]) == "pace" {
			if len(words) > 1 && words[1] == "run" {
				inner := paceRunArgv(words[2:])
				c, h := Classify(inner)
				if script, ok := shellScript(inner); !h && ok {
					// pace run -- bash -c '...': classify the script, all of it routed
					c, h, _ = ClassifyShell(script)
				}
				if h && !heavy {
					class, heavy = c, true
				}
			}
			continue
		}
		if c, h := Classify(words); h {
			if !heavy {
				class, heavy = c, true
			}
			routed = false
		}
	}
	if !heavy {
		routed = false
	}
	return class, heavy, routed
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true}

// paceRunArgv returns the command after 'pace run [flags] --'.
func paceRunArgv(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args[i+1:]
		}
	}
	return nil
}

// HookCommand is one command hook in a Claude Code settings.json or a Codex
// hooks.json "hooks" table.
type HookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// HookGroup is a matcher group: the hooks run for tools matching Matcher.
type HookGroup struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []HookCommand `json:"hooks"`
}

// Harness names a supported agent harness.
type Harness string

const (
	HarnessClaude Harness = "claude"
	HarnessCodex  Harness = "codex"
)

// PaceHooks returns the pace hook groups for harness, keyed by event name.
// Claude Code and Codex share the hooks layout and event names.
func PaceHooks(h Harness, paceBin string) map[string][]HookGroup {
	bin := ShellQuote(paceBin)
	return map[string][]HookGroup{
		"SessionStart": {{Hooks: []HookCommand{{Type: "command", Command: bin + " hook " + string(h) + "-session-start", Timeout: 5}}}},
		"PreToolUse":   {{Matcher: "Bash", Hooks: []HookCommand{{Type: "command", Command: bin + " hook " + string(h) + "-pre-tool-use", Timeout: 2}}}},
	}
}

// HooksSnippet returns the JSON fragment ({"hooks": ...}) that wires the
// pace hooks for harness h to paceBin.
func HooksSnippet(h Harness, paceBin string) string {
	b, _ := json.MarshalIndent(map[string]any{"hooks": PaceHooks(h, paceBin)}, "", "  ")
	return string(b) + "\n"
}

// ClaudeSettingsSnippet returns the settings.json fragment that wires the
// Claude Code hooks to paceBin.
func ClaudeSettingsSnippet(paceBin string) string { return HooksSnippet(HarnessClaude, paceBin) }
