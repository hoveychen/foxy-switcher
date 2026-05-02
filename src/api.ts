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
  cooldown_until: number;
  last_used_at: number;
  last_429_at: number;
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

export type AutoSwitchPolicy = "lru" | "lowest" | "rr";

export interface AutoSwitchSettings {
  enabled: boolean;
  policy: AutoSwitchPolicy;
}

export type DaemonMode = "attached" | "owned";

export const getDaemonMode = (): Promise<DaemonMode> =>
  invoke<DaemonMode>("get_daemon_mode");

export const getServerPort = (): Promise<number> => getPort();

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
};
