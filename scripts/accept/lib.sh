# shellcheck shell=sh
# lib.sh: shared helpers for the pace end-to-end acceptance scripts.
#
# POSIX sh only (runs unchanged on macOS and Linux). Source it from a
# scenario script in scripts/accept/:
#
#	. "$(dirname "$0")/lib.sh"
#	acc_init m0
#	C_MODE=observe acc_daemon_start
#	...
#	acc_finish
#
# Safety model (this runs on real machines):
#   - every daemon gets an isolated PACE_HOME under the work dir, a socket in a
#     short mktemp -d /tmp/pace.XXXX dir, a capacity override,
#     use_kernel_level=false and [unmanaged] scope_root_pid = $$ (the
#     scenario shell), so only processes this script started can ever be
#     governed or signalled;
#   - every process started from the private binary directory ($ACC_BIN) is
#     killed on exit (they all carry that path in their command line, so no
#     foreign process can match);
#   - the scenarios keep every allocation <= 1.5G per process and <= 2G total.
#
# Environment:
#   ACC_WORK  work directory (default: a new mktemp -d dir, removed on success)
#   ACC_BIN   directory with prebuilt pace and pacetest (default: build into
#             $ACC_WORK/bin from this repository)
#   ACC_KEEP  non-empty: keep the work directory even when everything passed
#   ACC_VERBOSE non-empty: print daemon logs on failure (default on)

set -u

ACC_DIR=$(cd "$(dirname "$0")" && pwd)
ACC_REPO=$(cd "$ACC_DIR/../.." && pwd)

# A governed parent must not turn our pace run calls into pass-throughs, and
# stray overrides must not leak into the isolated daemons.
# Every PACE_* and the legacy GOV_* names (envcompat falls back to them).
for _v in $(env | sed -n 's/^\(\(PACE\|GOV\)_[A-Za-z0-9_]*\)=.*/\1/p'); do
	unset "$_v" 2>/dev/null || true
done
unset _v

ACC_FAILS=0
ACC_PASSES=0
ACC_NAME=
W=
PACE=
GT=
SOCKDIR=
DAEMON_PID=
ACC_OWN_WORK=

# ---- output ----------------------------------------------------------------

acc_log() { printf '%s\n' "  .. $*"; }

pass() {
	ACC_PASSES=$((ACC_PASSES + 1))
	printf 'PASS %s\n' "$1"
	printf 'PASS %s\n' "$1" >>"$W/results"
}

# fail NAME DETAIL...
fail() {
	_n=$1
	shift
	ACC_FAILS=$((ACC_FAILS + 1))
	printf 'FAIL %s: %s\n' "$_n" "$*"
	printf 'FAIL %s: %s\n' "$_n" "$*" >>"$W/results"
}

# check NAME CMD...: PASS when CMD succeeds.
check() {
	_cn=$1
	shift
	if "$@"; then pass "$_cn"; else fail "$_cn" "command failed: $*"; fi
}

# dump FILE: show a file indented (for failure context).
dump() {
	[ -f "$1" ] || {
		echo "    (no $1)"
		return 0
	}
	echo "    --- $1 (last 25 lines)"
	tail -n 25 "$1" | sed 's/^/    | /'
}

# ---- time --------------------------------------------------------------------

now_s() { date +%s; }

# ---- lifecycle ---------------------------------------------------------------

# acc_init NAME: work dir, binaries, trap.
acc_init() {
	ACC_NAME=$1
	if [ -z "${ACC_WORK:-}" ]; then
		ACC_WORK=$(mktemp -d "${TMPDIR:-/tmp}/pace-accept.XXXXXX") || exit 2
		ACC_OWN_WORK=1
	fi
	# Physical path: pace canonicalises the paths it prints and writes.
	ACC_WORK=$(cd "$ACC_WORK" && pwd -P) || exit 2
	W=$ACC_WORK/$ACC_NAME
	rm -rf "$W"
	mkdir -p "$W/home" "$W/out"
	: >"$W/results"
	: >"$W/pids"
	if [ -z "${ACC_BIN:-}" ]; then
		ACC_BIN=$ACC_WORK/bin
		if [ ! -x "$ACC_BIN/pace" ] || [ ! -x "$ACC_BIN/pacetest" ]; then
			acc_build || exit 2
		fi
	fi
	PACE=$ACC_BIN/pace
	GT=$ACC_BIN/pacetest
	SOCKDIR=$(mktemp -d /tmp/pace.XXXX) || exit 2
	export PACE_HOME="$W/home"
	export PACE_SOCK="$SOCKDIR/s"
	export PACE_CONFIG="$W/pace.toml"
	trap 'acc_cleanup' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	ACC_T0=$(now_s)
	echo "== $ACC_NAME (work $W, pid $$)"
}

