# Architecture

Technical reference for Foxy Switcher — what runs where, the HTTP surface, and the layout on disk. For end-user docs see the [README](../README.md).

## Components

```
+------------------+   IPC    +-----------------------+
|  Tauri GUI       | <------> |  Go daemon (sidecar)  |
|  React + Vite    |   HTTP   |  127.0.0.1:<port>     |
+------------------+          +----+----+----+--------+
                                   |    |    |
                                   v    v    v
                           +-------+ +--+--+ +------------+
                           | SQLite| | OS  | | Anthropic  |
                           | state | | key | | Anthropic /|
                           | .db   | | chn | | OpenAI APIs|
                           +-------+ +-----+ +------------+
```

- **GUI** ([../src/](../src/)) — React single-page app, talks to the daemon over plain HTTP.
- **Daemon** ([../server/](../server/)) — Go, binds 127.0.0.1 only, no auth (single-user product).
- **TUI** ([../server/tui/](../server/tui/)) — `foxy-switcher tui`, reuses the same HTTP API.

### Daemon packages

| Package | Responsibility |
| --- | --- |
| [server/store/](../server/store/) | SQLite schema for accounts, tokens, cooldowns, usage snapshots |
| [server/authz/](../server/authz/) | PKCE state store for the OAuth login flow |
| [server/anthropic/](../server/anthropic/) | Anthropic OAuth + profile + usage API client |
| [server/openai/](../server/openai/) | Codex auth.json parsing, OpenAI refresh + usage client, atomic injection and restore |
| [server/selector/](../server/selector/) | Picks the next eligible account inside one provider pool, preferring LRU |
| [server/refresh/](../server/refresh/) | Provider-aware token refresh and usage polling |
| [server/credinject/](../server/credinject/) | Owns Claude Code's keychain entry while running; restores native login on shutdown |
| [server/httpapi/](../server/httpapi/) | The localhost HTTP surface |
| [server/tui/](../server/tui/) | `tui` subcommand — terminal client for the same API |

## Building from source

Prerequisites: Go 1.22+, Node 20+, pnpm, Rust toolchain (for Tauri).

```sh
pnpm install
scripts/build-server.sh host    # builds the Go sidecar for the current host
pnpm tauri build                # builds the desktop app
```

Platform bundles:

```sh
scripts/build-app.sh            # release build for the current host
scripts/build-app.sh mac        # universal .app + .pkg
scripts/build-app.sh win        # .exe + .msi (must run on Windows)
scripts/build-app.sh linux      # AppImage / .deb (must run on Linux)
scripts/build-app.sh dev        # debug build for the current host
```

The Go sidecar uses `modernc.org/sqlite` (pure Go), so cross-compilation works with `CGO_ENABLED=0` — no zig / mingw needed.

## Running headless

The daemon also runs without the GUI:

```sh
foxy-switcher --port=0                    # random port, written to ~/.foxy-switcher/port
foxy-switcher --data-dir=/path/to/dir     # override state location
foxy-switcher --no-cred-inject            # don't touch the keychain (debug mode)
foxy-switcher tui                         # terminal UI against a running daemon
```

Daemon flags (see [server/main.go](../server/main.go)):

| Flag | Description |
| --- | --- |
| `--data-dir` | Directory for `state.db` and the port file (default `~/.foxy-switcher`) |
| `--port` | TCP port on 127.0.0.1; `0` = random |
| `--parent-pid` | Sidecar safety net — exit when this PID disappears |
| `--no-cred-inject` | Skip the entire keychain lifecycle (no inject, no reverse-sync, no restore) |

## Credential injection

