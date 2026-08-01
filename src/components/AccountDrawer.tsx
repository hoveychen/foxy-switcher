import React, { useCallback, useEffect, useRef, useState } from "react";
import type {
  Account,
  AccountAttribution,
  DeviceShare,
  OpenRouterConfig,
  ThresholdInput,
  UsageWindow,
} from "../api";
import {
  accountHasOAuthToken,
  accountIsCooling,
  accountOutOfCredit,
  accountRefreshDue,
  accountResetAt,
  scopedIsThrottled,
  apiClient,
} from "../api";
import { Drawer } from "./Drawer";
import { FoxAvatar } from "./FoxAvatar";
import { t, tf, scopedUsageLabel } from "../i18n";

// Stable per-device segment colours for the attribution bars. Index-assigned
// (backend already sorts devices by total contribution), wrapping if a pool
// somehow has more devices than colours; the unattributed bucket uses grey.
const ATTR_PALETTE = ["#e0823d", "#3d9be0", "#6dbf6d", "#c062c0", "#d9b54a"];
const ATTR_UNATTRIBUTED_COLOR = "#9aa0a6";

type Tone = "ok" | "warn" | "danger" | "muted";

function fmtRemaining(ms: number): string {
  if (ms <= 0) return t("drawer.fmt.expired");
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function fmtResetsAt(rfc3339: string, nowMs: number): string {
  if (!rfc3339) return "—";
  const ts = Date.parse(rfc3339);
  if (Number.isNaN(ts)) return rfc3339;
  const diff = ts - nowMs;
  if (diff <= 0) return t("drawer.usage.rolling_over");
  return tf("drawer.usage.resets_in", { time: fmtRemaining(diff) });
}

function rowStatus(a: Account, nowMs: number): { text: string; tone: Tone } {
  // Foreign lease wins over other "healthy" statuses — saying "active"
  // when another device is actually holding it would contradict the
  // leased-pill rendered above and confuse the user.
  if (a.lease && !a.lease.mine) {
    return {
      text: tf("drawer.status.leased_by_other", {
        device: a.lease.device_name || a.lease.device_id || "—",
        remaining: fmtRemaining(a.lease.expires_at - nowMs),
      }),
      tone: "muted",
    };
  }
  if (a.status === "org_disabled")
    return { text: t("drawer.status.org_disabled"), tone: "danger" };
  if (a.status === "needs_reauth")
    return { text: t("drawer.status.needs_reauth"), tone: "danger" };
  if (a.status !== "active") return { text: t("drawer.status.paused"), tone: "muted" };
  if (a.token_expired) return { text: t("drawer.status.token_expired"), tone: "danger" };
  // Ahead of cooling: an empty account is unusable outright, not throttled.
  if (accountOutOfCredit(a))
    return { text: t("drawer.status.out_of_credit"), tone: "danger" };
  if (accountIsCooling(a)) {
    const reset = accountResetAt(a, new Date(nowMs));
    if (reset > 0) {
      return {
        text: tf("drawer.status.cooling", { time: fmtRemaining(reset - nowMs) }),
        tone: "warn",
      };
    }
    return { text: t("drawer.status.cooling_no_reset"), tone: "warn" };
  }
  if (accountRefreshDue(a, nowMs)) {
    return { text: t("drawer.status.refresh_due"), tone: "warn" };
  }
  return { text: t("drawer.status.active"), tone: "ok" };
}

function isSelectable(a: Account): boolean {
  // Foreign-leased accounts hit the leases_account_id_uniq index → 409 on
  // AcquireLease. Mirror AccountsPage so the drawer's primary button is
  // disabled instead of letting the user trigger an error toast.
  if (a.lease && !a.lease.mine) return false;
  return (
    a.status === "active" && !a.token_expired && !accountIsCooling(a)
  );
}

function foreignLease(a: Account) {
  return a.lease && !a.lease.mine ? a.lease : null;
}

function utilizationTone(pct: number): Tone {
  if (pct >= 90) return "danger";
  if (pct >= 75) return "warn";
  return "ok";
}

function UsageBar({
  label,
  win,
  nowMs,
  threshold,
  onCommitThreshold,
  readOnly = false,
}: {
  label: string;
  win: UsageWindow | undefined;
  nowMs: number;
  threshold: number;
  onCommitThreshold: (pct: number) => void;
  // Agent mode: render the threshold marker but disable dragging — the
  // value comes from the vault and admins set it on the vault web UI.
  readOnly?: boolean;
}) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const draggingRef = useRef(false);
  const [draft, setDraft] = useState<number | null>(null);

  const pctFromClientX = useCallback(
    (clientX: number) => {
      const el = wrapRef.current;
      if (!el) return threshold;
      const rect = el.getBoundingClientRect();
      if (rect.width <= 0) return threshold;
      const v = ((clientX - rect.left) / rect.width) * 100;
      if (v < 0) return 0;
      if (v > 100) return 100;
      return v;
    },
    [threshold],
  );

  const onPointerDown = (e: React.PointerEvent<HTMLDivElement>) => {
    e.preventDefault();
    e.stopPropagation();
    draggingRef.current = true;
    e.currentTarget.setPointerCapture(e.pointerId);
    setDraft(pctFromClientX(e.clientX));
  };
  const onPointerMove = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!draggingRef.current) return;
    setDraft(pctFromClientX(e.clientX));
  };
  const finishDrag = (e: React.PointerEvent<HTMLDivElement>) => {
    if (!draggingRef.current) return;
    draggingRef.current = false;
    try {
      e.currentTarget.releasePointerCapture(e.pointerId);
    } catch {
      // already released — ignore
    }
    const next = Math.round(pctFromClientX(e.clientX));
    setDraft(null);
    if (next !== Math.round(threshold)) {
      onCommitThreshold(next);
    }
  };

  if (!win) {
    return (
      <div className="usage-row">
        <span className="usage-label">{label}</span>
        <span className="usage-empty">{t("drawer.usage.no_data")}</span>
      </div>
    );
  }
  const pct = Math.max(0, Math.min(100, win.utilization));
  const tone = utilizationTone(pct);
  const markerPct = draft !== null ? draft : threshold;
  return (
    <div className="usage-row">
      <span className="usage-label">{label}</span>
      <div className="usage-track-wrap" ref={wrapRef}>
        <div className={`usage-track ${tone}`}>
          <div className="usage-fill" style={{ width: `${pct}%` }} />
        </div>
        <div
          className={`usage-threshold ${draft !== null ? "dragging" : ""} ${readOnly ? "readonly" : ""}`}
          style={{ left: `${markerPct}%` }}
          title={tf("drawer.usage.threshold_title", { pct: Math.round(markerPct) })}
          onPointerDown={readOnly ? undefined : onPointerDown}
          onPointerMove={readOnly ? undefined : onPointerMove}
          onPointerUp={readOnly ? undefined : finishDrag}
          onPointerCancel={readOnly ? undefined : finishDrag}
          role="slider"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(markerPct)}
          aria-label={tf("drawer.usage.threshold_aria", { label })}
        />
      </div>
      <span className="usage-pct">{pct.toFixed(0)}%</span>
      <span className="usage-resets">{fmtResetsAt(win.resets_at, nowMs)}</span>
    </div>
  );
}

