// Package assets carries the install / uninstall shell scripts and the
// apiKeyHelper helper that gets dropped into ~/.foxy-switcher.
//
// All three are templated as Go strings so the binary stays self-contained:
// `curl http://localhost:PORT/install.sh | bash` works without shipping any
// additional files alongside the foxy-switcher binary.
package assets

import (
	"fmt"
	"strings"
)

// helperScript is the body of ~/.foxy-switcher/get-token.sh, the script that
// `apiKeyHelper` in ~/.claude/settings.json points at. Keep it deliberately
// minimal — it must not print extra noise on stdout, since Claude Code
// captures stdout verbatim as the API key.
const helperScript = `#!/bin/bash
# foxy-switcher apiKeyHelper bridge.
# Reads the current server port from a discovery file, asks for the next
# available account token, and prints it. Anything written to stderr is
# ignored by Claude Code; stdout becomes the API key.
set -u

PORT_FILE="${FOXY_SWITCHER_PORT_FILE:-$HOME/.foxy-switcher/port}"
if [ ! -f "$PORT_FILE" ]; then
  echo "foxy-switcher: server not running (no $PORT_FILE)" >&2
  exit 1
fi
PORT=$(cat "$PORT_FILE")
if ! curl -sS --fail --max-time 30 "http://127.0.0.1:${PORT}/api/token"; then
  echo "foxy-switcher: failed to fetch token from 127.0.0.1:${PORT}" >&2
  exit 1
fi
`

// HelperScript returns the fixed helper body. Static — no port rendering
// needed; the helper reads the port from the discovery file at runtime.
func HelperScript() string {
	return helperScript
}

// RenderInstallScript returns the body of /install.sh. The caller passes the
// server's current port so the script can sanity-check connectivity before
// editing the user's settings.json. The helper itself is port-agnostic.
func RenderInstallScript(port int) string {
	const tpl = `#!/bin/bash
# foxy-switcher one-shot installer.
#
# What this does:
#   1. Creates ~/.foxy-switcher/ (0700) and writes the apiKeyHelper bridge
#      script (~/.foxy-switcher/get-token.sh) inside it.
#   2. Edits ~/.claude/settings.json to point apiKeyHelper at that script.
#
# Re-running is safe — both steps are idempotent. To remove, fetch and run
# /uninstall.sh from the same server.
set -euo pipefail

DIR="$HOME/.foxy-switcher"
SETTINGS="$HOME/.claude/settings.json"
HELPER="$DIR/get-token.sh"

mkdir -p "$DIR"
chmod 700 "$DIR"

# Sanity-check: the running server should be reachable on :__PORT__ at install
# time. Don't fail hard if it's not — the user may have stopped it after
# downloading this script.
if ! curl -sS --fail --max-time 5 "http://127.0.0.1:__PORT__/healthz" >/dev/null 2>&1; then
  echo "warning: foxy-switcher server not responding on :__PORT__ — installing anyway" >&2
fi

# Write helper script.
cat > "$HELPER" <<'__HELPER_EOF__'
__HELPER_BODY__
__HELPER_EOF__
chmod 755 "$HELPER"

# Patch ~/.claude/settings.json. Use python3 to keep the JSON pretty and
# preserve any unrelated keys.
mkdir -p "$HOME/.claude"
python3 - "$SETTINGS" "$HELPER" <<'__PY_EOF__'
import json, os, sys
path, helper = sys.argv[1], sys.argv[2]
data = {}
if os.path.exists(path):
    try:
        with open(path) as f:
            data = json.load(f)
    except Exception:
        pass
prev = data.get("apiKeyHelper")
data["apiKeyHelper"] = helper
with open(path, "w") as f:
    json.dump(data, f, indent=2)
    f.write("\n")
if prev and prev != helper:
    print(f"replaced apiKeyHelper (was {prev})", file=sys.stderr)
print(f"foxy-switcher hook installed at {helper}", file=sys.stderr)
__PY_EOF__

echo "Done. Try: claude -p 'say hi' --model claude-haiku-4-5"
`
	out := strings.ReplaceAll(tpl, "__PORT__", fmt.Sprintf("%d", port))
	out = strings.ReplaceAll(out, "__HELPER_BODY__", helperScript)
	return out
}

// RenderUninstallScript returns the body of /uninstall.sh. It removes the
// apiKeyHelper key from settings.json and the helper script. The SQLite
// account database is preserved unless `--purge` is passed.
func RenderUninstallScript() string {
	return `#!/bin/bash
# foxy-switcher uninstaller.
#
# Removes the apiKeyHelper hook and the get-token.sh bridge script. The
# account database in ~/.foxy-switcher/state.db is preserved by default —
# pass --purge as the first argument to delete it too.
set -euo pipefail

DIR="$HOME/.foxy-switcher"
SETTINGS="$HOME/.claude/settings.json"
HELPER="$DIR/get-token.sh"
PURGE="${1:-}"

if [ -f "$SETTINGS" ]; then
  python3 - "$SETTINGS" <<'__PY_EOF__'
import json, sys
path = sys.argv[1]
try:
    with open(path) as f:
        data = json.load(f)
except Exception:
    raise SystemExit(0)
if data.pop("apiKeyHelper", None) is not None:
    with open(path, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    print("removed apiKeyHelper from settings.json", file=sys.stderr)
__PY_EOF__
fi

rm -f "$HELPER"

if [ "$PURGE" = "--purge" ]; then
  rm -rf "$DIR"
  echo "purged $DIR"
else
  echo "kept $DIR (use --purge to also delete the account database)"
fi

echo "foxy-switcher hook removed."
`
}
