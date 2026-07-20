import { invoke } from "@tauri-apps/api/core";

// inTauri reflects whether this React build is running inside the
// desktop shell. Step 9 lets the same React bundle also be served
// directly from the cloud vault's embedded /app surface; in that path
// `invoke` doesn't exist, the daemon is "attached" by definition, and
// every API call should target the same origin instead of a port the
// host shell would have resolved. Centralising the detection keeps the
// Tauri-only commands isolated to call sites that gracefully degrade
// when this is false.
const inTauri =
  typeof window !== "undefined" &&
  ("__TAURI_INTERNALS__" in window || "__TAURI__" in window);

// tauriHttp is the desktop transport: instead of letting the webview fetch
// http://127.0.0.1:<port> (which reqwest/WKWebView route through the system
// proxy — a global VPN / 翻墙 proxy that doesn't bypass loopback then breaks
// every request), it hands the request to the Rust `api_request` command,
// which talks to the sidecar with a `.no_proxy()` reqwest client over the IPC
// bridge. The webview makes no network call, so the proxy is out of the loop.
//
// It re-implements just enough of fetch() for api()/PairVaultModal: method,
// headers, string body in; a real Response (status + headers + body) out.
// Connection-level failures surface as a thrown invoke rejection, matching the
// old plugin-http behaviour so api()'s rediscover-and-retry catch still fires.
async function tauriHttp(
  input: RequestInfo | URL,
  init?: RequestInit,
): Promise<Response> {
  const href = typeof input === "string" ? input : input.toString();
  const u = new URL(href);
  // Send path+query only; Rust resolves the loopback port itself.
  const path = u.pathname + u.search;
  const method = (init?.method ?? "GET").toUpperCase();
  const headers: [string, string][] = [];
  new Headers(init?.headers).forEach((v, k) => headers.push([k, v]));
  let body: string | undefined;
  if (init?.body != null) {
    body = typeof init.body === "string" ? init.body : String(init.body);
  }

  const resp = await invoke<{
    status: number;
    headers: [string, string][];
    body: string;
  }>("api_request", { req: { method, path, headers, body } });

  const respHeaders = new Headers();
  for (const [k, v] of resp.headers) respHeaders.append(k, v);
  // The Response constructor rejects a body on null-body statuses (204/205/304
  // and 1xx); pass null there so an empty 204 doesn't throw.
  const nullBody =
    resp.status === 204 ||
    resp.status === 205 ||
    resp.status === 304 ||
    resp.status < 200;
  return new Response(nullBody ? null : resp.body, {
    status: resp.status,
    headers: respHeaders,
  });
}

// httpFetch picks the right transport. In the desktop shell we go through
// tauriHttp (Rust, proxy-immune). The vault deployment serves /app and /api on
// the same origin, so the browser's built-in fetch works without CORS concerns.
// Exported so PairVaultModal can use the same selector.
export const httpFetch: typeof fetch = inTauri
  ? (tauriHttp as typeof fetch)
  : (...args) => globalThis.fetch(...args);

let cachedPort: number | null = null;

async function getPort(): Promise<number> {
  if (cachedPort !== null) return cachedPort;
  if (!inTauri) {
    // Browser path: there's no sidecar lookup — the React build is
    // served from the same origin as the API, so "port" maps to
    // window.location.port (empty for default-port deployments).
    const p = window.location.port;
    const num = p === "" ? (window.location.protocol === "https:" ? 443 : 80) : Number(p);
    cachedPort = num;
    return num;
  }
  const port = await invoke<number>("get_server_port");
  cachedPort = port;
  return port;
}

// apiBase returns the origin every API call targets. In Tauri the
// sidecar listens on 127.0.0.1:<port>; in the browser the React build
// sits on the same origin as the vault, so we just use whatever the
// page was loaded from.
function apiBase(port: number): string {
  if (inTauri) {
    return `http://127.0.0.1:${port}`;
  }
  return window.location.origin;
}

