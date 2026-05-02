import { Topbar } from "../components/Topbar";
import { FoxAvatar } from "../components/FoxAvatar";
import type { Account, UsageWindow } from "../api";
import type { Route } from "../components/Sidebar";

type Tone = "ok" | "warn" | "danger" | "muted";
type Policy = "lru" | "lowest" | "rr";

const POLICIES: Array<{ key: Policy; label: string; sub: string }> = [
  { key: "lru", label: "LRU", sub: "Least recently used" },
  { key: "lowest", label: "Lowest", sub: "Lowest utilization" },
  { key: "rr", label: "Round-robin", sub: "Cycle through pool" },
];

function fmtRemaining(ms: number): string {
  if (ms <= 0) return "expired";
  const s = Math.floor(ms / 1000);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

function greeting(now: Date): string {
  const h = now.getHours();
  if (h < 5) return "Up late";
  if (h < 12) return "Good morning";
  if (h < 18) return "Good afternoon";
  return "Good evening";
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
}: {
  accounts: Account[];
  managedAccountId: number;
  nowMs: number;
  onNavigate: (r: Route) => void;
  autoSwitch: { enabled: boolean; policy: Policy };
  onAutoSwitchChange: (v: { enabled: boolean; policy: Policy }) => void;
  onAutoSwitchToggle: () => void;
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
        title="Overview"
        status={
          active
            ? { label: `Managing ${active.name}`, tone: "ok" }
            : { label: "Idle", tone: "muted" }
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
              ? "Pool is empty — add an account to get started."
              : `Watching ${accounts.length} account${
                  accounts.length === 1 ? "" : "s"
                } in your pool.`}
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
                label="Pool size"
                value={
                  accounts.length === 0
                    ? "Empty"
                    : `${totals.healthy + totals.cooling}/${accounts.length}`
                }
                sub={
                  accounts.length === 0
                    ? "No accounts yet."
                    : [
                        `${totals.paused} paused`,
                        `${totals.cooling} in cooldown`,
                        ...(totals.expired > 0
                          ? [`${totals.expired} expired`]
                          : []),
                      ].join(" · ")
                }
                tone={accounts.length === 0 ? "muted" : "ok"}
              />
              <KpiCard
                label="Peak utilization"
                value={peakPct >= 0 ? `${peakPct.toFixed(0)}%` : "No data"}
                sub={
                  peakAcc ? `from ${peakAcc.name}` : "Waiting for first usage poll."
                }
                tone={peakPct >= 0 ? utilizationTone(peakPct) : "muted"}
              />
              <KpiCard
                label="Next cooldown ends"
                value={
                  nextCooldown
                    ? fmtRemaining(nextCooldown.cooldown_until - nowMs)
                    : "None"
                }
                sub={
                  nextCooldown
                    ? `${nextCooldown.name} thaws next`
                    : "No accounts in cooldown."
                }
                tone={nextCooldown ? "warn" : "muted"}
              />
            </div>

            <section className="dash-section">
              <div className="dash-section-header">
                <h3>Accounts</h3>
                <button
                  type="button"
                  className="btn btn-ghost"
                  onClick={() => onNavigate("accounts")}
                >
                  View all →
                </button>
              </div>
              {compact.length === 0 ? (
                <div className="page-empty">
                  <p>No accounts yet.</p>
                  <button
                    type="button"
                    className="btn btn-secondary"
                    onClick={() => onNavigate("accounts")}
                  >
                    Add your first account
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
                <h3>Recent activity</h3>
                <span className="text-meta">v0.2</span>
              </div>
              <div className="page-empty">
                <p>Activity timeline lights up once the daemon exposes</p>
                <code>GET /api/activity</code>
              </div>
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
        <div className="dash-hero-eyebrow">In use</div>
        <h2 className="dash-hero-title">{account.name}</h2>
        <div className="dash-hero-meta">
          {account.plan && <span className="pill">{account.plan}</span>}
          {account.email && (
            <span className="text-meta">{account.email}</span>
          )}
        </div>
        <div className="dash-hero-stat">
          Token expires in {fmtRemaining(account.expires_at - nowMs)}
        </div>
      </div>
      <button type="button" className="btn btn-secondary" onClick={onView}>
        Open
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
        <div className="dash-hero-eyebrow">In use</div>
        <h2 className="dash-hero-title">No account injected</h2>
        <div className="dash-hero-meta text-meta">
          Click Use now in Accounts to inject one.
        </div>
      </div>
      <button type="button" className="btn btn-primary" onClick={onAdd}>
        Add account
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
          <div className="auto-switch-eyebrow">Auto Switch</div>
          <h3>{value.enabled ? "On" : "Off"}</h3>
        </div>
        <button
          type="button"
          role="switch"
          aria-checked={value.enabled}
          className={`toggle ${value.enabled ? "on" : "off"}`}
          onClick={() => onChange({ ...value, enabled: !value.enabled })}
        >
          <span className="toggle-thumb" aria-hidden />
        </button>
      </header>
      <p className="text-meta auto-switch-desc">
        {value.enabled
          ? "Daemon picks an account when the current one cools or hits its threshold."
          : "Manual mode — selection only changes when you click Use now."}
      </p>
      <fieldset className="auto-switch-policy" disabled={!value.enabled}>
        <legend className="text-meta">Policy</legend>
        {POLICIES.map((p) => (
          <label key={p.key} className="auto-switch-radio">
            <input
              type="radio"
              name="auto-switch-policy"
              checked={value.policy === p.key}
              onChange={() => onChange({ ...value, policy: p.key })}
            />
            <span className="auto-switch-radio-text">
              <span className="auto-switch-radio-label">{p.label}</span>
              <span className="text-meta">{p.sub}</span>
            </span>
          </label>
        ))}
      </fieldset>
      <footer className="auto-switch-foot text-meta">
        UI live; persists once <code>POST /api/auto-switch</code> ships (v0.2).
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
  let statusText = "active";
  if (a.status !== "active") {
    statusTone = "muted";
    statusText = "paused";
  } else if (a.token_expired) {
    statusTone = "danger";
    statusText = "token expired";
  } else if (a.cooldown_until > nowMs) {
    statusTone = "warn";
    statusText = `cooldown ${fmtRemaining(a.cooldown_until - nowMs)}`;
  } else if (a.expires_at - nowMs < 5 * 60 * 1000) {
    statusTone = "warn";
    statusText = "refresh due";
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
          {isActive && <span className="pill active-pill">In use</span>}
        </div>
        <div className="compact-sub text-meta">{statusText}</div>
      </div>
      <div className="compact-trailing">
        {peak !== null ? `${peak.toFixed(0)}%` : "—"}
      </div>
    </div>
  );
}
