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
DIST="$ROOT/dist"
EMBED="$SRC/vault/webapp/static"
mkdir -p "$OUT"

# Strip symbol tables to slim the binary; the helper script already echoes
# the token verbatim so debug info has no caller-side use.
LDFLAGS="-s -w"

# bake_webapp populates the embed directory the vault binary's
# `//go:embed all:static` directive picks up. We do this once before any
# `go build` so every cross-compiled sidecar travels with the React
# account panel. If `dist/` is missing (devs who haven't run `pnpm build`
# yet) we leave the placeholder .gitkeep alone — webapp.Available()
# returns false at runtime and the vault keeps serving the bare Web UI.
bake_webapp() {
  if [ ! -f "$DIST/index.html" ]; then
    echo "==> webapp: dist/index.html missing, skipping embed (run \`pnpm build\` first)"
    return
  fi
  echo "==> webapp: copying dist/ → server/vault/webapp/static/"
  rm -rf "$EMBED"
  mkdir -p "$EMBED"
  cp -R "$DIST/"* "$EMBED/"
  # Keep .gitkeep around so a `git clean` survivor still satisfies
  # //go:embed; cp -R already overwrote it if dist had one, otherwise
  # we replace it explicitly.
  : > "$EMBED/.gitkeep"
}

bake_webapp

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

# Tauri's `--target universal-apple-darwin` resolves sidecars by that exact
# triple, so we lipo the two per-arch builds into a fat binary alongside them.
build_darwin_universal() {
  local arm="$OUT/foxy-switcher-server-aarch64-apple-darwin"
  local x86="$OUT/foxy-switcher-server-x86_64-apple-darwin"
  local out="$OUT/foxy-switcher-server-universal-apple-darwin"
  echo "==> lipo universal  →  $(basename "$out")"
  lipo -create "$arm" "$x86" -output "$out"
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
    build_darwin_universal
    ;;
  all|"")
    build darwin arm64 aarch64-apple-darwin
    build darwin amd64 x86_64-apple-darwin
    build_darwin_universal
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
