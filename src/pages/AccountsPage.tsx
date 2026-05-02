import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ask } from "@tauri-apps/plugin-dialog";
import { Account, UsageWindow, apiClient } from "../api";
import { Icon } from "../components/Icon";
import { Topbar } from "../components/Topbar";
import { FoxAvatar } from "../components/FoxAvatar";
import { Modal } from "../components/Modal";
import {
  ICON_PLUS,
  ICON_COPY,
  ICON_CHECK,
  ICON_CHEVRON_RIGHT,
  ICON_SEARCH,
} from "../components/icons";
import { t, tf } from "../i18n";

type LoginState =
  | { phase: "idle" }
  | { phase: "started"; state: string; authorizeUrl: string }
  | { phase: "submitting"; state: string; authorizeUrl: string }
  | { phase: "error"; message: string };

type Tone = "ok" | "warn" | "danger" | "muted";

type StatusFilter = "all" | "active" | "paused" | "cooling";

const STATUS_FILTERS: Array<{ key: StatusFilter; labelKey: string }> = [
  { key: "all", labelKey: "accounts.filters.all" },
  { key: "active", labelKey: "accounts.filters.active" },
  { key: "paused", labelKey: "accounts.filters.paused" },
  { key: "cooling", labelKey: "accounts.filters.cooling" },
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
  if (a.status !== "active") return { text: t("accounts.row.status.paused"), tone: "muted" };
  if (a.cooldown_until > nowMs) {
    return {
      text: tf("accounts.row.status.cooldown", { time: fmtRemaining(a.cooldown_until - nowMs) }),
      tone: "warn",
    };
  }
  if (a.expires_at - nowMs < 5 * 60 * 1000) {
    return { text: t("accounts.row.status.refresh_due"), tone: "warn" };
  }
  return { text: t("accounts.row.status.active"), tone: "ok" };
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

function matchesQuery(a: Account, q: string): boolean {
  if (!q) return true;
  const lower = q.toLowerCase();
  const haystack = [a.name, a.full_name, a.email, a.organization_name]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();
  return haystack.includes(lower);
}

function matchesStatus(a: Account, f: StatusFilter, nowMs: number): boolean {
  if (f === "all") return true;
  if (f === "paused") return a.status !== "active";
  if (f === "active") return a.status === "active" && a.cooldown_until <= nowMs;
  if (f === "cooling") return a.status === "active" && a.cooldown_until > nowMs;
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

function AccountRow({
  a,
  nowMs,
  isInUse,
  isSelected,
  onClickRow,
  onUseNow,
  onRefresh,
  onDelete,
  onTogglePause,
  busy,
}: {
  a: Account;
  nowMs: number;
  isInUse: boolean;
  isSelected: boolean;
  onClickRow: () => void;
  onUseNow: () => void;
  onRefresh: () => void;
  onDelete: () => void;
  onTogglePause: () => void;
  busy: boolean;
}) {
  const paused = a.status !== "active";
  const status = rowStatus(a, nowMs);
  const peak = peakUtilization(a);
  const ownerLine =
    a.full_name && a.email
      ? `${a.full_name} · ${a.email}`
      : a.email || a.full_name || "—";

  return (
    <div
      className={`row ${isSelected ? "selected" : ""} ${
        isInUse ? "active" : ""
      }`}
      onClick={onClickRow}
      role="button"
      tabIndex={0}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          onClickRow();
        }
      }}
    >
      <span className={`row-status ${status.tone}`} aria-label={status.text} />
      <FoxAvatar id={a.id} size={32} />
      <div className="row-main">
        <div className="row-title">
          <span className="name">{a.name}</span>
          {a.plan && <span className="pill">{a.plan}</span>}
          {isInUse && <span className="pill active-pill">{t("accounts.row.in_use")}</span>}
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
            label: isInUse ? t("accounts.kebab.in_use") : t("accounts.kebab.use_now"),
            onClick: onUseNow,
            disabled: isInUse || !isSelectable(a, nowMs),
          },
          { label: t("accounts.kebab.refresh"), onClick: onRefresh },
          { label: paused ? t("accounts.kebab.resume") : t("accounts.kebab.pause"), onClick: onTogglePause },
          { label: t("accounts.kebab.delete"), onClick: onDelete, danger: true },
        ]}
      />
      <Icon d={ICON_CHEVRON_RIGHT} className="row-chevron" />
    </div>
  );
}