async function api<T>(
  path: string,
  init?: RequestInit & { json?: unknown },
): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.json !== undefined) {
    headers.set("content-type", "application/json");
  }
  const body = init?.json !== undefined ? JSON.stringify(init.json) : init?.body;

  const doFetch = (port: number) =>
    httpFetch(`${apiBase(port)}${path}`, { ...init, headers, body });

  const firstPort = await getPort();
  let res: Response;
  try {
    res = await doFetch(firstPort);
  } catch (err) {
    // Connection-level failure (refused/reset/timeout) — typically the
    // attached daemon was restarted on a fresh port, leaving cachedPort
    // pointing at a dead listener. Drop the cache, re-invoke
    // get_server_port (which probes liveness and re-attaches via the port
    // file on the Rust side), and try once more. If the rediscovered port
    // matches what we just tried, the daemon really is down — surface the
    // original error so the disconnect banner shows.
    cachedPort = null;
    const freshPort = await getPort();
    if (freshPort === firstPort) throw err;
    res = await doFetch(freshPort);
  }
  if (!res.ok) {
    const text = await res.text().catch(() => "");
    throw new Error(`${res.status} ${res.statusText}: ${text}`);
  }
  const ct = res.headers.get("content-type") ?? "";
  if (ct.includes("application/json")) {
    return (await res.json()) as T;
  }
  return (await res.text()) as unknown as T;
}

export interface UsageWindow {
  utilization: number; // 0–100 percent
  resets_at: string; // RFC3339
}

export interface Account {
  id: number;
  provider: "claude" | "codex";
  in_use: boolean;
  name: string;
  status: string;
  // Derived flag: access_token has passed expires_at. Set server-side so the
  // UI doesn't have to re-implement the clock check (and selector / TUI /
  // desktop stay in lock-step on what counts as "expired").
  token_expired: boolean;
  account_uuid: string;
  organization_uuid: string;
  subscription_type: string;
  // Authoritative quota label from /api/oauth/profile.organization.rate_limit_tier:
  // "default_claude_pro" | "default_claude_max_5x" | "default_claude_max_20x".
  // Empty for legacy rows that haven't been backfilled by the next UsagePoller
  // tick. This is the only field that distinguishes personal Max 5x from Max
  // 20x; planWeight prefers it over subscription_type.
  rate_limit_tier: string;
  expires_at: number;
  last_used_at: number;
  scopes: string;
  created_at: number;
  updated_at: number;
  email: string;
  full_name: string;
  organization_name: string;
  plan: string;
  five_hour?: UsageWindow;
  seven_day?: UsageWindow;
  seven_day_sonnet?: UsageWindow;
  usage_fetched_at: number;
  // Per-window utilization thresholds (0–100). Schema default 95; 100 = no skip.
  five_hour_threshold: number;
  seven_day_threshold: number;
  seven_day_sonnet_threshold: number;
  // Per-account lease info populated by the vault when a remote agent is
  // currently using this account. Nil when no live lease exists.
  // - mine: true when the lease belongs to THIS daemon's device. The
  //   server resolves it from the BearerAuth ctx, so the frontend never
  //   has to know its own device_id; combined-mode requests have no
  //   auth context and get mine=true (loopback owner).
  // - device_id / device_name: only meaningful for foreign leases (badges
  //   render device_name in "in use by Device X"). Falls back to hostname
  //   when name is empty.
  // - acquired_at / expires_at are unix millis.
  lease?: AccountLease;
}

export interface AccountLease {
  device_id: string;
  device_name: string;
  mine: boolean;
  acquired_at: number;
  expires_at: number;
}

// DeviceShare is one device's estimated contribution to an account's usage,
// in utilization points (0–100, same unit as the bars) for the current
// 5h / 7d / 7d-sonnet window. Turn into a share % by dividing by the
// per-window total (sum of all devices + unattributed). The unattributed
// bucket has empty device_id / device_name.
export interface DeviceShare {
  device_id: string;
  device_name: string;
  five_hour: number;
  seven_day: number;
  seven_day_sonnet: number;
  // Approximate last time this device actually drove usage (unix millis).
  // Grounded in real utilization deltas, not the lease's acquired_at, so a
  // device that leased but never sent a request has no value here. Omitted
  // (undefined) when never observed and for the unattributed bucket.
  last_used_at?: number;
}