acc_build() {
	echo "building pace and pacetest into $ACC_BIN"
	mkdir -p "$ACC_BIN"
	(cd "$ACC_REPO" && CGO_ENABLED=0 go build -o "$ACC_BIN/" ./cmd/pace ./cmd/pacetest)
}

# proc_id PID: a stable identity for a live pid (its start time), "" if gone.
proc_id() { ps -o lstart= -p "$1" 2>/dev/null | tr -s ' '; }

# track PID: kill it on exit, if it is still the same process (pid and start
# time), so a pid reused by another process is never signalled.
track() { printf '%s|%s\n' "$1" "$(proc_id "$1")" >>"$W/pids"; }

# signal_tracked SIG: send SIG to every tracked pid that is still the process
# that was tracked.
signal_tracked() {
	[ -f "$W/pids" ] || return 0
	while IFS='|' read -r _p _id; do
		[ -n "$_p" ] || continue
		if [ -n "$_id" ] && [ "$(proc_id "$_p")" != "$_id" ]; then
			continue
		fi
		kill "-$1" "$_p" 2>/dev/null
	done <"$W/pids"
	return 0
}

# kill_ours SIG BIN: signal this user's processes whose argv[0] is our private
# BIN (anchored: a command line that merely mentions the path never matches).
kill_ours() {
	pkill "-$1" -U "$(id -u)" -f "^$ACC_BIN/$2( |\$)" 2>/dev/null
	return 0
}

# kill_tree_of_ours: TERM then KILL every tracked pid and every process
# running one of our private binaries.
acc_kill_ours() {
	signal_tracked TERM
	kill_ours TERM pacetest
	kill_ours TERM pace
	sleep 0.5
	signal_tracked CONT
	kill_ours CONT pacetest
	kill_ours KILL pacetest
	kill_ours KILL pace
	signal_tracked KILL
	return 0
}

acc_cleanup() {
	_rc=$?
	trap - EXIT
	acc_daemon_stop >/dev/null 2>&1
	[ -n "$PACE" ] && acc_kill_ours
	[ -n "$SOCKDIR" ] && rm -rf "$SOCKDIR"
	if [ -n "$ACC_OWN_WORK" ] && [ "$ACC_FAILS" -eq 0 ] && [ -z "${ACC_KEEP:-}" ] && [ "$_rc" -eq 0 ]; then
		rm -rf "$ACC_WORK"
	fi
	exit "$_rc"
}

# acc_finish: summary line and exit status.
acc_finish() {
	_dt=$(($(now_s) - ACC_T0))
	echo "SUMMARY $ACC_NAME pass=$ACC_PASSES fail=$ACC_FAILS time=${_dt}s"
	if [ "$ACC_FAILS" -gt 0 ]; then
		echo "  (work dir kept: $W)"
		exit 1
	fi
	exit 0
}

# ---- config and daemon -------------------------------------------------------

# Config knobs (override before acc_daemon_start / acc_config).
C_MODE=enforce
C_CAPACITY=4G
C_RESERVE=128M
C_ADMIT_BELOW_WARN=false
C_WARN_PCT=15.0
C_CRIT_PCT=7.0
C_SAMPLE=250ms
C_FAST=250ms
C_WARN_DEADLINE=3s
C_GRACE=1s
C_CRIT_GRACE=500ms
C_STEP=500ms
C_CRIT_STEP=500ms
C_RECOVER_HOLD=1s
C_ORPHAN_GRACE=2s
C_TOLERANCE=10.0
C_MIN_VICTIM=64M
C_QUEUE_TIMEOUT=2m
C_FAILOPEN_SLOTS=2
C_FAILOPEN_WAIT=1s
# Empty C_CAPACITY: no override, the daemon uses the real machine (or the
# container's memory.max). C_USE_KERNEL folds in the kernel level / PSI.
C_USE_KERNEL=false
# [unmanaged]: C_PROTECT is the inside of a TOML array ('"a", "b"');
# C_UNMANAGED_BUDGET empty = the default (none).
C_PROTECT=
C_UNMANAGED_PRIORITY=2
C_UNMANAGED_BUDGET=
# [cpu] and [disk]; C_DISK_PATHS is the inside of a TOML array, empty = default.
C_CPU_WARN=1000.0
C_CPU_OK=999.0
C_DISK_WARN=512M
C_DISK_CRIT=128M
C_DISK_PATHS=
C_EXTRA=

