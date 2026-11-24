#!/bin/sh
# M0 inform and warn: observe mode warns but never enforces; status and
# status JSON show the running job with its usage; the decision log is
# written and replays with zero differences; warn-only mode warns, logs what
# it would do, and never kills.
. "$(dirname "$0")/lib.sh"
acc_init m0

# ---- observe mode --------------------------------------------------------------
C_MODE=observe
C_CAPACITY=2G
acc_daemon_start || acc_finish

# A job far over its budget (300M used vs 200M budget, > 10% tolerance): in
# enforce mode it would be killed at once.
"$PACE" run --budget 200M -- "$GT" alloc --mem 300M --hold 5s --print-every 0 \
	>"$W/out/obs.out" 2>"$W/out/obs.err" &
P=$!
track $P

job_running_with_usage() {
	_s=$(status_json) || return 1
	_r=$(printf '%s\n' "$_s" | sed 's/.*"running":\[//')
	_u=$(printf '%s\n' "$_r" | jnum used_bytes)
	[ -n "$_u" ] && [ "$_u" -ge $((250 * 1048576)) ]
}
if wait_for 10 job_running_with_usage; then
	S=$(status_json)
	JOB=$(printf '%s\n' "$S" | sed 's/.*"running":\[//' | jstr id)
	U=$(printf '%s\n' "$S" | sed 's/.*"running":\[//' | jnum used_bytes)
	pass "m0-status-json-running-job"
	acc_log "status --json: running job $JOB used_bytes=$U ($(mib "$U"))"
	for k in mode capacity_bytes avail_bytes headroom_bytes pressure admitting suggested_concurrency; do
		if printf '%s\n' "$S" | grep -q "\"$k\":"; then pass "m0-status-json-has-$k"; else fail "m0-status-json-has-$k" "missing $k"; fi
	done
	case $(printf '%s\n' "$S" | jstr mode) in
	observe) pass "m0-status-mode-observe" ;;
	*) fail "m0-status-mode-observe" "mode is not observe" ;;
	esac
	"$PACE" status >"$W/out/status.txt" 2>&1
	if grep -q "$JOB" "$W/out/status.txt" && grep -q 'RUNNING (1)' "$W/out/status.txt"; then
		pass "m0-status-text-running-job"
	else
		fail "m0-status-text-running-job" "pace status does not show $JOB"
		dump "$W/out/status.txt"
	fi
	if job_json "$JOB" | grep -q "\"id\":\"$JOB\""; then pass "m0-status-job-json"; else fail "m0-status-job-json" "pace status --job $JOB --json"; fi
else
	fail "m0-status-json-running-job" "no running job with used_bytes >= 250M within 10s"
	status_json
fi

wait_pid 30 $P
expect_rc "m0-observe-overbudget-exit0" "$WAIT_RC" 0
expect_line "m0-observe-warn" "$W/out/obs.err" WARN policy=mem_budget used= budget=200M next=kill in= mode=observe job=
if hint_after "$W/out/obs.err" WARN; then pass "m0-observe-warn-hint"; else fail "m0-observe-warn-hint" "no hint after WARN"; fi
# Nothing is killed in observe mode, and the hint must not say otherwise.
if grep -q '^pace: hint: .*will be killed' "$W/out/obs.err"; then
	fail "m0-observe-hint-no-threat" "$(grep '^pace: hint:' "$W/out/obs.err" | head -n 1)"
else
	pass "m0-observe-hint-no-threat"
fi
expect_no_line "m0-observe-not-killed" "$W/out/obs.err" KILLED
if grep -q '^pacetest: alloc done' "$W/out/obs.out"; then pass "m0-observe-stdout-passthrough"; else fail "m0-observe-stdout-passthrough" "child stdout missing"; fi
if alert_has '"verb":"INFO"' '"policy":"mem_budget"' 'would kill'; then pass "m0-observe-would-kill-logged"; else fail "m0-observe-would-kill-logged" "no INFO would-kill in alerts.jsonl"; fi

