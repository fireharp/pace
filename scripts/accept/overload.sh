#!/bin/sh
# M1 "done when": a scripted overload, including jobs that bypass the
# wrapper and ignore warnings, finishes without the machine stalling, and
# every refused or killed job reports its reason.
#
# Simulated machine: capacity 1.5G (so an ungoverned run of this script would
# exceed it), warn below 25% available, critical below 15%. Real allocations
# stay <= 1.2G per process and <= 1.7G in total even if nothing were governed.
# Must pass unchanged on Linux (M3).
. "$(dirname "$0")/lib.sh"
acc_init overload

C_MODE=enforce
C_CAPACITY=1536M
C_RESERVE=128M
C_WARN_PCT=25.0
C_CRIT_PCT=15.0
C_WARN_DEADLINE=3s
acc_daemon_start || acc_finish
CAP=$(bytes 1536M)

sampler_start "$W/usage"

# ---- 1. admission queue -------------------------------------------------------
# Four jobs declaring 450M each: two fit the ~1.1G headroom (capacity minus
# the effective reserve, which is the 25% warn line), two queue.
"$PACE" run --mem 450M -- "$GT" alloc --mem 64M --hold 5s --print-every 0 >"$W/out/q1.out" 2>"$W/out/q1.err" &
Q1=$!
"$PACE" run --mem 450M -- "$GT" alloc --mem 64M --hold 7s --print-every 0 >"$W/out/q2.out" 2>"$W/out/q2.err" &
Q2=$!
track $Q1
track $Q2
if wait_for 10 running_at_least 2; then pass "queue-two-admitted"; else fail "queue-two-admitted" "2 jobs not running"; fi
"$PACE" run --mem 450M -- "$GT" alloc --mem 64M --hold 1s --print-every 0 >"$W/out/q3.out" 2>"$W/out/q3.err" &
Q3=$!
track $Q3
wait_for 5 queued_at_least 1
"$PACE" run --mem 450M -- "$GT" alloc --mem 64M --hold 1s --print-every 0 >"$W/out/q4.out" 2>"$W/out/q4.err" &
Q4=$!
track $Q4
wait_for 5 queued_at_least 2

# --no-wait while full: refused with the headroom reason and a retry-after.
"$PACE" run --no-wait --mem 600M -- "$GT" exit 0 >/dev/null 2>"$W/out/nowait.err"
RC=$?
expect_refused "refused-headroom" "$W/out/nowait.err" "$RC" headroom
expect_line "refused-headroom-retry-after" "$W/out/nowait.err" REFUSED reason=headroom retry_after=

# An impossible request: refused at once even without --no-wait.
"$PACE" run --mem 2G -- "$GT" exit 0 >/dev/null 2>"$W/out/toobig.err"
RC=$?
expect_refused "refused-too-big" "$W/out/toobig.err" "$RC" too_big
expect_line "refused-too-big-need" "$W/out/toobig.err" REFUSED reason=too_big need=2.0G

for i in 1 2 3 4; do
	eval "P=\$Q$i"
	wait_pid 60 "$P"
	expect_rc "queue-job$i-exit0" "$WAIT_RC" 0
done
expect_line "queue-job3-queued" "$W/out/q3.err" QUEUED reason=headroom free= need=450M position=1 eta= job=
expect_line "queue-job4-queued" "$W/out/q4.err" QUEUED reason=headroom need=450M position=2 eta= job=
expect_line "queue-job3-admitted" "$W/out/q3.err" ADMITTED waited= job=
expect_line "queue-job4-admitted" "$W/out/q4.err" ADMITTED waited= job=
expect_no_line "queue-job1-not-queued" "$W/out/q1.err" QUEUED
if grep -q '^pacetest: alloc done' "$W/out/q3.out" && grep -q '^pacetest: alloc done' "$W/out/q4.out"; then
	pass "queue-queued-jobs-ran"
else
	fail "queue-queued-jobs-ran" "queued jobs produced no output"
fi

# ---- 2. a job that ignores its budget warning ------------------------------------
# 410M against a 400M budget: inside the 10% tolerance, so WARN first and a
# kill at the warn deadline (3s).
T0=$(now_s)
"$PACE" run --budget 400M -- "$GT" alloc --mem 410M --print-every 0 >"$W/out/budget.out" 2>"$W/out/budget.err"
RC=$?
DT=$(($(now_s) - T0))
expect_line "budget-warn" "$W/out/budget.err" WARN policy=mem_budget used= budget=400M next=kill in=3s job=
if hint_after "$W/out/budget.err" WARN; then pass "budget-warn-hint"; else fail "budget-warn-hint" "no hint after WARN"; fi
expect_killed "budget-kill" "$W/out/budget.err" "$RC" mem_budget used= budget=400M
if [ "$DT" -ge 3 ] && [ "$DT" -le 12 ]; then pass "budget-kill-after-deadline"; else fail "budget-kill-after-deadline" "took ${DT}s, want 3..12s"; fi

# Far above budget + tolerance: killed at the next sample.
T0=$(now_s)
"$PACE" run --budget 300M -- "$GT" alloc --mem 600M --print-every 0 >/dev/null 2>"$W/out/budget2.err"
RC=$?
DT=$(($(now_s) - T0))
expect_killed "budget-tolerance-kill" "$W/out/budget2.err" "$RC" mem_budget budget=300M
if [ "$DT" -le 5 ]; then pass "budget-tolerance-kill-fast"; else fail "budget-tolerance-kill-fast" "took ${DT}s"; fi

