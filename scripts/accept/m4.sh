#!/bin/sh
# M4 integration: PATH shims route heavy commands through pace run without the
# caller choosing; nested pace run does not double-govern; the Claude Code
# hook handlers produce the documented output (never touching ~/.claude);
# GET /metrics serves Prometheus text; pace doctor runs.
. "$(dirname "$0")/lib.sh"
acc_init m4

C_MODE=enforce
C_CAPACITY=4G
acc_daemon_start || acc_finish

# Hooks, shims and doctor run with a throwaway HOME: nothing under the real
# ~/.claude, ~/.pace or ~/Library can be read-modified-written by this script.
FAKEHOME=$W/fakehome
mkdir -p "$FAKEHOME"
SHIMS=$W/shims
REAL=$W/realbin
mkdir -p "$REAL"
export PACE_SHIM_DIR="$SHIMS"

recent_done() { status_json | grep -o '"state":"done"' | wc -l | tr -d ' '; }

# A fake "make" (the real one is never run): prints the job id it sees.
cat >"$REAL/make" <<EOF
#!/bin/sh
echo "fake-make args=\$* job=\${PACE_JOB_ID:-none}"
exec "$GT" alloc --mem 32M --hold 500ms --print-every 0
EOF
chmod +x "$REAL/make"

# ---- 1. shims -------------------------------------------------------------------------
HOME=$FAKEHOME "$PACE" shims install --dir "$SHIMS" --tools make,go >"$W/out/shims.out" 2>&1
expect_rc "shims-install" $? 0
if [ -x "$SHIMS/make" ] && grep -q 'pace-shim' "$SHIMS/make"; then pass "shims-file-installed"; else fail "shims-file-installed" "$(ls -l "$SHIMS" 2>&1)"; fi
HOME=$FAKEHOME "$PACE" shims list --dir "$SHIMS" >"$W/out/shims-list.out" 2>&1
if grep -q make "$W/out/shims-list.out"; then pass "shims-list"; else fail "shims-list" "$(cat "$W/out/shims-list.out")"; fi

N0=$(recent_done)
PATH="$SHIMS:$REAL:$PATH" make all >"$W/out/make.out" 2>"$W/out/make.err"
RC=$?
expect_rc "shim-heavy-exit0" "$RC" 0
MJ=$(sed -n 's/^fake-make args=all job=//p' "$W/out/make.out")
case $MJ in
none | "") fail "shim-heavy-governed" "fake make ran without PACE_JOB_ID: $(cat "$W/out/make.out")" ;;
*)
	pass "shim-heavy-governed"
	acc_log "make all ran as job $MJ"
	CL=$(job_json "$MJ" | jstr class)
	if [ "$CL" = build ]; then pass "shim-heavy-class-build"; else fail "shim-heavy-class-build" "class=$CL"; fi
	;;
esac
PATH="$SHIMS:$REAL:$PATH" make -n all >"$W/out/maken.out" 2>&1
if grep -q 'job=none' "$W/out/maken.out"; then pass "shim-light-passthrough"; else fail "shim-light-passthrough" "$(cat "$W/out/maken.out")"; fi
PACE_SHIM_DISABLE=1 PATH="$SHIMS:$REAL:$PATH" make all >"$W/out/maked.out" 2>&1
if grep -q 'job=none' "$W/out/maked.out"; then pass "shim-disable-passthrough"; else fail "shim-disable-passthrough" "$(cat "$W/out/maked.out")"; fi
# pace missing: the shim still runs the real tool after a DEGRADED line.
mkdir -p "$W/shims2"
HOME=$FAKEHOME "$PACE" shims install --dir "$W/shims2" --tools make >/dev/null 2>&1
sed "s|^g=.*|g=$W/no-such-pace|" "$W/shims2/make" >"$W/shims2/make.new" && mv "$W/shims2/make.new" "$W/shims2/make" && chmod +x "$W/shims2/make"
PATH="$W/shims2:$REAL:$PATH" make all >"$W/out/makem.out" 2>"$W/out/makem.err"
expect_rc "shim-pace-missing-runs-tool" $? 0
expect_line "shim-pace-missing-degraded" "$W/out/makem.err" DEGRADED reason=pace_missing

