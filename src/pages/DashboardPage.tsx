import { useEffect, useRef, useState } from "react";
import { Topbar } from "../components/Topbar";
import { FoxAvatar } from "../components/FoxAvatar";
import {
  accountHasOAuthToken,
  accountIsCooling,
  accountLeaseHolders,
  accountOutOfCredit,
  accountRefreshDue,
  accountResetAt,
  codexUsageLabel,
  scopedIsThrottled,
  apiClient,
  poolWindowTotals,
  type Account,
  type ActivityEvent,
  type DashboardTrendBucket,
  type UsageWindow,
} from "../api";
import type { Route } from "../components/Sidebar";
import { t, tf, scopedUsageLabel } from "../i18n";

type Tone = "ok" | "warn" | "danger" | "muted";
type Policy = "lru" | "lowest" | "rr";

// fmtX renders a Pro-equivalent quantity. Drops the decimal when the value
// is an integer; otherwise keeps one digit so half-Pro contributions from
// partial utilization aren't rounded out of view.
function fmtX(v: number): string {
  if (Math.abs(v - Math.round(v)) < 0.05) return `${Math.round(v)}`;
  return v.toFixed(1);
}

function fmtRemaining(ms: number): string {
  if (ms <= 0) return t("dashboard.fmt.expired");
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

// fmtResetsAt mirrors the drawer's helper: parse the window's RFC3339
// resets_at and render "resets in 2h 13m" / "rolling over" / em-dash.
function fmtResetsAt(rfc3339: string, nowMs: number): string {
  if (!rfc3339) return "—";
  const ts = Date.parse(rfc3339);
  if (Number.isNaN(ts)) return rfc3339;
  const diff = ts - nowMs;
  if (diff <= 0) return t("drawer.usage.rolling_over");
  return tf("drawer.usage.resets_in", { time: fmtRemaining(diff) });
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
  onAutoSwitchToggle,
  recentEvents,
  stale,
  vaultMode = false,
}: {
  accounts: Account[];
  managedAccountId: number;
  nowMs: number;
  onNavigate: (r: Route) => void;
  autoSwitch?: { enabled: boolean; policy: Policy };
  onAutoSwitchToggle?: () => void;
  recentEvents: ActivityEvent[];
  stale: boolean;
  // Vault mode → render the multi-device admin surface: drop the
  // agent-singular hero/topbar and surface every active lease instead.
  vaultMode?: boolean;
}) {
  const active = accounts.find((a) => a.id === managedAccountId) ?? accounts.find((a) => a.in_use) ?? null;
  const leasedAccounts = accounts.filter((a) => !!a.lease);
  const greet = greeting(new Date(nowMs));
  const firstName = active?.full_name?.split(" ")[0];

  const totals = accounts.reduce(
    (acc, a) => {
      if (a.status !== "active") acc.paused += 1;
      else if (a.token_expired) acc.expired += 1;
      // An out-of-credit OpenRouter account is not available: the vault's
      // picker skips it. Counting it as healthy overstated "available
      // accounts". Grouped with cooling — both are "active but not being
      // handed out right now", which is what the sub-line means.
      else if (accountOutOfCredit(a) || accountIsCooling(a)) acc.cooling += 1;
      else acc.healthy += 1;
      return acc;
    },
    { healthy: 0, cooling: 0, paused: 0, expired: 0 },
  );

  const fiveHourPool = poolWindowTotals(accounts, "five_hour");
  const sevenDayPool = poolWindowTotals(accounts, "seven_day");

  // Pair each cooling account with its soonest throttled-window reset, then
  // pick the row with the earliest reset for the "Next reset" KPI. Accounts
  // whose throttled windows have no parseable resets_at fall to last so they
  // don't crowd out a real countdown.
  const coolingWithReset = accounts
    .filter((a) => a.status === "active" && accountIsCooling(a))
    .map((a) => ({ a, reset: accountResetAt(a, new Date(nowMs)) }))
    .sort((x, y) => {
      if (x.reset === 0 && y.reset === 0) return 0;
      if (x.reset === 0) return 1;
      if (y.reset === 0) return -1;
      return x.reset - y.reset;
    });
  const nextReset = coolingWithReset[0] ?? null;

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

  const topbarStatus = vaultMode
    ? {
        label: tf("dashboard.status.summary", {
          total: accounts.length,
          inUse: leasedAccounts.length,
        }),
        tone: "muted" as const,
      }
    : active
      ? {
          label: tf("dashboard.status.managing", { name: active.name }),
          tone: "ok" as const,
        }
      : { label: t("dashboard.status.idle"), tone: "muted" as const };

  return (
    <>
      <Topbar
        title={t("dashboard.title")}
        status={topbarStatus}
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

        <div className="dash-stack">
          {vaultMode ? (
            <VaultInUseCard accounts={leasedAccounts} />
          ) : active ? (
            <HeroCard
              account={active}
              nowMs={nowMs}
              onView={() => onNavigate("accounts")}
            />
          ) : (
            <HeroEmpty onAdd={() => onNavigate("accounts")} />
          )}

          {!vaultMode && <OthersInUse accounts={accounts} />}


          <div className="kpi-grid">
            <KpiCard
              label={t("dashboard.kpi.available")}
              value={
                accounts.length === 0
                  ? t("dashboard.kpi.pool_empty")
                  : `${totals.healthy}`
              }
              sub={
                accounts.length === 0
                  ? t("dashboard.kpi.no_accounts_yet")
                  : tf("dashboard.kpi.available_sub", {
                      total: accounts.length,
                      cooling: totals.cooling,
                      paused: totals.paused,
                    })
              }
              tone={
                accounts.length === 0
                  ? "muted"
                  : totals.healthy === 0
                    ? "warn"
                    : "ok"
              }
            />
            <KpiCard
              label={t("dashboard.kpi.five_hour_pool")}
              value={
                fiveHourPool.capacity > 0
                  ? `${fmtX(fiveHourPool.used)} / ${fmtX(fiveHourPool.capacity)}x`
                  : t("dashboard.kpi.no_data")
              }
              sub={
                fiveHourPool.capacity > 0
                  ? tf("dashboard.kpi.window_pct", {
                      pct: fiveHourPool.percent.toFixed(0),
                    })
                  : t("dashboard.kpi.no_usage")
              }
              tone={
                fiveHourPool.capacity > 0
                  ? utilizationTone(fiveHourPool.percent)
                  : "muted"
              }
            />
            <KpiCard
              label={t("dashboard.kpi.seven_day_pool")}
              value={
                sevenDayPool.capacity > 0
                  ? `${fmtX(sevenDayPool.used)} / ${fmtX(sevenDayPool.capacity)}x`
                  : t("dashboard.kpi.no_data")
              }
              sub={
                sevenDayPool.capacity > 0
                  ? tf("dashboard.kpi.window_pct", {
                      pct: sevenDayPool.percent.toFixed(0),
                    })
                  : t("dashboard.kpi.no_usage")
              }
              tone={
                sevenDayPool.capacity > 0
                  ? utilizationTone(sevenDayPool.percent)
                  : "muted"
              }
            />
            <KpiCard
              label={t("dashboard.kpi.next_reset")}
              value={
                nextReset && nextReset.reset > 0
                  ? fmtRemaining(nextReset.reset - nowMs)
                  : t("dashboard.kpi.no_reset")
              }
              sub={
                nextReset
                  ? tf("dashboard.kpi.resets_next", { name: nextReset.a.name })
                  : t("dashboard.kpi.no_cooling")
              }
              tone={nextReset ? "warn" : "muted"}
            />
          </div>

          <div className="dash-row-split">
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
          </div>

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
                    isActive={a.in_use || a.id === managedAccountId}
                    vaultMode={vaultMode}
                  />
                ))}
              </div>
            )}
          </section>
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
      <FoxAvatar name={account.name} size={64} className="dash-hero-fox" />
      <div className="dash-hero-body">
        <div className="dash-hero-eyebrow">{t("dashboard.hero.in_use")}</div>
        <h2 className="dash-hero-title">{account.name}</h2>
        <div className="dash-hero-meta">
          {account.plan && <span className="pill">{account.plan}</span>}
          {account.email && (
            <span className="text-meta">{account.email}</span>
          )}
        </div>
        {/* Only meaningful for OAuth accounts; an API-key account (OpenRouter)
            has no token lifetime and would read "expires in 已过期". */}
        {accountHasOAuthToken(account) && (
          <div className="dash-hero-stat">
            {tf("dashboard.hero.token_expires", {
              time: fmtRemaining(account.expires_at - nowMs),
            })}
          </div>
        )}
        <div className="usage-list">
          <HeroUsageBar
            label={t(account.provider === "codex" ? codexUsageLabel(account.five_hour, "primary") : "drawer.usage.5h")}
            win={account.five_hour}
            nowMs={nowMs}
          />
          <HeroUsageBar
            label={t(account.provider === "codex" ? codexUsageLabel(account.seven_day, "secondary") : "drawer.usage.7d_opus")}
            win={account.seven_day}
            nowMs={nowMs}
          />
          {account.provider !== "codex" && (
            <HeroUsageBar
              label={scopedUsageLabel(account.seven_day_scoped_label, scopedIsThrottled(account))}
              win={account.seven_day_sonnet}
              nowMs={nowMs}
            />
          )}
        </div>
      </div>
      <button type="button" className="btn btn-secondary" onClick={onView}>
        {t("dashboard.hero.open")}
      </button>
    </section>
  );
}