- **macOS**: the daemon writes Claude Code's existing keychain entry under `com.anthropic.claude-code` (see [server/credinject/darwin.go](../server/credinject/darwin.go)).
- **Linux / Windows**: the daemon replaces `~/.claude/.credentials.json` (mode 0600, atomic via `.tmp` + rename) and clears `primaryApiKey` in `~/.claude.json` so Claude Code falls through to the OAuth path (see [server/credinject/other.go](../server/credinject/other.go)).
- The user's pre-existing native login is captured before the first inject and restored on shutdown via `Coordinator.RestoreOnShutdown`.
- **Codex**: [`server/openai.Manager`](../server/openai/manager.go) atomically replaces the file-backed `CODEX_HOME/auth.json` (normally `~/.codex/auth.json`). It reverse-syncs token rotations written by Codex CLI and restores the original file on shutdown. Keyring-backed Codex credentials are not imported in this release.

Deep dive on macOS keychain layout: [keychain-credentials-pool.md](keychain-credentials-pool.md).

## HTTP API

All endpoints bind to `127.0.0.1:<port>` — read the port from `~/.foxy-switcher/port`. No auth (single-user, loopback only). Defined in [server/httpapi/routes.go](../server/httpapi/routes.go).

| Method + Path | Purpose |
| --- | --- |
| `GET /api/accounts` | List provider-tagged accounts with status, expiry, usage snapshot, and local in-use state |
| `POST /api/accounts/login` | Start a Claude PKCE flow, returns `{ state, authorize_url }` |
| `POST /api/accounts/callback` | Complete Claude PKCE — body `{ state, pasted }` |
| `POST /api/accounts/import-codex` | Import the current file-backed Codex CLI ChatGPT login |
| `DELETE /api/accounts/{id}` | Remove an account |
| `POST /api/accounts/{id}/pause` | Pause routing while continuing credential maintenance |
| `POST /api/accounts/{id}/resume` | Re-activate routing |
| `POST /api/accounts/{id}/refresh` | Refresh access_token + usage snapshot now |
| `POST /api/accounts/{id}/select` | Promote to front of LRU queue (one-shot) |
| `GET /api/cred/status` | Current credinject state — which account is injected, last error |
| `GET /healthz` | Liveness probe |

## Selection strategy

[`selector.PickProvider`](../server/selector/selector.go) evaluates each provider independently:

1. Skip accounts with `status != "active"`.
2. Skip expired tokens and accounts whose measured usage reached a configured threshold.
3. From the rest, return the one with the smallest `last_used_at` (LRU).

Returns `ErrNoAvailable` when every account is unusable. The matching provider manager then restores that CLI's native credentials.

## Usage tracking

`refresh.UsagePoller` polls the account's provider. Claude stores `five_hour`, `seven_day`, and `seven_day_sonnet`; Codex maps ChatGPT's `primary_window` and `secondary_window` onto the first two storage slots. Every window carries utilization on a **0–100 scale** and an RFC3339 reset time.

## Data layout

```
~/.foxy-switcher/
├── state.db          # SQLite — accounts, tokens, usage, last_used_at
├── port              # current daemon listen port (atomic write)
└── original-creds*   # snapshot of the user's pre-inject native login

~/.codex/
├── auth.json                 # live Codex CLI credentials (file mode)
└── auth.json.foxy-backup     # temporary native snapshot while Foxy manages Codex
```

`state.db` is chmod'd to `0600`.

## Project layout

```
foxy-switcher/
├── src/                React + TypeScript GUI
├── src-tauri/          Tauri 2 wrapper, externalBin = the Go sidecar
├── server/             Go daemon + TUI
├── scripts/            build-app.sh / build-server.sh / build-pkg.sh
└── docs/               architecture, design system, signing notes
```

## Security model

- All HTTP listens on `127.0.0.1` only; CORS is wildcard because there's no remote-attacker model and no cookies are used.
- `state.db` and the port file are mode `0600`.
- The daemon never persists raw passwords. `state.db` contains OAuth access/refresh tokens and complete Codex auth documents, and is therefore mode `0600`.
- `--no-cred-inject` lets you run alongside a real native login without clobbering it (useful for debugging the API surface in isolation).
