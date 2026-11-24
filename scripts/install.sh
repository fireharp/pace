#!/bin/sh
# Install the current user's Pace CLI. Start the daemon with pace install.
set -eu

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "Pace supports macOS and Linux." >&2; exit 1 ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=amd64 ;;
  *) echo "Pace supports arm64 and amd64." >&2; exit 1 ;;
esac

base=https://github.com/fireharp/pace/releases
case "${PACE_VERSION:-latest}" in
  latest) base=$base/latest/download ;;
  *) base=$base/download/$PACE_VERSION ;;
esac
archive=pace_${os}_${arch}.tar.gz
dest=${PACE_INSTALL_DIR:-$HOME/.local/bin}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

(
  cd "$tmp"
  awk -v name="$archive" '$2 == name { print; found++ } END { if (found != 1) exit 1 }' checksums.txt > selected.sha256
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c selected.sha256
  else
    shasum -a 256 -c selected.sha256
  fi
  tar -xzf "$archive" pace
)
mkdir -p "$dest"
staged=$(mktemp "$dest/.pace.XXXXXX")
trap 'rm -rf "$tmp"; rm -f "$staged"' EXIT
install -m 755 "$tmp/pace" "$staged"
mv -f "$staged" "$dest/pace"
echo "Installed $dest/pace"
