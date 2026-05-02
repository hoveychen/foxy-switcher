# Cloud Vault — Design

This doc captures the plan to split the existing single-binary daemon into a **Vault** service (token storage + refresh + usage + account selection, runnable in the cloud) and an **Agent** service (credential injection into Claude Code's keychain on the user's local machine). The current single-process behavior remains the default deployment mode (Vault + Agent inside one binary, talking via in-process calls).

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
| `--mode=agent --vault-url=https://…` | credinject.Coordinator + reverse-sync + OAuth callback receiver + HTTP client to vault. **No** local store, no scheduler, no usage poller. |

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
|  credinject +    |
|  reverse-sync +  |
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
    Pick(ctx, now time.Time) (*Account, error)        // selector.Pick wrapper
    MarkUsed(ctx, accountID int64) error
    UpdateTokens(ctx, accountID, accessToken, refreshToken, expiresAt) error
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
| `POST /agent/v1/pick` | Returns the next eligible account (selector.Pick), or 204 |
| `POST /agent/v1/accounts/{id}/used` | MarkUsed |
| `POST /agent/v1/accounts/{id}/tokens` | UpdateTokens (agent reports CC-rotated tokens) |
| `POST /agent/v1/leases` | AcquireLease — body: `{ account_id, device_id, ttl_ms }` |
| `POST /agent/v1/leases/{id}/renew` | RenewLease — body: `{ ttl_ms }` |
| `DELETE /agent/v1/leases/{id}` | ReleaseLease |

Frontend routes that previously appeared on this list (login, refresh-now, settings, dashboard, activity SSE) stay where they are under `/api/*`. The frontend talks to vault directly over HTTP regardless of deployment topology, so abstracting them through `vault.Service` would just create a parallel call path with no callers. They're documented in [architecture.md](architecture.md) and route definitions live in [server/httpapi/routes.go](../server/httpapi/routes.go).

The agent surface is also exposed in `--mode=combined`: a second device on the same LAN can `--mode=agent` against the local daemon. That's mostly a debugging convenience — the canonical multi-device topology is one cloud `--mode=vault` plus N local agents.

Agent's reconcile loop (replaces today's `Coordinator.choose`):

1. On startup: `POST /lease/pick` to acquire the next account.
2. Inject into keychain.
3. Subscribe to `/events`. On `account.changed` for the leased account, re-read tokens (vault has rotated them) and re-inject.
4. Periodically `RenewLease` (every TTL/3).
5. On `switch.requested` (e.g. user clicked "Use now" for a different account on the web UI): release current lease, pick again.
6. Reverse-sync: every 30s read keychain blob; if access_token changed and doesn't match the last lease snapshot, `POST /lease/{lease_id}/rotation`.

The HTTP boundary preserves today's contract: `combined` mode runs the same handlers but skips the network hop.

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
- **`selector.Pick` excludes leased-elsewhere accounts.** `vault.InProc.Pick` calls `selector.PickWithFilter` with `store.IsAccountLeased` as the extra disqualifier, so a device that runs Pick never sees an account another device holds.
- **`refresh.Scheduler` skips leased accounts** *except* when remaining lifetime drops below `InUseFallbackThreshold` (15 min). This generalises today's per-account predicate — any device with a live lease suppresses scheduler-driven rotation. The fallback covers the "agent crashed holding a stale lease, nobody is rotating CC's keychain" case.
- **TTL**: default 60s ([credinject.DefaultLeaseTTL](../server/credinject/coordinator.go)). The reconcile loop renews on every tick (5s) so even a missed tick leaves comfortable headroom. The vault sweeper runs on a 30s timer in `main` and reclaims expired rows.
- **Coordinator bails on contention.** If the agent's `AcquireLease` returns `ErrLeaseLocked`, the reconcile aborts without writing the keychain. The next reconcile re-runs `choose`, which calls `Pick` (which now excludes the contested account) and lands on a different free candidate.
- **Token rotation single-source-of-truth**: only Vault's refresh.Scheduler (and explicit `RefreshNow`) call `authz.RefreshToken`. Agents only report CC-side rotations via `UpdateTokens`, which writes the new tokens into the store without re-issuing them. This eliminates the refresh_token race entirely — Vault serialises via the existing `refresh.Scheduler` per-account mutex, agents never hit the OAuth endpoint.

Shutdown: when an agent exits gracefully, `RestoreOnShutdown` calls `ReleaseLease` so the account can be picked up by another device immediately rather than waiting for TTL.

## Frontend (Step 5)

Tauri Settings page gains a single field: **Vault URL**.

- Empty → embedded combined-mode daemon (today's behavior).
- Non-empty → frontend points its API client at the remote vault, sidecar runs in `--mode=agent --vault-url=…`.

Optionally vault ships a copy of the React build under `/web/` so a user without Tauri can manage accounts from any browser. The web UI is functionally identical except it can't show "currently injected on *this* machine" (no agent context) — it shows lease state across all paired devices.

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
- **Backfill of existing local installs.** Combined mode reads the existing `~/.foxy-switcher/state.db` unchanged. There's no data migration — the schema is the same. Switching an existing user to vault mode means: stand up vault on a remote host, copy `state.db` over, point the agent at the URL.