function SkeletonRow() {
  return (
    <div className="row skeleton" aria-busy="true" aria-label={t("accounts.skeleton.aria")}>
      <span className="row-status sk-dot" />
      <span className="sk-avatar" aria-hidden />
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

export function AccountsPage({
  accounts,
  managedAccountId,
  nowMs,
  selectedAccountId,
  busyAccountId,
  addAccountTick,
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
}: {
  accounts: Account[];
  managedAccountId: number;
  nowMs: number;
  selectedAccountId: number | null;
  busyAccountId: number | null;
  addAccountTick: number;
  autoSwitch: { enabled: boolean; policy: "lru" | "lowest" | "rr" };
  onAutoSwitchToggle: () => void;
  onSelectRow: (id: number | null) => void;
  onUseNow: (id: number) => void;
  onRefreshAccount: (id: number) => void;
  onTogglePause: (a: Account) => void;
  onDelete: (id: number) => void;
  onRefresh: () => Promise<void>;
  onError: (msg: string) => void;
  stale: boolean;
}) {
  const [loginState, setLoginState] = useState<LoginState>({ phase: "idle" });
  const [pasted, setPasted] = useState("");
  const [copied, setCopied] = useState(false);
  const [search, setSearch] = useState("");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");

  const submitting = loginState.phase === "submitting";
  const modalOpen =
    loginState.phase === "started" || loginState.phase === "submitting";
  const activeAccount =
    managedAccountId !== 0
      ? accounts.find((a) => a.id === managedAccountId) ?? null
      : null;

  const filtered = useMemo(() => {
    return accounts.filter(
      (a) => matchesQuery(a, search) && matchesStatus(a, statusFilter, nowMs),
    );
  }, [accounts, search, statusFilter, nowMs]);

  const startLogin = useCallback(async () => {
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
  }, []);

  useEffect(() => {
    if (addAccountTick === 0) return;
    if (loginState.phase === "idle") startLogin();
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

  return (
    <>
      <Topbar
        title={t("accounts.title")}
        status={
          activeAccount
            ? { label: tf("accounts.status.managing", { name: activeAccount.name }), tone: "ok" }
            : { label: t("accounts.status.idle"), tone: "muted" }
        }
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
        actions={
          <button
            className="btn btn-primary"
            onClick={startLogin}
            disabled={modalOpen}
          >
            <Icon d={ICON_PLUS} />
            {t("accounts.add_button")}
          </button>
        }
      />
      <div className="page">
        <div className="accounts-toolbar">
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
                  ? activeAccount
                    ? tf("accounts.section.total_one_active", { total: accounts.length })
                    : tf("accounts.section.total", { total: accounts.length })
                  : tf("accounts.section.filtered", {
                      shown: filtered.length,
                      total: accounts.length,
                    })}
            </span>
          </div>
          {accounts.length === 0 ? (
            <div className="list">
              {submitting ? (
                <SkeletonRow />
              ) : (
                <div className="list-empty">
                  <p>{t("accounts.empty.no_pool")}</p>
                  <button className="btn btn-secondary" onClick={startLogin}>
                    <Icon d={ICON_PLUS} />
                    {t("accounts.empty.add_first")}
                  </button>
                </div>
              )}
            </div>
          ) : filtered.length === 0 ? (
            <div className="list">
              <div className="list-empty">
                <p>{t("accounts.empty.no_match")}</p>
                <button
                  className="btn btn-ghost"
                  onClick={() => {
                    setSearch("");
                    setStatusFilter("all");
                  }}
                >
                  {t("accounts.empty.clear_filters")}
                </button>
              </div>
            </div>
          ) : (
            <div className="list">
              {submitting && <SkeletonRow />}
              {filtered.map((a) => (
                <AccountRow
                  key={a.id}
                  a={a}
                  nowMs={nowMs}
                  isInUse={a.id === managedAccountId}
                  isSelected={a.id === selectedAccountId}
                  onClickRow={() =>
                    onSelectRow(selectedAccountId === a.id ? null : a.id)
                  }
                  onUseNow={() => onUseNow(a.id)}
                  onRefresh={() => onRefreshAccount(a.id)}
                  onDelete={() => onDelete(a.id)}
                  onTogglePause={() => onTogglePause(a)}
                  busy={busyAccountId === a.id}
                />
              ))}
            </div>
          )}
        </div>

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
      </div>
    </>
  );
}
