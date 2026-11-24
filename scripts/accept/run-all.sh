#!/bin/sh
# Run every acceptance scenario group serially (M0-M4 on this machine).
# Exit 0 only when every check passed.
#
#   scripts/accept/run-all.sh            # all groups
#   scripts/accept/run-all.sh m0 m2      # selected groups
#
# Env: ACC_KEEP=1 keeps the work directory; ACC_BIN reuses prebuilt binaries.
set -u
DIR=$(cd "$(dirname "$0")" && pwd)
REPO=$(cd "$DIR/../.." && pwd)

ACC_ALL_GROUPS="m0 overload pressure m2 robust m4"
ACC_GROUPS=${*:-$ACC_ALL_GROUPS}

ACC_WORK=$(mktemp -d "${TMPDIR:-/tmp}/pace-accept.XXXXXX") || exit 2
ACC_WORK=$(cd "$ACC_WORK" && pwd -P) || exit 2
export ACC_WORK
if [ -z "${ACC_BIN:-}" ]; then
	ACC_BIN=$ACC_WORK/bin
	echo "building pace and pacetest into $ACC_BIN"
	(cd "$REPO" && CGO_ENABLED=0 go build -o "$ACC_BIN/" ./cmd/pace ./cmd/pacetest) || exit 2
fi
export ACC_BIN

T0=$(date +%s)
FAILED=
TOTAL_PASS=0
TOTAL_FAIL=0
for g in $ACC_GROUPS; do
	s=$DIR/$g.sh
	if [ ! -f "$s" ]; then
		echo "run-all: no such group: $g" >&2
		FAILED="$FAILED $g"
		continue
	fi
	# Each group runs in its own process group (job control), so nothing a
	# group starts can share a process group with this runner or its caller.
	set -m
	sh "$s" &
	pid=$!
	set +m
	# Watchdog: a hung group is stopped (its whole process group is ours)
	# and counts as failed.
	(
		i=0
		while kill -0 "$pid" 2>/dev/null; do
			if [ $i -ge "${ACC_GROUP_TIMEOUT:-300}" ]; then
				echo "FAIL $g: timed out after ${i}s" >&2
				kill -TERM -- "-$pid" 2>/dev/null
				sleep 3
				kill -KILL -- "-$pid" 2>/dev/null
				exit 0
			fi
			sleep 1
			i=$((i + 1))
		done
	) &
	wd=$!
	wait "$pid"
	rc=$?
	wait "$wd" 2>/dev/null
	p=$(grep -c '^PASS ' "$ACC_WORK/$g/results" 2>/dev/null)
	f=$(grep -c '^FAIL ' "$ACC_WORK/$g/results" 2>/dev/null)
	TOTAL_PASS=$((TOTAL_PASS + ${p:-0}))
	TOTAL_FAIL=$((TOTAL_FAIL + ${f:-0}))
	if [ "$rc" -ne 0 ]; then
		FAILED="$FAILED $g"
		[ "${f:-0}" -eq 0 ] && TOTAL_FAIL=$((TOTAL_FAIL + 1)) && echo "FAIL $g: exited $rc"
	fi
done
DT=$(($(date +%s) - T0))

echo
echo "==== acceptance summary"
for g in $ACC_GROUPS; do
	[ -f "$ACC_WORK/$g/results" ] || continue
	printf '  %-10s pass=%s fail=%s\n' "$g" "$(grep -c '^PASS ' "$ACC_WORK/$g/results")" "$(grep -c '^FAIL ' "$ACC_WORK/$g/results")"
done
[ -f "$ACC_WORK/overload/max-usage" ] && echo "  $(cat "$ACC_WORK/overload/max-usage")"
echo "  total pass=$TOTAL_PASS fail=$TOTAL_FAIL time=${DT}s"

# Nothing we started may outlive the run.
sleep 0.5
LEFT=$(pgrep -f "$ACC_BIN/" 2>/dev/null | tr '\n' ' ')
if [ -n "$LEFT" ]; then
	echo "FAIL leftover processes: $LEFT"
	pkill -KILL -f "$ACC_BIN/" 2>/dev/null
	FAILED="$FAILED leftovers"
fi

if [ -n "$FAILED" ]; then
	echo "RESULT: FAIL ($FAILED ) - work dir kept: $ACC_WORK"
	exit 1
fi
echo "RESULT: PASS"
[ -z "${ACC_KEEP:-}" ] && rm -rf "$ACC_WORK"
exit 0
