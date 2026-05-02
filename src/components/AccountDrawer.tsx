import React, { useCallback, useRef, useState } from "react";
import type { Account, ThresholdInput, UsageWindow } from "../api";
import { Drawer } from "./Drawer";
import { FoxAvatar } from "./FoxAvatar";

type Tone = "ok" | "warn" | "danger" | "muted";

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

function rowStatus(a: Account, nowMs: number): { text: string; tone: Tone } {
  if (a.status !== "active") return { text: "paused", tone: "muted" };
  if (a.token_expired) return { text: "token expired", tone: "danger" };
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
  return (
    a.status === "active" && !a.token_expired && a.cooldown_until <= nowMs
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
        <span className="usage-empty">No data yet</span>
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
          title={`Skip ≥ ${Math.round(markerPct)}%`}
          onPointerDown={onPointerDown}
          onPointerMove={onPointerMove}
          onPointerUp={finishDrag}
          onPointerCancel={finishDrag}
          role="slider"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(markerPct)}
          aria-label={`${label} skip threshold`}
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
          <FoxAvatar id={account.id} size={56} className="drawer-identity-fox" />
          <div className="drawer-identity-line">
            {account.full_name && (
              <span className="drawer-identity-name">{account.full_name}</span>
            )}
            {account.plan && <span className="pill">{account.plan}</span>}
            {isInUse && <span className="pill active-pill">In use</span>}
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
            disabled={isInUse || !isSelectable(account, nowMs) || busy}
          >
            {isInUse ? "In use" : "Use now"}
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onRefresh}
            disabled={busy}
          >
            Refresh
          </button>
          <button
            type="button"
            className="btn btn-secondary"
            onClick={onTogglePause}
            disabled={busy}
          >
            {paused ? "Resume" : "Pause"}
          </button>
          <button
            type="button"
            className="btn btn-ghost btn-danger"
            onClick={onDelete}
            disabled={busy}
          >
            Delete
          </button>
        </div>
      </div>

      <div className="drawer-section">
        <h3 className="drawer-section-title">Usage</h3>
        <div className="usage-list">
          <UsageBar
            label="5h"
            win={account.five_hour}
            nowMs={nowMs}
            threshold={account.five_hour_threshold}
            onCommitThreshold={(pct) => commit("five_hour", pct)}
          />
          <UsageBar
            label="7d Opus"
            win={account.seven_day}
            nowMs={nowMs}
            threshold={account.seven_day_threshold}
            onCommitThreshold={(pct) => commit("seven_day", pct)}
          />
          <UsageBar
            label="7d Sonnet"
            win={account.seven_day_sonnet}
            nowMs={nowMs}
            threshold={account.seven_day_sonnet_threshold}
            onCommitThreshold={(pct) => commit("seven_day_sonnet", pct)}
          />
        </div>
      </div>

      <div className="drawer-section">
        <h3 className="drawer-section-title">Details</h3>
        <dl className="detail-meta">
          <div>
            <dt>Status</dt>
            <dd>{status.text}</dd>
          </div>
          <div>
            <dt>Last used</dt>
            <dd>
              {account.last_used_at
                ? new Date(account.last_used_at).toLocaleString()
                : "Never"}
            </dd>
          </div>
          <div>
            <dt>Token expires</dt>
            <dd>{fmtRemaining(account.expires_at - nowMs)}</dd>
          </div>
          <div>
            <dt>Usage updated</dt>
            <dd>
              {account.usage_fetched_at
                ? `${fmtRemaining(nowMs - account.usage_fetched_at)} ago`
                : "Pending"}
            </dd>
          </div>
        </dl>
      </div>
    </Drawer>
  );
}