// HeroUsageBar is a read-only twin of AccountDrawer's UsageBar — same visual
// (track + fill + pct + resets_at) but no threshold marker / drag affordance,
// since the dashboard hero is for at-a-glance status, not editing.
function HeroUsageBar({
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
        <span className="usage-empty">{t("drawer.usage.no_data")}</span>
      </div>
    );
  }
  const pct = Math.max(0, Math.min(100, win.utilization));
  const tone = utilizationTone(pct);
  return (
    <div className="usage-row">
      <span className="usage-label">{label}</span>
      <div className="usage-track-wrap">
        <div className={`usage-track ${tone}`}>
          <div className="usage-fill" style={{ width: `${pct}%` }} />
        </div>
      </div>
      <span className="usage-pct">{pct.toFixed(0)}%</span>
      <span className="usage-resets">{fmtResetsAt(win.resets_at, nowMs)}</span>
    </div>
  );
}

// VaultInUseCard is the dashboard hero for the vault admin web view.
// The agent-singular HeroCard ("Managing X") doesn't fit a multi-device
// admin context — there is no caller-side "current" account — so vault
// mode swaps it for a list of every live lease (account · device).
// Empty state nudges the admin that nobody is using the pool right now,
// instead of the misleading "Idle" chip on the agent path.
function VaultInUseCard({ accounts }: { accounts: Account[] }) {
  if (accounts.length === 0) {
    return (
      <section className="dash-others-in-use">
        <div className="dash-hero-eyebrow">
          {t("dashboard.vault.in_use_title")}
        </div>
        <p className="text-meta">{t("dashboard.vault.in_use_empty")}</p>
      </section>
    );
  }
  return (
    <section className="dash-others-in-use">
      <div className="dash-hero-eyebrow">{t("dashboard.vault.in_use_title")}</div>
      <div className="dash-others-list">
        {accounts.flatMap((a) =>
          // One pill per HOLDER, not per account: a shared Codex account can
          // be held by several devices at once and this card's whole job is
          // showing who is using what.
          accountLeaseHolders(a).map((lease) => (
            <div
              key={`${a.id}:${lease.device_id}`}
              className="pill leased-pill"
              title={a.email || ""}
            >
              <strong>{a.name}</strong>
              <span> · </span>
              <span>{lease.device_name || lease.device_id || "—"}</span>
            </div>
          )),
        )}
      </div>
    </section>
  );
}

