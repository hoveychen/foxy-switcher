import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ask } from "@tauri-apps/plugin-dialog";
import { Account, UsageWindow, apiClient } from "./api";

type LoginState =
  | { phase: "idle" }
  | { phase: "started"; state: string; authorizeUrl: string }
  | { phase: "submitting"; state: string; authorizeUrl: string }
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

type Tone = "ok" | "warn" | "danger" | "muted";

function rowStatus(a: Account, nowMs: number): { text: string; tone: Tone } {
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

function isSelectable(a: Account, nowMs: number): boolean {
  return a.status === "active" && a.cooldown_until <= nowMs;
}

function peakUtilization(a: Account): number | null {
  const vals = [a.five_hour, a.seven_day, a.seven_day_sonnet]
    .filter((w): w is UsageWindow => !!w)
    .map((w) => w.utilization);
  if (vals.length === 0) return null;
  return Math.max(...vals);
}

function utilizationTone(pct: number): Tone {
  if (pct >= 90) return "danger";
  if (pct >= 75) return "warn";
  return "ok";
}

/* ─── Icons ──────────────────────────────────────────────── */

function Icon({
  d,
  className = "icon",
}: {
  d: string;
  className?: string;
}) {
  return (
    <svg
      className={className}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d={d} />
    </svg>
  );
}

const ICON_PLUS = "M8 3.5v9 M3.5 8h9";
const ICON_COPY =
  "M5 4V2.5a1 1 0 0 1 1-1h6.5a1 1 0 0 1 1 1V11a1 1 0 0 1-1 1H11 M3.5 4.5h7a1 1 0 0 1 1 1v8a1 1 0 0 1-1 1h-7a1 1 0 0 1-1-1v-8a1 1 0 0 1 1-1z";
const ICON_CHECK = "M3.5 8.5l3 3 6-6.5";
const ICON_CHEVRON = "M6 4l4 4-4 4";

function BrandMark() {
  return (
    <svg
      viewBox="0 0 16 16"
      width="14"
      height="14"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.6"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden
    >
      <path d="M3 5l2-2 3 1.5L11 3l2 2-1 4a4 4 0 0 1-8 0z" />
      <circle cx="6.5" cy="8" r="0.6" fill="currentColor" stroke="none" />
      <circle cx="9.5" cy="8" r="0.6" fill="currentColor" stroke="none" />
    </svg>
  );
}

/* ─── Usage bar ──────────────────────────────────────────── */

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
        <span className="usage-empty">No data yet</span>
      </div>
    );
  }
  const pct = Math.max(0, Math.min(100, win.utilization));
  const tone = utilizationTone(pct);
  return (
    <div className="usage-row">
      <span className="usage-label">{label}</span>
      <div className={`usage-track ${tone}`}>
        <div className="usage-fill" style={{ width: `${pct}%` }} />
      </div>
      <span className="usage-pct">{pct.toFixed(0)}%</span>
      <span className="usage-resets">{fmtResetsAt(win.resets_at, nowMs)}</span>
    </div>
  );
}

/* ─── Kebab menu ─────────────────────────────────────────── */

type KebabItem = {
  label: string;
  onClick: () => void;
  disabled?: boolean;
  danger?: boolean;
};