# ---- 2. nested pace run inside a governed job -------------------------------------------------
N1=$(recent_done)
"$PACE" run -- sh -c 'echo "outer=$PACE_JOB_ID"; "$1" run -- sh -c "echo inner=\$PACE_JOB_ID"; PATH="$2:$3:$PATH" make nested' \
	sh "$PACE" "$SHIMS" "$REAL" >"$W/out/nested.out" 2>"$W/out/nested.err"
RC=$?
expect_rc "nested-exit0" "$RC" 0
O=$(sed -n 's/^outer=//p' "$W/out/nested.out")
I=$(sed -n 's/^inner=//p' "$W/out/nested.out")
S=$(sed -n 's/^fake-make args=nested job=//p' "$W/out/nested.out")
if [ -n "$O" ] && [ "$O" = "$I" ] && [ "$O" = "$S" ]; then pass "nested-same-job-id"; else fail "nested-same-job-id" "outer=$O inner=$I shim=$S"; fi
sleep 0.5
N2=$(recent_done)
if [ $((N2 - N1)) -eq 1 ]; then pass "nested-single-governed-job"; else fail "nested-single-governed-job" "$((N2 - N1)) jobs recorded (before $N1, after $N2)"; fi
if grep -q '^pace: ' "$W/out/nested.err"; then fail "nested-no-pace-lines" "$(cat "$W/out/nested.err")"; else pass "nested-no-pace-lines"; fi
: "$N0"

# ---- 3. Claude Code hooks ----------------------------------------------------------------------
ENVF=$W/claude.env
SS='{"session_id":"accept","hook_event_name":"SessionStart","source":"startup","cwd":"/tmp"}'
for n in 1 2; do
	printf '%s\n' "$SS" | HOME=$FAKEHOME CLAUDE_ENV_FILE=$ENVF "$PACE" hook claude-session-start >"$W/out/ss$n.out" 2>"$W/out/ss$n.err"
	expect_rc "hook-session-start-exit0-run$n" $? 0
done
if [ "$(wc -l <"$W/out/ss1.out" | tr -d ' ')" = 1 ] && grep -q 'pace' "$W/out/ss1.out" && grep -q '201' "$W/out/ss1.out"; then
	pass "hook-session-start-one-line-context"
else
	fail "hook-session-start-one-line-context" "$(cat "$W/out/ss1.out")"
fi
if grep -q "export PATH=$SHIMS:" "$ENVF" 2>/dev/null; then pass "hook-session-start-env-file-path"; else fail "hook-session-start-env-file-path" "$(cat "$ENVF" 2>&1)"; fi
if [ "$(grep -c 'pace shims' "$ENVF")" = 1 ]; then pass "hook-session-start-idempotent"; else fail "hook-session-start-idempotent" "$(cat "$ENVF")"; fi
# The env line is valid sh and puts the shims first.
P2=$(env -i PATH=/usr/bin:/bin sh -c ". '$ENVF'; echo \"\$PATH\"")
case $P2 in "$SHIMS:"*) pass "hook-session-start-env-line-sources" ;; *) fail "hook-session-start-env-line-sources" "PATH=$P2" ;; esac

PT='{"session_id":"accept","hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"make all"}}'
printf '%s\n' "$PT" | HOME=$FAKEHOME "$PACE" hook claude-pre-tool-use >"$W/out/pt-normal.out" 2>&1
expect_rc "hook-pre-tool-use-exit0" $? 0
if [ ! -s "$W/out/pt-normal.out" ]; then pass "hook-pre-tool-use-silent-when-normal"; else fail "hook-pre-tool-use-silent-when-normal" "$(cat "$W/out/pt-normal.out")"; fi

# warn pressure (observe mode, warn below 90% available): context is added.
acc_daemon_stop
C_MODE=observe
C_CAPACITY=2G
C_WARN_PCT=90.0
C_CRIT_PCT=5.0
acc_daemon_start || acc_finish
"$PACE" run -- "$GT" alloc --mem 300M --hold 20s --print-every 0 >/dev/null 2>&1 &
H=$!
track $H
wait_for 15 pressure_is warn || fail "hook-pressure-warn-setup" "pressure never reached warn"
printf '%s\n' "$PT" | HOME=$FAKEHOME "$PACE" hook claude-pre-tool-use >"$W/out/pt-warn.out" 2>"$W/out/pt-warn.err"
expect_rc "hook-pre-tool-use-warn-exit0" $? 0
if grep -q '^{"hookSpecificOutput":{"hookEventName":"PreToolUse","additionalContext":"pace: mode=observe pressure=warn .*"}}$' "$W/out/pt-warn.out"; then
	pass "hook-pre-tool-use-json-shape"