// OthersInUse renders a compact strip of accounts currently held by
// OTHER devices' leases (a.lease.mine === false). Hidden when no
// foreign leases exist; the existing HeroCard already covers the
// caller's own injected account. Drives multi-device awareness without
// duplicating the hero layout for each device.
function OthersInUse({ accounts }: { accounts: Account[] }) {
  // Flattened to (account, foreign holder) pairs: a shared Codex account this
  // device co-holds still has other devices on it, and each of them belongs on
  // the strip. The caller's own hold is filtered out — HeroCard covers that.
  const others = accounts.flatMap((a) =>
    accountLeaseHolders(a)
      .filter((lease) => !lease.mine)
      .map((lease) => ({ account: a, lease })),
  );
  if (others.length === 0) return null;
  return (
    <section className="dash-others-in-use">
      <div className="dash-hero-eyebrow">{t("dashboard.others_in_use")}</div>
      <div className="dash-others-list">
        {others.map(({ account: a, lease }) => (
          <div
            key={`${a.id}:${lease.device_id}`}
            className="pill leased-pill"
            title={a.email || ""}
          >
            <strong>{a.name}</strong>
            <span> · </span>
            <span>{lease.device_name || lease.device_id || "—"}</span>
          </div>
        ))}
      </div>
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

// UsageTrendChart plots the pool's weighted utilization over 24 hours: one
// line for 5h and one for 7d, where each bucket's value is the
// Pro-equivalent used divided by Pro-equivalent capacity. Mirrors the 5h/7d
// KPI cards above so the eye can connect the current snapshot to its
// history. Inline SVG keeps the daemon dependency-light.
function UsageTrendChart({ trend }: { trend: DashboardTrendBucket[] }) {
  const W = 720;
  const H = 200;
  const PAD_LEFT = 32;
  const PAD_RIGHT = 12;
  const PAD_TOP = 36;
  const PAD_BOTTOM = 22;

  const [hoverIdx, setHoverIdx] = useState<number | null>(null);
  const svgRef = useRef<SVGSVGElement | null>(null);

  const hasData = trend.some(
    (b) => (b.five_hour_pct ?? 0) > 0 || (b.seven_day_pct ?? 0) > 0,
  );

  if (trend.length === 0 || !hasData) {
    return (
      <div className="trend-empty text-meta">
        {t("dashboard.trend.empty")}
      </div>
    );
  }

  const n = trend.length;
  const innerW = W - PAD_LEFT - PAD_RIGHT;
  const innerH = H - PAD_TOP - PAD_BOTTOM;
  const x = (i: number) =>
    PAD_LEFT + (n === 1 ? innerW / 2 : (i * innerW) / (n - 1));
  // Pool pct is 0–100; clamp so spurious >100 (e.g. capacity briefly 0)
  // doesn't blow past the chart area.
  const y = (v: number) =>
    PAD_TOP + innerH - (Math.max(0, Math.min(100, v)) / 100) * innerH;

  const path = (sel: (b: DashboardTrendBucket) => number) =>
    trend
      .map((b, i) => `${i === 0 ? "M" : "L"}${x(i).toFixed(1)},${y(sel(b)).toFixed(1)}`)
      .join(" ");

  // fmtPct keeps small values readable (one decimal under 10) so 0.4% doesn't
  // collapse to "0%" at a glance; large values stay integer for tidy chips.
  const fmtPct = (v: number) => {
    const c = Math.max(0, Math.min(100, v));
    return c < 10 && c > 0 ? c.toFixed(1) : `${Math.round(c)}`;
  };

  const fmtHour = (ts: number) => {
    const d = new Date(ts);
    return `${d.getHours().toString().padStart(2, "0")}:00`;
  };

  // Hour labels at 0, 25%, 50%, 75%, 100% positions across the window.
  const labelIdx = [0, Math.floor(n / 4), Math.floor(n / 2), Math.floor((3 * n) / 4), n - 1];

  const last = trend[n - 1];
  const cur5 = last.five_hour_pct ?? 0;
  const cur7 = last.seven_day_pct ?? 0;

  // mousemove → nearest bucket. We project the cursor into SVG-user space via
  // getScreenCTM so the math stays correct under any container width / DPR.
  const onMove = (e: React.MouseEvent<SVGSVGElement>) => {
    const svg = svgRef.current;
    if (!svg) return;
    const ctm = svg.getScreenCTM();
    if (!ctm) return;
    const pt = svg.createSVGPoint();
    pt.x = e.clientX;
    pt.y = e.clientY;
    const local = pt.matrixTransform(ctm.inverse());
    if (local.x < PAD_LEFT - 4 || local.x > W - PAD_RIGHT + 4) {
      setHoverIdx(null);
      return;
    }
    const ratio = (local.x - PAD_LEFT) / innerW;
    let idx = Math.round(ratio * (n - 1));
    if (idx < 0) idx = 0;
    if (idx > n - 1) idx = n - 1;
    setHoverIdx(idx);
  };

  // Tooltip layout: prefer right of the cursor, flip to the left when it would
  // clip the chart's right edge. Fixed dimensions keep the rect/text aligned
  // without needing post-render measurement.
  const TT_W = 150;
  const TT_H = 64;
  let tooltip: JSX.Element | null = null;
  if (hoverIdx !== null) {
    const b = trend[hoverIdx];
    const v5 = b.five_hour_pct ?? 0;
    const v7 = b.seven_day_pct ?? 0;
    const cx = x(hoverIdx);
    let tx = cx + 10;
    if (tx + TT_W > W - PAD_RIGHT) tx = cx - 10 - TT_W;
    if (tx < PAD_LEFT) tx = PAD_LEFT;
    const ty = PAD_TOP - 4;
    tooltip = (
      <g className="trend-cursor-group">
        <line
          x1={cx}
          x2={cx}
          y1={PAD_TOP}
          y2={H - PAD_BOTTOM}
          className="trend-cursor"
        />
        <circle cx={cx} cy={y(v5)} r={4} className="trend-dot trend-five" />
        <circle cx={cx} cy={y(v7)} r={4} className="trend-dot trend-seven" />
        <rect
          x={tx}
          y={ty}
          width={TT_W}
          height={TT_H}
          rx={6}
          className="trend-tooltip"
        />
        <text x={tx + 10} y={ty + 18} className="trend-tooltip-time">
          {fmtHour(b.ts)}
        </text>
        <text x={tx + 10} y={ty + 38} className="trend-tooltip-row trend-tooltip-five">
          {tf("dashboard.trend.tooltip.row", {
            label: t("dashboard.trend.legend.5h_pool"),
            pct: fmtPct(v5),
          })}
        </text>
        <text x={tx + 10} y={ty + 56} className="trend-tooltip-row trend-tooltip-seven">
          {tf("dashboard.trend.tooltip.row", {
            label: t("dashboard.trend.legend.7d_pool"),
            pct: fmtPct(v7),
          })}
        </text>
      </g>
    );
  }

  return (
    <svg
      ref={svgRef}
      className="trend-chart"
      viewBox={`0 0 ${W} ${H}`}
      role="img"
      aria-label={t("dashboard.trend.aria")}
      onMouseMove={onMove}
      onMouseLeave={() => setHoverIdx(null)}
    >
      {/* Y-axis labels — anchor to the left padding so 0/50/90/100 line up
          with the gridlines they describe. 90 is colored to match the warn
          band so the user knows which line is the threshold. */}
      <text x={PAD_LEFT - 6} y={y(100) + 3} textAnchor="end" className="trend-axis trend-axis-y">100</text>
      <text x={PAD_LEFT - 6} y={y(90) + 3} textAnchor="end" className="trend-axis trend-axis-y trend-axis-warn">90</text>
      <text x={PAD_LEFT - 6} y={y(50) + 3} textAnchor="end" className="trend-axis trend-axis-y">50</text>
      <text x={PAD_LEFT - 6} y={y(0) + 3} textAnchor="end" className="trend-axis trend-axis-y">0</text>

      {/* Reference grid at 50% and 90% (the threshold band). */}
      <line x1={PAD_LEFT} x2={W - PAD_RIGHT} y1={y(0)} y2={y(0)} className="trend-grid trend-grid-baseline" />
      <line x1={PAD_LEFT} x2={W - PAD_RIGHT} y1={y(50)} y2={y(50)} className="trend-grid" />
      <line x1={PAD_LEFT} x2={W - PAD_RIGHT} y1={y(90)} y2={y(90)} className="trend-grid trend-grid-warn" />

      <path
        className="trend-line trend-five"
        d={path((b) => b.five_hour_pct ?? 0)}
      />
      <path
        className="trend-line trend-seven"
        d={path((b) => b.seven_day_pct ?? 0)}
      />

      {/* End-of-series dots — anchor the eye to "what's the current value". */}
      <circle cx={x(n - 1)} cy={y(cur5)} r={3.5} className="trend-dot trend-five" />
      <circle cx={x(n - 1)} cy={y(cur7)} r={3.5} className="trend-dot trend-seven" />

      {labelIdx.map((i) => (
        <text
          key={i}
          x={x(i)}
          y={H - 6}
          textAnchor={i === 0 ? "start" : i === n - 1 ? "end" : "middle"}
          className="trend-axis"
        >
          {fmtHour(trend[i].ts)}
        </text>
      ))}

      {/* Legend now carries the live "current value" — answers "我池子现在用了多少"
          without needing the user to hover. */}
      <g className="trend-legend">
        <circle cx={PAD_LEFT + 4} cy={PAD_TOP - 16} r={3} className="trend-dot trend-five" />
        <text x={PAD_LEFT + 12} y={PAD_TOP - 12} className="trend-axis trend-legend-text">
          {tf("dashboard.trend.current.row", {
            label: t("dashboard.trend.legend.5h_pool"),
            pct: fmtPct(cur5),
          })}
        </text>
        <circle cx={PAD_LEFT + 124} cy={PAD_TOP - 16} r={3} className="trend-dot trend-seven" />
        <text x={PAD_LEFT + 132} y={PAD_TOP - 12} className="trend-axis trend-legend-text">
          {tf("dashboard.trend.current.row", {
            label: t("dashboard.trend.legend.7d_pool"),
            pct: fmtPct(cur7),
          })}
        </text>
      </g>

      {tooltip}
    </svg>
  );
}

function CompactRow({
  a,
  nowMs,
  isActive,
  vaultMode = false,
}: {
  a: Account;
  nowMs: number;
  isActive: boolean;
  // Vault mode → derive "in use" from a.lease and surface the device
  // name; the agent-singular `isActive` (managedAccountId match) is a
  // no-op here since the vault admin isn't a lease holder.
  vaultMode?: boolean;
}) {
  const vaultLease = vaultMode ? a.lease ?? null : null;
  const showActiveBadge = vaultMode ? !!vaultLease : isActive;
  const rowActive = vaultMode ? !!vaultLease : isActive;
  const peak = peakUtilization(a);
  let statusTone: Tone = "ok";
  let statusText = t("dashboard.compact.active");
  if (a.status !== "active") {
    statusTone = "muted";
    statusText = t("dashboard.compact.paused");
  } else if (a.token_expired) {
    statusTone = "danger";
    statusText = t("dashboard.compact.token_expired");
  } else if (accountOutOfCredit(a)) {
    // Ahead of cooling: an empty account is unusable outright, not throttled.
    statusTone = "danger";
    statusText = t("dashboard.compact.out_of_credit");
  } else if (accountIsCooling(a)) {
    statusTone = "warn";
    const reset = accountResetAt(a, new Date(nowMs));
    statusText = reset > 0
      ? tf("dashboard.compact.cooling", { time: fmtRemaining(reset - nowMs) })
      : t("dashboard.compact.cooling_no_reset");
  } else if (accountRefreshDue(a, nowMs)) {
    statusTone = "warn";
    statusText = t("dashboard.compact.refresh_due");
  }

  return (
    <div className={`compact-row ${rowActive ? "active" : ""}`}>
      <span className="compact-avatar-wrap">
        <FoxAvatar name={a.name} size={32} />
        <span
          className={`row-status compact-status ${statusTone}`}
          aria-label={statusText}
        />
      </span>
      <div className="compact-main">
        <div className="compact-title">
          <span className="name">{a.name}</span>
          {a.plan && <span className="pill">{a.plan}</span>}
          {showActiveBadge && (
            vaultLease ? (
              <span className="pill leased-pill">
                {tf("accounts.badge.in_use_by", {
                  device:
                    vaultLease.device_name || vaultLease.device_id || "—",
                })}
              </span>
            ) : (
              <span className="pill active-pill">
                {t("dashboard.hero.in_use")}
              </span>
            )
          )}
        </div>
        <div className="compact-sub text-meta">{statusText}</div>
      </div>
      <div className="compact-trailing">
        {peak !== null ? `${peak.toFixed(0)}%` : "—"}
      </div>
    </div>
  );
}
