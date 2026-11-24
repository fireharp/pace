#!/bin/sh
# M2 learn and reap: a job with no estimate is admitted on its learned
# history; a hung job is reaped within its class limit (policy=stuck); a
# wall-clock limit kills (policy=wall_clock); a job whose wrapper dies is
# reaped after orphan_grace (policy=orphaned). The freeze step (process state
# T before a kill) is checked in pressure.sh.
. "$(dirname "$0")/lib.sh"
acc_init m2

C_MODE=enforce
C_CAPACITY=2G
C_RESERVE=128M
C_WARN_DEADLINE=3s
C_ORPHAN_GRACE=2s
C_EXTRA='
[classes.lrn]
estimate = "100M"
budget = "1G"
slots = "0"

[classes.stk]
estimate = "64M"
budget = "512M"
slots = "0"
no_progress = "3s"
'
acc_daemon_start || acc_finish

# ---- 1. learning ----------------------------------------------------------------
for i in 1 2 3; do
	"$PACE" run --class lrn -- "$GT" alloc --mem 300M --hold 1s --print-every 0 >/dev/null 2>"$W/out/learn$i.err"
	expect_rc "learn-run$i-exit0" $? 0
done
# Hold 1.5G of reservations so that ~200M is left (capacity 2G minus the
# effective reserve, the 15% warn line): the class default (100M) would fit,
# the learned estimate (~330M) does not.
"$PACE" run --mem 1500M -- "$GT" hang --for 60s >/dev/null 2>"$W/out/blocker.err" &
B=$!
track $B
wait_for 10 running_at_least 1
"$PACE" run --class lrn -- "$GT" alloc --mem 300M --hold 1s --print-every 0 >/dev/null 2>"$W/out/learn4.err" &
L=$!
track $L
if wait_for 10 has_line "$W/out/learn4.err" QUEUED reason=headroom; then
	Q=$(pace_line "$W/out/learn4.err" QUEUED reason=headroom)
	NEED=$(field "$Q" need)
	acc_log "$Q"
	case $NEED in
	3[0-9][0-9]M | 4[0-9][0-9]M) pass "learn-queued-on-learned-estimate" ;;
	*) fail "learn-queued-on-learned-estimate" "need=$NEED, want the learned ~330M (class default is 100M)" ;;
	esac
else
	fail "learn-queued-on-learned-estimate" "no QUEUED reason=headroom (estimate not learned?)"
	dump "$W/out/learn4.err"
fi
kill -TERM $B 2>/dev/null
wait_pid 10 $B
if wait_pid 30 $L; then expect_rc "learn-run4-exit0" "$WAIT_RC" 0; else fail "learn-run4-exit0" "still running"; fi
expect_line "learn-admitted-after-blocker" "$W/out/learn4.err" ADMITTED waited=
ID4=$(field "$(pace_line "$W/out/learn4.err" ADMITTED)" job)
E4=$(job_json "$ID4" | jnum estimate_bytes)
if [ -n "$E4" ] && [ "$E4" -ge $((300 * 1048576)) ]; then
	pass "learn-status-estimate"
	acc_log "job $ID4 estimate_bytes=$E4 ($(mib "$E4"))"
else
	fail "learn-status-estimate" "estimate_bytes=${E4:-none} for $ID4"
fi

# ---- 2. stuck --------------------------------------------------------------------------
T0=$(now_s)
"$PACE" run --class stk -- "$GT" hang >/dev/null 2>"$W/out/stuck.err"
RC=$?
DT=$(($(now_s) - T0))
expect_line "stuck-warn" "$W/out/stuck.err" WARN policy=stuck idle= limit=3s next=kill in=3s job=
if hint_after "$W/out/stuck.err" WARN; then pass "stuck-warn-hint"; else fail "stuck-warn-hint" "no hint"; fi
expect_killed "stuck-kill" "$W/out/stuck.err" "$RC" stuck idle= limit=3s
# no_progress (3s) + warn deadline (3s), plus sampling slack.
if [ "$DT" -ge 5 ] && [ "$DT" -le 10 ]; then pass "stuck-reaped-within-limit"; else fail "stuck-reaped-within-limit" "took ${DT}s, want 5..10s"; fi

# ---- 3. wall clock -------------------------------------------------------------------------
T0=$(now_s)
"$PACE" run --timeout 4s -- "$GT" print --every 300ms >/dev/null 2>"$W/out/wall.err"
RC=$?
DT=$(($(now_s) - T0))
expect_line "wall-warn" "$W/out/wall.err" WARN policy=wall_clock elapsed= limit=4s next=kill in= job=
if hint_after "$W/out/wall.err" WARN; then pass "wall-warn-hint"; else fail "wall-warn-hint" "no hint"; fi
expect_killed "wall-kill" "$W/out/wall.err" "$RC" wall_clock elapsed= limit=4s
if [ "$DT" -ge 4 ] && [ "$DT" -le 8 ]; then pass "wall-kill-at-limit"; else fail "wall-kill-at-limit" "took ${DT}s, want 4..8s"; fi

# ---- 4. orphan ---------------------------------------------------------------------------------
"$PACE" run -- sh -c 'echo $$ >"$2/orphan.pid"; echo "$PACE_JOB_ID" >"$2/orphan.id"; exec "$1" hang --for 60s' sh "$GT" "$W/out" \
	>/dev/null 2>"$W/out/orphan.err" &
P=$!
track $P
orphan_started() { [ -s "$W/out/orphan.id" ] && [ -s "$W/out/orphan.pid" ]; }
if wait_for 10 orphan_started; then
	C=$(cat "$W/out/orphan.pid")
	ID=$(cat "$W/out/orphan.id")
	track "$C"
	wait_for 5 running_at_least 1
	sleep 0.5
	kill -KILL $P
	wait $P 2>/dev/null
	T0=$(now_s)
	sleep 0.5
	if alive "$C"; then pass "orphan-grace-respected"; else fail "orphan-grace-respected" "job died at once"; fi
	if wait_for 10 dead "$C"; then
		DT=$(($(now_s) - T0))
		pass "orphan-reaped"
		acc_log "orphaned job $ID (pid $C) reaped ${DT}s after its wrapper died"
	else
		fail "orphan-reaped" "pid $C alive 10s after its wrapper died"
	fi
	if wait_for 5 alert_has '"verb":"KILLED"' '"policy":"orphaned"' "\"job\":\"$ID\""; then pass "orphan-killed-event"; else fail "orphan-killed-event" "no KILLED policy=orphaned job=$ID in alerts"; fi
	if job_json "$ID" | grep -q '"killed_by":"orphaned"'; then pass "orphan-status-killed-by"; else fail "orphan-status-killed-by" "$(job_json "$ID")"; fi
else
	fail "orphan-started" "job did not start"
fi

acc_finish