function KebabMenu({ items, busy }: { items: KebabItem[]; busy: boolean }) {
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (!ref.current?.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  return (
    <div className="kebab" ref={ref}>
      <button
        type="button"
        className="btn-icon kebab-btn"
        aria-label="Account actions"
        aria-haspopup="menu"
        aria-expanded={open}
        disabled={busy}
        onClick={(e) => {
          e.stopPropagation();
          setOpen((v) => !v);
        }}
      >
        {busy ? <span className="spinner" aria-hidden /> : <span aria-hidden>⋯</span>}
      </button>
      {open && (
        <div
          className="kebab-menu"
          role="menu"
          onClick={(e) => e.stopPropagation()}
        >
          {items.map((it, i) => (
            <button
              key={i}
              type="button"
              role="menuitem"
              className={`kebab-item ${it.danger ? "danger" : ""}`}
              disabled={it.disabled}
              onClick={(e) => {
                e.stopPropagation();
                setOpen(false);
                it.onClick();
              }}
            >
              {it.label}
            </button>
          ))}
        </div>
      )}
    </div>
  );
}

/* ─── Account row ────────────────────────────────────────── */

function AccountRow({
  a,
  nowMs,
  isActive,
  expanded,
  onToggle,
  onSelect,
  onRefresh,
  onDelete,
  busy,
}: {
  a: Account;
  nowMs: number;
  isActive: boolean;
  expanded: boolean;
  onToggle: () => void;
  onSelect: () => void;
  onRefresh: () => void;
  onDelete: () => void;
  busy: boolean;
}) {
  const status = rowStatus(a, nowMs);
  const peak = peakUtilization(a);
  const ownerLine =
    a.full_name && a.email
      ? `${a.full_name} · ${a.email}`
      : a.email || a.full_name || "—";

  return (
    <>
      <div
        className={`row ${expanded ? "expanded" : ""} ${
          isActive ? "active" : ""
        }`}
        onClick={onToggle}
        role="button"
        tabIndex={0}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onToggle();
          }
        }}
      >
        <span className={`row-status ${status.tone}`} aria-label={status.text} />
        <div className="row-main">
          <div className="row-title">
            <span className="name">{a.name}</span>
            {a.plan && <span className="pill">{a.plan}</span>}
            {isActive && <span className="pill active-pill">In use</span>}
          </div>
          <div className="row-subtitle">
            {ownerLine}
            {a.organization_name ? ` · ${a.organization_name}` : ""}
          </div>
        </div>
        <div className="row-trailing">
          {peak !== null ? `${peak.toFixed(0)}%` : "—"}
        </div>
        <KebabMenu
          busy={busy}
          items={[
            {
              label: isActive ? "Already in use" : "Use now",
              onClick: onSelect,
              disabled: isActive || !isSelectable(a, nowMs),
            },
            { label: "Refresh now", onClick: onRefresh },
            { label: "Delete", onClick: onDelete, danger: true },
          ]}
        />
        <Icon d={ICON_CHEVRON} className="row-chevron" />
      </div>
      {expanded && (
        <div className="row-detail">
          <div className="usage-list">
            <UsageBar label="5h" win={a.five_hour} nowMs={nowMs} />
            <UsageBar label="7d Opus" win={a.seven_day} nowMs={nowMs} />
            <UsageBar label="7d Sonnet" win={a.seven_day_sonnet} nowMs={nowMs} />
          </div>
          <dl className="detail-meta">
            <div>
              <dt>Status</dt>
              <dd>{status.text}</dd>
            </div>
            <div>
              <dt>Last used</dt>
              <dd>
                {a.last_used_at
                  ? new Date(a.last_used_at).toLocaleString()
                  : "Never"}
              </dd>
            </div>
            <div>
              <dt>Token expires</dt>
              <dd>{fmtRemaining(a.expires_at - nowMs)}</dd>
            </div>
            <div>
              <dt>Usage updated</dt>
              <dd>
                {a.usage_fetched_at
                  ? `${fmtRemaining(nowMs - a.usage_fetched_at)} ago`
                  : "Pending"}
              </dd>
            </div>
          </dl>
        </div>
      )}
    </>
  );
}

/* ─── Skeleton row (during account creation) ───────────────── */

function SkeletonRow() {
  return (
    <div className="row skeleton" aria-busy="true" aria-label="Adding account">
      <span className="row-status sk-dot" />
      <div className="row-main">
        <div className="row-title">
          <span className="sk-bar sk-bar-name" />
        </div>
        <div className="row-subtitle">
          <span className="sk-bar sk-bar-sub" />
        </div>
      </div>
      <div className="row-trailing">
        <span className="sk-bar sk-bar-pct" />
      </div>
      <span className="kebab" aria-hidden />
      <span className="row-chevron" />
    </div>
  );
}

/* ─── App ────────────────────────────────────────────────── */

