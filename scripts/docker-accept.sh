#!/bin/sh
# Run the whole end-to-end acceptance suite (scripts/accept/run-all.sh) on a
# real Linux kernel in Docker: M3 "the M1 overload script passes unchanged on
# a Linux server", for every group.
#
# Cross-compiles pace and pacetest (CGO_ENABLED=0) for the Docker VM's native
# architecture, mounts the repository read-only and the binaries via ACC_BIN,
# and runs the LOCAL image (never pulls), one mode after the other:
#
#   root  --privileged --cgroupns=private as root: the namespace root is
#         prepared like a systemd Delegate=yes unit (every process moves into
#         a leaf, all controllers are enabled for the subtree), so pace runs
#         every job in its own cgroup v2 container (exact memory, memory.max,
#         cgroup.freeze, cgroup.kill, cpu/io weights);
#   user  --user 1000:1000, unprivileged: no cgroup delegation (cgroupfs is
#         read-only), so every fallback path runs (process sums, SIGSTOP,
#         sched/ioprio throttle).
#
# Each container gets --memory (default 5g: m4 simulates a 4G machine and a
# capacity override may not exceed the container's limit; the suite itself
# allocates at most 2G at any time). `sh` inside is bash --posix
# (as /bin/sh is on macOS): the suite relies on `set -m` giving every group,
# and every process it starts on purpose outside the wrapper, a process
# group of its own. Debian's dash turns job control off without a tty, and
# with one it stops a background script that turns it on (SIGTTIN), so it
# cannot run the suite as a developer machine does; bash creates the groups
# without a terminal.
#
# bookworm-slim has no procps: minimal ps/pgrep/pkill replacements that read
# /proc (only what the suite uses) are put first on PATH inside the
# container.
#
# Usage: scripts/docker-accept.sh [group...]     (default: all groups)
# Env:   MODES (default "root user"), IMAGE (default debian:bookworm-slim),
#        MEM (default 5g), ACC_GROUP_TIMEOUT (passed through), DA_KEEP=1
#        keeps the temporary directory (binaries, shims, per-mode logs).
# Exit 0 only when every mode passed.
set -eu

IMAGE=${IMAGE:-debian:bookworm-slim}
MEM=${MEM:-5g}
MODES=${MODES:-root user}

cd "$(dirname "$0")/.."
REPO=$(pwd -P)

if ! docker info >/dev/null 2>&1; then
	echo "docker-accept: docker daemon is not reachable" >&2
	exit 2
fi
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	echo "docker-accept: image $IMAGE is not present locally (this script never pulls)" >&2
	exit 2
fi
IMAGE_ARCH=$(docker image inspect -f '{{.Architecture}}' "$IMAGE")
# Static binaries for the VM's own architecture run natively in any image
# (see scripts/docker-linux-test.sh for why emulation would skew footprints).
case "$(docker info -f '{{.Architecture}}')" in
aarch64 | arm64) ARCH=arm64 ;;
x86_64 | amd64) ARCH=amd64 ;;
*)
	echo "docker-accept: unsupported docker host architecture" >&2
	exit 2
	;;
esac

TMP=$(mktemp -d /tmp/pace-da.XXXXXX)
if [ -n "${DA_KEEP:-}" ]; then
	echo "docker-accept: keeping $TMP (binaries, shims, logs)"
else
	trap 'rm -rf "$TMP"' EXIT INT TERM
fi
mkdir -p "$TMP/bin" "$TMP/pace-shim" "$TMP/logs"

echo "== building linux/$ARCH pace and pacetest"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -o "$TMP/bin/" ./cmd/pace ./cmd/pacetest

# ---- procps-lite for the container ---------------------------------------
cat >"$TMP/pace-shim/ps" <<'EOF'
#!/bin/sh
# ps -o FIELD= -p PID (FIELD: state pgid ppid pid comm), as the suite uses it.
f=
p=
while [ $# -gt 0 ]; do
	case $1 in
	-o) f=${2%=}; shift 2 ;;
	-p) p=$2; shift 2 ;;
	*) shift ;;
	esac
done
[ -n "$p" ] && [ -r "/proc/$p/stat" ] || exit 1
s=$(cat "/proc/$p/stat" 2>/dev/null) || exit 1
c=${s#*(}
c=${c%)*}
r=${s##*) }
set -- $r
case $f in
state) echo "$1" ;;
ppid) echo "$2" ;;
pgid) echo "$3" ;;
pid) echo "$p" ;;
comm) echo "$c" ;;
*) exit 1 ;;
esac
EOF
cat >"$TMP/pace-shim/pgrep" <<'EOF'
#!/bin/sh
# pgrep [-f] PATTERN: pids whose command line matches PATTERN (ERE). The
# pattern reaches awk through the environment: as an argument it would
# match awk itself.
pat=
for a in "$@"; do
	case $a in -f) ;; *) pat=$a ;; esac