// AttributionSection renders the per-device quota breakdown for an account:
// for each rate-limit window, a stacked bar split by each device's share, plus
// a legend listing devices with their 5h share (the window that drives
// cooldown). Fetched on open; silently hidden if the endpoint is unavailable
// (e.g. an agent-mode proxy that doesn't forward it) so it never breaks the
// drawer.
function AttributionSection({ accountId, scopedLabel }: { accountId: number; scopedLabel?: string }) {
  const [attr, setAttr] = useState<AccountAttribution | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    let live = true;
    setAttr(null);
    setFailed(false);
    apiClient.attribution(accountId).then(
      (a) => {
        if (live) setAttr(a);
      },
      () => {
        if (live) setFailed(true);
      },
    );
    return () => {
      live = false;
    };
  }, [accountId]);

  if (failed || !attr) return null;

  const windows: { key: keyof Omit<DeviceShare, "device_id" | "device_name" | "last_used_at">; label: string }[] = [
    { key: "five_hour", label: t("drawer.usage.5h") },
    { key: "seven_day", label: t("drawer.usage.7d_opus") },
    { key: "seven_day_sonnet", label: scopedUsageLabel(scopedLabel) },
  ];

  const rows = attr.devices.map((d, i) => ({
    id: d.device_id || `dev-${i}`,
    name: d.device_name || d.device_id || "—",
    share: d,
    color: ATTR_PALETTE[i % ATTR_PALETTE.length],
  }));
  if (attr.unattributed) {
    rows.push({
      id: "__unattributed__",
      name: t("drawer.attr.unattributed"),
      share: attr.unattributed,
      color: ATTR_UNATTRIBUTED_COLOR,
    });
  }

  const totalFor = (key: typeof windows[number]["key"]) =>
    rows.reduce((sum, r) => sum + r.share[key], 0);

  let body: React.ReactNode;
  if (attr.sample_count < 2) {
    body = <p className="attr-hint">{t("drawer.attr.thin_data")}</p>;
  } else if (attr.devices.length === 0) {
    // Usage was observed but none of it overlapped a recorded lease segment
    // (e.g. the account was held before device-usage recording began). Showing
    // a lone "no device 100%" bar reads as broken — surface a clear
    // accumulating hint instead.
    body = <p className="attr-hint">{t("drawer.attr.accumulating")}</p>;
  } else {
    const total5h = totalFor("five_hour");
    body = (
      <>
        {windows.map((w) => {
          const total = totalFor(w.key);
          return (
            <div className="attr-window" key={w.key}>
              <span className="attr-window-label">{w.label}</span>
              <div className="attr-bar">
                {total > 0 ? (
                  rows.map((r) => {
                    const pct = (r.share[w.key] / total) * 100;
                    if (pct <= 0) return null;
                    return (
                      <div
                        key={r.id}
                        className="attr-seg"
                        style={{ width: `${pct}%`, backgroundColor: r.color }}
                        title={`${r.name} · ${pct.toFixed(0)}%`}
                        aria-label={tf("drawer.attr.share_aria", {
                          device: r.name,
                          window: w.label,
                        })}
                      />
                    );
                  })
                ) : (
                  <div className="attr-seg attr-seg-empty" />
                )}
              </div>
            </div>
          );
        })}
        <ul className="attr-legend">
          {rows.map((r) => (
            <li key={r.id}>
              <span className="attr-swatch" style={{ backgroundColor: r.color }} aria-hidden />
              <span className="attr-legend-name">{r.name}</span>
              {r.id !== "__unattributed__" && r.share.last_used_at ? (
                <span
                  className="attr-legend-lastused"
                  title={new Date(r.share.last_used_at).toLocaleString()}
                >
                  {tf("drawer.attr.last_used", {
                    time: fmtRemaining(Date.now() - r.share.last_used_at),
                  })}
                </span>
              ) : null}
              {total5h > 0 && (
                <span className="attr-legend-pct">
                  {((r.share.five_hour / total5h) * 100).toFixed(0)}%
                </span>
              )}
            </li>
          ))}
        </ul>
      </>
    );
  }

  return (
    <div className="drawer-section">
      <h3 className="drawer-section-title">{t("drawer.section.attribution")}</h3>
      <p className="attr-hint">{t("drawer.attr.hint")}</p>
      {body}
    </div>
  );
}

