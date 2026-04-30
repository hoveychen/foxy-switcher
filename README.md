# foxy-switcher

> OAuth account pool for Claude Code — pick an account, inject its credentials, rotate when one hits a rate limit.

`foxy-switcher` runs a tiny localhost daemon that holds a pool of Claude subscription accounts and feeds Claude Code a fresh OAuth token from whichever account is healthiest. A Tauri desktop app drives the daemon as a sidecar; a terminal UI is also available for headless setups.

When the daemon exits — gracefully, on Ctrl-C, or because its parent GUI died — it restores your original native Claude Code login so the keychain is never left in a foreign state.

## Why

If you have multiple Claude subscriptions (personal, team, premium) and you're constantly hitting per-account 5-hour or 7-day usage caps in Claude Code, you end up logging in and out by hand. `foxy-switcher` automates that: enroll each account once via OAuth, and the daemon picks the least-recently-used non-cooldown account and injects it into Claude Code's keychain entry.

## Architecture

```
+------------------+   IPC    +-----------------------+
|  Tauri GUI       | <------> |  Go daemon (sidecar)  |
|  React + Vite    |   HTTP   |  127.0.0.1:<port>     |
+------------------+          +----+----+----+--------+
                                   |    |    |
                                   v    v    v
                           +-------+ +--+--+ +------------+
                           | SQLite| | OS  | | Anthropic  |
                           | state | | key | | OAuth +    |
                           | .db   | | chn | | usage API  |
                           +-------+ +-----+ +------------+
```

- **GUI** ([src/](src/)) — React single-page app, talks to the daemon over plain HTTP.
- **Daemon** ([server/](server/)) — Go, binds 127.0.0.1 only, no auth (single-user product).
- **TUI** ([server/tui/](server/tui/)) — `foxy-switcher tui`, reuses the same HTTP API.

### Daemon packages

| Package | Responsibility |
| --- | --- |
| [server/store/](server/store/) | SQLite schema for accounts, tokens, cooldowns, usage snapshots |
| [server/authz/](server/authz/) | PKCE state store for the OAuth login flow |
| [server/anthropic/](server/anthropic/) | Anthropic OAuth + profile + usage API client |
| [server/selector/](server/selector/) | Picks the next account — skip disabled / in-cooldown, prefer LRU |
| [server/refresh/](server/refresh/) | `Scheduler` refreshes expiring tokens; `UsagePoller` pulls 5h / 7d / 7d-Sonnet windows |
| [server/credinject/](server/credinject/) | Owns Claude Code's keychain entry while running; restores native login on shutdown |
| [server/httpapi/](server/httpapi/) | The localhost HTTP surface |
| [server/tui/](server/tui/) | `tui` subcommand — terminal client for the same API |

## Install

### From source

Prerequisites: Go 1.22+, Node 20+, pnpm, Rust toolchain (for Tauri).

```sh
pnpm install
scripts/build-server.sh host    # builds the Go sidecar for the current host
pnpm tauri build                # builds the desktop app
```

Platform bundles are produced via:

```sh
scripts/build-app.sh mac        # universal .app + .dmg
scripts/build-app.sh win        # .exe + .msi (must run on Windows)
scripts/build-app.sh linux      # AppImage / .deb (must run on Linux)
scripts/build-app.sh dev        # debug build for the current host
```

The Go sidecar uses `modernc.org/sqlite` (pure Go), so cross-compilation works with `CGO_ENABLED=0` — no zig / mingw needed.

### Headless / standalone daemon

The daemon also runs without the GUI:

```sh
foxy-switcher --port=0                    # random port, written to ~/.foxy-switcher/port
foxy-switcher --data-dir=/path/to/dir     # override state location
foxy-switcher --no-cred-inject            # don't touch the keychain (debug mode)
foxy-switcher tui                         # terminal UI against a running daemon
```

Daemon flags (see [server/main.go](server/main.go)):

| Flag | Description |
| --- | --- |
| `--data-dir` | Directory for `state.db` and the port file (default `~/.foxy-switcher`) |
| `--port` | TCP port on 127.0.0.1; `0` = random |
| `--parent-pid` | Sidecar safety net — exit when this PID disappears |
| `--no-cred-inject` | Skip the entire keychain lifecycle (no inject, no reverse-sync, no restore) |