done
[ -n "$pat" ] || exit 2
for d in /proc/[0-9]*; do
	pid=${d#/proc/}
	[ "$pid" = $$ ] && continue
	c=$(tr '\0' ' ' <"$d/cmdline" 2>/dev/null) || continue
	case $c in "" | */pace-shim/*) continue ;; esac
	printf '%s %s\n' "$pid" "$c"
done | PAT=$pat awk '{ l = $0; sub(/^[0-9]+ /, "", l); if (l ~ ENVIRON["PAT"]) { print $1; n++ } } END { exit n ? 0 : 1 }'
EOF
cat >"$TMP/pace-shim/sh" <<'EOF'
#!/bin/bash
exec /bin/bash --posix "$@"
EOF
cat >"$TMP/pace-shim/pkill" <<'EOF'
#!/bin/sh
# pkill -SIG -f PATTERN
sig=TERM
pat=
for a in "$@"; do
	case $a in -f) ;; -*) sig=${a#-} ;; *) pat=$a ;; esac
done
[ -n "$pat" ] || exit 2
pids=$(/pace-shim/pgrep -f "$pat") || exit 1
for p in $pids; do kill -"$sig" "$p" 2>/dev/null; done
exit 0
EOF
chmod 0755 "$TMP" "$TMP/bin" "$TMP/pace-shim" "$TMP/bin/"* "$TMP/pace-shim/"*

# Prepares the namespace root like a delegated systemd unit (root mode only),
# then runs the suite.
cat >"$TMP/entry.sh" <<'EOF'
#!/bin/sh
cg=/sys/fs/cgroup
if [ "$(id -u)" = 0 ] && [ -w "$cg/cgroup.procs" ] && ! grep -qw memory "$cg/cgroup.subtree_control"; then
	mkdir -p "$cg/init"
	for p in $(cat "$cg/cgroup.procs"); do echo "$p" >"$cg/init/cgroup.procs" 2>/dev/null; done
	for c in $(cat "$cg/cgroup.controllers"); do echo "+$c" >"$cg/cgroup.subtree_control" 2>/dev/null; done
	echo "docker-accept: cgroup root prepared: subtree_control=$(cat "$cg/cgroup.subtree_control")"
fi
echo "docker-accept: uid=$(id -u) kernel=$(uname -r) pgid=$(ps -o pgid= -p $$)"
exec sh /src/scripts/accept/run-all.sh "$@"
EOF
chmod 0755 "$TMP/entry.sh"

status=0
SUMMARY=
for mode in $MODES; do
	case $mode in
	root) extra="--privileged --cgroupns=private" ;;
	user) extra="--user 1000:1000 -e HOME=/tmp" ;;
	*)
		echo "docker-accept: unknown mode $mode" >&2
		exit 2
		;;
	esac
	echo "== mode $mode ($extra)"
	log=$TMP/logs/$mode.log
	rc=0
	# shellcheck disable=SC2086
	docker run --rm --pull=never --platform "linux/$IMAGE_ARCH" --init \
		--memory="$MEM" $extra \
		-e ACC_BIN=/acc/bin -e PATH=/pace-shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
		-e "ACC_GROUP_TIMEOUT=${ACC_GROUP_TIMEOUT:-300}" \
		-v "$REPO:/src:ro" -v "$TMP/bin:/acc/bin:ro" -v "$TMP/pace-shim:/pace-shim:ro" -v "$TMP/entry.sh:/entry.sh:ro" \
		-w /src "$IMAGE" sh /entry.sh "$@" 2>&1 | tee "$log" || rc=1
	if ! grep -q '^RESULT: PASS' "$log"; then
		rc=1
	fi
	[ "$rc" -eq 0 ] || status=1
	SUMMARY="$SUMMARY
-- $mode: $(grep -E '^RESULT:' "$log" | tail -n 1)
$(sed -n '/^==== acceptance summary/,/total pass=/p' "$log" | sed '1d')"
done

echo
echo "==== docker-accept summary ($IMAGE, linux/$ARCH binaries, --memory=$MEM)"
echo "$SUMMARY"
exit "$status"
