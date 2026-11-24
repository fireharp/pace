#!/bin/sh
# Run from the repository root on a Mac with the Go and Swift toolchains.
set -eu
tag=${1:?usage: package-release.sh vX.Y.Z output-directory}
out=${2:?missing output directory}
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for os in darwin linux; do
  for arch in arm64 amd64; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      go build -trimpath -ldflags="-s -w -X main.version=$tag" -o "$work/pace" ./cmd/pace
    tar -czf "$out/pace_${os}_${arch}.tar.gz" -C "$work" pace -C "$PWD" THIRD-PARTY-NOTICES.txt
  done
done

mkdir "$work/PaceBar"
PACEBAR_VERSION=${tag#v} sh tray/macos/build.sh "$work/PaceBar/PaceBar.app"
codesign --verify --strict "$work/PaceBar/PaceBar.app"
cp tray/macos/install.sh "$work/PaceBar/"
(cd "$work" && ditto -c -k --keepParent PaceBar "$out/PaceBar-macOS.zip")
cp scripts/install.sh "$out/"
(cd "$out" && shasum -a 256 pace_*.tar.gz PaceBar-macOS.zip install.sh > checksums.txt)
