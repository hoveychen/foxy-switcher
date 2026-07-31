# Cloud Vault — Design

This doc captures the split between a **Vault** service (token storage + refresh + usage + provider-aware account selection, runnable in the cloud) and an **Agent** service (credential injection into Claude Code and Codex CLI on the user's local machine). The current single-process behavior remains the default deployment mode (Vault + Agent inside one binary, talking via in-process calls).

For the existing architecture see [architecture.md](architecture.md). This doc only covers what changes.

## Goals

1. Allow the account vault to run on a remote machine (a personal cloud VM, a home server) so token refresh and usage polling are not gated on the user's laptop being awake.
2. Keep the single-binary local-only deployment working unchanged. No regression for users who don't want a remote vault.
3. Allow one user to point multiple machines (laptop + desktop) at the same vault without corrupting the OAuth refresh-token rotation chain.

## Non-goals

1. Multi-tenant SaaS. The vault is single-user (one human, possibly several devices). No org/team model.
2. Hosted vault offered by the project. Each user runs their own.
3. End-to-end encrypted token storage. Vault holds raw tokens — same threat model as today's local SQLite at `0600`. Vault deployments must rely on disk encryption + TLS termination at the reverse proxy.

## Deployment modes

One binary, three modes via a single flag:

| Flag | Components active |
|---|---|
| `--mode=combined` (default) | Vault + Agent in-process. Identical behavior to today. |
| `--mode=vault` | Store, refresh.Scheduler, refresh.UsagePoller, authz, selector, HTTP API. **No** credinject. |
| `--mode=agent --vault-url=https://…` | Claude `credinject.Coordinator` + Codex `openai.RemoteManager` + reverse-sync + HTTP client to vault. **No** account store, scheduler, or usage poller. |

`combined` shares a single in-memory `vault.Service` so we don't pay HTTP overhead for the local case.

## Layer split

```
+------------------+           +-----------------------+
|  Frontend        |           |  Vault                |
|  Tauri / Web UI  | <-------> |  store + refresh +    |
|                  |   HTTP    |  usage + selector +   |
|                  |           |  authz + activity bus |
+--------+---------+           +----+------------------+
         |                          ^
         |                          | HTTP + SSE
         v                          |
+------------------+                |
|  Agent           | <--------------+
|  Claude + Codex  |
|  injection/sync  |
|  OAuth receiver  |
+------------------+
```

- Frontend talks to **Vault** for accounts/settings/dashboard/activity. It talks to **Agent** for credinject status (which account is currently injected on this machine).
- **Agent** is the only component that touches the local keychain. The Vault never knows what's actually injected — it only knows which device holds a *lease* on which account.
- The OAuth PKCE flow:
  1. Agent calls `POST /api/accounts/login` on Vault → vault returns `{ authorize_url, state }` and stashes the verifier server-side.
  2. User pastes the resulting `code#state` into the Agent UI.
  3. Agent calls `POST /api/accounts/callback` with `{ state, pasted }` → Vault finishes the exchange, fetches profile, stores the row, fires `account.added` activity.

  The verifier lives on Vault because Vault is the party that talks to `platform.claude.com/v1/oauth/token`. Agent never holds verifier or refresh tokens (other than the one currently injected, which it received via `PickNext`).

## `vault.Service` interface

A new package `server/vault/` defines the interface the **agent** depends on. The frontend / TUI continue to talk to `httpapi` over HTTP because httpapi *is* the vault's HTTP surface — there is no second client tier to abstract. So `vault.Service` carries only the methods `credinject.Coordinator` needs; it stays narrow on purpose, and grows in later steps only when an actual caller appears on the agent side.

Step 1 surface (landed):

```go
type Service interface {
    ListAccounts(ctx) ([]Account, error)
    GetAutoSwitch(ctx) (AutoSwitch, error)
    Pick(ctx, now time.Time) (*Account, error)        // legacy Claude default
    PickProviderForDevice(ctx, now, deviceID, provider) (*Account, error)
    MarkUsed(ctx, accountID int64) error
    UpdateTokens(ctx, accountID, accessToken, refreshToken, expiresAt) error
    UpdateProviderCredential(ctx, accountID, accessToken, refreshToken, expiresAt, credentialJSON) error
}
```

The combined-mode implementation (`vault.InProc`) is a 5-method delegate over `store` + `selector`. The agent's `credinject.Coordinator` is the only caller today.

Methods the doc previously listed (login, refresh-now, settings, dashboard, activity stream, lease) all live on the **vault HTTP surface** that frontend already calls. They are *not* in `vault.Service` because they don't need an in-process abstraction — they're directly routed handlers in `httpapi`. Steps 2 and 4 will:

- **Step 2** — Wrap `httpapi` in `vault/httpserver/` for the cloud deployment, and add a `vault/httpclient/` impl of `Service` so the agent can hit a remote vault. The Service interface gains `AcquireLease/RenewLease/ReleaseLease/ReportRotation` at that point.
- **Step 4** — Lease semantics become enforceable; `Pick` starts honoring leases.

```go
// LeasedAccount is added in Step 2.
type LeasedAccount struct {
    Account
    LeaseID    string
    LeaseUntil int64                 // unix millis
}
```

## Wire protocol

Two surfaces share one listener:

- **Frontend surface** (`/api/*`) — the existing httpapi routes, unchanged. The Tauri / web UI hits these. Tokens redacted.
- **Agent surface** (`/agent/v1/*`) — what credinject calls remotely. Tokens included; the agent needs them to inject. Step 2 lands the subset the in-process Service exposes; Step 3 will gate every route behind the device-flow `Authorization: Bearer …`.

Step 2 agent routes:

| Method + path | Purpose |
|---|---|
| `GET /agent/v1/accounts` | List accounts (raw, tokens included) |
| `GET /agent/v1/auto-switch` | Auto-switch policy |
| `POST /agent/v1/pick?provider=claude\|codex` | Returns the next eligible account for one provider, or 204; omitted provider defaults to Claude for old agents |
| `POST /agent/v1/accounts/{id}/used` | MarkUsed |
| `POST /agent/v1/accounts/{id}/tokens` | Reports CLI-rotated tokens and optional provider-native credential JSON |
| `POST /agent/v1/leases` | AcquireLease — body: `{ account_id, device_id, ttl_ms }` |
| `POST /agent/v1/leases/{id}/renew` | RenewLease — body: `{ ttl_ms }` |
| `DELETE /agent/v1/leases/{id}` | ReleaseLease |
| `GET /agent/v1/openrouter/config` | The calling device's OpenRouter grant (derived key + allowed models), or 204 |

Frontend routes that previously appeared on this list (login, refresh-now, settings, dashboard, activity SSE) stay where they are under `/api/*`. The frontend talks to vault directly over HTTP regardless of deployment topology, so abstracting them through `vault.Service` would just create a parallel call path with no callers. They're documented in [architecture.md](architecture.md) and route definitions live in [server/httpapi/routes.go](../server/httpapi/routes.go).

The agent surface is also exposed in `--mode=combined`: a second device on the same LAN can `--mode=agent` against the local daemon. That's mostly a debugging convenience — the canonical multi-device topology is one cloud `--mode=vault` plus N local agents.

Each provider's reconcile loop:

1. On startup: `POST /lease/pick` to acquire the next account.
2. Acquire/renew that provider's lease, then inject into its native credential store.
3. Subscribe to `/events`. On `account.changed` for the leased account, re-read tokens (vault has rotated them) and re-inject.
4. Periodically `RenewLease` (every TTL/3).
5. On `switch.requested` (e.g. user clicked "Use now" for a different account on the web UI): release current lease, pick again.
6. Reverse-sync local CLI rotations into the vault. Claude reports its token tuple; Codex also reports the complete normalized auth document.

The HTTP boundary preserves today's contract: `combined` mode runs the same handlers but skips the network hop.

## OpenRouter — a provider without leases

OpenRouter is in the pool alongside Claude and Codex, but almost none of the
lease machinery applies to it, and that asymmetry is deliberate rather than an
omission.

Claude and Codex are **subscriptions**: one account has one rate-limit window,
so two devices using it at once eat each other's quota. Hence one lease per
account, LRU rotation, and a reconcile loop that renews.

OpenRouter is **pay-as-you-go**: spend is capped per key by a guardrail, and
concurrent use is harmless. Leasing it would buy nothing and cost real
usability — devices fighting over one account, keys churning on every rotation.
So instead of sharing one credential, **each authorised device gets its own
derived key**.

### Storage

| Where | What | Reaches a device? |
|---|---|---|
| `accounts` row (`provider="openrouter"`) | The derivation *template*: `credential_json` holds allowed models, spend cap, workspace. No secret at all. | Yes — `GET /agent/v1/accounts` serialises the row verbatim |
| `openrouter_management_keys` | The key that mints and revokes runtime keys. | **Never.** Its own table precisely so it can't ride along on an account serialisation |
| `device_openrouter_keys` | `(device_id, account_id)` → the derived key's hash, secret, and guardrail id | Only the row's own device, via `/agent/v1/openrouter/config` |

`key_hash` is OpenRouter's handle and the only way to revoke a key, so a local
row is deleted only *after* the upstream key is gone. A failed revoke keeps the
row and surfaces an error — losing the hash would leave a live credential
nobody can kill.

### Derivation is one call, or three

The default is **one call**: `POST /api/v1/keys`. A derived key already carries
everything the per-device model needs —

| Need | Field on the key | Guardrail required? |
|---|---|---|
| Per-device spend cap | `limit` + `limit_reset` | no |
| Per-device usage tracking | `usage`, `usage_daily/weekly/monthly` | no |
| Per-device revocation | `DELETE /keys/{hash}` | no |
| **Device can't call a model outside the list** | — | **yes** |

So a guardrail adds exactly one thing, and it isn't the expensive one: the spend
cap bounds the money either way, so skipping enforcement changes how much work
the capped dollars buy, not how many dollars are at risk. `enforce_models` is
therefore **off by default**; turn it on for devices you trust less than the cap
alone allows for.

The model list is still always sent to the device, enforcement or not — it is
what drives the profile files, and therefore what appears in Fleet's picker.

When `enforce_models` **is** on, `POST /api/v1/keys` has **no model field**, so
model restriction needs a separate Guardrail. A constrained device key is then:

1. `POST /api/v1/guardrails` — `{name, allowed_models, limit_usd, reset_interval}`
2. `POST /api/v1/keys` — `{name: "foxy-<device-id>", limit, expires_at}`
3. `POST /api/v1/guardrails/{id}/assignments/keys` — `{key_hashes: [hash]}`

Every partial failure leaves live upstream state and is unwound: a failed step 2
deletes the guardrail; a failed step 3 deletes **both**, because at that point a
live *unconstrained* key exists and shipping it would record a restriction that
isn't there.

> **Verified 2026-07-31** against the live API with a real provisioning key:
> **guardrails are available on a personal account** — `POST /guardrails`
> returned 201. The design's one open prerequisite is settled.
>
> Three details only the live run revealed, each of which was wrong in the first
> implementation (see `openrouter/live_contract_test.go`):
>
> - Assignment takes `{"key_hashes":[…]}` — plural, an array. The singular form
>   400s, and since a failed assignment is fatal, **every** derivation would
>   have failed.
> - A guardrail carrying `limit_usd` must also carry `reset_interval`; there is
>   no lifetime budget window. `/keys` is different — a `limit` with no
>   `limit_reset` is a valid lifetime cap on that key. The admin API now rejects
>   the impossible combination at save time.
> - `allowed_models` entries must be real model slugs; `openrouter/auto` is
>   rejected. The capability probe therefore sends a name-only guardrail, so its
>   answer can't be muddied by an unrelated validation error.
>
> `ErrGuardrailsUnavailable` (402/403/404) remains as a fallback but has not been
> observed. It is **fatal by default** — silently degrading would mint an
> unrestricted key; degrading is opt-in and additionally requires a spend cap.
> Re-run the probe against any new account via Accounts → *Test key*, or
> `POST /api/accounts/{id}/openrouter/check`.

### The allowlist is one source of truth

The admin edits one model list per account. It drives **both** the guardrail
(server-side enforcement) and which profile files each device writes
(client-side visibility), so the model picker can never offer something the key
would be rejected for.

Editing the policy revokes every key already derived from that account: each one
has the *old* policy baked into its own guardrail. Devices re-derive on their
next sync.

### Revocation actually revokes

A derived key talks to OpenRouter directly and never presents the device token.
Deleting the device row, stamping `disabled_at`, or dropping the grant therefore
does **nothing** to it on its own. Device revoke, suspend, and
withdraw-OpenRouter all explicitly call `DELETE /api/v1/keys/{hash}` — otherwise
"suspended" would leave a fully working credential spending on the pool.

### Device side: config files, not credentials

The agent writes what codex reads, into `$CODEX_HOME`:

- `config.toml` gains a `[model_providers.openrouter]` block, **edited in place**
  between sentinel comments. It is the user's file — model, features, project
  trust list — so it is never rewritten wholesale, and removal cuts exactly the
  sentinel interval.
- `or-<model>.config.toml`, one per allowed model. A provider block carries no
  model list and codex 0.145.0 hard-rejects an inline `[profiles.x]` table, so
  one file per model is the only way to make a model selectable. Fleet already
  scans these; adding a model is just another file.

Ownership is proven by a sentinel comment, never by filename: a file we didn't
write is never overwritten and never deleted, no matter how well its name
matches. A collision is reported and skipped rather than fatal.

No key reaches disk. `config.toml` names a command instead:

```toml
[model_providers.openrouter.auth]
command = "<foxy path>"
args = ["cred", "openrouter-token"]
```

`foxy-switcher cred openrouter-token` loops back to the local daemon, which
holds the key in memory, and prints it to stdout — codex reads the whole of
stdout as the bearer token, so the command prints that and nothing else, and
exits non-zero (never an empty token) when unauthorised. `env_key` was not an
option: codex is spawned by Fleet, an environment foxy cannot reach. The
loopback endpoint is loopback-gated and carries no auth of its own — running on
this machine *is* the authorisation, which is sound because codex runs here too.

### Not in the reconcile loop

Claude and Codex need a 5s tick because leases expire and tokens rotate
underneath them. An OpenRouter key does neither. Only the *authorisation* can
change, so the writer polls on a 5-minute interval. Losing the grant tears the
config down; a transient vault outage does **not** — that would drop codex's
provider mid-session over a blip.

## Authentication (Step 3) — device flow

The current daemon binds 127.0.0.1 with no auth. Vault on a public address needs auth. Chosen flow:

**First-run setup**: Vault's web UI (`/setup` page) prompts for a password the first time it's hit. One user, one password. Stored as bcrypt in a new `users` table.

**Pairing a device**:
1. Agent calls `POST /api/v1/devices/pair-init` (no auth) with `{ device_name, client_nonce }`.
2. Vault returns `{ user_code: "QXY7-92", verification_url: "https://vault.example.com/pair", expires_in: 600 }`.
3. Agent prints/displays the code + URL.
4. User opens the URL on any browser, logs in to vault with their password, enters the code, approves.
5. Agent polls `POST /api/v1/devices/pair-poll` with `{ client_nonce }` until vault returns `{ device_token: "<opaque>", device_id: "<uuid>" }` or `{ status: "pending" }`.
6. Agent persists `device_token` in `~/.foxy-switcher/agent-config.json` (mode 0600).

**Subsequent calls**: `Authorization: Bearer <device_token>`. Tokens never expire; user revokes them from the web UI's Devices page. Tokens are random 256-bit, stored as sha256 in `devices` table.

**Combined mode**: bypasses auth (in-process calls have no HTTP boundary). The web UI on combined mode runs over loopback as today, no login prompt.

**TLS**: Vault listens HTTP only. The deployment story is "put it behind caddy/traefik/nginx with TLS termination". This keeps the daemon itself free of cert management.

## Lease semantics (Step 4)

```
+------+   AcquireLease   +--------+
| free | --------+------> | leased |
+------+         |        +---+----+
   ^             |            |
   |             | ttl expired|
   |             v            |
   |        +---------+       |
   +--------| expired |<------+
   sweeper  +---------+  Release
```

Schema (landed in [server/store/auth.go](../server/store/auth.go)):

```sql
CREATE TABLE leases (
  id            TEXT    PRIMARY KEY,          -- 128-bit hex (vault/auth.NewID)
  account_id    INTEGER NOT NULL,
  device_id     TEXT    NOT NULL,
  acquired_at   INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL
);
CREATE UNIQUE INDEX leases_account_id_uniq ON leases (account_id);
CREATE INDEX leases_device_id  ON leases (device_id);
CREATE INDEX leases_expires_at ON leases (expires_at);
```

The `account_id` UNIQUE constraint is enforced absolutely — only one live row per account at a time. AcquireLease's transaction sweeps expired rows before its INSERT so a long-dead lease can't block the next acquire.

Rules:

- **One live lease per account.** `AcquireLease` for an already-leased account returns `vault.ErrLeaseLocked` (HTTP 409 from `/agent/v1/leases`) unless the caller is the same device — same-device acquire just refreshes the TTL on the existing row.
- **Multiple leases per device.** There is no unique constraint on `device_id`; a paired agent normally holds one Claude lease and one Codex lease concurrently.
- **`selector.Pick` excludes leased-elsewhere accounts.** `vault.InProc.Pick` calls `selector.PickWithFilter` with `store.IsAccountLeased` as the extra disqualifier, so a device that runs Pick never sees an account another device holds.
- **`refresh.Scheduler` skips leased accounts** *except* when remaining lifetime drops below `InUseFallbackThreshold` (15 min). This generalises today's per-account predicate — any device with a live lease suppresses scheduler-driven rotation. The fallback covers the "agent crashed holding a stale lease, nobody is rotating CC's keychain" case.
- **TTL**: default 60s ([credinject.DefaultLeaseTTL](../server/credinject/coordinator.go)). The reconcile loop renews on every tick (5s) so even a missed tick leaves comfortable headroom. The vault sweeper runs on a 30s timer in `main` and reclaims expired rows.
- **Coordinator bails on contention.** If the agent's `AcquireLease` returns `ErrLeaseLocked`, the reconcile aborts without writing the keychain. The next reconcile re-runs `choose`, which calls `Pick` (which now excludes the contested account) and lands on a different free candidate.
- **Token rotation single-source-of-truth**: vault schedulers perform provider refreshes while agents report rotations observed from the local CLIs. Provider-aware credential updates keep Codex's id token and metadata in sync with the common token columns.

Shutdown: when an agent exits gracefully, `RestoreOnShutdown` calls `ReleaseLease` so the account can be picked up by another device immediately rather than waiting for TTL.

## Frontend (Step 5)

Step 5 keeps the frontend untouched. Instead, `--mode=agent` is a transparent reverse proxy:

- Local listener on the same port the Tauri sidecar always used.
- `GET /healthz` and `GET /api/cred/status` served locally — status includes the locally injected Claude and Codex account IDs.
- Everything else under `/` proxies to the vault, with `Authorization: Bearer <device_token>` injected on every request. Streaming routes (the activity SSE) work because `httputil.ReverseProxy` already handles them.
- Upstream-unreachable failures surface as `502 {"error":"vault unreachable: …"}` so the React error toast renders cleanly.

This means the existing Tauri React build, the TUI, and any external scripts that hit `127.0.0.1:<port>/api/...` keep working without source changes. To pair a new agent, the user runs `foxy-switcher pair --vault-url=https://vault.example.com` once; the resulting `~/.foxy-switcher/agent-config.json` is what `--mode=agent` reads at startup.

Step 6 closes the auth gap on `/api/*`. When `--mode=vault` runs, main installs `httpserver.BearerAuth` as middleware on the frontend `httpapi.Server`, so every `/api/*` route requires a paired device's bearer token. Combined mode leaves the middleware slice empty — loopback is its own attacker model and existing local Tauri / TUI clients keep working without changes. `/healthz` is registered on the root mux above the wrap so liveness probes still succeed without credentials. The agent's reverse proxy already injects Bearer on every forwarded request, so an `--mode=agent` topology gets through transparently.

Step 9 ships an embedded React account panel: the vault binary ships the React build (via `//go:embed all:static`) and exposes it at `/app`, `/app/`, and `/assets/*`. The same React build also runs inside Tauri unchanged — `src/api.ts` detects whether it's hosted by Tauri (via `__TAURI_INTERNALS__`) and either calls Tauri commands or falls back to plain `window.location.origin` for API access. Tauri-only commands (autostart, reveal data dir, save agent config) reject with `NotInTauriError` in browser mode so the UI can render "this action is only available in the desktop app" cleanly instead of mistaking a missing host bridge for a network error.

Auth in browser mode uses the same Web UI session cookie the admin console (`/setup`, `/login`, `/devices`) already uses; vault's `/api/*` middleware now accepts either a Bearer token (agent path) or a `foxy_session` cookie (browser path).

The build pipeline (`scripts/build-server.sh`) bakes `dist/` into `server/vault/webapp/static/` before `go build`, so every cross-compiled sidecar travels with the React panel. A fresh checkout that hasn't run `pnpm build` yet still builds — `webapp.Available()` returns false at runtime and the vault home page falls back to the bare server-rendered template.

## Migration plan recap

Steps 1–9 have all landed. Each step kept the boundary stable, so combined-mode behaviour is identical to the pre-split daemon and the only files a Step N+1 needed to touch were the ones whose semantics actually changed.

## Migration plan

| Step | Scope | Risk | Verification |
|---|---|---|---|
| 1 | Define `vault.Service` (agent-facing subset), in-process impl, refactor `credinject.Coordinator` | Low — pure refactor | All existing tests pass; manual smoke of GUI |
| 2 | HTTP transport + `--mode` flag; widen Service to include lease + rotation reporting | Medium — new wire format | New integration test: spawn vault + agent, pick lease, inject |
| 3 | Device-flow auth | Medium — web UI + new tables | E2E pair flow test |
| 4 | Lease coordination | High — touches selector + refresh.Scheduler | Multi-device test (two agents one vault) |
| 5 | Frontend wiring + optional web UI | Low | Manual |

Each step lands in its own PR. After step 1, every subsequent step is "add another implementation of `Service`" or "add another concern around it" — the boundary stays stable.

## Open questions

- **Activity bus across the wire.** Today's `activity.Bus` lives in-process and is consumed by SSE handlers. In split mode, vault is the publisher; the agent and frontend are subscribers. The existing SSE handler at `/api/activity/stream` becomes the canonical wire format; in-process subscribers wrap it. No schema change.
- **Settings split.** Some settings are frontend-only (theme, sidebar mode). Some belong to the vault (poll interval, default thresholds). One belongs to the *agent* (`restore_native_on_quit` — it gates a local action). The new schema scopes those: vault stores vault settings; agent has its own small `agent-config.json` for restore_on_quit and vault_url.
- **Web UI shipping.** TBD whether to embed the React build inside the vault binary (`embed.FS`) or ship it as a separate static file directory. Lean toward embed for a single self-contained binary.
- ~~**OpenRouter guardrail availability.**~~ **Settled 2026-07-31**: guardrails work on a personal account (201 on create). The degraded path is retained for accounts where they turn out not to be, and `guardrail_enforced` on the grant reports which one applied so the UI never implies enforcement that isn't there.
- **Multi-account OpenRouter pools.** Several OpenRouter accounts may be configured, but selection is deliberately not the LRU selector — it is the lowest-id active, fully-configured account, so a device keeps deriving from the same one. "Several configured, the first wins", not a load-balanced pool. Spreading devices across accounts would only make a device's key hop on unrelated pool changes; if a real need appears, it wants its own design.
- **Backfill of existing local installs.** Combined mode reads the existing `~/.foxy-switcher/state.db` unchanged. There's no data migration — the schema is the same. Switching an existing user to vault mode means: stand up vault on a remote host, copy `state.db` over, point the agent at the URL.
