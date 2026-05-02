import React, { useCallback, useRef, useState } from "react";
import type { Account, ThresholdInput, UsageWindow } from "../api";
import { accountIsCooling, accountResetAt } from "../api";
import { Drawer } from "./Drawer";
import { FoxAvatar } from "./FoxAvatar";
import { t, tf } from "../i18n";

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
  if (a.status !== "active") return { text: t("drawer.status.paused"), tone: "muted" };
  if (a.token_expired) return { text: t("drawer.status.token_expired"), tone: "danger" };
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
  if (a.expires_at - nowMs < 5 * 60 * 1000) {
    return { text: t("drawer.status.refresh_due"), tone: "warn" };
  }
  return { text: t("drawer.status.active"), tone: "ok" };
}

function isSelectable(a: Account): boolean {
  return (
    a.status === "active" && !a.token_expired && !accountIsCooling(a)
  );
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
}: {
  label: string;
  win: UsageWindow | undefined;
  nowMs: number;
  threshold: number;
  onCommitThreshold: (pct: number) => void;
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
          className={`usage-threshold ${draft !== null ? "dragging" : ""}`}
          style={{ left: `${markerPct}%` }}
          title={tf("drawer.usage.threshold_title", { pct: Math.round(markerPct) })}
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={finishDrag}
          onPointerCancel={finishDrag}
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

export function AccountDrawer({
  account,
  nowMs,
  isInUse,
  busy,
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
  onClose: () => void;
  onSelect: () => void;
  onRefresh: () => void;
  onTogglePause: () => void;
  onSetThresholds: (t: ThresholdInput) => void;
  onDelete: () => void;
}) {
  const status = rowStatus(account, nowMs);
  const paused = account.status !== "active";

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
          <button
            type="button"
            className="btn btn-primary"
            onClick={onSelect}
            disabled={isInUse || !isSelectable(account) || busy}
          >
            {isInUse ? t("drawer.actions.in_use") : t("drawer.actions.use_now")}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onRefresh}
            disabled={busy}
          >
            {t("drawer.actions.refresh")}
          </button>
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
        </div>
      </div>

      <div className="drawer-section">
        <h3 className="drawer-section-title">{t("drawer.section.usage")}</h3>
        <div className="usage-list">
          <UsageBar
            label={t("drawer.usage.5h")}
            win={account.five_hour}
            nowMs={nowMs}
            threshold={account.five_hour_threshold}
            onCommitThreshold={(pct) => commit("five_hour", pct)}
          />
          <UsageBar
            label={t("drawer.usage.7d_opus")}
            win={account.seven_day}
            nowMs={nowMs}
            threshold={account.seven_day_threshold}
            onCommitThreshold={(pct) => commit("seven_day", pct)}
          />
          <UsageBar
            label={t("drawer.usage.7d_sonnet")}
            win={account.seven_day_sonnet}
            nowMs={nowMs}
            threshold={account.seven_day_sonnet_threshold}
            onCommitThreshold={(pct) => commit("seven_day_sonnet", pct)}
          />
        </div>
      </div>

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
          <div>
            <dt>{t("drawer.detail.token_expires")}</dt>
            <dd>{fmtRemaining(account.expires_at - nowMs)}</dd>
          </div>
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
