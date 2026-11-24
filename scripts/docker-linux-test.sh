#!/bin/sh
# Run the Linux platform backend tests on a real Linux kernel in Docker.
#
# Cross-compiles the internal/platform test binary (CGO_ENABLED=0) for the
# Docker VM's native architecture and runs it, in the LOCAL image, twice:
#   1. root, --privileged --cgroupns=private --memory=2g: everything, incl.
#      cgroup v2 job containers and the container memory limit;
#   2. uid 1000, unprivileged: what a normal user may do (SetBackground
#      reversibility, uid filtering); cgroup tests skip.
#
# Never pulls an image (--pull=never). Usage:
#   scripts/docker-linux-test.sh [extra go test flags, e.g. -test.run Cgroup]
# Env: IMAGE (default debian:bookworm-slim), MEASURE=1 for cost measurements,
#      MEM (default 2g).
set -eu

IMAGE=${IMAGE:-debian:bookworm-slim}
MEM=${MEM:-2g}
MEASURE=${MEASURE:-0}

cd "$(dirname "$0")/.."

if ! docker info >/dev/null 2>&1; then
	echo "docker-linux-test: docker daemon is not reachable" >&2
	exit 2
fi
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
	echo "docker-linux-test: image $IMAGE is not present locally (this script never pulls)" >&2
	exit 2
fi
IMAGE_ARCH=$(docker image inspect -f '{{.Architecture}}' "$IMAGE")
# Build for the Docker VM's native architecture, not the image's: the test
# binary is static (CGO_ENABLED=0), so an arm64 binary runs natively inside an
# amd64 image on Apple silicon. Under emulation (Rosetta/qemu) every process'
# exe is the emulator and its translation cache adds ~40 MiB of anonymous
# memory per process, which ruins the footprint measurements.
case "$(docker info -f '{{.Architecture}}')" in
aarch64 | arm64) ARCH=arm64 ;;
x86_64 | amd64) ARCH=amd64 ;;
*)
	echo "docker-linux-test: unsupported docker host architecture" >&2
	exit 2
	;;
esac

case "$MEM" in
*g) LIMIT=$((${MEM%g} * 1024 * 1024 * 1024)) ;;
*m) LIMIT=$((${MEM%m} * 1024 * 1024)) ;;
*) LIMIT=$MEM ;;
esac

TMP=$(mktemp -d /tmp/pace-lt.XXXXXX)
trap 'rm -rf "$TMP"' EXIT INT TERM

echo "== building linux/$ARCH test binary"
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go test -c -o "$TMP/platform.test" ./internal/platform/
chmod 0755 "$TMP" "$TMP/platform.test"

run() {
	docker run --rm --pull=never --platform "linux/$IMAGE_ARCH" --init \
		--cgroupns=private --memory="$MEM" \
		-e PACE_LINUX_IN_DOCKER=1 -e PACE_LINUX_EXPECT_MEM_LIMIT="$LIMIT" -e PACE_LINUX_MEASURE="$MEASURE" \
		-v "$TMP:/t:ro" -v "$PWD/internal/platform:/pkg:ro" -w /pkg "$@"
}

status=0
echo "== root, privileged, private cgroup namespace"
run --privileged -e PACE_LINUX_REQUIRE_CGROUP=1 "$IMAGE" \
	/t/platform.test -test.v -test.count=1 -test.timeout=10m "$@" || status=1

echo "== uid 1000, unprivileged"
run --user 1000:1000 -e HOME=/tmp "$IMAGE" \
	/t/platform.test -test.v -test.count=1 -test.timeout=10m "$@" || status=1

if [ "$status" -eq 0 ]; then
	echo "== docker-linux-test: PASS"
else
	echo "== docker-linux-test: FAIL" >&2
fi
exit "$status"
