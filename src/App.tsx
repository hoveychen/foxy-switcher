import { useCallback, useEffect, useState } from "react";
import { Account, UsageWindow, apiClient } from "./api";

type LoginState =
  | { phase: "idle" }
  | { phase: "started"; state: string; authorizeUrl: string }
  | { phase: "submitting" }
  | { phase: "error"; message: string };

function fmtRemaining(ms: number): string {
  if (ms <= 0) return "expired";
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function fmtResetsAt(rfc3339: string, nowMs: number): string {
  if (!rfc3339) return "—";
  const t = Date.parse(rfc3339);
  if (Number.isNaN(t)) return rfc3339;
  const diff = t - nowMs;
  if (diff <= 0) return "rolling over";
  return `resets in ${fmtRemaining(diff)}`;
}

function statusBadge(a: Account, nowMs: number): { text: string; tone: string } {
  if (a.status !== "active") return { text: a.status, tone: "muted" };
  if (a.cooldown_until > nowMs) {
    return {
      text: `cooldown ${fmtRemaining(a.cooldown_until - nowMs)}`,
      tone: "warn",
    };
  }
  if (a.expires_at - nowMs < 5 * 60 * 1000) {
    return { text: "refresh due", tone: "warn" };
  }
  return { text: "active", tone: "ok" };
}

function UsageBar({
  label,
  win,
  nowMs,
}: {
  label: string;
  win: UsageWindow | undefined;
  nowMs: number;
}) {
  if (!win) {
    return (
      <div className="usage-row">
        <span className="usage-label">{label}</span>
        <span className="usage-empty">—</span>
      </div>
    );
  }
  const pct = Math.max(0, Math.min(100, win.utilization));
  const tone = pct >= 90 ? "danger" : pct >= 75 ? "warn" : "ok";
  return (
    <div className="usage-row">
      <span className="usage-label">{label}</span>
      <div className={`usage-track ${tone}`}>
        <div className="usage-fill" style={{ width: `${pct}%` }} />
      </div>
      <span className="usage-pct">{pct.toFixed(1)}%</span>
      <span className="usage-resets">{fmtResetsAt(win.resets_at, nowMs)}</span>
    </div>
  );
}

export default function App() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [hookInstalled, setHookInstalled] = useState<boolean>(false);
  const [loginState, setLoginState] = useState<LoginState>({ phase: "idle" });
  const [pasted, setPasted] = useState("");
  const [accountName, setAccountName] = useState("");
  const [now, setNow] = useState<number>(Date.now());
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const [list, hook] = await Promise.all([
        apiClient.listAccounts(),
        apiClient.hookStatus(),
      ]);
      setAccounts(list);
      setHookInstalled(hook.installed);
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }, []);

  useEffect(() => {
    refresh();
    const i = setInterval(refresh, 5000);
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => {
      clearInterval(i);
      clearInterval(t);
    };
  }, [refresh]);

  async function startLogin() {
    setError(null);
    setCopied(false);
    try {
      const r = await apiClient.startLogin();
      setLoginState({
        phase: "started",
        state: r.state,
        authorizeUrl: r.authorize_url,
      });
    } catch (e) {
      setLoginState({ phase: "error", message: String(e) });
    }
  }

  async function copyAuthorizeUrl(url: string) {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch (e) {
      setError(String(e));
    }
  }

  async function finishLogin() {
    if (loginState.phase !== "started") return;
    setLoginState({ phase: "submitting" });
    try {
      await apiClient.finishLogin(
        loginState.state,
        pasted.trim(),
        accountName.trim() || `account-${Date.now()}`,
      );
      setPasted("");
      setAccountName("");
      setLoginState({ phase: "idle" });
      await refresh();
    } catch (e) {
      setLoginState({ phase: "error", message: String(e) });
    }
  }

  async function onRefreshAccount(id: number) {
    try {
      await apiClient.refreshAccount(id);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  }

  async function onDeleteAccount(id: number) {
    if (!confirm("Delete this account from the pool?")) return;
    try {
      await apiClient.deleteAccount(id);
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  }

  return (
    <main className="app">
      <header className="hdr">
        <h1>foxy-switcher</h1>
        <div className={`hook ${hookInstalled ? "ok" : "off"}`}>
          {hookInstalled ? "Hook installed" : "Hook not installed"}
        </div>
      </header>

      {error && <div className="banner err">{error}</div>}

      <section>
        <div className="row">
          <h2>Accounts ({accounts.length})</h2>
          <button onClick={startLogin}>+ Add account</button>
        </div>
        {accounts.length === 0 && (
          <p className="muted">
            Pool is empty. Click "Add account" to start the OAuth flow.
          </p>
        )}
        <ul className="accounts">
          {accounts.map((a) => {
            const b = statusBadge(a, now);
            const ownerLine =
              a.full_name && a.email
                ? `${a.full_name} <${a.email}>`
                : a.email || a.full_name || "—";
            return (
              <li key={a.id}>
                <div className="acc-main">
                  <div className="acc-title">
                    <strong>{a.name}</strong>
                    {a.plan && (
                      <span className="plan-tag">{a.plan}</span>
                    )}
                  </div>
                  <span className={`badge ${b.tone}`}>{b.text}</span>
                </div>
                <div className="acc-owner">
                  <span>{ownerLine}</span>
                  {a.organization_name && (
                    <span className="muted"> · {a.organization_name}</span>
                  )}
                </div>
                <div className="acc-usage">
                  <UsageBar label="5h" win={a.five_hour} nowMs={now} />
                  <UsageBar label="7d Opus" win={a.seven_day} nowMs={now} />
                  <UsageBar
                    label="7d Sonnet"
                    win={a.seven_day_sonnet}
                    nowMs={now}
                  />
                </div>
                <div className="acc-meta">
                  <span>
                    last used:{" "}
                    {a.last_used_at
                      ? new Date(a.last_used_at).toLocaleString()
                      : "never"}
                  </span>
                  <span>
                    token expires in: {fmtRemaining(a.expires_at - now)}
                  </span>
                  <span>
                    usage:{" "}
                    {a.usage_fetched_at
                      ? `updated ${fmtRemaining(now - a.usage_fetched_at)} ago`
                      : "pending"}
                  </span>
                </div>
                <div className="acc-actions">
                  <button onClick={() => onRefreshAccount(a.id)}>
                    Refresh now
                  </button>
                  <button
                    className="danger"
                    onClick={() => onDeleteAccount(a.id)}
                  >
                    Delete
                  </button>
                </div>
              </li>
            );
          })}
        </ul>
      </section>

      {loginState.phase === "started" && (
        <section className="login-box">
          <h3>Finish OAuth login</h3>
          <ol>
            <li>
              Copy the authorization URL below and open it in the browser
              profile you want to sign in with.
            </li>
            <li>
              After approving, the page will display a code (format{" "}
              <code>code#state</code>). Paste it below.
            </li>
          </ol>
          <label className="muted">Authorization URL</label>
          <div className="url-row">
            <input
              className="url-input"
              readOnly
              value={loginState.authorizeUrl}
              onFocus={(e) => e.currentTarget.select()}
            />
            <button
              className="secondary"
              onClick={() => copyAuthorizeUrl(loginState.authorizeUrl)}
            >
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
          <input
            placeholder="account label (optional)"
            value={accountName}
            onChange={(e) => setAccountName(e.target.value)}
          />
          <input
            placeholder="paste code#state here"
            value={pasted}
            onChange={(e) => setPasted(e.target.value)}
          />
          <div className="row">
            <button onClick={finishLogin} disabled={!pasted}>
              Submit
            </button>
            <button
              className="secondary"
              onClick={() => setLoginState({ phase: "idle" })}
            >
              Cancel
            </button>
          </div>
        </section>
      )}

      {loginState.phase === "error" && (
        <div className="banner err">
          Login failed: {loginState.message}{" "}
          <button onClick={() => setLoginState({ phase: "idle" })}>
            Dismiss
          </button>
        </div>
      )}
    </main>
  );
}
