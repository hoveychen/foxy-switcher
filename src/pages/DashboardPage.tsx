import { useEffect, useState } from "react";
import { Topbar } from "../components/Topbar";
import { FoxAvatar } from "../components/FoxAvatar";
import {
  apiClient,
  type Account,
  type ActivityEvent,
  type DashboardTrendBucket,
  type UsageWindow,
} from "../api";
import type { Route } from "../components/Sidebar";
import { t, tf } from "../i18n";

type Tone = "ok" | "warn" | "danger" | "muted";
type Policy = "lru" | "lowest" | "rr";

const POLICIES: Array<{ key: Policy; labelKey: string; subKey: string }> = [
  { key: "lru", labelKey: "dashboard.policies.lru.label", subKey: "dashboard.policies.lru.sub" },
  { key: "lowest", labelKey: "dashboard.policies.lowest.label", subKey: "dashboard.policies.lowest.sub" },
  { key: "rr", labelKey: "dashboard.policies.rr.label", subKey: "dashboard.policies.rr.sub" },
];

function fmtRemaining(ms: number): string {
  if (ms <= 0) return t("dashboard.fmt.expired");
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function fmtRelativeShort(ms: number, now: number): string {
  const diff = Math.max(0, now - ms);
  const s = Math.floor(diff / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  const d = Math.floor(h / 24);
  return `${d}d`;
}

function greeting(now: Date): string {
  const h = now.getHours();
  if (h < 5) return t("dashboard.greeting.late");
  if (h < 12) return t("dashboard.greeting.morning");
  if (h < 18) return t("dashboard.greeting.afternoon");
  return t("dashboard.greeting.evening");
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

export function DashboardPage({
  accounts,
  managedAccountId,
  nowMs,
  onNavigate,
  autoSwitch,
  onAutoSwitchChange,
  onAutoSwitchToggle,
  recentEvents,
  stale,
}: {
  accounts: Account[];
  managedAccountId: number;
  nowMs: number;
  onNavigate: (r: Route) => void;
  autoSwitch: { enabled: boolean; policy: Policy };
  onAutoSwitchChange: (v: { enabled: boolean; policy: Policy }) => void;
  onAutoSwitchToggle: () => void;
  recentEvents: ActivityEvent[];
  stale: boolean;
}) {
  const active = accounts.find((a) => a.id === managedAccountId) ?? null;
  const greet = greeting(new Date(nowMs));
  const firstName = active?.full_name?.split(" ")[0];

  const totals = accounts.reduce(
    (acc, a) => {
      if (a.status !== "active") acc.paused += 1;
      else if (a.token_expired) acc.expired += 1;
      else if (a.cooldown_until > nowMs) acc.cooling += 1;
      else acc.healthy += 1;
      return acc;
    },
    { healthy: 0, cooling: 0, paused: 0, expired: 0 },
  );

  let peakAcc: Account | null = null;
  let peakPct = -1;
  for (const a of accounts) {
    const p = peakUtilization(a);
    if (p !== null && p > peakPct) {
      peakPct = p;
      peakAcc = a;
    }
  }

  const cooling = accounts
    .filter((a) => a.status === "active" && a.cooldown_until > nowMs)
    .sort((a, b) => a.cooldown_until - b.cooldown_until);
  const nextCooldown = cooling[0] ?? null;

  // Fetch the 24h trend on mount + every 5 min. Lives here (not in App.tsx)
  // because no other page needs it — re-mounting the chart on tab return is
  // cheap and avoids polling when the user is on Activity / Settings.
  const [trend, setTrend] = useState<DashboardTrendBucket[]>([]);
  useEffect(() => {
    let alive = true;
    const fetchOnce = () => {
      apiClient
        .getDashboard()
        .then((d) => {
          if (alive) setTrend(d.trend);
        })
        .catch(() => {
          // Older daemon without /api/dashboard — leave the card empty.
        });
    };
    fetchOnce();
    const i = setInterval(fetchOnce, 5 * 60 * 1000);
    return () => {
      alive = false;
      clearInterval(i);
    };
  }, []);

  const compact = [...accounts]
    .sort((a, b) => {
      const ap = peakUtilization(a) ?? -1;
      const bp = peakUtilization(b) ?? -1;
      return bp - ap;
    })
    .slice(0, 5);

  return (
    <>
      <Topbar
        title={t("dashboard.title")}
        status={
          active
            ? { label: tf("dashboard.status.managing", { name: active.name }), tone: "ok" }
            : { label: t("dashboard.status.idle"), tone: "muted" }
        }
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
      />
      <div className="page">
        <div className="dash-welcome">
          <h2>
            {greet}
            {firstName ? `, ${firstName}` : ""}
          </h2>
          <p className="text-meta">
            {accounts.length === 0
              ? t("dashboard.welcome.empty")
              : accounts.length === 1
                ? t("dashboard.welcome.watching_one")
                : tf("dashboard.welcome.watching_many", { count: accounts.length })}
          </p>
        </div>

        <div className="dash-grid">
          <div className="dash-main">
            {active ? (
              <HeroCard
                account={active}
                nowMs={nowMs}
                onView={() => onNavigate("accounts")}
              />
            ) : (
              <HeroEmpty onAdd={() => onNavigate("accounts")} />
            )}

            <div className="kpi-grid kpi-grid-3">
              <KpiCard
                label={t("dashboard.kpi.pool_size")}
                value={
                  accounts.length === 0
                    ? t("dashboard.kpi.pool_empty")
                    : `${totals.healthy + totals.cooling}/${accounts.length}`
                }
                sub={
                  accounts.length === 0
                    ? t("dashboard.kpi.no_accounts_yet")
                    : [
                        tf("dashboard.kpi.paused", { count: totals.paused }),
                        tf("dashboard.kpi.in_cooldown", { count: totals.cooling }),
                        ...(totals.expired > 0
                          ? [tf("dashboard.kpi.expired", { count: totals.expired })]
                          : []),
                      ].join(" · ")
                }
                tone={accounts.length === 0 ? "muted" : "ok"}
              />
              <KpiCard
                label={t("dashboard.kpi.peak_util")}
                value={peakPct >= 0 ? `${peakPct.toFixed(0)}%` : t("dashboard.kpi.no_data")}
                sub={
                  peakAcc
                    ? tf("dashboard.kpi.from", { name: peakAcc.name })
                    : t("dashboard.kpi.no_usage")
                }
                tone={peakPct >= 0 ? utilizationTone(peakPct) : "muted"}
              />
              <KpiCard
                label={t("dashboard.kpi.next_cooldown")}
                value={
                  nextCooldown
                    ? fmtRemaining(nextCooldown.cooldown_until - nowMs)
                    : t("dashboard.kpi.no_cooldown")
                }
                sub={
                  nextCooldown
                    ? tf("dashboard.kpi.thaws_next", { name: nextCooldown.name })
                    : t("dashboard.kpi.no_in_cooldown")
                }
                tone={nextCooldown ? "warn" : "muted"}
              />
            </div>

            <section className="dash-section">
              <div className="dash-section-header">
                <h3>
                  {t("dashboard.section.utilization")}
                  {stale && <StaleDot />}
                </h3>
                <span className="text-meta">{t("dashboard.section.utilization_sub")}</span>
              </div>
              <UsageTrendChart trend={trend} />
            </section>

            <section className="dash-section">
              <div className="dash-section-header">
                <h3>
                  {t("dashboard.section.accounts")}
                  {stale && <StaleDot />}
                </h3>
                <button
                  type="button"
                  className="btn btn-ghost"
                  onClick={() => onNavigate("accounts")}
                >
                  {t("dashboard.section.view_all")}
                </button>
              </div>
              {compact.length === 0 ? (
                <div className="page-empty">
                  <p>{t("dashboard.empty.no_accounts")}</p>
                  <button
                    type="button"
                    className="btn btn-secondary"
                    onClick={() => onNavigate("accounts")}
                  >
                    {t("dashboard.empty.add_first")}
                  </button>
                </div>
              ) : (
                <div className="compact-list">
                  {compact.map((a) => (
                    <CompactRow
                      key={a.id}
                      a={a}
                      nowMs={nowMs}
                      isActive={a.id === managedAccountId}
                    />
                  ))}
                </div>
              )}
            </section>
          </div>

          <aside className="dash-side">
            <AutoSwitchCard
              value={autoSwitch}
              onChange={onAutoSwitchChange}
            />

            <section className="dash-section">
              <div className="dash-section-header">
                <h3>
                  {t("dashboard.section.recent_activity")}
                  {stale && <StaleDot />}
                </h3>
                <button
                  type="button"
                  className="btn btn-ghost"
                  onClick={() => onNavigate("activity")}
                >
                  {t("dashboard.section.view_all")}
                </button>
              </div>
              {recentEvents.length === 0 ? (
                <div className="page-empty">
                  <p className="text-meta">{t("dashboard.empty.no_events")}</p>
                </div>
              ) : (
                <ol className="recent-activity-list">
                  {recentEvents.slice(0, 5).map((ev) => (
                    <li
                      key={ev.id}
                      className={`recent-activity-item sev-${ev.severity}`}
                    >
                      <span
                        className={`recent-activity-dot sev-${ev.severity}`}
                        aria-hidden
                      />
                      <div className="recent-activity-body">
                        <div className="recent-activity-message">
                          {ev.message}
                        </div>
                        <div className="text-meta recent-activity-meta">
                          {fmtRelativeShort(ev.timestamp, nowMs)} · {ev.type}
                        </div>
                      </div>
                    </li>
                  ))}
                </ol>
              )}
            </section>
          </aside>
        </div>
      </div>
    </>
  );
}

function HeroCard({
  account,
  nowMs,
  onView,
}: {
  account: Account;
  nowMs: number;
  onView: () => void;
}) {
  return (
    <section className="dash-hero">
      <FoxAvatar id={account.id} size={64} className="dash-hero-fox" />
      <div className="dash-hero-body">
        <div className="dash-hero-eyebrow">{t("dashboard.hero.in_use")}</div>
        <h2 className="dash-hero-title">{account.name}</h2>
        <div className="dash-hero-meta">
          {account.plan && <span className="pill">{account.plan}</span>}
          {account.email && (
            <span className="text-meta">{account.email}</span>
          )}
        </div>
        <div className="dash-hero-stat">
          {tf("dashboard.hero.token_expires", {
            time: fmtRemaining(account.expires_at - nowMs),
          })}
        </div>
      </div>
      <button type="button" className="btn btn-secondary" onClick={onView}>
        {t("dashboard.hero.open")}
      </button>
    </section>
  );
}

function HeroEmpty({ onAdd }: { onAdd: () => void }) {
  return (
    <section className="dash-hero dash-hero-empty">
      <div className="dash-hero-avatar muted">
        <span>?</span>
      </div>
      <div className="dash-hero-body">
        <div className="dash-hero-eyebrow">{t("dashboard.hero.in_use")}</div>
        <h2 className="dash-hero-title">{t("dashboard.hero.no_account")}</h2>
        <div className="dash-hero-meta text-meta">
          {t("dashboard.hero.click_use")}
        </div>
      </div>
      <button type="button" className="btn btn-primary" onClick={onAdd}>
        {t("dashboard.hero.add_account")}
      </button>
    </section>
  );
}

function AutoSwitchCard({
  value,
  onChange,
}: {
  value: { enabled: boolean; policy: Policy };
  onChange: (v: { enabled: boolean; policy: Policy }) => void;
}) {
  return (
    <section className="auto-switch-card">
      <header className="auto-switch-head">
        <div>
          <div className="auto-switch-eyebrow">{t("dashboard.auto_switch.eyebrow")}</div>
          <h3>{value.enabled ? t("dashboard.auto_switch.on") : t("dashboard.auto_switch.off")}</h3>
        </div>
        <button
          type="button"
          role="switch"
          aria-checked={value.enabled}
          aria-label={t("dashboard.auto_switch.aria")}
          className={`toggle ${value.enabled ? "on" : "off"}`}
          onClick={() => onChange({ ...value, enabled: !value.enabled })}
        >
          <span className="toggle-thumb" aria-hidden />
        </button>
      </header>
      <p className="text-meta auto-switch-desc">
        {value.enabled
          ? t("dashboard.auto_switch.desc.on")
          : t("dashboard.auto_switch.desc.off")}
      </p>
      <fieldset className="auto-switch-policy" disabled={!value.enabled}>
        <legend className="text-meta">{t("dashboard.auto_switch.policy_legend")}</legend>
        {POLICIES.map((p) => (
          <label key={p.key} className="auto-switch-radio">
            <input
              type="radio"
              name="auto-switch-policy"
              checked={value.policy === p.key}
              onChange={() => onChange({ ...value, policy: p.key })}
            />
            <span className="auto-switch-radio-text">
              <span className="auto-switch-radio-label">{t(p.labelKey)}</span>
              <span className="text-meta">{t(p.subKey)}</span>
            </span>
          </label>
        ))}
      </fieldset>
      <footer className="auto-switch-foot text-meta">
        {t("dashboard.auto_switch.foot.prefix")}
        <code>POST /api/auto-switch</code>
        {t("dashboard.auto_switch.foot.suffix")}
      </footer>
    </section>
  );
}

function KpiCard({
  label,
  value,
  sub,
  tone,
}: {
  label: string;
  value: string;
  sub: string;
  tone: Tone;
}) {
  return (
    <div className={`kpi-card tone-${tone}`}>
      <span className="kpi-label">{label}</span>
      <span className="kpi-value">{value}</span>
      <span className="kpi-sub">{sub}</span>
    </div>
  );
}

// StaleDot is a tiny inline indicator that lives inside section headers and
// pulses when the daemon stops answering. Keeps "this card is showing old
// data" visible without commandeering the whole header for a banner.
function StaleDot() {
  return (
    <span
      className="stale-dot"
      role="status"
      title={t("dashboard.stale.title")}
      aria-label={t("dashboard.stale.aria")}
    />
  );
}

// UsageTrendChart renders three polylines (5h / 7d / 7d-Sonnet) over a
// 24-bucket horizontal axis. Inline SVG is used instead of a chart library so
// the daemon stays dependency-light — the visual is deliberately compact.
function UsageTrendChart({ trend }: { trend: DashboardTrendBucket[] }) {
  const W = 720;
  const H = 160;
  const PAD_X = 12;
  const PAD_TOP = 8;
  const PAD_BOTTOM = 20;

  const hasData = trend.some(
    (b) => b.five_hour > 0 || b.seven_day > 0 || b.seven_day_sonnet > 0,
  );

  if (trend.length === 0 || !hasData) {
    return (
      <div className="trend-empty text-meta">
        {t("dashboard.trend.empty")}
      </div>
    );
  }

  const n = trend.length;
  const innerW = W - PAD_X * 2;
  const innerH = H - PAD_TOP - PAD_BOTTOM;
  const x = (i: number) => PAD_X + (n === 1 ? innerW / 2 : (i * innerW) / (n - 1));
  // Utilization is 0–100; scale linearly.
  const y = (v: number) => PAD_TOP + innerH - (Math.max(0, Math.min(100, v)) / 100) * innerH;

  const path = (sel: (b: DashboardTrendBucket) => number) =>
    trend.map((b, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(sel(b)).toFixed(1)}`).join(" ");

  // Hour labels at 0, 6, 12, 18, 24h-back markers.
  const labelIdx = [0, Math.floor(n / 4), Math.floor(n / 2), Math.floor((3 * n) / 4), n - 1];
  const fmtHour = (ts: number) => {
    const d = new Date(ts);
    return `${d.getHours().toString().padStart(2, "0")}:00`;
  };

  return (
    <svg
      className="trend-chart"
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={t("dashboard.trend.aria")}
    >
      {/* Reference grid at 50% and 90% (the threshold band). */}
      <line
        x1={PAD_X}
        x2={W - PAD_X}
        y1={y(50)}
        y2={y(50)}
        className="trend-grid"
      />
      <line
        x1={PAD_X}
        x2={W - PAD_X}
        y1={y(90)}
        y2={y(90)}
        className="trend-grid trend-grid-warn"
      />

      <path className="trend-line trend-five" d={path((b) => b.five_hour)} />
      <path className="trend-line trend-seven" d={path((b) => b.seven_day)} />
      <path
        className="trend-line trend-sonnet"
        d={path((b) => b.seven_day_sonnet)}
      />

      {labelIdx.map((i) => (
        <text
          key={i}
          x={x(i)}
          y={H - 4}
          textAnchor={i === 0 ? "start" : i === n - 1 ? "end" : "middle"}
          className="trend-axis"
        >
          {fmtHour(trend[i].ts)}
        </text>
      ))}

      {/* Legend dots — relative to top-right corner. */}
      <g className="trend-legend">
        <circle cx={PAD_X + 6} cy={PAD_TOP + 4} r={3} className="trend-dot trend-five" />
        <text x={PAD_X + 14} y={PAD_TOP + 8} className="trend-axis">{t("dashboard.trend.legend.5h")}</text>
        <circle cx={PAD_X + 50} cy={PAD_TOP + 4} r={3} className="trend-dot trend-seven" />
        <text x={PAD_X + 58} y={PAD_TOP + 8} className="trend-axis">{t("dashboard.trend.legend.7d")}</text>
        <circle cx={PAD_X + 94} cy={PAD_TOP + 4} r={3} className="trend-dot trend-sonnet" />
        <text x={PAD_X + 102} y={PAD_TOP + 8} className="trend-axis">{t("dashboard.trend.legend.7d_sonnet")}</text>
      </g>
    </svg>
  );
}

function CompactRow({
  a,
  nowMs,
  isActive,
}: {
  a: Account;
  nowMs: number;
  isActive: boolean;
}) {
  const peak = peakUtilization(a);
  let statusTone: Tone = "ok";
  let statusText = t("dashboard.compact.active");
  if (a.status !== "active") {
    statusTone = "muted";
    statusText = t("dashboard.compact.paused");
  } else if (a.token_expired) {
    statusTone = "danger";
    statusText = t("dashboard.compact.token_expired");
  } else if (a.cooldown_until > nowMs) {
    statusTone = "warn";
    statusText = tf("dashboard.compact.cooldown", {
      time: fmtRemaining(a.cooldown_until - nowMs),
    });
  } else if (a.expires_at - nowMs < 5 * 60 * 1000) {
    statusTone = "warn";
    statusText = t("dashboard.compact.refresh_due");
  }

  return (
    <div className={`compact-row ${isActive ? "active" : ""}`}>
      <span className="compact-avatar-wrap">
        <FoxAvatar id={a.id} size={32} />
        <span
          className={`row-status compact-status ${statusTone}`}
          aria-label={statusText}
        />
      </span>
      <div className="compact-main">
        <div className="compact-title">
          <span className="name">{a.name}</span>
          {a.plan && <span className="pill">{a.plan}</span>}
          {isActive && <span className="pill active-pill">{t("dashboard.hero.in_use")}</span>}
        </div>
        <div className="compact-sub text-meta">{statusText}</div>
      </div>
      <div className="compact-trailing">
        {peak !== null ? `${peak.toFixed(0)}%` : "—"}
      </div>
    </div>
  );
}