# ---- 3. a wrapped job that escapes its process group ---------------------------------
# The escaped child calls setsid(); it keeps PACE_JOB_ID (env tag) in the first
# run. In the second it drops it and is found by lineage only, so its
# launcher lingers for 1s (a parent that exits within one sampling interval
# leaves no lineage to follow on macOS, which has no cgroups).
for v in tagged untagged; do
	X=
	[ $v = untagged ] && X="--clear-env --linger 1s"
	"$PACE" run --budget 300M -- sh -c '"$1" escape --mem 400M --hold 60s '"$X"'; exec "$1" hang --for 60s' sh "$GT" \
		>"$W/out/esc-$v.out" 2>"$W/out/esc-$v.err" &
	P=$!
	track $P
	wait_pid 30 $P || kill -TERM $P
	RC=$WAIT_RC
	expect_killed "escape-$v-kill" "$W/out/esc-$v.err" "$RC" mem_budget budget=300M
	EP=$(pid_from "$W/out/esc-$v.out" escape)
	if [ -n "$EP" ]; then
		track "$EP"
		if wait_for 5 dead "$EP"; then pass "escape-$v-escaped-child-killed"; else fail "escape-$v-escaped-child-killed" "escaped pid $EP still alive" && kill -KILL "$EP"; fi
	else
		fail "escape-$v-escaped-child-killed" "no escape child pid in output"
		dump "$W/out/esc-$v.out"
	fi
done

# ---- 4. a process that bypasses the wrapper ---------------------------------------------
# A wrapped job (priority 5) holds 400M; then a process started directly from
# this shell, in its own process group, grows towards 1.2G. Unchecked the
# total would exceed capacity; critical pressure must kill the bypasser
# (unmanaged, priority 2) and leave the wrapped job alone.
"$PACE" run --mem 450M -- "$GT" alloc --mem 400M --hold 40s --print-every 0 >"$W/out/keeper.out" 2>"$W/out/keeper.err" &
K=$!
track $K
job_used_400() {
	_s=$(status_json | sed 's/.*"running":\[//; s/"queued":\[.*//') || return 1
	_u=$(printf '%s\n' "$_s" | grep -o '"used_bytes":[0-9]*' | cut -d: -f2 | sort -n | tail -n 1)
	[ -n "$_u" ] && [ "$_u" -ge $((380 * 1048576)) ]
}
wait_for 15 job_used_400 || fail "bypass-keeper-running" "wrapped job not at 400M"
start_own_group U "$GT" alloc --mem 1200M --step 100M --every 150ms --print-every 0 >"$W/out/bypass.out" 2>&1
if wait_pid 30 "$U"; then
	case $WAIT_RC in
	143 | 137) pass "bypass-killed-by-signal" ;;
	*) fail "bypass-killed-by-signal" "bypasser exit status $WAIT_RC" ;;
	esac
else
	fail "bypass-killed-by-signal" "bypasser still running after 30s"
	kill -KILL "$U" 2>/dev/null
fi
if wait_for 5 alert_has '"verb":"KILLED"' '"policy":"pressure"' "\"unit\":\"pg:$U\""; then
	pass "bypass-pressure-kill-unmanaged-unit"
	acc_log "$(grep -F '"verb":"KILLED"' "$PACE_HOME/logs/alerts.jsonl" | grep -F "\"unit\":\"pg:$U\"" | head -n 1)"
else
	fail "bypass-pressure-kill-unmanaged-unit" "no KILLED policy=pressure unit=pg:$U in alerts.jsonl"
	dump "$PACE_HOME/logs/alerts.jsonl"
fi
if alert_has '"verb":"KILLED"' "\"unit\":\"pg:$U\"" '"comm":' '"pid":'; then pass "bypass-kill-names-pid-comm"; else fail "bypass-kill-names-pid-comm" "KILLED lacks pid/comm"; fi
if alive "$K"; then pass "bypass-wrapped-job-survives"; else fail "bypass-wrapped-job-survives" "wrapped job died"; fi
expect_no_line "bypass-wrapped-job-not-killed" "$W/out/keeper.err" KILLED
kill -TERM "$K" 2>/dev/null
wait_pid 10 "$K"

sampler_stop
# The daemon stayed responsive throughout.
if daemon_up; then pass "daemon-responsive"; else fail "daemon-responsive" "status failed"; fi
# Every sample the daemon decided on is in its decision log (records with
# changes, plus heartbeats); add them to what pace status --json showed. Stop
# the daemon first so the log is flushed.
acc_daemon_stop
grep -o '"scoped_used_bytes":[0-9]*' "$PACE_HOME/logs/decisions.jsonl" 2>/dev/null | cut -d: -f2 >>"$W/usage"
MAX=$(max_of "$W/usage")
LIMIT=$((CAP + CAP / 10))
acc_log "max scoped usage $(mib "${MAX:-0}") of capacity $(mib "$CAP") (limit with tolerance $(mib "$LIMIT"), $(wc -l <"$W/usage" | tr -d ' ') samples)"
echo "OVERLOAD_MAX_USED_BYTES=${MAX:-0} CAPACITY_BYTES=$CAP" >"$W/max-usage"
if [ -n "$MAX" ] && [ "$MAX" -le "$LIMIT" ]; then
	pass "usage-never-exceeds-capacity"
else
	fail "usage-never-exceeds-capacity" "max $(mib "${MAX:-0}") > $(mib "$LIMIT")"
fi

acc_finish
