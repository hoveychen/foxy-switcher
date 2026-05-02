import { useEffect, useState } from "react";
import { Topbar } from "../components/Topbar";
import {
  AboutResponse,
  DaemonMode,
  Settings,
  Theme,
  apiClient,
  autostartIsEnabled,
  autostartSet,
  dataDirPath,
  getDaemonMode,
  getServerPort,
  revealDataDir,
} from "../api";
import { Locale, getLocaleOverride, setLocaleOverride, t, tf } from "../i18n";

type Policy = "lru" | "lowest" | "rr";

const THEMES: Array<{ key: Theme; labelKey: string; subKey: string }> = [
  { key: "system", labelKey: "settings.themes.system.label", subKey: "settings.themes.system.sub" },
  { key: "light", labelKey: "settings.themes.light.label", subKey: "settings.themes.light.sub" },
  { key: "dark", labelKey: "settings.themes.dark.label", subKey: "settings.themes.dark.sub" },
];

const LOCALES: Array<{ key: Locale; labelKey: string }> = [
  { key: "auto", labelKey: "settings.language.auto" },
  { key: "en", labelKey: "settings.language.en" },
  { key: "zh", labelKey: "settings.language.zh" },
];

const POLICIES: Array<{ key: Policy; labelKey: string; subKey: string }> = [
  { key: "lru", labelKey: "dashboard.policies.lru.label", subKey: "dashboard.policies.lru.sub" },
  { key: "lowest", labelKey: "dashboard.policies.lowest.label", subKey: "dashboard.policies.lowest.sub" },
  { key: "rr", labelKey: "dashboard.policies.rr.label", subKey: "dashboard.policies.rr.sub" },
];

function aboutVersionPill(a: AboutResponse | null): string {
  if (!a) return "v0.1.0";
  // Module versions ship as "(devel)" until a tag is cut; fall back to a
  // shorter SHA so the pill always renders something meaningful.
  return a.version === "(devel)" || a.version === ""
    ? a.commit
      ? a.commit
      : "(devel)"
    : a.version;
}

