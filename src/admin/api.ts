// Cloud vault admin SPA API client. All calls hit /admin/api/* on the
// same origin the bundle is served from, so we use relative paths and
// rely on the session cookie set by POST /admin/api/login. Returns
// parsed JSON or throws AdminApiError so pages can branch on
// status/code without sniffing response shapes.

export type AdminMe = {
  has_password: boolean;
  signed_in: boolean;
};

export type AdminDevice = {
  id: string;
  name: string;
  hostname?: string;
  os?: string;
  os_version?: string;
  arch?: string;
  model?: string;
  app_version?: string;
  client_type?: string;
  created_at: number;
  last_seen_at: number;
  // disabled_at is 0 for an active device and the suspend timestamp
  // (epoch ms) for a suspended one. When non-zero the device's token is
  // rejected until an admin resumes it (no re-pair needed).
  disabled_at: number;
  // allow_claude / allow_codex / allow_openrouter is the per-device provider
  // allowlist: which credential pools this device may lease/inject. Chosen at
  // approval and editable here. Devices paired before this feature are
  // claude-only.
  allow_claude: boolean;
  allow_codex: boolean;
  // OpenRouter is not leased — granting it lets the vault mint a derived,
  // spend-capped API key for this device; withdrawing it revokes that key
  // upstream immediately.
  allow_openrouter: boolean;
  // current_lease names the account this device is currently holding,
  // joined with account_name server-side. Absent when the device has no
  // live lease.
  current_lease?: AdminDeviceLease;
};

export type AdminDeviceLease = {
  account_id: number;
  account_name: string;
  acquired_at: number;
  expires_at: number;
};

export type AdminPairLookup = {
  code: string;
  device_name: string;
  status: "pending" | "approved" | "denied";
};

export class AdminApiError extends Error {
  status: number;
  // `code` mirrors the JSON `error` field — backend uses tokens like
  // `setup_required`, `unauthorized`, `wrong password` so callers can
  // branch on the string rather than parsing free-form text.
  code: string;
  constructor(status: number, code: string) {
    super(code);
    this.status = status;
    this.code = code;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, {
    credentials: "same-origin",
    headers: {
      "Content-Type": "application/json",
      ...(init?.headers ?? {}),
    },
    ...init,
  });
  if (resp.status === 204) {
    return undefined as T;
  }
  const text = await resp.text();
  let body: unknown = {};
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = { error: text };
    }
  }
  if (!resp.ok) {
    const code =
      typeof body === "object" && body !== null && "error" in body
        ? String((body as { error: unknown }).error)
        : `http_${resp.status}`;
    throw new AdminApiError(resp.status, code);
  }
  return body as T;
}

export const adminApi = {
  me: () => request<AdminMe>("/admin/api/me"),
  login: (password: string) =>
    request<{ ok: true }>("/admin/api/login", {
      method: "POST",
      body: JSON.stringify({ password }),
    }),
  logout: () =>
    request<void>("/admin/api/logout", { method: "POST" }),
  setup: (password: string, confirm: string) =>
    request<{ ok: true }>("/admin/api/setup", {
      method: "POST",
      body: JSON.stringify({ password, confirm }),
    }),
  listDevices: () =>
    request<{ devices: AdminDevice[] }>("/admin/api/devices"),
  revokeDevice: (id: string) =>
    request<void>("/admin/api/devices/revoke", {
      method: "POST",
      body: JSON.stringify({ id }),
    }),
  suspendDevice: (id: string) =>
    request<void>("/admin/api/devices/suspend", {
      method: "POST",
      body: JSON.stringify({ id }),
    }),
  resumeDevice: (id: string) =>
    request<void>("/admin/api/devices/resume", {
      method: "POST",
      body: JSON.stringify({ id }),
    }),
  renameDevice: (id: string, name: string) =>
    request<void>("/admin/api/devices/rename", {
      method: "POST",
      body: JSON.stringify({ id, name }),
    }),
  lookupPair: (code: string) =>
    request<AdminPairLookup>(
      `/admin/api/pair?code=${encodeURIComponent(code)}`,
    ),
  resolvePair: (
    code: string,
    action: "approve" | "deny",
    providers?: {
      allow_claude: boolean;
      allow_codex: boolean;
      allow_openrouter: boolean;
    },
  ) =>
    request<{ result: "approved" | "denied" }>("/admin/api/pair", {
      method: "POST",
      body: JSON.stringify({ code, action, ...providers }),
    }),
  setDeviceProviders: (
    id: string,
    allow_claude: boolean,
    allow_codex: boolean,
    allow_openrouter: boolean,
  ) =>
    request<void>("/admin/api/devices/providers", {
      method: "POST",
      body: JSON.stringify({ id, allow_claude, allow_codex, allow_openrouter }),
    }),
  changePassword: (current: string, next: string, confirm: string) =>
    request<{ ok: true }>("/admin/api/password", {
      method: "POST",
      body: JSON.stringify({ current, next, confirm }),
    }),
};
