import { invoke } from "@tauri-apps/api/core";
import { fetch } from "@tauri-apps/plugin-http";

let cachedPort: number | null = null;

async function getPort(): Promise<number> {
  if (cachedPort !== null) return cachedPort;
  const port = await invoke<number>("get_server_port");
  cachedPort = port;
  return port;
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
    fetch(`http://127.0.0.1:${port}${path}`, { ...init, headers, body });

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
  name: string;
  status: string;
  // Derived flag: access_token has passed expires_at. Set server-side so the
  // UI doesn't have to re-implement the clock check (and selector / TUI /
  // desktop stay in lock-step on what counts as "expired").
  token_expired: boolean;
  organization_uuid: string;
  subscription_type: string;
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
  default_threshold_percent: number; // 0–100; server clamps
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

export interface DashboardKPIs {
  pool_size: number;
  active_count: number;
  in_use_account_id: number;
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
}

export type DaemonMode = "attached" | "owned";

export const getDaemonMode = (): Promise<DaemonMode> =>
  invoke<DaemonMode>("get_daemon_mode");

export const getServerPort = (): Promise<number> => getPort();

// Returns the new port. Errors if the daemon is in attached mode (we don't
// own its lifecycle) or the spawn fails — surface the message in the
// disconnect banner. The port cache is updated to the new value so the next
// api() call hits the freshly-spawned daemon instead of the dead one.
export const restartDaemon = async (): Promise<number> => {
  const port = await invoke<number>("restart_daemon");
  cachedPort = port;
  return port;
};

// Launch-at-login wrappers. The platform-specific work (LaunchAgent /
// Run key / .desktop file) lives entirely in tauri-plugin-autostart; we
// just relay the user's toggle through to it.
export const autostartIsEnabled = (): Promise<boolean> =>
  invoke<boolean>("autostart_is_enabled");
export const autostartSet = (enabled: boolean): Promise<void> =>
  invoke<void>("autostart_set", { enabled });

// Settings § General — show the data dir and offer Reveal in Finder.
export const dataDirPath = (): Promise<string> => invoke<string>("data_dir_path");
export const revealDataDir = (): Promise<void> => invoke<void>("reveal_data_dir");

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

  deleteAccount: (id: number) =>
    api<void>(`/api/accounts/${id}`, { method: "DELETE" }),

  credStatus: () =>
    api<{
      managed_account_id: number;
      native_backup_present: boolean;
      injected_at: number;
    }>("/api/cred/status"),

  getAutoSwitch: () => api<AutoSwitchSettings>("/api/auto-switch"),

  setAutoSwitch: (v: AutoSwitchSettings) =>
    api<AutoSwitchSettings>("/api/auto-switch", { method: "POST", json: v }),

  getSettings: () => api<Settings>("/api/settings"),
  setSettings: (v: Partial<Settings>) =>
    api<Settings>("/api/settings", { method: "PUT", json: v }),

  getDashboard: () => api<DashboardResponse>("/api/dashboard"),

  getAbout: () => api<AboutResponse>("/api/about"),

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