// AccountAttribution is the per-device breakdown for one account, returned by
// GET /api/accounts/{id}/attribution. devices is sorted by total contribution
// descending. sample_count < 2 means there isn't enough history yet to diff.
export interface AccountAttribution {
  account_id: number;
  devices: DeviceShare[];
  unattributed?: DeviceShare;
  sample_count: number;
  sample_start: number;
}

export interface ThresholdInput {
  five_hour: number;
  seven_day: number;
  seven_day_sonnet: number;
}

// accountIsCooling mirrors selector.exceedsThreshold in the Go daemon: true
// when any per-window utilization has reached the matching threshold. Use it
// everywhere the UI needs to flag accounts the selector would skip.
export function accountIsCooling(a: Account): boolean {
  const w = [
    [a.five_hour, a.five_hour_threshold] as const,
    [a.seven_day, a.seven_day_threshold] as const,
    [a.seven_day_sonnet, a.seven_day_sonnet_threshold] as const,
  ];
  for (const [u, t] of w) {
    if (u && u.utilization >= t) return true;
  }
  return false;
}

// accountResetAt returns the soonest future reset (unix ms) across the
// account's throttled windows, or 0 when no window is throttled / no
// throttled window has a future parseable resets_at. Pair with
// accountIsCooling to render "cooling — resets in N min".
export function accountResetAt(a: Account, now: Date = new Date()): number {
  const candidates: UsageWindow[] = [];
  if (a.five_hour && a.five_hour.utilization >= a.five_hour_threshold) {
    candidates.push(a.five_hour);
  }
  if (a.seven_day && a.seven_day.utilization >= a.seven_day_threshold) {
    candidates.push(a.seven_day);
  }
  if (
    a.seven_day_sonnet &&
    a.seven_day_sonnet.utilization >= a.seven_day_sonnet_threshold
  ) {
    candidates.push(a.seven_day_sonnet);
  }
  let best = 0;
  for (const c of candidates) {
    if (!c.resets_at) continue;
    const t = Date.parse(c.resets_at);
    if (Number.isNaN(t) || t <= now.getTime()) continue;
    if (best === 0 || t < best) best = t;
  }
  return best;
}

// PlanWeight is the multiplier an account contributes to the pool's aggregated
// quota relative to a Pro baseline. Anthropic doesn't publish raw quota
// numbers, so we model the pool as "Pro-equivalents": a Pro seat is 1, a
// Max 5x is 5, a Max 20x is 20 (5h) / 10 (7d). The 5h and 7d ratios diverge
// upstream, hence the two fields.
//
// Primary key is rate_limit_tier (organization.rate_limit_tier from the
// OAuth profile) — that's the only field that tells personal Max 5x apart
// from Max 20x, and it reveals when a Team Premium org actually has 5x
// limits (one observed Team Premium org reports default_claude_max_5x, not
// 20x). subscription_type is only used as a legacy fallback for rows whose
// tier hasn't been backfilled yet. Mirrors server/httpapi:planWeight.
export interface PlanWeight {
  five_hour: number;
  seven_day: number;
}

export function planWeight(
  rateLimitTier: string,
  subscriptionType: string,
): PlanWeight {
  switch (rateLimitTier) {
    case "default_claude_pro":
      return { five_hour: 1, seven_day: 1 };
    case "default_claude_max_5x":
      return { five_hour: 5, seven_day: 5 };
    case "default_claude_max_20x":
      return { five_hour: 20, seven_day: 10 };
  }
  // Legacy fallback for rows whose tier hasn't been backfilled.
  // Conservatively assume Max=5x and Team Premium=5x (the observed value
  // at our example org), erring on the side of under-stating rather than
  // over-stating the pool.
  switch (subscriptionType) {
    case "pro":
      return { five_hour: 1, seven_day: 1 };
    case "max":
      return { five_hour: 5, seven_day: 5 };
    case "team":
      return { five_hour: 5, seven_day: 5 };
    case "team_premium":
      return { five_hour: 5, seven_day: 5 };
    default:
      return { five_hour: 0, seven_day: 0 };
  }
}

