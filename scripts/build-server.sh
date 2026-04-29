#!/bin/bash
# Build the Go server for every target Tauri may need to bundle as a sidecar.
#
# Tauri 2 resolves sidecars via "<binary>-<rust-target-triple>" (with .exe on
# Windows). modernc.org/sqlite is pure Go, so we can cross-compile from any
# host with CGO_ENABLED=0 — no zig / mingw setup needed.
#
# Output goes to src-tauri/binaries/, which is what tauri.conf.json points at
# via `bundle.externalBin`.
#
# Usage:
#   scripts/build-server.sh           # build all four sidecars
#   scripts/build-server.sh host      # build only the current host
#   scripts/build-server.sh darwin    # arm64 + amd64 (mac universal feed)
#
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/server"
OUT="$ROOT/src-tauri/binaries"
mkdir -p "$OUT"

# Strip symbol tables to slim the binary; the helper script already echoes
# the token verbatim so debug info has no caller-side use.
LDFLAGS="-s -w"

build() {
  local goos="$1" goarch="$2" triple="$3"
  local ext=""
  if [ "$goos" = "windows" ]; then
    ext=".exe"
  fi
  local out="$OUT/foxy-switcher-server-${triple}${ext}"
  echo "==> $goos/$goarch  →  $(basename "$out")"
  ( cd "$SRC" && CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
      go build -trimpath -ldflags="$LDFLAGS" -o "$out" . )
}

mode="${1:-all}"

case "$mode" in
  host)
    triple="$(rustc -vV | awk '/^host:/ {print $2}')"
    case "$triple" in
      *-apple-darwin) goos=darwin ;;
      *-windows-*)    goos=windows ;;
      *-linux-*)      goos=linux ;;
      *) echo "unsupported host triple: $triple" >&2; exit 1 ;;
    esac
    case "$triple" in
      x86_64-*)  goarch=amd64 ;;
      aarch64-*) goarch=arm64 ;;
      *) echo "unsupported host arch: $triple" >&2; exit 1 ;;
    esac
    build "$goos" "$goarch" "$triple"
    ;;
  darwin)
    build darwin arm64 aarch64-apple-darwin
    build darwin amd64 x86_64-apple-darwin
    ;;
  all|"")
    build darwin arm64 aarch64-apple-darwin
    build darwin amd64 x86_64-apple-darwin
    build windows amd64 x86_64-pc-windows-msvc
    build linux amd64 x86_64-unknown-linux-gnu
    ;;
  *)
    echo "unknown mode: $mode (expected: host | darwin | all)" >&2
    exit 1
    ;;
esac

echo
echo "binaries in $OUT:"
ls -lh "$OUT"
