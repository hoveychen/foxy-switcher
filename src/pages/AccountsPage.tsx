import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ask } from "@tauri-apps/plugin-dialog";
import { Account, UsageWindow, apiClient } from "../api";
import {
  accountIsCooling,
  accountIsShared,
  accountLeaseHolders,
  accountOutOfCredit,
  accountRefreshDue,
  accountResetAt,
  scopedIsThrottled,
} from "../api";
import { Icon } from "../components/Icon";
import { Topbar } from "../components/Topbar";
import { FoxAvatar } from "../components/FoxAvatar";
import { Modal } from "../components/Modal";
import { OpenRouterModal } from "../components/OpenRouterModal";
import {
  ICON_PLUS,
  ICON_COPY,
  ICON_CHECK,
  ICON_SEARCH,
  ICON_GRID,
  ICON_LIST,
} from "../components/icons";
import { t, tf, scopedUsageLabel } from "../i18n";

type LoginState =
  | { phase: "idle" }
  | { phase: "started"; state: string; authorizeUrl: string }
  | { phase: "submitting"; state: string; authorizeUrl: string }
  | { phase: "error"; message: string };

// CodexLoginState drives the Codex OAuth paste-code modal: request an authorize
// URL, let the user sign in and paste the callback URL back, then close on
// completion. Mirrors the Claude LoginState — the flows are the same shape, but
// kept separate because Codex builds a different account and has its own strings.
type CodexLoginState =
  | { phase: "idle" }
  | { phase: "starting" }
  | { phase: "started"; state: string; authorizeUrl: string }
  | { phase: "submitting"; state: string; authorizeUrl: string }
  | { phase: "error"; message: string };

type Tone = "ok" | "warn" | "danger" | "muted";

type StatusFilter = "all" | "active" | "paused" | "cooling";
type ProviderFilter = "all" | "claude" | "codex" | "openrouter";

type ViewMode = "grid" | "list";

const VIEW_MODE_KEY = "foxy.accounts.viewMode";

function loadViewMode(): ViewMode {
  try {
    const v = localStorage.getItem(VIEW_MODE_KEY);
    return v === "list" ? "list" : "grid";
  } catch {
    return "grid";
  }
}

const STATUS_FILTERS: Array<{ key: StatusFilter; labelKey: string }> = [
  { key: "all", labelKey: "accounts.filters.all" },
  { key: "active", labelKey: "accounts.filters.active" },
  { key: "paused", labelKey: "accounts.filters.paused" },
  { key: "cooling", labelKey: "accounts.filters.cooling" },
];

const PROVIDER_FILTERS: Array<{ key: ProviderFilter; labelKey: string }> = [
  { key: "all", labelKey: "accounts.providers.all" },
  { key: "claude", labelKey: "accounts.providers.claude" },
  { key: "codex", labelKey: "accounts.providers.codex" },
  { key: "openrouter", labelKey: "accounts.providers.openrouter" },
];