// PoolWindowTotals are the weighted-sum aggregates for one usage window
// across the live pool. Used is in Pro-equivalents, so used/capacity gives
// the pool's effective utilization. Capacity is the sum of weights; for a
// pool with one Pro and one Max5x, capacity for 5h is 6.
export interface PoolWindowTotals {
  used: number;
  capacity: number;
  percent: number; // used / capacity * 100, 0 when capacity is 0
}

export function poolWindowTotals(
  accounts: Account[],
  window: "five_hour" | "seven_day",
): PoolWindowTotals {
  let used = 0;
  let capacity = 0;
  for (const a of accounts) {
    if (a.status !== "active") continue;
    const w = planWeight(a.rate_limit_tier, a.subscription_type)[window];
    if (w <= 0) continue;
    capacity += w;
    const u = a[window]?.utilization;
    if (typeof u === "number") {
      used += (Math.max(0, Math.min(100, u)) / 100) * w;
    }
  }
  const percent = capacity > 0 ? (used / capacity) * 100 : 0;
  return { used, capacity, percent };
}

export type AutoSwitchPolicy = "lru" | "lowest" | "rr";

export interface AutoSwitchSettings {
  enabled: boolean;
  policy: AutoSwitchPolicy;
}

export type Theme = "system" | "light" | "dark";
export type SidebarMode = "expanded" | "auto";

export interface Settings {
  theme: Theme;
  sidebar_mode: SidebarMode;
  // 30–300; server clamps. Applies on next daemon restart.
  usage_poll_interval_sec: number;
  // Pool-wide per-window defaults for new accounts (0–100; server clamps).
  // The "apply to all accounts" action writes these across existing rows.
  default_five_hour: number;
  default_seven_day: number;
  default_seven_day_sonnet: number;
  restore_native_on_quit: boolean;
}

export type ActivitySeverity = "info" | "warn" | "error";

// Mirrors server/activity.Event. Payload is rendered verbatim when present —
// keep it small and JSON-shaped (the bus persists it as TEXT).
export interface ActivityEvent {
  id: number;
  timestamp: number; // unix milliseconds
  type: string;
  severity: ActivitySeverity;
  account_id?: number;
  message: string;
  payload?: unknown;
}

export interface ActivityFilter {
  limit?: number;
  since?: number;
  // Comma-joined list; supports the "error.*" wildcard suffix the server honors.
  type?: string;
  severity?: ActivitySeverity;
}

export interface InUseEntry {
  account_id: number;
  device_id: string;
  device_name: string;
  // Mine === true when the lease belongs to the caller's device (resolved
  // server-side from BearerAuth ctx; combined mode → true).
  mine: boolean;
  expires_at: number;
}

export interface DashboardKPIs {
  pool_size: number;
  active_count: number;
  // Every active lease, joined with the holding device's display name.
  // Replaces the legacy single in_use_account_id — multi-device
  // deployments see one entry per holder; empty list when no agent
  // currently holds any account.
  in_use: InUseEntry[];
  // Number of accounts whose latest usage poll showed at least one window
  // at or above its threshold (i.e. the daemon's selector would skip them).
  cooling_count: number;
  // Soonest reset (unix ms) across throttled windows of cooling accounts.
  // 0 when no account is cooling or no throttled window has a parseable
  // resets_at.
  next_reset_at: number;
  peak_util_percent: number;
}

export interface DashboardTrendBucket {
  ts: number;
  five_hour: number;
  seven_day: number;
  seven_day_sonnet: number;
  // Weighted pool aggregates per bucket. used and capacity are in
  // Pro-equivalents (see planWeight); percent is used/capacity*100 with 0
  // when capacity is 0. Older daemons omit these fields — treat 0 as "no
  // data" and fall back to the legacy max-utilization lines.
  five_hour_used?: number;
  five_hour_capacity?: number;
  five_hour_pct?: number;
  seven_day_used?: number;
  seven_day_capacity?: number;
  seven_day_pct?: number;
}

export interface DashboardResponse {
  kpis: DashboardKPIs;
  trend: DashboardTrendBucket[];
}

