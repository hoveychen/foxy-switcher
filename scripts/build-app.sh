#!/bin/bash
# End-to-end Tauri build orchestrator.
#
#   scripts/build-app.sh             # release build for the current host
#   scripts/build-app.sh mac         # universal .app + .pkg
#   scripts/build-app.sh win         # Windows .exe + .msi  (must run on Windows)
#   scripts/build-app.sh linux       # AppImage / .deb      (must run on Linux)
#   scripts/build-app.sh dev         # debug build of the current host only
#
# The Go server is rebuilt for the targets that match each platform; we don't
# cross-compile the Tauri app itself because that requires the platform's SDK
# (Xcode / MSVC build-tools / GTK + WebKit2GTK).
#
# macOS signing (mac branch only):
#   When APPLE_SIGNING_IDENTITY is unset, the script scans the keychain for a
#   "Developer ID Application" identity and exports the single match before
#   invoking Tauri so the .app and its inner binaries get hardened-runtime +
#   timestamped signatures (required for Apple notarization). Multiple matches
#   error out; set APPLE_SIGNING_IDENTITY=none to force an ad-hoc/unsigned
#   build. The .pkg installer identity is auto-detected separately by
#   build-pkg.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [ $# -gt 0 ]; then
  mode="$1"
else
  case "$(uname -s)" in
    Darwin)              mode=mac ;;
    Linux)               mode=linux ;;
    MINGW*|MSYS*|CYGWIN*) mode=win ;;
    *)
      echo "cannot auto-detect host platform from uname='$(uname -s)'" >&2
      echo "pass an explicit mode: mac | win | linux | dev" >&2
      exit 1
      ;;
  esac
fi

# Make sure the JS bundle is fresh — Tauri 2 will run beforeBuildCommand, but
# we surface failures earlier here.
pnpm install --frozen-lockfile
pnpm build

case "$mode" in
  mac)
    scripts/build-server.sh darwin
    # Auto-detect a Developer ID Application identity for code-signing the
    # .app + inner binaries. Tauri reads APPLE_SIGNING_IDENTITY at build time
    # and falls back to ad-hoc when it's unset, which fails notarization.
    if [ -z "${APPLE_SIGNING_IDENTITY:-}" ]; then
      app_idents="$(security find-identity -p basic -v 2>/dev/null \
        | awk -F'"' '/Developer ID Application/ {print $2}')"
      app_count="$(printf '%s' "$app_idents" | grep -c '^.' || true)"
      if [ "$app_count" -eq 1 ]; then
        export APPLE_SIGNING_IDENTITY="$app_idents"
        echo "==> using APPLE_SIGNING_IDENTITY=$APPLE_SIGNING_IDENTITY (auto-detected)"
      elif [ "$app_count" -gt 1 ]; then
        echo "ERROR: multiple Developer ID Application identities found in keychain:" >&2
        printf '%s\n' "$app_idents" | sed 's/^/  - /' >&2
        echo "Set APPLE_SIGNING_IDENTITY to pick one, or 'none' to skip codesigning." >&2
        exit 1
      fi
    fi
    if [ "${APPLE_SIGNING_IDENTITY:-}" = "none" ]; then
      unset APPLE_SIGNING_IDENTITY
      echo "==> skipping app codesigning (APPLE_SIGNING_IDENTITY=none)"
    fi
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
