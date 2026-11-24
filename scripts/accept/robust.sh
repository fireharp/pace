#!/bin/sh
# Robustness: daemon crash and restart mid-job (re-attach, still governed);
# no daemon at all (fail open, bounded slots); exit-code passthrough incl.
# reserved-code collisions and signal deaths; stdout/stderr passthrough with
# pace lines never spliced into the middle of a child's line.
. "$(dirname "$0")/lib.sh"
acc_init robust

C_MODE=enforce
C_CAPACITY=2G
C_RESERVE=128M
C_FAILOPEN_SLOTS=2
C_FAILOPEN_WAIT=1s
acc_daemon_start || acc_finish

# ---- 1. daemon killed -9 mid-job, restarted: job re-attached and governed --------
GO=$W/out/go
"$PACE" run --budget 400M -- sh -c 'while [ ! -f "$2" ]; do sleep 0.2; done; exec "$1" alloc --mem 600M --print-every 0' sh "$GT" "$GO" \
	>"$W/out/crash.out" 2>"$W/out/crash.err" &
P=$!
track $P
wait_for 10 running_at_least 1 || fail "crash-job-running" "job not running"
acc_daemon_kill9
if wait_for 10 has_line "$W/out/crash.err" DEGRADED reason=daemon_lost job=; then
	pass "crash-wrapper-degraded-daemon-lost"
	acc_log "$(pace_line "$W/out/crash.err" DEGRADED)"
else
	fail "crash-wrapper-degraded-daemon-lost" "no DEGRADED reason=daemon_lost"
	dump "$W/out/crash.err"
fi
if alive $P; then pass "crash-job-keeps-running"; else fail "crash-job-keeps-running" "wrapper exited"; fi
acc_daemon_start || acc_finish
if wait_for 15 has_line "$W/out/crash.err" INFO msg=reattached; then
	pass "crash-wrapper-reattached"
	acc_log "$(pace_line "$W/out/crash.err" INFO msg=reattached)"
else
	fail "crash-wrapper-reattached" "no 'INFO ... msg=reattached'"
	dump "$W/out/crash.err"
fi
if running_at_least 1; then pass "crash-job-in-status-after-restart"; else fail "crash-job-in-status-after-restart" "no running job"; fi
: >"$GO"
wait_pid 30 $P
expect_killed "crash-budget-kill-after-restart" "$W/out/crash.err" "$WAIT_RC" mem_budget budget=400M
acc_daemon_stop

# ---- 2. no daemon: fail open, bounded by fail-open slots --------------------------------
T0=$(now_s)
"$PACE" run -- "$GT" exit 7 >/dev/null 2>"$W/out/fo1.err"
RC=$?
DT=$(($(now_s) - T0))
expect_rc "failopen-exit-code-passthrough" "$RC" 7
expect_line "failopen-degraded" "$W/out/fo1.err" DEGRADED reason=daemon_unreachable
if hint_after "$W/out/fo1.err" DEGRADED; then pass "failopen-degraded-hint"; else fail "failopen-degraded-hint" "no hint"; fi
if [ "$DT" -le 5 ]; then pass "failopen-bounded-wait"; else fail "failopen-bounded-wait" "took ${DT}s"; fi
"$PACE" run -- "$GT" hang --for 6s >/dev/null 2>"$W/out/fo-a.err" &
FA=$!
"$PACE" run -- "$GT" hang --for 6s >/dev/null 2>"$W/out/fo-b.err" &
FB=$!
track $FA
track $FB
both_degraded() { has_line "$W/out/fo-a.err" DEGRADED && has_line "$W/out/fo-b.err" DEGRADED; }
if wait_for 10 both_degraded; then pass "failopen-two-slots-used"; else fail "failopen-two-slots-used" "slots not taken"; fi
"$PACE" run --no-wait -- "$GT" exit 0 >/dev/null 2>"$W/out/fo-full.err"
RC=$?
expect_rc "failopen-full-nowait-refused-status" "$RC" 200
expect_line "failopen-full-nowait-refused" "$W/out/fo-full.err" REFUSED reason=failopen_full
if hint_after "$W/out/fo-full.err" REFUSED; then pass "failopen-full-nowait-hint"; else fail "failopen-full-nowait-hint" "no hint after REFUSED"; fi
expect_exit_last "failopen-full-nowait-exit-line" "$W/out/fo-full.err" 200 pace
"$PACE" run -- "$GT" exit 3 >/dev/null 2>"$W/out/fo-wait.err"
RC=$?
expect_rc "failopen-full-waits-then-runs" "$RC" 3
expect_line "failopen-full-queued" "$W/out/fo-wait.err" QUEUED reason=failopen_full
wait_pid 15 $FA
wait_pid 15 $FB

acc_daemon_start || acc_finish

# ---- 3. exit codes --------------------------------------------------------------------------
for c in 0 1 7 125 255; do
	"$PACE" run -- "$GT" exit $c >/dev/null 2>"$W/out/exit$c.err"
	RC=$?
	expect_rc "exit-$c-passthrough" "$RC" $c
	if grep -q '^pace: EXIT' "$W/out/exit$c.err"; then fail "exit-$c-no-exit-line" "unexpected EXIT line"; else pass "exit-$c-no-exit-line"; fi
