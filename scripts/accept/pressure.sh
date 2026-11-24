#!/bin/sh
# Pressure ladder: under warn pressure the lowest-priority job is warned,
# then THROTTLED (and UNTHROTTLED after recovery); if pressure persists it is
# FROZEN (observable: process state T, or on Linux a job container frozen
# with cgroup.freeze) and then KILLED; at critical pressure
# the victim is killed at once. Victim order: lowest priority first, then
# youngest.
#
# Simulated machine: capacity 2G, warn below 50% available (used > 1G),
# critical below 35% (used > 1.3G). Peak real allocation 1.4G.
. "$(dirname "$0")/lib.sh"
acc_init pressure

C_MODE=enforce
C_CAPACITY=2G
C_RESERVE=128M
# These scenarios admit jobs that honestly declare their footprint and
# together push the machine into warn and critical pressure: admission must
# be allowed to hand out memory below the warn line (by default the warn
# line is part of the reserve, so that cannot happen).
C_ADMIT_BELOW_WARN=true
C_WARN_PCT=50.0
C_CRIT_PCT=35.0
C_WARN_DEADLINE=2s
C_STEP=4s
C_CRIT_STEP=500ms
C_RECOVER_HOLD=1s
acc_daemon_start || acc_finish

# job_used_at_least FILE BYTES: the job whose child printed into FILE (its
# pid) uses at least BYTES according to pace status --json.
max_running_used() {
	status_json | sed 's/.*"running":\[//; s/"queued":\[.*//' | grep -o '"used_bytes":[0-9]*' | cut -d: -f2 | sort -n | tail -n 1
}
used_at_least() { _m=$(max_running_used) && [ -n "$_m" ] && [ "$_m" -ge "$1" ]; }
total_used_at_least() { _m=$(scoped_used) && [ "$_m" -ge "$1" ]; }

# start_job NAME PRIORITY MEM ALLOC: wrapped job holding ALLOC forever.
start_job() {
	"$PACE" run --priority "$2" --mem "$3" -- "$GT" alloc --mem "$4" --print-every 0 \
		>"$W/out/$1.out" 2>"$W/out/$1.err" &
	eval "JP_$1=$!"
	track $!
}
jp() { eval "echo \$JP_$1"; }
child_of() { pid_from "$W/out/$1.out" alloc; }

stop_job() {
	_p=$(jp "$1")
	kill -TERM "$_p" 2>/dev/null
	wait_pid 10 "$_p" || kill -KILL "$_p" 2>/dev/null
}

wait_normal() { wait_for 20 pressure_is normal; }

# ---- 1. throttle at warn, unthrottle on recovery ------------------------------
start_job th_high 8 550M 500M
wait_for 15 total_used_at_least $((480 * 1048576))
start_job th_low 1 650M 600M
if wait_for 15 has_line "$W/out/th_low.err" THROTTLED policy=pressure; then
	pass "throttle-low-priority-throttled"
else
	fail "throttle-low-priority-throttled" "no THROTTLED for the low-priority job"
	dump "$W/out/th_low.err"
	status_json
fi
expect_line "throttle-warn-first" "$W/out/th_low.err" WARN policy=pressure level=warn used= free= next=throttle in=2s job=
if hint_after "$W/out/th_low.err" WARN; then pass "throttle-warn-hint"; else fail "throttle-warn-hint" "no hint after pressure WARN"; fi
expect_line "throttle-line" "$W/out/th_low.err" THROTTLED policy=pressure level=warn next=freeze in=4s job=
expect_no_line "throttle-high-priority-untouched" "$W/out/th_high.err" THROTTLED
expect_no_line "throttle-high-priority-not-warned" "$W/out/th_high.err" WARN
# Relieve the pressure: the high-priority job finishes.
stop_job th_high
if wait_for 15 has_line "$W/out/th_low.err" UNTHROTTLED policy=pressure; then
	pass "throttle-unthrottled-after-recovery"
	acc_log "$(pace_line "$W/out/th_low.err" UNTHROTTLED)"