// OpenRouterSection is what the "usage" section becomes for an OpenRouter
// account. There are no subscription windows to draw — the account is
// pay-as-you-go — so the two things that actually govern it take their place:
// how much money is left, and the policy each device's key is minted under.
//
// Everything here comes from the accountView.openrouter payload the accounts
// list already carries; the drawer makes no extra request.
function OpenRouterSection({ or, nowMs }: { or: OpenRouterConfig; nowMs: number }) {
  const credit = or.credit;
  // The bar shows what is LEFT, not what was spent, so its length agrees with
  // the dollar figure printed next to it — a bar that fills up while the number
  // beside it shrinks reads as "$22.51 used".
  const remainingPct =
    credit && credit.total > 0
      ? Math.max(0, Math.min(100, (credit.remaining / credit.total) * 100))
      : 0;
  // out_of_credit first: it is the selector's own verdict, so the bar can't read
  // "healthy" on an account the vault has already stopped handing out.
  const tone: Tone =
    or.out_of_credit || remainingPct < 10
      ? "danger"
      : remainingPct < 25
        ? "warn"
        : "ok";
  // $0.12 of $510 is 0.02% — a sub-pixel fill that renders as an empty grey
  // track, indistinguishable from a broken bar. Floor any non-zero balance at a
  // visible sliver so "almost nothing left" still reads as red, not as missing.
  const barPct = remainingPct > 0 ? Math.max(1.5, remainingPct) : 0;

  return (
    <div className="drawer-section">
      <h3 className="drawer-section-title">{t("drawer.section.openrouter")}</h3>
      <div className="usage-list">
        <div className="usage-row">
          <span className="usage-label">{t("drawer.openrouter.credit")}</span>
          {credit ? (
            <>
              <div className="usage-track-wrap">
                <div className={`usage-track ${tone}`}>
                  <div className="usage-fill" style={{ width: `${barPct}%` }} />
                </div>
              </div>
              <span className={`usage-pct ${or.out_of_credit ? "or-hint-error" : ""}`}>
                ${credit.remaining.toFixed(2)}
              </span>
              <span className="usage-resets">
                {tf("drawer.openrouter.of_total", { total: credit.total.toFixed(2) })}
              </span>
            </>
          ) : (
            <span className="usage-empty">{t("drawer.openrouter.credit_unknown")}</span>
          )}
        </div>
      </div>
      {/* Reuses the modal's wording: the consequence is the part that matters,
          and it must read identically wherever the state is surfaced. */}
      {or.out_of_credit && credit && (
        <p className="text-meta or-hint or-hint-error">
          {tf("openrouter.credit.empty", { remaining: credit.remaining.toFixed(2) })}
        </p>
      )}

      <dl className="detail-meta">
        {credit && (
          <div>
            <dt>{t("drawer.openrouter.checked_at")}</dt>
            <dd>
              {tf("drawer.detail.usage_updated.ago", {
                time: fmtRemaining(nowMs - credit.checked_at),
              })}
            </dd>
          </div>
        )}
        <div>
          <dt>{t("drawer.openrouter.device_limit")}</dt>
          <dd>
            {or.limit_usd
              ? tf("drawer.openrouter.device_limit_value", {
                  limit: or.limit_usd.toFixed(2),
                  reset: t(`openrouter.reset.${or.limit_reset || "lifetime"}`),
                })
              : t("accounts.openrouter.no_limit")}
          </dd>
        </div>
        <div>
          <dt>{t("drawer.openrouter.key_kind")}</dt>
          <dd>
            {t(
              or.is_provisioning
                ? "drawer.openrouter.key_kind.provisioning"
                : "drawer.openrouter.key_kind.shared",
            )}
          </dd>
        </div>
        <div>
          <dt>{t("drawer.openrouter.derived_keys")}</dt>
          <dd>{or.derived_key_count}</dd>
        </div>
        {or.workspace_id && (
          <div>
            <dt>{t("openrouter.field.workspace")}</dt>
            <dd>{or.workspace_id}</dd>
          </div>
        )}
      </dl>

      <div className="or-models">
        <span className="usage-label">
          {tf("drawer.openrouter.models", { count: or.allowed_models.length })}
        </span>
        {or.allowed_models.length > 0 ? (
          <ul className="or-model-list">
            {or.allowed_models.map((m) => (
              <li key={m} className="pill or-model-pill">
                {m}
              </li>
            ))}
          </ul>
        ) : (
          <p className="usage-empty or-models-empty">{t("drawer.openrouter.models_none")}</p>
        )}
      </div>
    </div>
  );
}