# acc_config: print the test config. scope_root_pid is always this shell: the
# test daemon may only ever act on processes this script started.
acc_config() {
	case $$ in '' | 0 | 1 | *[!0-9]*)
		echo "acc_config: refusing scope root pid '$$'" >&2
		return 1
		;;
	esac
	_cap_line="capacity = \"$C_CAPACITY\""
	[ -n "$C_CAPACITY" ] || _cap_line="# capacity: real (no override)"
	_ub_line="budget = \"$C_UNMANAGED_BUDGET\""
	[ -n "$C_UNMANAGED_BUDGET" ] || _ub_line="# budget: default"
	_dp_line="paths = [$C_DISK_PATHS]"
	[ -n "$C_DISK_PATHS" ] || _dp_line="# paths: default"
	cat <<EOF
# generated by scripts/accept/lib.sh for $ACC_NAME
mode = "$C_MODE"
$_cap_line
reserve = "$C_RESERVE"
admit_below_warn = $C_ADMIT_BELOW_WARN
use_kernel_level = $C_USE_KERNEL
sample_interval = "$C_SAMPLE"
fast_interval = "$C_FAST"

[pressure]
warn_avail_pct = $C_WARN_PCT
crit_avail_pct = $C_CRIT_PCT

[enforce]
warn_deadline = "$C_WARN_DEADLINE"
grace = "$C_GRACE"
critical_grace = "$C_CRIT_GRACE"
step = "$C_STEP"
critical_step = "$C_CRIT_STEP"
recover_hold = "$C_RECOVER_HOLD"
orphan_grace = "$C_ORPHAN_GRACE"
tolerance_pct = $C_TOLERANCE
min_victim = "$C_MIN_VICTIM"

[unmanaged]
scope_root_pid = $$
priority = $C_UNMANAGED_PRIORITY
protect = [$C_PROTECT]
$_ub_line

[queue]
timeout = "$C_QUEUE_TIMEOUT"

[cpu]
# by default the host's own load must not throttle test jobs
warn_load_per_core = $C_CPU_WARN
ok_load_per_core = $C_CPU_OK

[disk]
warn_free = "$C_DISK_WARN"
crit_free = "$C_DISK_CRIT"
$_dp_line

[failopen]
slots = $C_FAILOPEN_SLOTS
wait = "$C_FAILOPEN_WAIT"

[log]
heartbeat = "5s"

$C_EXTRA
EOF
}

# daemon_up: the daemon answers with a real sample (not the boot snapshot).
daemon_up() {
	_du=$("$PACE" status --json 2>/dev/null) || return 1
	case $_du in *'"no sample yet"'*) return 1 ;; esac
	return 0
}

# acc_daemon_start: write the config and start a daemon; wait until it answers.
acc_daemon_start() {
	if ! acc_config >"$PACE_CONFIG"; then
		fail "$ACC_NAME-config" "acc_config refused"
		return 1
	fi
	if ! "$PACE" config check >"$W/config-check.out" 2>&1; then
		fail "$ACC_NAME-config" "pace config check failed: $(cat "$W/config-check.out")"
		return 1
	fi
	"$PACE" daemon >>"$W/daemon.log" 2>&1 &
	DAEMON_PID=$!
	track "$DAEMON_PID"
	if ! wait_for 10 daemon_up; then
		fail "$ACC_NAME-daemon-start" "daemon did not answer within 10s"
		dump "$W/daemon.log"
		return 1
	fi
	return 0
}

