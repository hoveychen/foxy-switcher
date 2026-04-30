#!/bin/bash
# End-to-end Tauri build orchestrator.
#
#   scripts/build-app.sh mac        # universal .app + .dmg
#   scripts/build-app.sh win        # Windows .exe + .msi  (must run on Windows)
#   scripts/build-app.sh linux      # AppImage / .deb     (must run on Linux)
#   scripts/build-app.sh dev        # debug build of the current host only
#
# The Go server is rebuilt for the targets that match each platform; we don't
# cross-compile the Tauri app itself because that requires the platform's SDK
# (Xcode / MSVC build-tools / GTK + WebKit2GTK).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

mode="${1:-dev}"

# Make sure the JS bundle is fresh — Tauri 2 will run beforeBuildCommand, but
# we surface failures earlier here.
pnpm install --frozen-lockfile
pnpm build

case "$mode" in
  mac)
    scripts/build-server.sh darwin
    # Tauri only emits .app/.dmg on macOS — wrap into .pkg via pkgbuild.
    pnpm tauri build --target universal-apple-darwin --bundles app
    scripts/build-pkg.sh universal-apple-darwin
    ;;
  win)
    scripts/build-server.sh host
    pnpm tauri build --bundles msi
    ;;
  linux)
    scripts/build-server.sh host
    pnpm tauri build
    ;;
  dev)
    scripts/build-server.sh host
    pnpm tauri build --debug
    ;;
  *)
    echo "unknown mode: $mode  (expected: mac | win | linux | dev)" >&2
    exit 1
    ;;
esac