## Usage

1. Launch the GUI (or the daemon + `foxy-switcher tui`).
2. Click **Add account** — the daemon opens an OAuth PKCE flow against `console.anthropic.com`. The account name is derived from the profile (email preferred, then full name).
3. Repeat for each subscription.
4. Open Claude Code — it will pick up whichever account the daemon currently has injected.
5. When an account hits a rate limit, mark it as in-cooldown (or the daemon will infer it from the usage poller); on the next reconcile, credinject swaps to the next LRU candidate.

### What gets injected where

- **macOS**: the daemon writes Claude Code's existing keychain entry under `com.anthropic.claude-code` (see [server/credinject/darwin.go](server/credinject/darwin.go)). Other platforms are stubbed in [server/credinject/other.go](server/credinject/other.go).
- The user's pre-existing native login is captured before the first inject and restored on shutdown via `Coordinator.RestoreOnShutdown`.

## HTTP API

All endpoints bind to `127.0.0.1:<port>` — read the port from `~/.foxy-switcher/port`. No auth (single-user, loopback only). Defined in [server/httpapi/routes.go](server/httpapi/routes.go).

| Method + Path | Purpose |
| --- | --- |
| `GET /api/accounts` | List accounts with status, expiry, cooldown, usage snapshot |
| `POST /api/accounts/login` | Start a PKCE flow, returns `{ state, authorize_url }` |
| `POST /api/accounts/callback` | Complete PKCE — body `{ state, code }`, enrolls the account |
| `DELETE /api/accounts/{id}` | Remove an account |
| `POST /api/accounts/{id}/disable` | Mark inactive (skipped by the selector) |
| `POST /api/accounts/{id}/enable` | Re-activate |
| `POST /api/accounts/{id}/cooldown` | Manually park an account in cooldown |
| `POST /api/accounts/{id}/refresh` | Refresh access_token + usage snapshot now |
| `POST /api/accounts/{id}/select` | Promote to front of LRU queue (one-shot) |
| `GET /api/cred/status` | Current credinject state — which account is injected, last error |
| `GET /healthz` | Liveness probe |

## Selection strategy

[`selector.Pick`](server/selector/selector.go):

1. Skip accounts with `status != "active"`.
2. Skip accounts whose `cooldown_until` is still in the future.
3. From the rest, return the one with the smallest `last_used_at` (LRU).

Returns `ErrNoAvailable` when every account is unusable — the credinject coordinator treats this as the trigger to restore native credentials so Claude Code can fall back to the user's own login.

## Usage tracking

`refresh.UsagePoller` polls Anthropic's usage API and stores three windows per account: `five_hour`, `seven_day`, `seven_day_sonnet`. Each window carries `utilization` on a **0–100 scale** (not 0–1) and `resets_at`. The frontend uses the peak of the three to color-code each row (warn ≥ 75, danger ≥ 90).

## Data layout

```
~/.foxy-switcher/
├── state.db          # SQLite — accounts, tokens, usage, last_used_at
├── port              # current daemon listen port (atomic write)
└── original-creds*   # snapshot of the user's pre-inject native login
```

`state.db` is chmod'd to `0600`.

## Project layout

```
foxy-switcher/
├── src/                React + TypeScript GUI
├── src-tauri/          Tauri 2 wrapper, externalBin = the Go sidecar
├── server/             Go daemon + TUI
├── scripts/            build-app.sh / build-server.sh / build-pkg.sh
└── docs/               keychain-credentials-pool.md, release-signing.md
```

## Security model

- All HTTP listens on `127.0.0.1` only; CORS is wildcard because there's no remote-attacker model and no cookies are used.
- `state.db` and the port file are mode `0600`.
- The daemon never persists raw passwords — only OAuth refresh + access tokens.
- `--no-cred-inject` lets you run alongside a real native login without clobbering it (useful for debugging the API surface in isolation).

## Status

`v0.1.0` — single-user, macOS-first. Other platforms compile but the keychain backend is stubbed.