# acc_daemon_stop: SIGTERM the daemon and wait for it.
acc_daemon_stop() {
	[ -n "$DAEMON_PID" ] || return 0
	kill -TERM "$DAEMON_PID" 2>/dev/null
	_i=0
	while kill -0 "$DAEMON_PID" 2>/dev/null && [ $_i -lt 50 ]; do
		sleep 0.1
		_i=$((_i + 1))
	done
	kill -KILL "$DAEMON_PID" 2>/dev/null
	wait "$DAEMON_PID" 2>/dev/null
	DAEMON_PID=
	return 0
}

# acc_daemon_kill9: crash the daemon.
acc_daemon_kill9() {
	[ -n "$DAEMON_PID" ] || return 0
	kill -KILL "$DAEMON_PID" 2>/dev/null
	wait "$DAEMON_PID" 2>/dev/null
	DAEMON_PID=
}

# acc_daemon_restart [knobs already changed]: stop, rewrite config, start.
acc_daemon_restart() {
	acc_daemon_stop
	acc_daemon_start
}

# ---- waiting -----------------------------------------------------------------

# wait_for TIMEOUT_S CMD...: poll CMD every 0.2s until it succeeds.
wait_for() {
	_wt=$1
	shift
	_we=$(($(now_s) + _wt))
	while :; do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		[ "$(now_s)" -ge "$_we" ] && return 1
		sleep 0.2
	done
}

# wait_pid TIMEOUT_S PID: wait for a child of this shell, set WAIT_RC.
# Returns 1 on timeout (the process is then still running).
wait_pid() {
	_wt=$1
	_wp=$2
	_we=$(($(now_s) + _wt))
	while kill -0 "$_wp" 2>/dev/null; do
		if [ "$(now_s)" -ge "$_we" ]; then
			WAIT_RC=timeout
			return 1
		fi
		sleep 0.1
	done
	wait "$_wp" 2>/dev/null
	WAIT_RC=$?
	return 0
}

alive() { kill -0 "$1" 2>/dev/null; }
dead() { ! kill -0 "$1" 2>/dev/null; }

# proc_state PID: one-letter process state (R, S, T, Z, ...), "" if gone.
proc_state() { ps -o state= -p "$1" 2>/dev/null | tr -d ' ' | cut -c1; }

# freeze_state PID: "T" when PID is frozen: stopped (SIGSTOP), or, on Linux,
# in a cgroup v2 job container frozen with cgroup.freeze (the process itself
# then shows state S); else its state letter.
freeze_state() {
	_fs=$(proc_state "$1")
	if [ "$_fs" != T ] && [ -r "/proc/$1/cgroup" ]; then
		_fc=$(sed -n 's/^0:://p' "/proc/$1/cgroup" 2>/dev/null)
		if [ -n "$_fc" ] && grep -qx 'frozen 1' "/sys/fs/cgroup$_fc/cgroup.events" 2>/dev/null; then
			_fs=T
		fi
	fi
	echo "$_fs"
}

# ---- pace line assertions -----------------------------------------------------

# pace_line FILE VERB [TOKEN...]: print the first "pace: VERB ..." line of FILE
# that carries every TOKEN. A TOKEN "key=" only requires the key; "key=value"
# requires that exact bare value.
pace_line() {
	_gf=$1
	_gv=$2
	shift 2
	[ -f "$_gf" ] || return 1
	awk -v verb="$_gv" -v toks="$*" '
		BEGIN { n = split(toks, T, " ") }
		{
			pre = "pace: " verb
			if (!(index($0, pre " ") == 1 || $0 == pre)) next
			line = " " substr($0, length(pre) + 1) " "
			ok = 1
			for (i = 1; i <= n; i++) {
				t = T[i]
				if (substr(t, length(t)) == "=") {
					if (index(line, " " t) == 0) ok = 0
				} else if (index(line, " " t " ") == 0) ok = 0
			}
			if (ok) { print; found = 1; exit }
		}
		END { exit found ? 0 : 1 }' "$_gf"
}

has_line() { pace_line "$@" >/dev/null; }

# hint_after FILE VERB: the line right after the first VERB line is a hint.
hint_after() {
	awk -v verb="$2" '
		f { if (index($0, "pace: hint: ") == 1 && length($0) > 11) ok = 1; exit }
		index($0, "pace: " verb " ") == 1 { f = 1 }
		END { exit ok ? 0 : 1 }' "$1"
}

# no_hint_after FILE VERB: no hint line follows the first VERB line.
no_hint_after() { ! hint_after "$@"; }

