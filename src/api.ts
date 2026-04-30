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
  const port = await getPort();
  const url = `http://127.0.0.1:${port}${path}`;
  const headers = new Headers(init?.headers);
  if (init?.json !== undefined) {
    headers.set("content-type", "application/json");
  }
  const res = await fetch(url, {
    ...init,
    headers,
    body: init?.json !== undefined ? JSON.stringify(init.json) : init?.body,
  });
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
}

export const apiClient = {
  listAccounts: () =>
    api<{ accounts: Account[] }>("/api/accounts").then((r) => r.accounts),

  startLogin: () =>
    api<{ authorize_url: string; state: string }>("/api/accounts/login", {
      method: "POST",
    }),

  finishLogin: (state: string, pasted: string, name: string) =>
    api<{ account: Account }>("/api/accounts/callback", {
      method: "POST",
      json: { state, pasted, name },
    }),

  refreshAccount: (id: number) =>
    api<{ account: Account }>(`/api/accounts/${id}/refresh`, { method: "POST" }),

  setEnabled: (id: number, enabled: boolean) =>
    api<void>(`/api/accounts/${id}/${enabled ? "enable" : "disable"}`, {
      method: "POST",
    }),

  deleteAccount: (id: number) =>
    api<void>(`/api/accounts/${id}`, { method: "DELETE" }),

  fetchToken: () => api<string>("/api/token"),
};

export async function installHook(): Promise<string> {
  return invoke<string>("install_hook");
}

export async function uninstallHook(purge: boolean): Promise<string> {
  return invoke<string>("uninstall_hook", { purge });
}

export async function isHookInstalled(): Promise<boolean> {
  return invoke<boolean>("is_hook_installed");
}