done
for c in 200 201 202; do
	"$PACE" run -- "$GT" exit $c >/dev/null 2>"$W/out/exit$c.err"
	RC=$?
	expect_rc "exit-$c-collision-passthrough" "$RC" $c
	expect_exit_last "exit-$c-collision-source-child" "$W/out/exit$c.err" $c child
done
"$PACE" run --mem lots -- "$GT" exit 0 >/dev/null 2>"$W/out/usage.err"
RC=$?
expect_rc "exit-usage-202" "$RC" 202
expect_exit_last "exit-usage-202-source-pace" "$W/out/usage.err" 202 pace
for s in TERM:143 KILL:137 INT:130; do
	sig=${s%%:*}
	want=${s#*:}
	"$PACE" run -- "$GT" exit --signal $sig >/dev/null 2>"$W/out/sig$sig.err"
	RC=$?
	expect_rc "signal-$sig-reraised" "$RC" "$want"
	if grep -q '^pace: ' "$W/out/sig$sig.err"; then fail "signal-$sig-no-pace-lines" "$(cat "$W/out/sig$sig.err")"; else pass "signal-$sig-no-pace-lines"; fi
done
# Killed from outside (not by pace): the wrapper dies of the same signal.
"$PACE" run -- sh -c 'echo $$ >"$2"; exec "$1" hang --for 60s' sh "$GT" "$W/out/ext.pid" >/dev/null 2>"$W/out/ext.err" &
P=$!
track $P
if wait_for 10 test -s "$W/out/ext.pid"; then
	C=$(cat "$W/out/ext.pid")
	sleep 0.3
	kill -KILL "$C"
	wait_pid 10 $P
	expect_rc "external-sigkill-reraised" "$WAIT_RC" 137
	expect_no_line "external-sigkill-not-a-pace-kill" "$W/out/ext.err" KILLED
else
	fail "external-sigkill-reraised" "child did not start"
fi

# ---- 4. stdout / stderr passthrough -------------------------------------------------------------
"$PACE" run -- sh -c 'echo out1; echo err1 >&2; echo out2' >"$W/out/split.out" 2>"$W/out/split.err"
RC=$?
expect_rc "stdio-split-exit0" "$RC" 0
if [ "$(cat "$W/out/split.out")" = "out1
out2" ]; then pass "stdio-stdout-exact"; else fail "stdio-stdout-exact" "stdout: $(cat "$W/out/split.out")"; fi
if [ "$(cat "$W/out/split.err")" = "err1" ]; then pass "stdio-stderr-exact"; else fail "stdio-stderr-exact" "stderr: $(cat "$W/out/split.err")"; fi
printf 'line one\nline two\n' | "$PACE" run -- sh -c 'cat' >"$W/out/stdin.out" 2>/dev/null
if [ "$(cat "$W/out/stdin.out")" = "line one
line two" ]; then pass "stdio-stdin-passthrough"; else fail "stdio-stdin-passthrough" "$(cat "$W/out/stdin.out")"; fi

# 2>&1 into one file while pace emits a WARN mid-stream: the child writes
# every line in two pieces; no pace line may land inside a child line.
"$PACE" run --budget 300M -- sh -c '
	"$1" alloc --mem 285M --print-every 0 >/dev/null 2>&1 &
	a=$!
	i=0
	while [ $i -lt 120 ]; do
		printf "out-%d-" $i; printf "END\n"
		printf "err-%d-" $i >&2; printf "END\n" >&2
		i=$((i + 1))
		sleep 0.02
	done
	kill $a' sh "$GT" >"$W/out/mixed.log" 2>&1
RC=$?
expect_rc "stdio-mixed-exit0" "$RC" 0
expect_line "stdio-mixed-has-warn" "$W/out/mixed.log" WARN policy=mem_budget
if all_pace_lines_whole "$W/out/mixed.log"; then pass "stdio-mixed-pace-lines-whole"; else fail "stdio-mixed-pace-lines-whole" "a pace line is not at a line start or malformed" && dump "$W/out/mixed.log"; fi
NO=$(grep -cE '^out-[0-9]+-END$' "$W/out/mixed.log")
NE=$(grep -cE '^err-[0-9]+-END$' "$W/out/mixed.log")
BAD=$(grep -vE '^(out|err)-[0-9]+-END$|^pace: ' "$W/out/mixed.log" | wc -l | tr -d ' ')
if [ "$NO" = 120 ] && [ "$NE" = 120 ] && [ "$BAD" = 0 ]; then
	pass "stdio-mixed-child-lines-intact"
else
	fail "stdio-mixed-child-lines-intact" "out=$NO err=$NE broken=$BAD"
	grep -vE '^(out|err)-[0-9]+-END$|^pace: ' "$W/out/mixed.log" | head -n 5
fi

acc_finish