export default function App() {
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [managedAccountId, setManagedAccountId] = useState<number>(0);
  const [loginState, setLoginState] = useState<LoginState>({ phase: "idle" });
  const [pasted, setPasted] = useState("");
  const [now, setNow] = useState<number>(Date.now());
  const [error, setError] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const [busyAccountId, setBusyAccountId] = useState<number | null>(null);
  const [expandedId, setExpandedId] = useState<number | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [list, cred] = await Promise.all([
        apiClient.listAccounts(),
        apiClient.credStatus(),
      ]);
      setAccounts(list);
      setManagedAccountId(cred.managed_account_id);
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

  // Auto-expand the active account on first load.
  useEffect(() => {
    if (expandedId === null && managedAccountId !== 0) {
      setExpandedId(managedAccountId);
    }
  }, [managedAccountId, expandedId]);

  const activeAccount = useMemo(
    () =>
      managedAccountId !== 0
        ? accounts.find((a) => a.id === managedAccountId) ?? null
        : null,
    [accounts, managedAccountId],
  );

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
    const { state, authorizeUrl } = loginState;
    setLoginState({ phase: "submitting", state, authorizeUrl });
    try {
      await apiClient.finishLogin(state, pasted.trim());
      setPasted("");
      setLoginState({ phase: "idle" });
      await refresh();
    } catch (e) {
      setLoginState({ phase: "error", message: String(e) });
    }
  }

  async function onRefreshAccount(id: number) {
    setBusyAccountId(id);
    try {
      await apiClient.refreshAccount(id);
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusyAccountId((curr) => (curr === id ? null : curr));
    }
  }

  async function onSelectAccount(id: number) {
    setBusyAccountId(id);
    try {
      await apiClient.selectAccount(id);
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusyAccountId((curr) => (curr === id ? null : curr));
    }
  }

  async function onDeleteAccount(id: number) {
    const ok = await ask("Delete this account from the pool?", {
      title: "Confirm delete",
      kind: "warning",
    });
    if (!ok) return;
    setBusyAccountId(id);
    try {
      await apiClient.deleteAccount(id);
      await refresh();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusyAccountId((curr) => (curr === id ? null : curr));
    }
  }

  const submitting = loginState.phase === "submitting";

  return (
    <main className="app">
      <header className="toolbar">
        <div className="brand">
          <span className="brand-mark">
            <BrandMark />
          </span>
          <span className="brand-name">Foxy Switcher</span>
          <span className={`toolbar-status ${activeAccount ? "ok" : ""}`}>
            <span className="dot" />
            {activeAccount
              ? `Managing ${activeAccount.name}`
              : "Idle"}
          </span>
        </div>
        <button
          className="btn btn-primary"
          onClick={startLogin}
          disabled={loginState.phase === "started" || submitting}
        >
          <Icon d={ICON_PLUS} />
          Add Account
        </button>
      </header>

      {error && (
        <div className="banner err">
          <span>{error}</span>
          <button className="btn btn-ghost" onClick={() => setError(null)}>
            Dismiss
          </button>
        </div>
      )}

      <section className="section">
        <div className="section-header">
          <h2 className="section-title">Accounts</h2>
          <span className="section-meta">
            {accounts.length === 0
              ? "Empty"
              : `${accounts.length} total${
                  activeAccount ? " · 1 active" : ""
                }`}
          </span>
        </div>
        {accounts.length === 0 ? (
          <div className="list">
            {submitting ? (
              <SkeletonRow />
            ) : (
              <div className="list-empty">
                <p>No accounts in the pool yet.</p>
                <button className="btn btn-secondary" onClick={startLogin}>
                  <Icon d={ICON_PLUS} />
                  Add your first account
                </button>
              </div>
            )}
          </div>
        ) : (
          <div className="list">
            {submitting && <SkeletonRow />}
            {accounts.map((a) => (
              <AccountRow
                key={a.id}
                a={a}
                nowMs={now}
                isActive={a.id === managedAccountId}
                expanded={expandedId === a.id}
                onToggle={() =>
                  setExpandedId((curr) => (curr === a.id ? null : a.id))
                }
                onSelect={() => onSelectAccount(a.id)}
                onRefresh={() => onRefreshAccount(a.id)}
                onDelete={() => onDeleteAccount(a.id)}
                busy={busyAccountId === a.id}
              />
            ))}
          </div>
        )}
      </section>

      {(loginState.phase === "started" || loginState.phase === "submitting") && (
        <section className="sheet">
          <h3 className="sheet-title">Add a new account</h3>
          <p className="sheet-subtitle">
            Sign in with the Claude account you want to add to the pool.
          </p>

          <div className="sheet-step">
            <span className="sheet-step-num">1</span>
            <div className="sheet-step-body">
              Copy the authorization URL and open it in the browser profile you
              want to sign in with.
              <div className="sheet-field">
                <input
                  className="mono"
                  readOnly
                  value={loginState.authorizeUrl}
                  onFocus={(e) => e.currentTarget.select()}
                />
                <button
                  className="btn btn-secondary"
                  onClick={() => copyAuthorizeUrl(loginState.authorizeUrl)}
                  disabled={submitting}
                >
                  {copied ? (
                    <>
                      <Icon d={ICON_CHECK} />
                      Copied
                    </>
                  ) : (
                    <>
                      <Icon d={ICON_COPY} />
                      Copy
                    </>
                  )}
                </button>
              </div>
            </div>
          </div>

          <div className="sheet-step">
            <span className="sheet-step-num">2</span>
            <div className="sheet-step-body">
              After approving, paste the code shown on the page (format{" "}
              <code>code#state</code>).
              <div className="sheet-field">
                <input
                  className="mono"
                  placeholder="paste code#state here"
                  value={pasted}
                  onChange={(e) => setPasted(e.target.value)}
                  disabled={submitting}
                  autoFocus
                />
              </div>
            </div>
          </div>

          <div className="sheet-actions">
            <button
              className="btn btn-secondary"
              onClick={() => {
                setPasted("");
                setLoginState({ phase: "idle" });
              }}
              disabled={submitting}
            >
              Cancel
            </button>
            <button
              className="btn btn-primary"
              onClick={finishLogin}
              disabled={!pasted || submitting}
            >
              {submitting ? (
                <>
                  <span className="spinner" aria-hidden />
                  Submitting
                </>
              ) : (
                "Sign In"
              )}
            </button>
          </div>
        </section>
      )}

      {loginState.phase === "error" && (
        <div className="banner err">
          <span>Login failed: {loginState.message}</span>
          <button
            className="btn btn-ghost"
            onClick={() => setLoginState({ phase: "idle" })}
          >
            Dismiss
          </button>
        </div>
      )}
    </main>
  );
}