export interface AboutResponse {
  version: string;
  commit: string;
  commit_dirty: boolean;
  build_time: string;
  go_version: string;
  os: string;
  arch: string;
  pid: number;
  port: number;
  data_dir: string;
  sqlite_path: string;
  sqlite_size_b: number;
  started_at_ms: number;
  uptime_seconds: number;
  // Cloud-vault deployment topology. Empty / "combined" = local-only;
  // "vault" = headless cloud server; "agent" = local credinject talking
  // to a remote vault. Settings page renders the Vault card off this.
  mode?: string;
  // Upstream URL when mode === "agent"; empty otherwise.
  vault_url?: string;
  // Device id paired against the upstream vault. Set in agent mode.
  device_id?: string;
}

export type DaemonMode = "attached" | "owned";

// isTauriHost lets UI surfaces hide / disable Tauri-only buttons (reveal
// data dir, restart daemon, autostart toggle, save agent config) when
// the same React build runs in the browser via the vault's /app embed.
export const isTauriHost = (): boolean => inTauri;

// NotInTauriError marks the deliberate browser-mode rejection. Callers
// should catch this distinctly from network errors so they can render
// "this action is only available in the desktop app" rather than
// "request failed".
export class NotInTauriError extends Error {
  constructor(action: string) {
    super(`${action} is only available in the desktop app`);
    this.name = "NotInTauriError";
  }
}

export const getDaemonMode = async (): Promise<DaemonMode> => {
  // In a browser session there's no Tauri-owned sidecar — the daemon
  // is by definition "attached" (managed elsewhere, we just connect).
  if (!inTauri) return "attached";
  return invoke<DaemonMode>("get_daemon_mode");
};

export const getServerPort = (): Promise<number> => getPort();

// Returns the new port. Errors if the daemon is in attached mode (we don't
// own its lifecycle) or the spawn fails — surface the message in the
// disconnect banner. The port cache is updated to the new value so the next
// api() call hits the freshly-spawned daemon instead of the dead one.
export const restartDaemon = async (): Promise<number> => {
  if (!inTauri) throw new NotInTauriError("restart daemon");
  const port = await invoke<number>("restart_daemon");
  cachedPort = port;
  return port;
};

// Launch-at-login wrappers. The platform-specific work (LaunchAgent /
// Run key / .desktop file) lives entirely in tauri-plugin-autostart; we
// just relay the user's toggle through to it.
export const autostartIsEnabled = async (): Promise<boolean> => {
  if (!inTauri) return false;
  return invoke<boolean>("autostart_is_enabled");
};
export const autostartSet = async (enabled: boolean): Promise<void> => {
  if (!inTauri) throw new NotInTauriError("autostart");
  return invoke<void>("autostart_set", { enabled });
};

// Settings § General — show the data dir and offer Reveal in Finder.
export const dataDirPath = async (): Promise<string> => {
  if (!inTauri) return "";
  return invoke<string>("data_dir_path");
};
export const revealDataDir = async (): Promise<void> => {
  if (!inTauri) throw new NotInTauriError("reveal data directory");
  return invoke<void>("reveal_data_dir");
};

// saveAgentConfig writes the device-flow result to
// ~/.foxy-switcher/agent-config.json. The next daemon launched with
// --mode=agent reads this file to learn vault URL + bearer token. Returns
// the file path so the UI can show "saved to …" feedback.
export const saveAgentConfig = async (params: {
  vault_url: string;
  device_id: string;
  device_token: string;
}): Promise<string> => {
  if (!inTauri) throw new NotInTauriError("save agent config");
  return invoke<string>("save_agent_config", {
    vaultUrl: params.vault_url,
    deviceId: params.device_id,
    deviceToken: params.device_token,
  });
};

// clearAgentConfig deletes ~/.foxy-switcher/agent-config.json. The next
// daemon launch (after restartDaemon) implicitly falls back to combined
// mode via detectModeFromConfig. Used by Settings → Vault "Unpair".
// Missing file is treated as success on the Rust side.
export const clearAgentConfig = async (): Promise<void> => {
  if (!inTauri) throw new NotInTauriError("clear agent config");
  return invoke<void>("clear_agent_config");
};

