import { useCallback, useEffect, useState } from "react";
import { openUrl } from "@tauri-apps/plugin-opener";
import {
  Account,
  apiClient,
  installHook,
  isHookInstalled,
  uninstallHook,
} from "./api";

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

export default function App() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [hookInstalled, setHookInstalled] = useState<boolean>(false);
  const [loginState, setLoginState] = useState<LoginState>({ phase: "idle" });
  const [pasted, setPasted] = useState("");
  const [accountName, setAccountName] = useState("");
  const [now, setNow] = useState<number>(Date.now());
  const [error, setError] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [list, hook] = await Promise.all([
        apiClient.listAccounts(),
        isHookInstalled(),
      ]);
      setAccounts(list);
      setHookInstalled(hook);
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
    try {
      const r = await apiClient.startLogin();
      setLoginState({
        phase: "started",
        state: r.state,
        authorizeUrl: r.authorize_url,
      });
      await openUrl(r.authorize_url);
    } catch (e) {
      setLoginState({ phase: "error", message: String(e) });
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

  async function onInstall() {
    try {
      await installHook();
      await refresh();
    } catch (e) {
      setError(String(e));
    }
  }

  async function onUninstall() {
    try {
      await uninstallHook(false);
      await refresh();
    } catch (e) {
      setError(String(e));
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
          <button onClick={hookInstalled ? onUninstall : onInstall}>
            {hookInstalled ? "Uninstall" : "Install"}
          </button>
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
            return (
              <li key={a.id}>
                <div className="acc-main">
                  <strong>{a.name}</strong>
                  <span className={`badge ${b.tone}`}>{b.text}</span>
                </div>
                <div className="acc-meta">
                  <span>tier: {a.rate_limit_tier || "—"}</span>
                  <span>sub: {a.subscription_type || "—"}</span>
                  <span>
                    last used:{" "}
                    {a.last_used_at
                      ? new Date(a.last_used_at).toLocaleString()
                      : "never"}
                  </span>
                  <span>
                    expires in: {fmtRemaining(a.expires_at - now)}
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
              A browser tab opened to Anthropic's authorization page. If not,{" "}
              <a
                href="#"
                onClick={(e) => {
                  e.preventDefault();
                  openUrl(loginState.authorizeUrl);
                }}
              >
                click here
              </a>
              .
            </li>
            <li>
              After approving, the page will display a code (format
              <code>code#state</code>). Paste it below.
            </li>
          </ol>
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