function formatUptime(secs: number): string {
  if (!Number.isFinite(secs) || secs < 0) return "—";
  if (secs < 60) return `${secs}s`;
  const m = Math.floor(secs / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

function formatDate(rfc: string): string {
  const d = new Date(rfc);
  if (Number.isNaN(d.getTime())) return rfc;
  return d.toISOString().slice(0, 10);
}

export function SettingsPage({
  autoSwitch,
  onAutoSwitchToggle,
  onAutoSwitchChange,
  settings,
  onSettingsChange,
}: {
  autoSwitch: { enabled: boolean; policy: Policy };
  onAutoSwitchToggle: () => void;
  onAutoSwitchChange: (v: { enabled: boolean; policy: Policy }) => void;
  settings: Settings;
  onSettingsChange: (patch: Partial<Settings>) => void;
}) {
  const [daemonMode, setDaemonMode] = useState<DaemonMode | null>(null);
  const [daemonPort, setDaemonPort] = useState<number | null>(null);
  const [autostartEnabled, setAutostartEnabled] = useState<boolean | null>(null);
  const [dataDir, setDataDir] = useState<string | null>(null);
  const [autostartBusy, setAutostartBusy] = useState(false);
  const [revealBusy, setRevealBusy] = useState(false);
  const [about, setAbout] = useState<AboutResponse | null>(null);
  const [confirmReset, setConfirmReset] = useState(false);
  const [resetBusy, setResetBusy] = useState(false);
  const [resetError, setResetError] = useState<string | null>(null);
  const [pairCmdCopied, setPairCmdCopied] = useState(false);
  const localeOverride = getLocaleOverride();

  const vaultMode = (about?.mode || "combined").toLowerCase();
  const vaultModeLabelKey =
    vaultMode === "agent"
      ? "settings.vault.mode.agent"
      : vaultMode === "vault"
        ? "settings.vault.mode.vault"
        : vaultMode === "combined"
          ? "settings.vault.mode.combined"
          : "settings.vault.mode.unknown";

  const onCopyPairCmd = async () => {
    try {
      const cmd = t("settings.vault.pair.command").replace(
        "<URL>",
        about?.vault_url || "https://vault.example.com",
      );
      await navigator.clipboard.writeText(cmd);
      setPairCmdCopied(true);
      setTimeout(() => setPairCmdCopied(false), 2000);
    } catch {
      // Clipboard may be unavailable in some webviews; the command is
      // visible inline anyway, so silent failure is the kindest UX.
    }
  };

  useEffect(() => {
    getDaemonMode().then(setDaemonMode).catch(() => {});
    getServerPort().then(setDaemonPort).catch(() => {});
    autostartIsEnabled().then(setAutostartEnabled).catch(() => {});
    dataDirPath().then(setDataDir).catch(() => {});
    apiClient.getAbout().then(setAbout).catch(() => {});
  }, []);

  const onResetData = async () => {
    if (resetBusy) return;
    setResetBusy(true);
    setResetError(null);
    try {
      await apiClient.resetData();
      // The daemon process exits inside the handler. The Tauri shell
      // respawns the sidecar; bouncing the window is the simplest way to
      // re-pick the new port + re-hydrate every page from the empty store.
      setTimeout(() => window.location.reload(), 800);
    } catch (e) {
      setResetError(e instanceof Error ? e.message : String(e));
      setResetBusy(false);
      setConfirmReset(false);
    }
  };

  const toggleAutostart = async () => {
    if (autostartBusy || autostartEnabled === null) return;
    setAutostartBusy(true);
    const next = !autostartEnabled;
    try {
      await autostartSet(next);
      setAutostartEnabled(next);
    } catch {
      // best-effort: re-read truth from the OS
      autostartIsEnabled().then(setAutostartEnabled).catch(() => {});
    } finally {
      setAutostartBusy(false);
    }
  };

  const onReveal = async () => {
    if (revealBusy) return;
    setRevealBusy(true);
    try {
      await revealDataDir();
    } finally {
      setRevealBusy(false);
    }
  };

  const daemonSub =
    daemonMode === "attached"
      ? t("settings.about.daemon.attached")
      : daemonMode === "owned"
        ? t("settings.about.daemon.owned")
        : t("settings.about.daemon.unknown");
  const daemonPill =
    daemonMode === null
      ? "…"
      : daemonPort !== null
        ? `${daemonMode} · :${daemonPort}`
        : daemonMode;

  return (
    <>
      <Topbar
        title={t("settings.title")}
        autoSwitch={autoSwitch}
        onAutoSwitchToggle={onAutoSwitchToggle}
      />
      <div className="page">
        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.general")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.launch_at_login.label")}</div>
                <div className="settings-row-sub text-meta">
                  {t("settings.launch_at_login.sub")}
                </div>
              </div>
              <button
                type="button"
                role="switch"
                aria-checked={!!autostartEnabled}
                aria-label={t("settings.launch_at_login.aria")}
                disabled={autostartEnabled === null || autostartBusy}
                className={`toggle ${autostartEnabled ? "on" : "off"}`}
                onClick={toggleAutostart}
              >
                <span className="toggle-thumb" aria-hidden />
              </button>
            </div>

            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.data_dir.label")}</div>
                <div className="settings-row-sub text-meta">
                  {dataDir ?? t("settings.data_dir.resolving")}
                </div>
              </div>
              <button
                type="button"
                className="btn"
                disabled={!dataDir || revealBusy}
                onClick={onReveal}
              >
                {t("settings.data_dir.reveal")}
              </button>
            </div>

            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.language.label")}</div>
                <div className="settings-row-sub text-meta">
                  {t("settings.language.sub")}
                </div>
              </div>
              <div
                className="seg-control"
                role="radiogroup"
                aria-label={t("settings.language.aria")}
              >
                {LOCALES.map((loc) => {
                  const active = localeOverride === loc.key;
                  return (
                    <button
                      key={loc.key}
                      type="button"
                      role="radio"
                      aria-checked={active}
                      className={`seg-control-btn ${active ? "active" : ""}`}
                      onClick={() => {
                        if (active) return;
                        setLocaleOverride(loc.key);
                      }}
                    >
                      {t(loc.labelKey)}
                    </button>
                  );
                })}
              </div>
            </div>
          </div>
        </section>

        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.auto_switch")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">
                  {t("dashboard.auto_switch.eyebrow")}
                </div>
                <div className="settings-row-sub text-meta">
                  {autoSwitch.enabled
                    ? t("dashboard.auto_switch.desc.on")
                    : t("dashboard.auto_switch.desc.off")}
                </div>
              </div>
              <button
                type="button"
                role="switch"
                aria-checked={autoSwitch.enabled}
                aria-label={t("dashboard.auto_switch.aria")}
                className={`toggle ${autoSwitch.enabled ? "on" : "off"}`}
                onClick={onAutoSwitchToggle}
              >
                <span className="toggle-thumb" aria-hidden />
              </button>
            </div>

            <div className="settings-row settings-row-stack">
              <div className="settings-row-text">
                <div className="settings-row-label">
                  {t("dashboard.auto_switch.policy_legend")}
                </div>
                <div className="settings-row-sub text-meta">
                  {t("settings.auto_switch.policy_sub")}
                </div>
              </div>
              <fieldset
                className="auto-switch-policy"
                disabled={!autoSwitch.enabled}
              >
                <legend className="visually-hidden">
                  {t("dashboard.auto_switch.policy_legend")}
                </legend>
                {POLICIES.map((p) => (
                  <label key={p.key} className="auto-switch-radio">
                    <input
                      type="radio"
                      name="auto-switch-policy"
                      checked={autoSwitch.policy === p.key}
                      onChange={() =>
                        onAutoSwitchChange({ ...autoSwitch, policy: p.key })
                      }
                    />
                    <span className="auto-switch-radio-text">
                      <span className="auto-switch-radio-label">
                        {t(p.labelKey)}
                      </span>
                      <span className="text-meta">{t(p.subKey)}</span>
                    </span>
                  </label>
                ))}
              </fieldset>
            </div>
          </div>
        </section>

        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.appearance")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.theme.label")}</div>
                <div className="settings-row-sub text-meta">
                  {t("settings.theme.sub")}
                </div>
              </div>
              <div className="seg-control" role="radiogroup" aria-label={t("settings.theme.aria")}>
                {THEMES.map((th) => (
                  <button
                    key={th.key}
                    type="button"
                    role="radio"
                    aria-checked={settings.theme === th.key}
                    title={t(th.subKey)}
                    className={`seg-control-btn ${
                      settings.theme === th.key ? "active" : ""
                    }`}
                    onClick={() => onSettingsChange({ theme: th.key })}
                  >
                    {t(th.labelKey)}
                  </button>
                ))}
              </div>
            </div>
          </div>
        </section>

        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.behavior")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">
                  {t("settings.restore.label")}
                </div>
                <div className="settings-row-sub text-meta">
                  {t("settings.restore.sub")}
                </div>
              </div>
              <button
                type="button"
                role="switch"
                aria-checked={settings.restore_native_on_quit}
                aria-label={t("settings.restore.aria")}
                className={`toggle ${
                  settings.restore_native_on_quit ? "on" : "off"
                }`}
                onClick={() =>
                  onSettingsChange({
                    restore_native_on_quit: !settings.restore_native_on_quit,
                  })
                }
              >
                <span className="toggle-thumb" aria-hidden />
              </button>
            </div>

            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.poll.label")}</div>
                <div className="settings-row-sub text-meta">
                  {t("settings.poll.sub")}
                </div>
              </div>
              <NumberStepper
                value={settings.usage_poll_interval_sec}
                min={30}
                max={300}
                step={15}
                suffix="s"
                onChange={(v) => onSettingsChange({ usage_poll_interval_sec: v })}
              />
            </div>

            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">
                  {t("settings.threshold_default.label")}
                </div>
                <div className="settings-row-sub text-meta">
                  {t("settings.threshold_default.sub")}
                </div>
              </div>
              <NumberStepper
                value={settings.default_threshold_percent}
                min={50}
                max={100}
                step={5}
                suffix="%"
                onChange={(v) =>
                  onSettingsChange({ default_threshold_percent: v })
                }
              />
            </div>
          </div>
        </section>

        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.vault")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">
                  {t("settings.vault.mode.label")}
                </div>
                <div className="settings-row-sub text-meta">
                  {t(vaultModeLabelKey)}
                </div>
              </div>
              <span className="pill">{vaultMode}</span>
            </div>
            {vaultMode === "agent" && (
              <>
                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">
                      {t("settings.vault.upstream.label")}
                    </div>
                    <div className="settings-row-sub text-meta">
                      {about?.vault_url || ""}
                    </div>
                  </div>
                </div>
                {about?.device_id && (
                  <div className="settings-row">
                    <div className="settings-row-text">
                      <div className="settings-row-label">
                        {t("settings.vault.device.label")}
                      </div>
                      <div className="settings-row-sub text-meta">
                        {about.device_id}
                      </div>
                    </div>
                  </div>
                )}
              </>
            )}
            {vaultMode === "combined" && (
              <div className="settings-row">
                <div className="settings-row-text">
                  <div className="settings-row-label">
                    {t("settings.vault.local.label")}
                  </div>
                  <div className="settings-row-sub text-meta">
                    {t("settings.vault.local.sub")}
                  </div>
                </div>
              </div>
            )}
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">
                  {t("settings.vault.pair.label")}
                </div>
                <div className="settings-row-sub text-meta">
                  {t("settings.vault.pair.sub")}
                </div>
                <code
                  style={{
                    display: "block",
                    marginTop: 6,
                    padding: "6px 8px",
                    background: "rgba(128,128,128,.15)",
                    borderRadius: 4,
                    fontFamily:
                      "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
                    fontSize: "0.85em",
                    overflowX: "auto",
                    whiteSpace: "nowrap",
                  }}
                >
                  {t("settings.vault.pair.command").replace(
                    "<URL>",
                    about?.vault_url || "https://vault.example.com",
                  )}
                </code>
              </div>
              <button type="button" className="btn" onClick={onCopyPairCmd}>
                {pairCmdCopied
                  ? t("settings.vault.pair.copied")
                  : t("settings.vault.pair.copy")}
              </button>
            </div>
          </div>
        </section>

        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.about")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.about.app_name")}</div>
                <div className="settings-row-sub text-meta">
                  {t("settings.about.app_sub")}
                </div>
              </div>
              <span className="pill">{aboutVersionPill(about)}</span>
            </div>
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.about.daemon")}</div>
                <div className="settings-row-sub text-meta">{daemonSub}</div>
              </div>
              <span
                className={`pill ${daemonMode === "attached" ? "active-pill" : ""}`}
              >
                {daemonPill}
              </span>
            </div>
            {about && (
              <>
                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">{t("settings.about.build")}</div>
                    <div className="settings-row-sub text-meta">
                      {about.commit
                        ? `${about.commit}${about.commit_dirty ? t("settings.about.build.dirty_suffix") : ""}${
                            about.build_time ? ` · ${formatDate(about.build_time)}` : ""
                          }`
                        : t("settings.about.build.no_vcs")}
                    </div>
                  </div>
                  <span className="pill">{about.go_version}</span>
                </div>
                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">{t("settings.about.process")}</div>
                    <div className="settings-row-sub text-meta">
                      {tf("settings.about.process.line", {
                        pid: about.pid,
                        os: about.os,
                        arch: about.arch,
                        uptime: formatUptime(about.uptime_seconds),
                      })}
                    </div>
                  </div>
                  <span className="pill">:{about.port}</span>
                </div>
                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">{t("settings.about.sqlite")}</div>
                    <div className="settings-row-sub text-meta">
                      {about.sqlite_path}
                      {about.sqlite_size_b >= 0
                        ? ` · ${formatBytes(about.sqlite_size_b)}`
                        : ""}
                    </div>
                  </div>
                </div>
              </>
            )}
          </div>
        </section>

        <section className="settings-group">
          <h3 className="settings-group-title">{t("settings.group.danger")}</h3>
          <div className="settings-card">
            <div className="settings-row">
              <div className="settings-row-text">
                <div className="settings-row-label">{t("settings.danger.reset.label")}</div>
                <div className="settings-row-sub text-meta">
                  {t("settings.danger.reset.sub")}
                </div>
                {resetError && (
                  <div className="settings-row-sub text-error">
                    {tf("settings.danger.reset.failed", { message: resetError })}
                  </div>
                )}
              </div>
              {confirmReset ? (
                <div className="row-actions">
                  <button
                    type="button"
                    className="btn"
                    onClick={() => setConfirmReset(false)}
                    disabled={resetBusy}
                  >
                    {t("common.cancel")}
                  </button>
                  <button
                    type="button"
                    className="btn btn-danger"
                    onClick={onResetData}
                    disabled={resetBusy || daemonMode === "attached"}
                    title={
                      daemonMode === "attached"
                        ? t("settings.danger.reset.disabled_attached")
                        : undefined
                    }
                  >
                    {resetBusy
                      ? t("settings.danger.reset.resetting")
                      : t("settings.danger.reset.confirm_btn")}
                  </button>
                </div>
              ) : (
                <button
                  type="button"
                  className="btn btn-danger"
                  onClick={() => setConfirmReset(true)}
                  disabled={daemonMode === "attached"}
                  title={
                    daemonMode === "attached"
                      ? t("settings.danger.reset.disabled_attached_long")
                      : undefined
                  }
                >
                  {t("settings.danger.reset.btn")}
                </button>
              )}
            </div>
          </div>
        </section>

        <p className="text-meta settings-footnote">
          {t("settings.footnote")}
        </p>
      </div>
    </>
  );
}

function NumberStepper({
  value,
  min,
  max,
  step,
  suffix,
  onChange,
}: {
  value: number;
  min: number;
  max: number;
  step: number;
  suffix: string;
  onChange: (v: number) => void;
}) {
  const clamp = (v: number) => Math.max(min, Math.min(max, v));
  return (
    <div className="number-stepper" role="spinbutton" aria-valuenow={value}>
      <button
        type="button"
        className="number-stepper-btn"
        aria-label={t("settings.stepper.decrease")}
        disabled={value <= min}
        onClick={() => onChange(clamp(value - step))}
      >
        −
      </button>
      <span className="number-stepper-value">
        {value}
        {suffix}
      </span>
      <button
        type="button"
        className="number-stepper-btn"
        aria-label={t("settings.stepper.increase")}
        disabled={value >= max}
        onClick={() => onChange(clamp(value + step))}
      >
        +
      </button>
    </div>
  );
}
