#!/bin/bash
# Wrap a Tauri-built .app into a macOS .pkg installer.
#
# Tauri 2's bundler only emits `.app` / `.dmg` on macOS — there's no built-in
# pkg target. We invoke the system `pkgbuild` to produce a component-style
# installer that drops the .app under /Applications, then optionally sign it
# with `productsign` when a Developer ID Installer identity is provided.
#
# Env (all optional):
#   APPLE_INSTALLER_SIGNING_IDENTITY  — e.g. "Developer ID Installer: Name (TEAMID)"
#                                       Explicit override. If unset, the script
#                                       auto-detects a "Developer ID Installer"
#                                       identity from the user's keychain and
#                                       signs with it when exactly one is found.
#                                       Set to "none" to force an unsigned .pkg.
#
# Usage:
#   scripts/build-pkg.sh                          # universal-apple-darwin
#   scripts/build-pkg.sh <rust-target-triple>     # e.g. aarch64-apple-darwin
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TARGET_DIR="${1:-universal-apple-darwin}"
CONF="$ROOT/src-tauri/tauri.conf.json"

read_conf() { node -p "require('$CONF').$1"; }

APP_NAME="$(read_conf productName)"
IDENT="$(read_conf identifier)"
VERSION="$(read_conf version)"

BUNDLE_DIR="$ROOT/src-tauri/target/$TARGET_DIR/release/bundle/macos"
APP_PATH="$BUNDLE_DIR/$APP_NAME.app"
PKG_PATH="$BUNDLE_DIR/${APP_NAME}_${VERSION}_universal.pkg"
TMP_PKG="$BUNDLE_DIR/.${APP_NAME}_${VERSION}_universal.unsigned.pkg"

if [ ! -d "$APP_PATH" ]; then
  echo "ERROR: $APP_PATH not found." >&2
  echo "Run: pnpm tauri build --target $TARGET_DIR --bundles app" >&2
  exit 1
fi

echo "==> wrapping $APP_PATH"

# Build via --root + a hand-edited component plist so we can override two
# pkgbuild defaults that quietly break installs:
#   - BundleIsRelocatable=true: PackageKit looks the bundle id up in the
#     LaunchServices database and rewrites --install-location to wherever
#     a copy of the .app is already registered (e.g. a stale build sitting
#     in target/release/bundle/macos/), so /Applications is silently
#     ignored and the install appears to succeed without changing anything.
#   - BundleIsVersionChecked=true: PackageKit refuses to overwrite a bundle
#     when an equal-or-newer CFBundleVersion is already on disk, so a
#     re-install of the same tag is a silent no-op. Setting it to false
#     forces overwrite regardless of version.
# Do NOT switch BundleOverwriteAction to "update" to "force overwrite" —
# despite the name, "update" means update-only (pkgbuild(1): "the package
# bundle will not be installed at all if there is not already a version on
# disk"), so on a fresh user machine PackageKit silently skips the entire
# component while Installer.app still reports success. v1.1.6..v1.1.8 had
# this misconfiguration and shipped pkgs that installed nothing on first-
# time users. Leave BundleOverwriteAction at the default "upgrade".
PKG_STAGING="$(mktemp -d)"
trap 'rm -rf "$PKG_STAGING"' EXIT
cp -R "$APP_PATH" "$PKG_STAGING/"

COMPONENT_PLIST="$PKG_STAGING/component.plist"
pkgbuild --analyze --root "$PKG_STAGING" "$COMPONENT_PLIST" >/dev/null
/usr/libexec/PlistBuddy -c "Set :0:BundleIsRelocatable false"   "$COMPONENT_PLIST"
/usr/libexec/PlistBuddy -c "Set :0:BundleIsVersionChecked false" "$COMPONENT_PLIST"

pkgbuild \
  --root "$PKG_STAGING" \
  --component-plist "$COMPONENT_PLIST" \
  --install-location /Applications \
  --identifier "$IDENT" \
  --version "$VERSION" \
  "$TMP_PKG"

identity="${APPLE_INSTALLER_SIGNING_IDENTITY:-}"
if [ -z "$identity" ]; then
  # Auto-detect a Developer ID Installer identity from the user's keychain.
  identities="$(security find-identity -p basic -v 2>/dev/null \
    | awk -F'"' '/Developer ID Installer/ {print $2}')"
  count="$(printf '%s' "$identities" | grep -c '^.' || true)"
  if [ "$count" -eq 1 ]; then
    identity="$identities"
  elif [ "$count" -gt 1 ]; then
    echo "ERROR: multiple Developer ID Installer identities found in keychain:" >&2
    printf '%s\n' "$identities" | sed 's/^/  - /' >&2
    echo "Set APPLE_INSTALLER_SIGNING_IDENTITY to pick one explicitly." >&2
    exit 1
  fi
fi

if [ -n "$identity" ] && [ "$identity" != "none" ]; then
  echo "==> signing with: $identity"
  productsign --sign "$identity" "$TMP_PKG" "$PKG_PATH"
  rm -f "$TMP_PKG"
else
  mv "$TMP_PKG" "$PKG_PATH"
  echo "==> unsigned (no Developer ID Installer identity in keychain; set APPLE_INSTALLER_SIGNING_IDENTITY to override)"
fi

echo
echo "wrote $PKG_PATH"