function fmtRemaining(ms: number): string {
  if (ms <= 0) return t("accounts.fmt.expired");
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function rowStatus(a: Account, nowMs: number): { text: string; tone: Tone } {
  if (a.status === "org_disabled")
    return { text: t("accounts.row.status.org_disabled"), tone: "danger" };
  if (a.status === "needs_reauth")
    return { text: t("accounts.row.status.needs_reauth"), tone: "danger" };
  if (a.status !== "active") return { text: t("accounts.row.status.paused"), tone: "muted" };
  // Ahead of cooling: an empty account is unusable outright, not throttled.
  if (accountOutOfCredit(a))
    return { text: t("accounts.row.status.out_of_credit"), tone: "danger" };
  if (accountIsCooling(a)) {
    const reset = accountResetAt(a, new Date(nowMs));
    if (reset > 0) {
      return {
        text: tf("accounts.row.status.cooling", { time: fmtRemaining(reset - nowMs) }),
        tone: "warn",
      };
    }
    return { text: t("accounts.row.status.cooling_no_reset"), tone: "warn" };
  }
  if (accountRefreshDue(a, nowMs)) {
    return { text: t("accounts.row.status.refresh_due"), tone: "warn" };
  }
  return { text: t("accounts.row.status.active"), tone: "ok" };
}

function isSelectable(a: Account): boolean {
  // Foreign-held lease (vault sees another device using this account)
  // makes manual select pointless on an EXCLUSIVE account: AcquireLease
  // would hit leases_exclusive_account_uniq and 409. Skip in the UI so
  // users see the badge instead of a confusing error toast. Shared
  // (Codex) accounts have no such lock — co-holding is the point.
  if (a.lease && !a.lease.mine && !accountIsShared(a)) return false;
  return a.status === "active" && !accountIsCooling(a);
}

function foreignLease(a: Account) {
  return a.lease && !a.lease.mine ? a.lease : null;
}

// extraHolders: how many devices hold this account BEYOND the one named on
// the badge, so a shared Codex account reads "in use by Alpha +2" instead of
// silently hiding its other tenants.
function extraHolders(a: Account): number {
  return Math.max(0, accountLeaseHolders(a).length - 1);
}

// holderTooltip lists every holding device for the badge's title attribute.
function holderTooltip(a: Account): string | undefined {
  const holders = accountLeaseHolders(a);
  if (holders.length < 2) return undefined;
  return holders.map((l) => l.device_name || l.device_id || "—").join(", ");
}

// HolderOverflow appends "+N" inside a lease badge when a shared account has
// more holders than the one the badge names. Renders nothing for the ordinary
// single-holder case, so exclusive (Claude) badges look exactly as before.
function HolderOverflow({ account }: { account: Account }) {
  const extra = extraHolders(account);
  if (extra <= 0) return null;
  return <span className="lease-overflow"> +{extra}</span>;
}

function utilizationTone(pct: number): Tone {
  if (pct >= 90) return "danger";
  if (pct >= 75) return "warn";
  return "ok";
}

function matchesQuery(a: Account, q: string): boolean {
  if (!q) return true;
  const lower = q.toLowerCase();
  const haystack = [a.name, a.full_name, a.email, a.organization_name]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();
  return haystack.includes(lower);
}

function matchesStatus(a: Account, f: StatusFilter): boolean {
  if (f === "all") return true;
  if (f === "paused") return a.status === "paused";
  // Same rule as the dashboard's healthy/cooling split: an out-of-credit
  // account is active-but-not-served, so it belongs under "cooling", not
  // "active" — otherwise the filter promises an account the vault won't hand out.
  if (f === "active")
    return a.status === "active" && !accountIsCooling(a) && !accountOutOfCredit(a);
  if (f === "cooling")
    return a.status === "active" && (accountIsCooling(a) || accountOutOfCredit(a));
  return true;
}

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
        aria-label={t("accounts.kebab.aria")}
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

function UsageMiniBar({
  label,
  win,
}: {
  label: string;
  win: UsageWindow | undefined;
}) {
  if (!win) {
    return (
      <div className="usage-row usage-row-compact">
        <span className="usage-label">{label}</span>
        <span className="usage-empty">{t("drawer.usage.no_data")}</span>
      </div>
    );
  }
  const pct = Math.max(0, Math.min(100, win.utilization));
  const tone = utilizationTone(pct);
  return (
    <div className="usage-row usage-row-compact">
      <span className="usage-label">{label}</span>
      <div className={`usage-track ${tone}`}>
        <div className="usage-fill" style={{ width: `${pct}%` }} />
      </div>
      <span className="usage-pct">{pct.toFixed(0)}%</span>
    </div>
  );
}

function AccountCard({
  a,
  nowMs,
  isInUse,
  isSelected,
  vaultMode,
  onClickCard,
  onUseNow,
  onRefresh,
  onReauth,
  onDelete,
  onTogglePause,
  onEditOpenRouter,
  busy,
  disableAdminActions,
}: {
  a: Account;
  nowMs: number;
  isInUse: boolean;
  isSelected: boolean;
  vaultMode: boolean;
  onClickCard: () => void;
  onUseNow: () => void;
  onRefresh: () => void;
  onReauth: () => void;
  onDelete: () => void;
  onTogglePause: () => void;
  onEditOpenRouter: () => void;
  busy: boolean;
  disableAdminActions: boolean;
}) {
  const paused = a.status !== "active";
  const status = rowStatus(a, nowMs);
  const ownerLine =
    a.full_name && a.email
      ? `${a.full_name} · ${a.email}`
      : a.email || a.full_name || "";

  // Vault mode renders one combined "in use by Device X" badge for any
  // live lease — the page is the multi-device admin surface, so the
  // agent-singular "this is mine" branch (active-pill, no device name)
  // would just hide which device is holding what. The agent/desktop
  // viewer keeps the legacy split because there only one lease can be
  // "mine" at a time and the device name is implicit.
  const fLease = foreignLease(a);
  const cardForeign = !!fLease;
  const vaultBadgeLease = vaultMode ? a.lease ?? null : null;
  return (
    <div
      className={`account-card ${isSelected ? "selected" : ""} ${
        isInUse && !vaultMode ? "active" : ""
      } ${fLease ? "leased-foreign" : ""} ${busy ? "busy" : ""}`}
      onClick={onClickCard}
      role="button"
      tabIndex={0}
      aria-label={a.name}
      aria-busy={busy}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClickCard();
        }
      }}
    >
      <div className="account-card-head">
        <span className={`row-status ${status.tone}`} aria-label={status.text} />
        <FoxAvatar name={a.name} size={32} />
        <div className="account-card-title">
          <div className="account-card-title-line">
            <span className="name">{a.name}</span>
            {busy && (
              <span className="pill switching-pill">
                <span className="spinner" aria-hidden />
                {t("drawer.actions.switching")}
              </span>
            )}
            {vaultMode
              ? vaultBadgeLease && (
                  <span className="pill leased-pill" title={holderTooltip(a)}>
                    {tf("accounts.badge.in_use_by", {
                      device:
                        vaultBadgeLease.device_name ||
                        vaultBadgeLease.device_id ||
                        "—",
                    })}
                    <HolderOverflow account={a} />
                  </span>
                )
              : (
                  <>
                    {isInUse && (
                      <span className="pill active-pill" title={holderTooltip(a)}>
                        {t("accounts.row.in_use")}
                        <HolderOverflow account={a} />
                      </span>
                    )}
                    {fLease && !isInUse && (
                      <span className="pill leased-pill" title={holderTooltip(a)}>
                        {tf("accounts.badge.in_use_by", {
                          device:
                            fLease.device_name || fLease.device_id || "—",
                        })}
                        <HolderOverflow account={a} />
                      </span>
                    )}
                  </>
                )}
          </div>
          {(ownerLine || a.plan) && (
            <div className="account-card-meta">
              {ownerLine && (
                <span className="account-card-owner">{ownerLine}</span>
              )}
              {a.plan && (
                <span className="pill account-card-plan">{a.plan}</span>
              )}
            </div>
          )}
        </div>
      </div>
      <div className="account-card-usage">
        {a.provider === "openrouter" ? (
          // OpenRouter is pay-as-you-go: there are no subscription usage
          // windows to render. Show the policy that actually governs it — how
          // many models devices may pick and the per-key spend cap.
          <>
            <div className="usage-row usage-row-compact">
              <span className="usage-label">{t("accounts.openrouter.policy")}</span>
              <span className="usage-empty">
                {tf("accounts.openrouter.policy_value", {
                  models: a.openrouter?.allowed_models.length ?? 0,
                  limit: a.openrouter?.limit_usd
                    ? `$${a.openrouter.limit_usd}`
                    : t("accounts.openrouter.no_limit"),
                })}
              </span>
            </div>
            <div className="usage-row usage-row-compact">
              <span className="usage-label">{t("accounts.openrouter.credit")}</span>
              <span className={`usage-empty ${a.openrouter?.out_of_credit ? "or-hint-error" : ""}`}>
                {a.openrouter?.credit
                  ? `$${a.openrouter.credit.remaining.toFixed(2)}`
                  : t("accounts.openrouter.credit_unknown")}
              </span>
            </div>
          </>
        ) : (
          <>
            <UsageMiniBar
              label={t(a.provider === "codex" ? "accounts.usage.primary" : "drawer.usage.5h")}
              win={a.five_hour}
            />
            <UsageMiniBar
              label={t(a.provider === "codex" ? "accounts.usage.secondary" : "drawer.usage.7d_opus")}
              win={a.seven_day}
            />
            {a.provider !== "codex" && (
              <UsageMiniBar
                label={scopedUsageLabel(a.seven_day_scoped_label, scopedIsThrottled(a))}
                win={a.seven_day_sonnet}
              />
            )}
          </>
        )}
      </div>
      <KebabMenu
        busy={busy}
        items={[
          // Re-login is the only useful action on a needs_reauth account: its
          // refresh_token is dead, so it reruns the OAuth add flow (Upsert
          // dedups by uuid/email and resets status to active). Admin-gated like
          // the other write actions — agent mode does CRUD on the vault UI.
          ...(a.status === "needs_reauth" && !disableAdminActions
            ? [
                {
                  label: t("accounts.kebab.reauth"),
                  onClick: onReauth,
                },
              ]
            : []),
          // OpenRouter is configured, not selected: it holds no lease and
          // every authorised device uses it at once, so "use now" and
          // "refresh" are meaningless. Editing its policy is the real action.
          ...(a.provider === "openrouter"
            ? disableAdminActions
              ? []
              : [
                  {
                    label: t("accounts.kebab.edit_openrouter"),
                    onClick: onEditOpenRouter,
                  },
                ]
            : [
                {
                  label: fLease
                    ? t("accounts.kebab.leased_by_other")
                    : isInUse
                      ? t("accounts.kebab.in_use")
                      : t("accounts.kebab.use_now"),
                  onClick: onUseNow,
                  disabled: isInUse || !isSelectable(a),
                },
              ]),
          // Refresh is hidden in agent mode (cloud vault owns token
          // rotation). Disabled when another device holds the lease so
          // we don't 401 their live CC session.
          ...(disableAdminActions || a.provider === "openrouter"
            ? []
            : [
                {
                  label: t("accounts.kebab.refresh"),
                  onClick: onRefresh,
                  disabled: cardForeign,
                },
              ]),
          // Pause/Resume + Delete write to vault state, hidden in
          // agent mode (admin lives on the vault web UI).
          ...(disableAdminActions
            ? []
            : [
                {
                  label: paused
                    ? t("accounts.kebab.resume")
                    : t("accounts.kebab.pause"),
                  onClick: onTogglePause,
                },
                {
                  label: t("accounts.kebab.delete"),
                  onClick: onDelete,
                  danger: true,
                },
              ]),
        ]}
      />
    </div>
  );
}