# exit_last FILE CODE SOURCE: the last line is "pace: EXIT code=CODE source=SOURCE ...".
exit_last() {
	_el=$(tail -n 1 "$1")
	case "$_el" in
	"pace: EXIT code=$2 source=$3" | "pace: EXIT code=$2 source=$3 "*) return 0 ;;
	esac
	return 1
}

# field LINE KEY: the bare value of KEY in a pace line.
field() {
	printf '%s\n' "$1" | tr ' ' '\n' | sed -n "s/^$2=//p" | head -n 1
}

# expect_line NAME FILE VERB TOKEN...: PASS/FAIL on pace_line.
expect_line() {
	_xn=$1
	_xf=$2
	shift 2
	if _xl=$(pace_line "$_xf" "$@"); then
		pass "$_xn"
		acc_log "$_xl"
	else
		fail "$_xn" "no line 'pace: $*' in $_xf"
		dump "$_xf"
	fi
}

# expect_no_line NAME FILE VERB [TOKEN...]
expect_no_line() {
	_xn=$1
	_xf=$2
	shift 2
	if _xl=$(pace_line "$_xf" "$@"); then
		fail "$_xn" "unexpected line: $_xl"
	else
		pass "$_xn"
	fi
}

# expect_rc NAME GOT WANT
expect_rc() {
	if [ "$2" = "$3" ]; then pass "$1"; else fail "$1" "exit status $2, want $3"; fi
}

# expect_exit_last NAME FILE CODE SOURCE
expect_exit_last() {
	if exit_last "$2" "$3" "$4"; then
		pass "$1"
	else
		fail "$1" "last stderr line is '$(tail -n 1 "$2")', want 'pace: EXIT code=$3 source=$4 ...'"
	fi
}

# expect_refused NAME FILE RC REASON: the full refusal contract.
expect_refused() {
	_rn=$1
	_rf=$2
	expect_line "$_rn-line" "$_rf" REFUSED "reason=$4" free= need= job=
	if hint_after "$_rf" REFUSED; then pass "$_rn-hint"; else fail "$_rn-hint" "no hint after REFUSED" && dump "$_rf"; fi
	expect_exit_last "$_rn-exit-line" "$_rf" 200 pace
	expect_rc "$_rn-status" "$3" 200
}

# expect_killed NAME FILE RC POLICY [TOKEN...]: the full kill contract.
expect_killed() {
	_kn=$1
	_kf=$2
	_krc=$3
	_kp=$4
	shift 4
	expect_line "$_kn-line" "$_kf" KILLED "policy=$_kp" job= "$@"
	if [ "$_kp" = orphaned ]; then :; elif hint_after "$_kf" KILLED; then pass "$_kn-hint"; else fail "$_kn-hint" "no hint after KILLED" && dump "$_kf"; fi
	expect_exit_last "$_kn-exit-line" "$_kf" 201 pace
	expect_rc "$_kn-status" "$_krc" 201
}

# all_pace_lines_whole FILE: every "pace: " occurrence starts a line, and
# every pace line parses as VERB key=value...
all_pace_lines_whole() {
	_a=$(grep -c 'pace: ' "$1")
	_b=$(grep -c '^pace: ' "$1")
	[ "$_a" = "$_b" ] || return 1
	! grep '^pace: ' "$1" | grep -v '^pace: hint: ' | grep -Ev '^pace: [A-Z]+( [a-z][a-z0-9_]*=("([^"\\]|\\.)*"|[^ "=]+))*$' >/dev/null
}

# ---- status JSON ---------------------------------------------------------------

# jnum KEY: first integer value of "KEY": in stdin.
jnum() { grep -o "\"$1\":-\{0,1\}[0-9]*" | head -n 1 | cut -d: -f2; }

# jstr KEY: first string value of "KEY": in stdin.
jstr() { grep -o "\"$1\":\"[^\"]*\"" | head -n 1 | cut -d: -f2- | tr -d '"'; }

status_json() { "$PACE" status --json 2>/dev/null; }

# job_json ID: pace status --job ID --json.
job_json() { "$PACE" status --job "$1" --json 2>/dev/null; }

