<div align="center">
  <img src="docs/foxy-icon.png" alt="Foxy Switcher" width="128" />
  <h1>Foxy Switcher</h1>
  <p><strong>An account pool for Claude Code. Stop logging in and out.</strong></p>
  <p>
    <img alt="Platform" src="https://img.shields.io/badge/platform-macOS%20%7C%20Windows-blue" />
    <img alt="Status" src="https://img.shields.io/badge/status-beta-orange" />
  </p>
</div>

---

If you keep multiple Claude subscriptions, Foxy Switcher pools them together and quietly hands Claude Code whichever one still has runway. When an account trips a 5-hour or 7-day cap, the next one slides in. When you quit, your original login is restored — exactly as you left it.

## Why

If you live in Claude Code, one subscription is rarely enough. You hit a 5-hour cap mid-task, switch to your team plan, hit the 7-day Sonnet cap a day later, and end up running `claude logout` / `claude login` so often you've memorized which account is which.

Foxy Switcher takes that loop off your hands. Enroll each subscription once. The daemon watches the usage windows, picks the least-recently-used account that still has runway, and quietly swaps Claude Code's credentials in place — no restart, no copy-paste, no terminal dance. When everything cools down, your original login comes back as if nothing happened.

## Features

- **Pool any number of Claude subscriptions** — personal, team, premium. Add each once via the standard Claude OAuth flow.
- **Automatic rotation** — least-recently-used policy with a cooldown lane for accounts that just hit a cap.
- **Live usage at a glance** — see each account's 5-hour, 7-day, and 7-day Sonnet windows side by side, so you know who's about to cool down.
- **Manual override** — flip Auto Switch off and pick the account yourself with one click.
- **Safe by default** — Foxy never leaves Claude Code in a foreign state. Clean exit, crash, or force-quit, your original keychain entry comes back.
- **GUI and TUI** — full desktop app, plus a terminal UI for headless / SSH setups.

## Install

### Download

Grab the latest build from the [Releases page](https://github.com/hoveychen/foxy-switcher/releases).

- **macOS** — universal `.pkg` (Intel + Apple Silicon)
- **Windows** — `.msi` installer (x64)
- **Linux** — no prebuilt installer yet; build from source. The daemon, TUI, and credential injector all run cross-platform (Linux/Windows just swap Claude Code's `~/.claude/.credentials.json` directly instead of the macOS keychain).

### From source

```sh
pnpm install
pnpm tauri build
```

Build instructions for sidecar binaries and platform bundles live in [docs/architecture.md](docs/architecture.md#building-from-source).

## Quick start

1. **Launch the app** and click **Add account**. Your browser opens the standard Claude login.
2. **Repeat** for each subscription you want in the pool.
3. **Open Claude Code** as you normally would. Foxy is already standing in for one of your accounts; when it caps out, the next one takes over.

Want to take over manually? Toggle **Auto Switch** off and click **Use now** on any account.

## FAQ

**Does this need my password?**
No. Foxy Switcher uses Claude's official OAuth login — the same flow `claude login` runs. It never sees your password.

**Will it conflict with my normal Claude Code login?**
No. On startup Foxy snapshots whatever you had logged in. On exit — clean, crash, or force-quit — your original credentials go back exactly where they were.

**What if every account in the pool is exhausted?**
The daemon detects that and restores your native login, so Claude Code falls back to your normal account instead of getting stuck.

**Is my data sent anywhere?**
Everything stays on your machine. The daemon only talks to Anthropic's official OAuth and usage APIs — the same endpoints Claude Code uses.

**Does it work without the GUI?**
Yes. Run `foxy-switcher` headless and manage the pool from a terminal with `foxy-switcher tui`.

## Documentation

- [Architecture & API](docs/architecture.md) — how the daemon, GUI, and credential injector fit together; flags, HTTP API, data layout.
- [Keychain mechanics (macOS)](docs/keychain-credentials-pool.md) — research notes on how Claude Code stores credentials and how Foxy swaps them.
- [Release signing](docs/release-signing.md) — notarization and code-signing setup for the macOS build.
- [Design system](docs/DESIGN_SYSTEM.md) and [PRD](docs/PRD.md) — for contributors.

## Status

`v1.0.0` — single-user, macOS-first. Prebuilt installers ship for macOS (`.pkg`, universal) and Windows (`.msi`, x64). Linux runs from source today — the daemon, TUI, and credential injector all work there (the injector edits `~/.claude/.credentials.json` directly). Follow [issues](https://github.com/hoveychen/foxy-switcher/issues) for progress on a packaged Linux release.