# ---- pace test-alert: a real WARN that a status poller sees after it ended ----------
"$PACE" test-alert --mem 160M --hold 10s >"$W/out/test-alert.out" 2>"$W/out/test-alert.err"
expect_rc "test-alert-exit0-in-observe" $? 0
if grep -q '^pace: WARN policy=mem_budget .*mode=observe' "$W/out/test-alert.err"; then pass "test-alert-warn-line"; else fail "test-alert-warn-line" "$(head -n 5 "$W/out/test-alert.err")"; fi
if status_json | grep -q '"recent_alerts":\[.*"verb":"WARN".*"policy":"mem_budget"'; then pass "test-alert-in-recent-alerts-after-it-ended"; else fail "test-alert-in-recent-alerts-after-it-ended" "$(status_json | head -c 400)"; fi
"$PACE" status >"$W/out/status-alerts.out" 2>&1
if grep -q '^RECENT ALERTS' "$W/out/status-alerts.out"; then pass "status-shows-recent-alerts"; else fail "status-shows-recent-alerts" "$(tail -n 8 "$W/out/status-alerts.out")"; fi

# ---- decision log + replay -------------------------------------------------------
acc_daemon_stop
DL="$PACE_HOME/logs/decisions.jsonl"
if [ -s "$DL" ]; then
	pass "m0-decision-log-written"
	acc_log "decision log: $(wc -l <"$DL" | tr -d ' ') records"
else
	fail "m0-decision-log-written" "$DL missing or empty"
fi
if grep -q '"verb":"WARN"' "$DL" 2>/dev/null; then pass "m0-decision-log-has-warn"; else fail "m0-decision-log-has-warn" "no WARN in decision log"; fi
"$PACE" replay "$DL" >"$W/out/replay.out" 2>&1
RC=$?
expect_rc "m0-replay-exit0" "$RC" 0
if grep -Eq '^replay: records=[1-9][0-9]* replayed=[1-9][0-9]* mismatches=0$' "$W/out/replay.out"; then
	pass "m0-replay-zero-differences"
	acc_log "$(tail -n 1 "$W/out/replay.out")"
else
	fail "m0-replay-zero-differences" "$(tail -n 3 "$W/out/replay.out")"
fi

# ---- warn-only mode --------------------------------------------------------------
C_MODE=warn-only
acc_daemon_start || acc_finish
"$PACE" run --budget 200M -- "$GT" alloc --mem 300M --hold 4s --print-every 0 \
	>"$W/out/wo.out" 2>"$W/out/wo.err"
RC=$?
expect_rc "m0-warnonly-overbudget-exit0" "$RC" 0
expect_line "m0-warnonly-warn" "$W/out/wo.err" WARN policy=mem_budget budget=200M next=kill mode=warn-only
if grep -q '^pace: hint: .*will be killed' "$W/out/wo.err"; then
	fail "m0-warnonly-hint-no-threat" "$(grep '^pace: hint:' "$W/out/wo.err" | head -n 1)"
else
	pass "m0-warnonly-hint-no-threat"
fi
expect_no_line "m0-warnonly-not-killed" "$W/out/wo.err" KILLED
S=$(status_json)
case $(printf '%s\n' "$S" | jstr mode) in
warn-only) pass "m0-status-mode-warnonly" ;;
*) fail "m0-status-mode-warnonly" "mode is not warn-only" ;;
esac
# warn-only still runs admission: an impossible request is refused.
"$PACE" run --mem 3G -- true >/dev/null 2>"$W/out/wo-big.err"
RC=$?
expect_refused "m0-warnonly-too-big" "$W/out/wo-big.err" "$RC" too_big
acc_daemon_stop
"$PACE" replay "$PACE_HOME"/logs/decisions.jsonl >"$W/out/replay2.out" 2>&1
RC=$?
expect_rc "m0-replay-warnonly-exit0" "$RC" 0

acc_finish