// openExternal hands a URL off to the OS default browser. Tauri's webview
// silently drops <a target="_blank">, so every external link in the UI
// has to call this instead. In a browser host (vault server's /app embed)
// we fall back to window.open.
export const openExternal = async (url: string): Promise<void> => {
  if (!url) return;
  if (inTauri) {
    try {
      await invoke<void>("open_external_url", { url });
      return;
    } catch (e) {
      console.warn("open_external_url failed", e);
    }
  }
  if (typeof window !== "undefined") {
    window.open(url, "_blank", "noopener,noreferrer");
  }
};

// VersionCheckResult mirrors the Rust struct in src-tauri/src/version_check.rs.
// `latest_version` is empty when the GitHub fetch failed; the UI treats that
// as "no update" and renders nothing.
export interface VersionCheckResult {
  current_version: string;
  latest_version: string;
  has_update: boolean;
  release_url: string;
}

// checkAppVersion asks the Rust side to consult the GitHub Releases API
// (with a 1-day on-disk cache). `force=true` bypasses the cache — used by
// the Help → Check for Updates menu and the Settings "Check now" button so
// manual triggers always go to the network. In a browser host there is no
// updater story (the vault server delivers its own bundle on refresh), so
// we return null and callers skip the banner.
export const checkAppVersion = async (force = false): Promise<VersionCheckResult | null> => {
  if (!inTauri) return null;
  return invoke<VersionCheckResult>("check_app_version", { force });
};

// VaultDevice is the wire shape /api/devices returns. Mirrors deviceView
// in server/httpapi/routes.go. Hostname/OS/Model/etc. are empty for
// devices paired before the device-meta migration shipped.
export interface VaultDevice {
  id: string;
  name: string;
  hostname: string;
  os: string;
  os_version: string;
  arch: string;
  model: string;
  app_version: string;
  client_type: string;
  created_at: number;
  last_seen_at: number;
}

// PairMeta is the optional device-meta payload published during pair-init
// so the vault's /devices admin page can show real hostname / OS / model
// instead of just a self-chosen device_name. Mirrors the shape Go side
// (deviceinfo.Info → httpclient.PairMetadata) sends.
export interface PairMeta {
  hostname?: string;
  os?: string;
  os_version?: string;
  arch?: string;
  model?: string;
  app_version?: string;
  client_type?: string;
}

// getDeviceInfo invokes the Tauri-side get_device_info command. Browser
// builds have no equivalent (navigator.platform is too coarse), so we
// return undefined there — pairInit accepts a missing meta and the
// vault treats the columns as empty. Failures are swallowed for the
// same reason: a partial fingerprint is preferable to a refused pair.
export const getDeviceInfo = async (): Promise<PairMeta | undefined> => {
  if (!inTauri) return undefined;
  try {
    return await invoke<PairMeta>("get_device_info");
  } catch {
    return undefined;
  }
};