else
	fail "hook-pre-tool-use-json-shape" "$(cat "$W/out/pt-warn.out")"
fi
if command -v python3 >/dev/null 2>&1; then
	if python3 -c 'import json,sys; d=json.load(sys.stdin); c=d["hookSpecificOutput"]["additionalContext"]; assert d["hookSpecificOutput"]["hookEventName"]=="PreToolUse"; assert "pressure=warn" in c and "pace: hint:" in c and "class=build" in c' <"$W/out/pt-warn.out"; then
		pass "hook-pre-tool-use-valid-json"
	else
		fail "hook-pre-tool-use-valid-json" "$(cat "$W/out/pt-warn.out")"
	fi
else
	echo "SKIP hook-pre-tool-use-valid-json (no python3)"
fi
# observe admits every job at once and enforces nothing: the hint says so and
# never claims queueing, refusals, waiting or stopping.
if grep -q 'this mode (observe) only reports' "$W/out/pt-warn.out" &&
	! grep -q -e 'will queue or be refused' -e 'will wait for headroom' -e 'stopping jobs' "$W/out/pt-warn.out"; then
	pass "hook-pre-tool-use-observe-honest"
else
	fail "hook-pre-tool-use-observe-honest" "$(cat "$W/out/pt-warn.out")"
fi
printf '%s\n' '{"hook_event_name":"PreToolUse","tool_name":"Read","tool_input":{"file_path":"/x"}}' |
	HOME=$FAKEHOME "$PACE" hook claude-pre-tool-use >"$W/out/pt-read.out" 2>&1
if [ ! -s "$W/out/pt-read.out" ]; then pass "hook-pre-tool-use-ignores-non-bash"; else fail "hook-pre-tool-use-ignores-non-bash" "$(cat "$W/out/pt-read.out")"; fi
printf 'not json' | HOME=$FAKEHOME "$PACE" hook claude-pre-tool-use >/dev/null 2>&1
expect_rc "hook-pre-tool-use-bad-input-exit0" $? 0

# ---- 4. metrics and health over the unix socket -----------------------------------------------------
if command -v curl >/dev/null 2>&1; then
	curl -s --max-time 5 --unix-socket "$PACE_SOCK" http://pace/metrics >"$W/out/metrics.txt"
	expect_rc "metrics-http-ok" $? 0
	if grep -q '^# TYPE pace_' "$W/out/metrics.txt" && grep -Eq '^pace_[a-z0-9_]+(\{[^}]*\})? [0-9.e+-]+$' "$W/out/metrics.txt"; then
		pass "metrics-prometheus-text"
		acc_log "$(grep -c '^pace_' "$W/out/metrics.txt") pace_ samples, e.g. $(grep '^pace_' "$W/out/metrics.txt" | head -n 1)"
	else
		fail "metrics-prometheus-text" "$(head -n 5 "$W/out/metrics.txt")"
	fi
	if [ "$(curl -s --max-time 5 --unix-socket "$PACE_SOCK" http://pace/healthz)" = ok ]; then pass "healthz-ok"; else fail "healthz-ok" "not ok"; fi
else
	echo "SKIP metrics (no curl)"
fi
kill -TERM $H 2>/dev/null
wait_pid 10 $H

# ---- 5. hooks with the daemon down; doctor -------------------------------------------------------------
acc_daemon_stop
T0=$(now_s)
printf '%s\n' "$PT" | HOME=$FAKEHOME "$PACE" hook claude-pre-tool-use >"$W/out/pt-down.out" 2>&1
RC=$?
DT=$(($(now_s) - T0))
expect_rc "hook-pre-tool-use-daemon-down-exit0" "$RC" 0
if [ ! -s "$W/out/pt-down.out" ] && [ "$DT" -le 1 ]; then pass "hook-pre-tool-use-daemon-down-silent-fast"; else fail "hook-pre-tool-use-daemon-down-silent-fast" "took ${DT}s: $(cat "$W/out/pt-down.out")"; fi
printf '%s\n' "$SS" | HOME=$FAKEHOME CLAUDE_ENV_FILE=$ENVF "$PACE" hook claude-session-start >"$W/out/ss-down.out" 2>&1
expect_rc "hook-session-start-daemon-down-exit0" $? 0
if grep -q 'unreachable' "$W/out/ss-down.out"; then pass "hook-session-start-daemon-down-context"; else fail "hook-session-start-daemon-down-context" "$(cat "$W/out/ss-down.out")"; fi

