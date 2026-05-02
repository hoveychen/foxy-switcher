import { useEffect, useState } from "react";
import { Topbar } from "../components/Topbar";
import { DaemonMode, getDaemonMode, getServerPort } from "../api";

const APP_VERSION = "0.1.0";

type Group = {
  title: string;
  rows: Array<{ label: string; sub?: string; value: string }>;
};

const STUB_GROUPS: Group[] = [
  {
    title: "General",
    rows: [
      {
        label: "Launch at login",
        sub: "Start daemon when you log into your Mac.",
        value: "v0.2",
      },
      {
        label: "Start minimized in tray",
        sub: "Skip the main window on launch; live in the menu bar.",
        value: "v0.2",
      },
      {
        label: "Data directory",
        sub: "Where state.db, refresh tokens, and the log live.",
        value: "v0.2",
      },
    ],
  },
  {
    title: "Appearance",
    rows: [
      {
        label: "Theme",
        sub: "System / Light / Dark.",
        value: "Follows OS",
      },
      {
        label: "Sidebar",
        sub: "Always expanded vs auto-collapse below 1024px.",
        value: "Responsive",
      },
    ],
  },
  {
    title: "Behavior",
    rows: [
      {
        label: "Auto Switch policy",
        sub: "LRU / Lowest utilization / Round-robin.",
        value: "v0.2",
      },
      {
        label: "Pool cooldown threshold",
        sub: "Pool-wide default for utilization → cooldown.",
        value: "v0.2",
      },
      {
        label: "Refresh interval",
        sub: "How often the daemon polls Anthropic usage (30–300s).",
        value: "v0.2",
      },
      {
        label: "Restore native credentials on quit",
        sub: "Put your original ~/.claude/.credentials.json back.",
        value: "v0.2",
      },
    ],
  },
];

export function SettingsPage({
  autoSwitch,
  onAutoSwitchToggle,
}: {
  autoSwitch: { enabled: boolean; policy: "lru" | "lowest" | "rr" };
  onAutoSwitchToggle: () => void;
}) {
  const [daemonMode, setDaemonMode] = useState<DaemonMode | null>(null);
  const [daemonPort, setDaemonPort] = useState<number | null>(null);

  useEffect(() => {
    getDaemonMode().then(setDaemonMode).catch(() => {});
    getServerPort().then(setDaemonPort).catch(() => {});
  }, []);

  const daemonSub =
    daemonMode === "attached"
      ? "Sharing a daemon another process started (TUI embed or a manual run)."
      : daemonMode === "owned"
        ? "This window started the daemon and will SIGTERM it on quit."
        : "Live health is shown in the Sidebar footer.";
  const daemonPill =
    daemonMode === null
      ? "…"
      : daemonPort !== null
        ? `${daemonMode} · :${daemonPort}`
        : daemonMode;

  return (
    <>
      <Topbar
        title="Settings"
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
      />
      <div className="page">
        {STUB_GROUPS.map((g) => (
          <section key={g.title} className="settings-group">
            <h3 className="settings-group-title">{g.title}</h3>
            <div className="settings-card">
              {g.rows.map((r) => (
                <div key={r.label} className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">{r.label}</div>
                    {r.sub && (
                      <div className="settings-row-sub text-meta">{r.sub}</div>
                    )}
                  </div>
                  <span
                    className={`pill ${r.value === "v0.2" ? "" : "active-pill"}`}
                  >
                    {r.value}
                  </span>
                </div>
              ))}
            </div>
          </section>
        ))}

        <section className="settings-group">
          <h3 className="settings-group-title">About</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">Foxy Switcher</div>
                <div className="settings-row-sub text-meta">
                  Local Claude account pool · Tauri 2 + Go sidecar
                </div>
              </div>
              <span className="pill">v{APP_VERSION}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">Daemon</div>
                <div className="settings-row-sub text-meta">{daemonSub}</div>
              </div>
              <span
                className={`pill ${daemonMode === "attached" ? "active-pill" : ""}`}
              >
                {daemonPill}
              </span>
            </div>
          </div>
        </section>

        <p className="text-meta settings-footnote">
          Editable controls land in v0.2 alongside <code>POST /api/settings</code>{" "}
          (PRD §5.4 / §7).
        </p>
      </div>
    </>
  );
}