export const apiClient = {
  listAccounts: () =>
    api<{ accounts: Account[] }>("/api/accounts").then((r) => r.accounts),

  startLogin: () =>
    api<{ authorize_url: string; state: string }>("/api/accounts/login", {
      method: "POST",
    }),

  finishLogin: (state: string, pasted: string) =>
    api<{ account: Account }>("/api/accounts/callback", {
      method: "POST",
      json: { state, pasted },
    }),

  importCodex: () =>
    api<{ account: Account }>("/api/accounts/import-codex", {
      method: "POST",
    }),

  // Codex OAuth login (authorize + paste): start returns the authorize URL the
  // user opens plus the state we round-trip. Mirrors startLogin/finishLogin for
  // Claude. Unlike importCodex it needs no local Codex CLI, so it works in the
  // vault web UI; unlike the old device-code flow it doesn't depend on the
  // workspace's device-code toggle.
  startCodexLogin: () =>
    api<{ authorize_url: string; state: string }>("/api/accounts/codex-login", {
      method: "POST",
    }),

  finishCodexLogin: (state: string, pasted: string) =>
    api<{ account: Account }>("/api/accounts/codex-login/callback", {
      method: "POST",
      json: { state, pasted },
    }),

  refreshAccount: (id: number) =>
    api<{ account: Account }>(`/api/accounts/${id}/refresh`, { method: "POST" }),

  selectAccount: (id: number) =>
    api<void>(`/api/accounts/${id}/select`, { method: "POST" }),

  setPaused: (id: number, paused: boolean) =>
    api<void>(`/api/accounts/${id}/${paused ? "pause" : "resume"}`, {
      method: "POST",
    }),

  setThresholds: (id: number, t: ThresholdInput) =>
    api<void>(`/api/accounts/${id}/thresholds`, {
      method: "POST",
      json: t,
    }),

  attribution: (id: number) =>
    api<AccountAttribution>(`/api/accounts/${id}/attribution`),

  deleteAccount: (id: number) =>
    api<void>(`/api/accounts/${id}`, { method: "DELETE" }),

  credStatus: () =>
    api<{
      managed_account_id: number;
      codex_managed_account_id?: number;
      native_backup_present: boolean;
      injected_at: number;
      marker_state: "intact" | "overwritten" | "missing" | "sidecar-missing" | "unknown" | "";
    }>("/api/cred/status"),

  getAutoSwitch: () => api<AutoSwitchSettings>("/api/auto-switch"),

  setAutoSwitch: (v: AutoSwitchSettings) =>
    api<AutoSwitchSettings>("/api/auto-switch", { method: "POST", json: v }),

  getSettings: () => api<Settings>("/api/settings"),
  setSettings: (v: Partial<Settings>) =>
    api<Settings>("/api/settings", { method: "PUT", json: v }),
  // Overwrites every account's per-window thresholds with the currently
  // persisted defaults; returns how many accounts were updated. Save settings
  // first — the server applies what's persisted, not the in-flight form state.
  applyThresholdDefaults: () =>
    api<{ updated: number }>("/api/settings/apply-thresholds", {
      method: "POST",
    }),

  getDashboard: () => api<DashboardResponse>("/api/dashboard"),

  getAbout: () => api<AboutResponse>("/api/about"),

  // Vault pairing goes through the local daemon so the Tauri http-plugin
  // scope (which only allows http://127.0.0.1:* + localhost) doesn't have
  // to be widened to every possible vault host. The daemon's handler at
  // /api/pair/init / /api/pair/poll forwards to vault and returns the
  // device-flow envelope verbatim.
  pairInit: (
    vault_url: string,
    device_name: string,
    client_nonce: string,
    device_meta?: PairMeta,
  ) =>
    api<{
      user_code: string;
      verification_url: string;
      expires_in_ms: number;
    }>("/api/pair/init", {
      method: "POST",
      json: device_meta
        ? { vault_url, device_name, client_nonce, device_meta }
        : { vault_url, device_name, client_nonce },
    }),

  pairPoll: (vault_url: string, client_nonce: string) =>
    api<{
      status: "pending" | "approved" | "denied" | "expired";
      device_id?: string;
      device_token?: string;
    }>("/api/pair/poll", {
      method: "POST",
      json: { vault_url, client_nonce },
    }),

  listDevices: () =>
    api<{ devices: VaultDevice[] }>("/api/devices").then((r) => r.devices),

  // Wipes state.db and exits the daemon. The Tauri shell respawns the
  // sidecar; in attached mode the caller must restart their own daemon.
  // The cached port is invalidated so the next api() call rediscovers the
  // freshly-spawned daemon.
  resetData: async () => {
    await api<void>("/api/reset", { method: "POST" });
    cachedPort = null;
  },

  listActivity: (filter: ActivityFilter = {}) => {
    const q = new URLSearchParams();
    if (filter.limit) q.set("limit", String(filter.limit));
    if (filter.since) q.set("since", String(filter.since));
    if (filter.type) q.set("type", filter.type);
    if (filter.severity) q.set("severity", filter.severity);
    const suffix = q.toString();
    return api<{ events: ActivityEvent[] }>(
      `/api/activity${suffix ? `?${suffix}` : ""}`,
    ).then((r) => r.events);
  },
};