HOME=$FAKEHOME "$PACE" integrate claude >"$W/out/integrate.out" 2>&1
expect_rc "integrate-claude-snippet" $? 0
if grep -q 'claude-pre-tool-use' "$W/out/integrate.out" && grep -q 'claude-session-start' "$W/out/integrate.out"; then pass "integrate-claude-names-hooks"; else fail "integrate-claude-names-hooks" "$(head -n 5 "$W/out/integrate.out")"; fi
if [ -e "$FAKEHOME/.claude" ]; then fail "hooks-never-write-claude-dir" "$(ls -la "$FAKEHOME/.claude")"; else pass "hooks-never-write-claude-dir"; fi

# Codex hooks: daemon down, a heavy command outside pace gets a reminder (exit 0, fast).
CX='{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"cd svc && go test ./..."}}'
T0=$(now_s)
printf '%s\n' "$CX" | HOME=$FAKEHOME "$PACE" hook codex-pre-tool-use >"$W/out/cx-down.out" 2>&1
RC=$?
DT=$(($(now_s) - T0))
expect_rc "codex-pre-tool-use-exit0" "$RC" 0
if grep -q '"additionalContext":"pace: this command is heavy (class=test) but runs outside pace' "$W/out/cx-down.out" && [ "$DT" -le 1 ]; then pass "codex-pre-tool-use-reminds"; else fail "codex-pre-tool-use-reminds" "took ${DT}s: $(cat "$W/out/cx-down.out")"; fi
printf '%s\n' '{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"pace run -- go test ./..."}}' | HOME=$FAKEHOME "$PACE" hook codex-pre-tool-use >"$W/out/cx-routed.out" 2>&1
if [ ! -s "$W/out/cx-routed.out" ]; then pass "codex-pre-tool-use-quiet-when-routed"; else fail "codex-pre-tool-use-quiet-when-routed" "$(cat "$W/out/cx-routed.out")"; fi
printf '%s\n' "$SS" | HOME=$FAKEHOME "$PACE" hook codex-session-start >"$W/out/cx-ss.out" 2>&1
if grep -q ' run \[--class' "$W/out/cx-ss.out"; then pass "codex-session-start-context"; else fail "codex-session-start-context" "$(cat "$W/out/cx-ss.out")"; fi

# --apply / --remove on a throwaway Codex hooks.json that has another tool's hooks.
mkdir -p "$FAKEHOME/.codex"
printf '%s\n' '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"/x/other >/dev/null 2>&1"}]}]}}' >"$FAKEHOME/.codex/hooks.json"
HOME=$FAKEHOME CODEX_HOME= "$PACE" integrate codex --apply >"$W/out/cx-apply.out" 2>&1
expect_rc "integrate-codex-apply" $? 0
if grep -q 'hook codex-pre-tool-use' "$FAKEHOME/.codex/hooks.json" && grep -q '/x/other >/dev/null 2>&1' "$FAKEHOME/.codex/hooks.json" && ls "$FAKEHOME/.codex/" | grep -q 'hooks.json.pace-backup-'; then pass "integrate-codex-apply-merges"; else fail "integrate-codex-apply-merges" "$(cat "$FAKEHOME/.codex/hooks.json")"; fi
HOME=$FAKEHOME CODEX_HOME= "$PACE" integrate codex --remove >/dev/null 2>&1
if ! grep -q 'pace' "$FAKEHOME/.codex/hooks.json" && grep -q '/x/other' "$FAKEHOME/.codex/hooks.json"; then pass "integrate-codex-remove"; else fail "integrate-codex-remove" "$(cat "$FAKEHOME/.codex/hooks.json")"; fi

acc_daemon_start || acc_finish
HOME=$FAKEHOME "$PACE" doctor >"$W/out/doctor.out" 2>&1
RC=$?
case $RC in
0 | 1) pass "doctor-runs" ;;
*) fail "doctor-runs" "exit $RC" ;;
esac
if grep -q 'daemon' "$W/out/doctor.out"; then pass "doctor-reports-daemon"; else fail "doctor-reports-daemon" "$(head -n 10 "$W/out/doctor.out")"; fi
acc_log "doctor exit $RC: $(grep -c . "$W/out/doctor.out") lines, $(grep -c 'FAIL' "$W/out/doctor.out") FAIL"

acc_finish