function SkeletonCard() {
  const skRow = (
    <div className="usage-row usage-row-compact">
      <span className="sk-bar" style={{ width: 28 }} />
      <span className="sk-bar sk-bar-track" style={{ width: "100%" }} />
      <span className="sk-bar" style={{ width: 28 }} />
    </div>
  );
  return (
    <div
      className="account-card skeleton"
      aria-busy="true"
      aria-label={t("accounts.skeleton.aria")}
    >
      <div className="account-card-head">
        <span className="row-status sk-dot" />
        <span className="sk-avatar" aria-hidden />
        <div className="account-card-title">
          <span className="sk-bar sk-bar-name" />
        </div>
      </div>
      <div className="account-card-usage">
        {skRow}
        {skRow}
        {skRow}
      </div>
    </div>
  );
}

export function AccountsPage({
  accounts,
  managedAccountId,
  nowMs,
  selectedAccountId,
  busyAccountId,
  addAccountTick,
  onAddAccountConsumed,
  autoSwitch,
  onAutoSwitchToggle,
  onSelectRow,
  onUseNow,
  onRefreshAccount,
  onTogglePause,
  onDelete,
  onRefresh,
  onError,
  stale,
  disableAdminActions = false,
  vaultMode = false,
}: {
  accounts: Account[];
  managedAccountId: number;
  nowMs: number;
  selectedAccountId: number | null;
  busyAccountId: number | null;
  addAccountTick: number;
  onAddAccountConsumed: () => void;
  autoSwitch?: { enabled: boolean; policy: "lru" | "lowest" | "rr" };
  onAutoSwitchToggle?: () => void;
  onSelectRow: (id: number | null) => void;
  onUseNow: (id: number) => void;
  onRefreshAccount: (id: number) => void;
  onTogglePause: (a: Account) => void;
  onDelete: (id: number) => void;
  onRefresh: () => Promise<void>;
  onError: (msg: string) => void;
  stale: boolean;
  // Agent mode → vault owns account CRUD; hide Add/Pause/Resume/Delete
  // UI on this page so the user isn't led to operations the agent will
  // 405. Currently driven by `about.mode === "agent"` from /api/about.
  disableAdminActions?: boolean;
  // Vault mode → the page is the multi-device admin surface, so swap the
  // agent-singular "Managing X" topbar status and active-pill badge for a
  // multi-device summary + per-card lease.device_name badge.
  vaultMode?: boolean;
}) {
  const [loginState, setLoginState] = useState<LoginState>({ phase: "idle" });
  const [pasted, setPasted] = useState("");
  const [copied, setCopied] = useState(false);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [providerFilter, setProviderFilter] = useState<ProviderFilter>("all");
  const [codexImporting, setCodexImporting] = useState(false);
  const [codexLogin, setCodexLogin] = useState<CodexLoginState>({ phase: "idle" });
  const [codexPasted, setCodexPasted] = useState("");
  // null + closed = no modal; null + open = "add"; an account = "edit".
  const [openRouterEditing, setOpenRouterEditing] = useState<Account | null>(null);
  const [openRouterAdding, setOpenRouterAdding] = useState(false);
  const [viewMode, setViewMode] = useState<ViewMode>(loadViewMode);

  const setViewModePersisted = useCallback((mode: ViewMode) => {
    setViewMode(mode);
    try {
      localStorage.setItem(VIEW_MODE_KEY, mode);
    } catch {
      // private browsing / quota — fall back to in-memory only
    }
  }, []);

  const submitting = loginState.phase === "submitting";
  const modalOpen =
    loginState.phase === "started" || loginState.phase === "submitting";
  const codexSubmitting = codexLogin.phase === "submitting";
  const codexModalOpen =
    codexLogin.phase === "starting" ||
    codexLogin.phase === "started" ||
    codexLogin.phase === "submitting";
  const activeAccount =
    managedAccountId !== 0
      ? accounts.find((a) => a.id === managedAccountId) ?? null
      : accounts.find((a) => a.in_use) ?? null;

  const filtered = useMemo(() => {
    return accounts.filter(
      (a) =>
        (providerFilter === "all" || a.provider === providerFilter) &&
        matchesQuery(a, search) &&
        matchesStatus(a, statusFilter),
    );
  }, [accounts, search, statusFilter, providerFilter, nowMs]);

  const startLogin = useCallback(async () => {
    // In agent mode the daemon proxies admin writes to the vault, which
    // returns 405 "agent mode is read-only". Block at the source so the
    // native menu (Cmd+N) and any other future caller don't surface that
    // error banner.
    if (disableAdminActions) return;
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
  }, [disableAdminActions]);

  const importCodex = useCallback(async () => {
    if (disableAdminActions || vaultMode) return;
    setCodexImporting(true);
    try {
      await apiClient.importCodex();
      setLoginState({ phase: "idle" });
      await onRefresh();
    } catch (e) {
      setLoginState({ phase: "error", message: String(e) });
    } finally {
      setCodexImporting(false);
    }
  }, [disableAdminActions, vaultMode, onRefresh]);

  const startCodexLogin = useCallback(async () => {
    if (disableAdminActions) return;
    setCopied(false);
    setCodexPasted("");
    setCodexLogin({ phase: "starting" });
    try {
      const r = await apiClient.startCodexLogin();
      setCodexLogin({
        phase: "started",
        state: r.state,
        authorizeUrl: r.authorize_url,
      });
    } catch (e) {
      setCodexLogin({ phase: "error", message: String(e) });
    }
  }, [disableAdminActions]);

  const finishCodexLogin = useCallback(async () => {
    if (codexLogin.phase !== "started") return;
    const { state, authorizeUrl } = codexLogin;
    setCodexLogin({ phase: "submitting", state, authorizeUrl });
    try {
      await apiClient.finishCodexLogin(state, codexPasted.trim());
      setCodexPasted("");
      setCodexLogin({ phase: "idle" });
      await onRefresh();
    } catch (e) {
      setCodexLogin({ phase: "error", message: String(e) });
    }
  }, [codexLogin, codexPasted, onRefresh]);

  useEffect(() => {
    if (addAccountTick === 0) return;
    if (loginState.phase === "idle") startLogin();
    onAddAccountConsumed();
    // intentionally only depend on tick — fires once per increment
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [addAccountTick]);

  async function copyAuthorizeUrl(url: string) {
    try {
      await navigator.clipboard.writeText(url);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch (e) {
      onError(String(e));
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
      await onRefresh();
    } catch (e) {
      setLoginState({ phase: "error", message: String(e) });
    }
  }

  const requestCloseModal = useCallback(async () => {
    if (submitting) return;
    if (pasted.trim().length > 0) {
      const ok = await ask(
        t("accounts.modal.discard_prompt"),
        { title: t("accounts.modal.discard_title"), kind: "warning" },
      );
      if (!ok) return;
    }
    setPasted("");
    setLoginState({ phase: "idle" });
  }, [pasted, submitting]);

  // In vault mode the topbar describes the multi-device pool — there is no
  // single "managed" account from the admin's perspective. Counts come from
  // the same accounts list the cards render off, so the header stays in sync
  // with what the user is looking at without an extra round-trip.
  const inUseCount = useMemo(
    () => accounts.filter((a) => !!a.lease).length,
    [accounts],
  );
  const topbarStatus = vaultMode
    ? {
        label: tf("accounts.status.summary", {
          total: accounts.length,
          inUse: inUseCount,
        }),
        tone: "muted" as const,
      }
    : activeAccount
      ? {
          label: tf("accounts.status.managing", { name: activeAccount.name }),
          tone: "ok" as const,
        }
      : { label: t("accounts.status.idle"), tone: "muted" as const };

  return (
    <>
      <Topbar
        title={t("accounts.title")}
        status={topbarStatus}
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
        actions={
          disableAdminActions ? null : (
            <div className="accounts-add-actions">
              {!vaultMode && (
                <button
                  className="btn btn-secondary"
                  onClick={importCodex}
                  disabled={modalOpen || codexImporting || codexModalOpen}
                >
                  {codexImporting && <span className="spinner" aria-hidden />}
                  {t("accounts.import_codex")}
                </button>
              )}
              {vaultMode && (
                // Vault has no local codex CLI to read, so add Codex via the
                // OAuth authorize + paste flow instead of the local-import path.
                <button
                  className="btn btn-secondary"
                  onClick={startCodexLogin}
                  disabled={modalOpen || codexModalOpen}
                >
                  {codexModalOpen && <span className="spinner" aria-hidden />}
                  {t("accounts.add_codex")}
                </button>
              )}
              <button
                className="btn btn-secondary"
                onClick={() => {
                  setOpenRouterEditing(null);
                  setOpenRouterAdding(true);
                }}
                disabled={modalOpen || codexImporting || codexModalOpen}
              >
                {t("accounts.add_openrouter")}
              </button>
              <button
                className="btn btn-primary"
                onClick={startLogin}
                disabled={modalOpen || codexImporting || codexModalOpen}
              >
                <Icon d={ICON_PLUS} />
                {t("accounts.add_claude")}
              </button>
            </div>
          )
        }
      />
      <div className="page">
        <div className="accounts-toolbar">
          <div className="filter-chips" role="tablist" aria-label={t("accounts.providers.aria")}>
            {PROVIDER_FILTERS.map((f) => (
              <button
                key={f.key}
                type="button"
                role="tab"
                aria-selected={providerFilter === f.key}
                className={`filter-chip ${providerFilter === f.key ? "active" : ""}`}
                onClick={() => setProviderFilter(f.key)}
              >
                {t(f.labelKey)}
              </button>
            ))}
          </div>
          <label className="search-input">
            <Icon d={ICON_SEARCH} />
            <input
              type="search"
              placeholder={t("accounts.search_placeholder")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              aria-label={t("accounts.search_aria")}
            />
          </label>
          <div className="filter-chips" role="tablist" aria-label={t("accounts.filters.aria")}>
            {STATUS_FILTERS.map((f) => (
              <button
                key={f.key}
                type="button"
                role="tab"
                aria-selected={statusFilter === f.key}
                className={`filter-chip ${statusFilter === f.key ? "active" : ""}`}
                onClick={() => setStatusFilter(f.key)}
              >
                {t(f.labelKey)}
              </button>
            ))}
          </div>
          <div
            className="view-toggle"
            role="radiogroup"
            aria-label={t("accounts.view.aria")}
          >
            <button
              type="button"
              role="radio"
              aria-checked={viewMode === "grid"}
              className={`view-toggle-btn ${viewMode === "grid" ? "active" : ""}`}
              onClick={() => setViewModePersisted("grid")}
              title={t("accounts.view.grid.title")}
              aria-label={t("accounts.view.grid")}
            >
              <Icon d={ICON_GRID} />
            </button>
            <button
              type="button"
              role="radio"
              aria-checked={viewMode === "list"}
              className={`view-toggle-btn ${viewMode === "list" ? "active" : ""}`}
              onClick={() => setViewModePersisted("list")}
              title={t("accounts.view.list.title")}
              aria-label={t("accounts.view.list")}
            >
              <Icon d={ICON_LIST} />
            </button>
          </div>
        </div>
        <div className="section">
          <div className="section-header">
            <h2 className="section-title">
              {t("accounts.section.pool")}
              {stale && (
                <span
                  className="stale-dot"
                  role="status"
                  title={t("accounts.stale.title")}
                  aria-label={t("accounts.stale.aria")}
                />
              )}
            </h2>
            <span className="section-meta">
              {accounts.length === 0
                ? t("accounts.section.empty")
                : filtered.length === accounts.length
                  ? !vaultMode && activeAccount
                    ? tf("accounts.section.total_one_active", { total: accounts.length })
                    : tf("accounts.section.total", { total: accounts.length })
                  : tf("accounts.section.filtered", {
                      shown: filtered.length,
                      total: accounts.length,
                    })}
            </span>
          </div>
          {accounts.length === 0 ? (
            submitting ? (
              <div className={`accounts-grid mode-${viewMode}`}>
                <SkeletonCard />
              </div>
            ) : (
              <div className="list">
                <div className="list-empty">
                  <p>{t("accounts.empty.no_pool")}</p>
                  {!disableAdminActions && (
                    <div className="accounts-add-actions">
                      {!vaultMode && (
                        <button className="btn btn-secondary" onClick={importCodex}>
                          {t("accounts.import_codex")}
                        </button>
                      )}
                      <button className="btn btn-primary" onClick={startLogin}>
                        <Icon d={ICON_PLUS} />
                        {t("accounts.add_claude")}
                      </button>
                    </div>
                  )}
                </div>
              </div>
            )
          ) : filtered.length === 0 ? (
            <div className="list">
              <div className="list-empty">
                <p>{t("accounts.empty.no_match")}</p>
                <button
                  className="btn btn-ghost"
                  onClick={() => {
                    setSearch("");
                    setStatusFilter("all");
                    setProviderFilter("all");
                  }}
                >
                  {t("accounts.empty.clear_filters")}
                </button>
              </div>
            </div>
          ) : (
            <div className={`accounts-grid mode-${viewMode}`}>
              {submitting && <SkeletonCard />}
              {filtered.map((a) => (
                <AccountCard
                  key={a.id}
                  a={a}
                  nowMs={nowMs}
                  isInUse={a.in_use || a.id === managedAccountId}
                  isSelected={a.id === selectedAccountId}
                  vaultMode={vaultMode}
                  onClickCard={() =>
                    onSelectRow(selectedAccountId === a.id ? null : a.id)
                  }
                  onUseNow={() => onUseNow(a.id)}
                  onRefresh={() => onRefreshAccount(a.id)}
                  onReauth={a.provider === "codex" ? importCodex : startLogin}
                  onDelete={() => onDelete(a.id)}
                  onTogglePause={() => onTogglePause(a)}
                  onEditOpenRouter={() => setOpenRouterEditing(a)}
                  busy={busyAccountId === a.id}
                  disableAdminActions={disableAdminActions}
                />
              ))}
            </div>
          )}
        </div>

        <OpenRouterModal
          open={openRouterAdding || openRouterEditing !== null}
          account={openRouterEditing}
          onClose={() => {
            setOpenRouterAdding(false);
            setOpenRouterEditing(null);
          }}
          onSaved={onRefresh}
        />

        <Modal
          open={modalOpen}
          title={t("accounts.modal.title")}
          subtitle={t("accounts.modal.subtitle")}
          onClose={() => {
            setPasted("");
            setLoginState({ phase: "idle" });
          }}
          onRequestClose={requestCloseModal}
          footer={
            <>
              <button
                className="btn btn-secondary"
                onClick={requestCloseModal}
                disabled={submitting}
              >
                {t("common.cancel")}
              </button>
              <button
                className="btn btn-primary"
                onClick={finishLogin}
                disabled={!pasted || submitting}
              >
                {submitting ? (
                  <>
                    <span className="spinner" aria-hidden />
                    {t("accounts.modal.submitting")}
                  </>
                ) : (
                  t("accounts.modal.sign_in")
                )}
              </button>
            </>
          }
        >
          <div className="sheet-step">
            <span className="sheet-step-num">1</span>
            <div className="sheet-step-body">
              {t("accounts.modal.step1")}
              <div className="sheet-field">
                <input
                  className="mono"
                  readOnly
                  aria-label={t("accounts.modal.url_aria")}
                  value={
                    loginState.phase === "started" ||
                    loginState.phase === "submitting"
                      ? loginState.authorizeUrl
                      : ""
                  }
                  onFocus={(e) => e.currentTarget.select()}
                />
                <button
                  className="btn btn-secondary"
                  onClick={() => {
                    if (
                      loginState.phase === "started" ||
                      loginState.phase === "submitting"
                    ) {
                      copyAuthorizeUrl(loginState.authorizeUrl);
                    }
                  }}
                  disabled={submitting}
                >
                  {copied ? (
                    <>
                      <Icon d={ICON_CHECK} />
                      {t("accounts.modal.copied")}
                    </>
                  ) : (
                    <>
                      <Icon d={ICON_COPY} />
                      {t("accounts.modal.copy")}
                    </>
                  )}
                </button>
              </div>
            </div>
          </div>

          <div className="sheet-step">
            <span className="sheet-step-num">2</span>
            <div className="sheet-step-body">
              {t("accounts.modal.step2.prefix")}
              <code>code#state</code>
              {t("accounts.modal.step2.suffix")}
              <div className="sheet-field">
                <input
                  className="mono"
                  placeholder={t("accounts.modal.code_placeholder")}
                  aria-label={t("accounts.modal.code_aria")}
                  value={pasted}
                  onChange={(e) => setPasted(e.target.value)}
                  disabled={submitting}
                  autoFocus
                />
              </div>
            </div>
          </div>
        </Modal>

        {loginState.phase === "error" && (
          <div className="banner err">
            <span>{tf("accounts.error.login_failed", { message: loginState.message })}</span>
            <button
              className="btn btn-ghost"
              onClick={() => setLoginState({ phase: "idle" })}
            >
              {t("banner.dismiss")}
            </button>
          </div>
        )}

        <Modal
          open={codexModalOpen}
          title={t("accounts.codex_modal.title")}
          subtitle={t("accounts.codex_modal.subtitle")}
          onClose={() => {
            setCodexPasted("");
            setCodexLogin({ phase: "idle" });
          }}
          onRequestClose={() => {
            if (codexSubmitting) return;
            setCodexPasted("");
            setCodexLogin({ phase: "idle" });
          }}
          footer={
            <>
              <button
                className="btn btn-secondary"
                onClick={() => {
                  setCodexPasted("");
                  setCodexLogin({ phase: "idle" });
                }}
                disabled={codexSubmitting}
              >
                {t("common.cancel")}
              </button>
              <button
                className="btn btn-primary"
                onClick={finishCodexLogin}
                disabled={!codexPasted.trim() || codexSubmitting}
              >
                {codexSubmitting ? (
                  <>
                    <span className="spinner" aria-hidden />
                    {t("accounts.modal.submitting")}
                  </>
                ) : (
                  t("accounts.modal.sign_in")
                )}
              </button>
            </>
          }
        >
          {codexLogin.phase === "starting" && (
            <div className="sheet-step">
              <span className="spinner" aria-hidden />
              {t("accounts.codex_modal.starting")}
            </div>
          )}
          {(codexLogin.phase === "started" ||
            codexLogin.phase === "submitting") && (
            <>
              <div className="sheet-step">
                <span className="sheet-step-num">1</span>
                <div className="sheet-step-body">
                  {t("accounts.codex_modal.step1")}
                  <div className="sheet-field">
                    <input
                      className="mono"
                      readOnly
                      aria-label={t("accounts.codex_modal.url_aria")}
                      value={codexLogin.authorizeUrl}
                      onFocus={(e) => e.currentTarget.select()}
                    />
                    <button
                      className="btn btn-secondary"
                      onClick={() => copyAuthorizeUrl(codexLogin.authorizeUrl)}
                      disabled={codexSubmitting}
                    >
                      {copied ? (
                        <>
                          <Icon d={ICON_CHECK} />
                          {t("accounts.modal.copied")}
                        </>
                      ) : (
                        <>
                          <Icon d={ICON_COPY} />
                          {t("accounts.modal.copy")}
                        </>
                      )}
                    </button>
                  </div>
                </div>
              </div>
              <div className="sheet-step">
                <span className="sheet-step-num">2</span>
                <div className="sheet-step-body">
                  {t("accounts.codex_modal.step2")}
                  <div className="sheet-field">
                    <input
                      className="mono"
                      placeholder={t("accounts.codex_modal.code_placeholder")}
                      aria-label={t("accounts.codex_modal.code_aria")}
                      value={codexPasted}
                      onChange={(e) => setCodexPasted(e.target.value)}
                      disabled={codexSubmitting}
                      autoFocus
                    />
                  </div>
                </div>
              </div>
            </>
          )}
        </Modal>

        {codexLogin.phase === "error" && (
          <div className="banner err">
            <span>{tf("accounts.error.login_failed", { message: codexLogin.message })}</span>
            <button
              className="btn btn-ghost"
              onClick={() => setCodexLogin({ phase: "idle" })}
            >
              {t("banner.dismiss")}
            </button>
          </div>
        )}
      </div>
    </>
  );
}