export function AccountDrawer({
  account,
  nowMs,
  isInUse,
  busy,
  disableAdminActions = false,
  onClose,
  onSelect,
  onRefresh,
  onTogglePause,
  onSetThresholds,
  onDelete,
}: {
  account: Account;
  nowMs: number;
  isInUse: boolean;
  busy: boolean;
  // Agent mode hides Pause/Resume/Delete + threshold sliders; the
  // vault is the source of truth for those in agent topology and the
  // agent-side proxy 405s the corresponding /api/* routes.
  disableAdminActions?: boolean;
  onClose: () => void;
  onSelect: () => void;
  onRefresh: () => void;
  onTogglePause: () => void;
  onSetThresholds: (t: ThresholdInput) => void;
  onDelete: () => void;
}) {
  const status = rowStatus(account, nowMs);
  const paused = account.status !== "active";
  const fLease = foreignLease(account);

  const commit = (
    which: "five_hour" | "seven_day" | "seven_day_sonnet",
    pct: number,
  ) => {
    onSetThresholds({
      five_hour:
        which === "five_hour" ? pct : account.five_hour_threshold,
      seven_day:
        which === "seven_day" ? pct : account.seven_day_threshold,
      seven_day_sonnet:
        which === "seven_day_sonnet" ? pct : account.seven_day_sonnet_threshold,
    });
  };

  return (
    <Drawer open={true} title={account.name} onClose={onClose}>
      <div className="drawer-section">
        <div className="drawer-identity">
          <FoxAvatar name={account.name} size={56} className="drawer-identity-fox" />
          <div className="drawer-identity-line">
            {account.full_name && (
              <span className="drawer-identity-name">{account.full_name}</span>
            )}
            {account.plan && <span className="pill">{account.plan}</span>}
            {isInUse && <span className="pill active-pill">{t("drawer.identity.in_use")}</span>}
            {fLease && !isInUse && (
              <span className="pill leased-pill">
                {tf("drawer.identity.leased_by", {
                  device: fLease.device_name || fLease.device_id || "—",
                  remaining: fmtRemaining(fLease.expires_at - nowMs),
                })}
              </span>
            )}
          </div>
          {account.email && (
            <div className="text-meta">{account.email}</div>
          )}
          {account.organization_name && (
            <div className="text-meta">{account.organization_name}</div>
          )}
          <div className={`row-status-line ${status.tone}`}>
            <span className={`row-status ${status.tone}`} aria-hidden />
            {status.text}
          </div>
        </div>

        <div className="drawer-actions">
          {/* No "use now" for OpenRouter: it holds no lease, every authorised
              device is served from the lowest-id funded account at once, and
              /select now 409s rather than reporting a switch that never
              happened. The card kebab already made this call (AccountsPage
              swaps it for "edit policy"); the drawer had been missed. */}
          {account.provider !== "openrouter" && (
          <button
            type="button"
            className="btn btn-primary"
            onClick={onSelect}
            disabled={isInUse || !isSelectable(account) || busy}
            aria-busy={busy}
          >
            {busy ? (
              <>
                <span className="spinner" aria-hidden />
                {t("drawer.actions.switching")}
              </>
            ) : fLease ? (
              t("drawer.actions.leased_by_other")
            ) : isInUse ? (
              t("drawer.actions.in_use")
            ) : (
              t("drawer.actions.use_now")
            )}
          </button>
          )}
          {/* Refresh is hidden in agent mode — the cloud vault owns
              token rotation; surfacing the button here would be a
              foot-gun (no-op or 405 depending on whitelist). Also
              disabled when another device holds the lease, so we
              don't 401 their live CC session. Hidden too for accounts
              with no OAuth token at all (OpenRouter): there is nothing
              for a token refresh to rotate. */}
          {!disableAdminActions && accountHasOAuthToken(account) && (
            <button
              type="button"
              className="btn btn-secondary"
              onClick={onRefresh}
              disabled={busy || !!fLease}
            >
              {t("drawer.actions.refresh")}
            </button>
          )}
          {!disableAdminActions && (
            <>
              <button
                type="button"
                className="btn btn-secondary"
                onClick={onTogglePause}
                disabled={busy}
              >
                {paused ? t("drawer.actions.resume") : t("drawer.actions.pause")}
              </button>
              <button
                type="button"
                className="btn btn-ghost btn-danger"
                onClick={onDelete}
                disabled={busy}
              >
                {t("drawer.actions.delete")}
              </button>
            </>
          )}
        </div>
      </div>

      {/* Pay-as-you-go: no subscription windows, no per-device attribution to
          derive from them. Money and policy take their place. */}
      {account.provider === "openrouter" && account.openrouter ? (
        <OpenRouterSection or={account.openrouter} nowMs={nowMs} />
      ) : (
      <div className="drawer-section">
        <h3 className="drawer-section-title">{t("drawer.section.usage")}</h3>
        <div className="usage-list">
          <UsageBar
            label={t(account.provider === "codex" ? "accounts.usage.primary" : "drawer.usage.5h")}
            win={account.five_hour}
            nowMs={nowMs}
            threshold={account.five_hour_threshold}
            onCommitThreshold={(pct) => commit("five_hour", pct)}
            readOnly={disableAdminActions}
          />
          <UsageBar
            label={t(account.provider === "codex" ? "accounts.usage.secondary" : "drawer.usage.7d_opus")}
            win={account.seven_day}
            nowMs={nowMs}
            threshold={account.seven_day_threshold}
            onCommitThreshold={(pct) => commit("seven_day", pct)}
            readOnly={disableAdminActions}
          />
          {account.provider !== "codex" && (
            <UsageBar
              label={scopedUsageLabel(account.seven_day_scoped_label, scopedIsThrottled(account))}
              win={account.seven_day_sonnet}
              nowMs={nowMs}
              threshold={account.seven_day_sonnet_threshold}
              onCommitThreshold={(pct) => commit("seven_day_sonnet", pct)}
              readOnly={disableAdminActions}
            />
          )}
        </div>
      </div>
      )}

      {account.provider === "claude" && (
        <AttributionSection accountId={account.id} scopedLabel={account.seven_day_scoped_label} />
      )}

      <div className="drawer-section">
        <h3 className="drawer-section-title">{t("drawer.section.details")}</h3>
        <dl className="detail-meta">
          <div>
            <dt>{t("drawer.detail.status")}</dt>
            <dd>{status.text}</dd>
          </div>
          <div>
            <dt>{t("drawer.detail.last_used")}</dt>
            <dd>
              {account.last_used_at
                ? new Date(account.last_used_at).toLocaleString()
                : t("drawer.detail.last_used.never")}
            </dd>
          </div>
          {/* Both rows are OAuth/usage-poller shaped. An OpenRouter account has
              neither — no token lifetime, and no Anthropic usage window to
              poll — so rendering them printed a permanent "expired / pending"
              on an account that is perfectly healthy. */}
          {accountHasOAuthToken(account) && (
            <div>
              <dt>{t("drawer.detail.token_expires")}</dt>
              <dd>{fmtRemaining(account.expires_at - nowMs)}</dd>
            </div>
          )}
          {account.provider !== "openrouter" && (
            <div>
              <dt>{t("drawer.detail.usage_updated")}</dt>
              <dd>
                {account.usage_fetched_at
                  ? tf("drawer.detail.usage_updated.ago", {
                      time: fmtRemaining(nowMs - account.usage_fetched_at),
                    })
                  : t("drawer.detail.usage_updated.pending")}
              </dd>
            </div>
          )}
          {account.account_uuid && (
            <div>
              <dt>{t("drawer.detail.account_uuid")}</dt>
              <dd className="detail-uuid">{account.account_uuid}</dd>
            </div>
          )}
        </dl>
      </div>
    </Drawer>
  );
}