# running_count: number of running jobs in pace status --json.
running_count() {
	_s=$(status_json) || return 1
	printf '%s\n' "$_s" | sed 's/"queued":\[.*//' | grep -o '"state":"running"' | wc -l | tr -d ' '
}

# running_at_least N
running_at_least() { _rc2=$(running_count) && [ "$_rc2" -ge "$1" ]; }

# queued_at_least N
queued_at_least() {
	_s=$(status_json) || return 1
	_q=$(printf '%s\n' "$_s" | grep -o '"state":"queued"' | wc -l | tr -d ' ')
	[ "$_q" -ge "$1" ]
}

# scoped_used: capacity - avail (the scoped usage under a capacity override).
scoped_used() {
	_s=$(status_json) || return 1
	_c=$(printf '%s\n' "$_s" | jnum capacity_bytes)
	_v=$(printf '%s\n' "$_s" | jnum avail_bytes)
	echo $((_c - _v))
}

pressure_is() { _p=$(status_json | jstr pressure) && [ "$_p" = "$1" ]; }

# file_has FILE PATTERN (grep -E)
file_has() { [ -f "$1" ] && grep -E "$2" "$1" >/dev/null 2>&1; }

# alert_has TOKEN...: alerts.jsonl has one JSON line containing every TOKEN
# (literal substrings such as '"verb":"KILLED"').
alert_has() {
	_af="$PACE_HOME/logs/alerts.jsonl"
	[ -f "$_af" ] || return 1
	_ac=$(cat "$_af")
	for _t in "$@"; do
		_ac=$(printf '%s\n' "$_ac" | grep -F -- "$_t") || return 1
	done
	[ -n "$_ac" ]
}

# bytes SIZE (K/M/G binary) -> integer
bytes() {
	case "$1" in
	*G) echo $((${1%G} * 1073741824)) ;;
	*M) echo $((${1%M} * 1048576)) ;;
	*K) echo $((${1%K} * 1024)) ;;
	*) echo "$1" ;;
	esac
}

# mib BYTES -> "123M"
mib() { echo "$(($1 / 1048576))M"; }

# ---- process helpers -------------------------------------------------------------

# pid_from FILE LABEL: pid printed by pacetest as "pacetest: LABEL ... pid=N"
# (or "escape child=N").
pid_from() {
	[ -f "$1" ] || return 1
	_pl=$(grep "^pacetest: $2" "$1" | head -n 1) || return 1
	case "$2" in
	escape) _pv=$(field "$_pl" child) ;;
	*) _pv=$(field "$_pl" pid) ;;
	esac
	[ -n "$_pv" ] && echo "$_pv"
}

# start_own_group VAR CMD...: run CMD in the background in its own process
# group (job control), as a process that bypasses the wrapper. Sets VAR to
# its pid (== its pgid).
start_own_group() {
	_sv=$1
	shift
	bg_own_group "$@"
	_sp=$BG_PID
	track "$_sp"
	eval "$_sv=$_sp"
}

# bg_own_group CMD...: start CMD in the background as the leader of its own
# process group; sets BG_PID. setsid(1) where it exists (Linux: dash cannot
# do job control without a terminal), else job control (macOS sh is bash).
# A background child of a shell without job control is not a group leader,
# so setsid(1) calls setsid() in place and BG_PID is the command's own pid.
bg_own_group() {
	if command -v setsid >/dev/null 2>&1; then
		setsid "$@" &
		BG_PID=$!
	else
		set -m
		"$@" &
		BG_PID=$!
		set +m
	fi
}

# pgid_of PID
pgid_of() { ps -o pgid= -p "$1" 2>/dev/null | tr -d ' '; }

# ---- usage sampler -----------------------------------------------------------------

# sampler_start FILE: record scoped usage every 0.2s into FILE.
sampler_start() {
	_sf=$1
	(
		while :; do
			_u=$(scoped_used 2>/dev/null) && echo "$_u" >>"$_sf"
			sleep 0.2
		done
	) &
	SAMPLER_PID=$!
	track "$SAMPLER_PID"
}

sampler_stop() {
	[ -n "${SAMPLER_PID:-}" ] && kill "$SAMPLER_PID" 2>/dev/null && wait "$SAMPLER_PID" 2>/dev/null
	SAMPLER_PID=
}

# max_of FILE
max_of() { sort -n "$1" 2>/dev/null | tail -n 1; }
