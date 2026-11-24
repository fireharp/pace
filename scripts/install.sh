#!/bin/sh
# Install the current user's Pace CLI. Start the daemon with pace install.
set -eu

usage() { echo "Usage: install.sh [--with-pacebar | --cli-only]"; }
bar=ask
if [ "$#" -gt 1 ]; then usage >&2; exit 2; fi
case "${1:-}" in
  "") ;;
  --with-pacebar) bar=yes ;;
  --cli-only) bar=no ;;
  -h|--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

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

if [ "$os" != darwin ] && [ "$bar" = yes ]; then
  echo "PaceBar requires macOS." >&2
  exit 2
fi

base=https://github.com/fireharp/pace/releases
case "${PACE_VERSION:-latest}" in
  latest) base=$base/latest/download ;;
  *) base=$base/download/$PACE_VERSION ;;
esac
archive=pace_${os}_${arch}.tar.gz
dest=${PACE_INSTALL_DIR:-$HOME/.local/bin}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

download_verified() (
  cd "$tmp"
  curl -fsSL "$base/$1" -o "$1"
  awk -v name="$1" '$2 == name { print; found++ } END { if (found != 1) exit 1 }' checksums.txt > selected.sha256
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum -c selected.sha256
  else
    shasum -a 256 -c selected.sha256
  fi
)
download_verified "$archive"
tar -xzf "$tmp/$archive" -C "$tmp" pace
mkdir -p "$dest"
dest=$(cd "$dest" && pwd)
staged=$(mktemp "$dest/.pace.XXXXXX")
trap 'rm -rf "$tmp"; rm -f "$staged"' EXIT
install -m 755 "$tmp/pace" "$staged"
mv -f "$staged" "$dest/pace"
echo "Installed $dest/pace"

if [ "$os" = darwin ] && [ "$bar" = ask ] && [ -t 1 ] && ( : </dev/tty ) 2>/dev/null; then
  printf 'Install PaceBar in ~/Applications and start it at login? [y/N] ' >/dev/tty
  if IFS= read -r answer </dev/tty; then
    case "$answer" in [Yy]|[Yy][Ee][Ss]) bar=yes ;; esac
  fi
fi
if [ "$bar" = yes ]; then
  download_verified PaceBar-macOS.zip
  ditto -x -k "$tmp/PaceBar-macOS.zip" "$tmp"
  PACE_BIN="$dest/pace" sh "$tmp/PaceBar/install.sh" "$tmp/PaceBar/PaceBar.app"
fi