else
	fail "throttle-unthrottled-after-recovery" "no UNTHROTTLED"
	dump "$W/out/th_low.err"
fi
expect_line "throttle-ok-after-recovery" "$W/out/th_low.err" OK policy=pressure job=
expect_no_line "throttle-not-frozen" "$W/out/th_low.err" FROZEN
if alive "$(jp th_low)"; then pass "throttle-low-job-survives"; else fail "throttle-low-job-survives" "low job died"; fi
stop_job th_low
wait_normal

# ---- 2. sustained warn: throttle -> freeze (state T) -> kill -------------------------
start_job fz_high 8 550M 500M
wait_for 15 total_used_at_least $((480 * 1048576))
start_job fz_low 1 650M 600M
if wait_for 20 has_line "$W/out/fz_low.err" FROZEN policy=pressure; then
	pass "freeze-frozen"

	C=$(child_of fz_low)
	ST=$(freeze_state "$C")
	if [ "$ST" = T ]; then pass "freeze-process-state-T"; else fail "freeze-process-state-T" "child $C state '$ST'"; fi
else
	fail "freeze-frozen" "no FROZEN line"
	dump "$W/out/fz_low.err"
fi
expect_line "freeze-line" "$W/out/fz_low.err" FROZEN policy=pressure next=kill in=4s job=
P=$(jp fz_low)
wait_pid 30 "$P"
expect_killed "freeze-then-kill" "$W/out/fz_low.err" "$WAIT_RC" pressure
# order on the job's stderr: WARN, THROTTLED, FROZEN, KILLED
ORDER=$(grep -o '^pace: [A-Z]*' "$W/out/fz_low.err" | sed 's/pace: //' | grep -E '^(WARN|THROTTLED|FROZEN|KILLED)$' | tr '\n' ' ')
if [ "$ORDER" = "WARN THROTTLED FROZEN KILLED " ]; then pass "freeze-ladder-order"; else fail "freeze-ladder-order" "got '$ORDER'"; fi
expect_no_line "freeze-high-priority-untouched" "$W/out/fz_high.err" KILLED
if alive "$(jp fz_high)"; then pass "freeze-high-job-survives"; else fail "freeze-high-job-survives" "high job died"; fi
stop_job fz_high
wait_normal

# ---- 3. critical: lowest priority is killed at once ------------------------------------
start_job v1_a 3 500M 450M
wait_for 15 total_used_at_least $((430 * 1048576))
start_job v1_b 6 500M 450M
wait_for 15 total_used_at_least $((880 * 1048576))
start_job v1_d 9 550M 500M
P=$(jp v1_a)
wait_pid 20 "$P"
expect_killed "critical-lowest-priority" "$W/out/v1_a.err" "$WAIT_RC" pressure level=critical
expect_line "critical-warn-with-kill" "$W/out/v1_a.err" WARN policy=pressure level=critical next=kill in=0ms
expect_no_line "critical-b-survives" "$W/out/v1_b.err" KILLED
expect_no_line "critical-d-survives" "$W/out/v1_d.err" KILLED
stop_job v1_a
stop_job v1_b
stop_job v1_d
wait_normal

# ---- 4. critical, equal priority: the youngest is killed ---------------------------------
start_job v2_old 6 500M 450M
wait_for 15 total_used_at_least $((430 * 1048576))
sleep 1
start_job v2_young 6 500M 450M
wait_for 15 total_used_at_least $((880 * 1048576))
start_job v2_d 9 550M 500M
P=$(jp v2_young)
wait_pid 20 "$P"
expect_killed "critical-youngest" "$W/out/v2_young.err" "$WAIT_RC" pressure level=critical
expect_no_line "critical-oldest-survives" "$W/out/v2_old.err" KILLED
expect_no_line "critical-d2-survives" "$W/out/v2_d.err" KILLED
stop_job v2_young
stop_job v2_old
stop_job v2_d

acc_finish
