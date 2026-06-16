import { useEffect, useState } from "react";
import { PairVaultModal } from "../components/PairVaultModal";
import { Topbar } from "../components/Topbar";
import {
  AboutResponse,
  DaemonMode,
  Settings,
  Theme,
  VaultDevice,
  apiClient,
  autostartIsEnabled,
  autostartSet,
  clearAgentConfig,
  dataDirPath,
  getDaemonMode,
  getServerPort,
  isTauriHost,
  openExternal,
  restartDaemon,
  revealDataDir,
} from "../api";
import { Locale, getLocaleOverride, setLocaleOverride, t, tf } from "../i18n";
import { AUTO_UPDATE_CHECK_KEY } from "../components/UpdateNotice";

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

// fmtDeviceTime renders a unix-millis timestamp as YYYY-MM-DD HH:mm in
// the user's local zone. Used for /api/devices' created_at /
// last_seen_at columns. Returns "—" for the zero value so callers can
// show e.g. "Last seen: never" via a separate i18n key.
function fmtDeviceTime(ms: number): string {
  if (!ms) return "—";
  const d = new Date(ms);
  if (Number.isNaN(d.getTime())) return "—";
  const pad = (n: number) => n.toString().padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function SettingsPage({
  autoSwitch,
  onAutoSwitchToggle,
  onAutoSwitchChange,
  settings,
  onSettingsChange,
  markerState,
}: {
  autoSwitch?: { enabled: boolean; policy: Policy };
  onAutoSwitchToggle?: () => void;
  onAutoSwitchChange?: (v: { enabled: boolean; policy: Policy }) => void;
  settings: Settings;
  onSettingsChange: (patch: Partial<Settings>) => void;
  markerState?: string;
}) {
  const [daemonMode, setDaemonMode] = useState<DaemonMode | null>(null);
  const [daemonPort, setDaemonPort] = useState<number | null>(null);
  const [autostartEnabled, setAutostartEnabled] = useState<boolean | null>(null);
  const [dataDir, setDataDir] = useState<string | null>(null);
  const [autostartBusy, setAutostartBusy] = useState(false);
  const [revealBusy, setRevealBusy] = useState(false);
  // Update-check auto-run toggle is mirrored to localStorage on every flip;
  // UpdateNotice reads the same key on mount. Default "true" means a brand
  // new install will check once at first launch.
  const [autoUpdateCheck, setAutoUpdateCheck] = useState<boolean>(() => {
    try {
      return localStorage.getItem(AUTO_UPDATE_CHECK_KEY) !== "false";
    } catch {
      return true;
    }
  });
  const [about, setAbout] = useState<AboutResponse | null>(null);
  const [confirmReset, setConfirmReset] = useState(false);
  const [resetBusy, setResetBusy] = useState(false);
  const [resetError, setResetError] = useState<string | null>(null);
  const [pairCmdCopied, setPairCmdCopied] = useState(false);
  const [pairOpen, setPairOpen] = useState(false);
  const [confirmUnpair, setConfirmUnpair] = useState(false);
  const [unpairBusy, setUnpairBusy] = useState(false);
  const [unpairError, setUnpairError] = useState<string | null>(null);
  const [devices, setDevices] = useState<VaultDevice[] | null>(null);
  const [devicesError, setDevicesError] = useState<string | null>(null);
  const [applyThresholdsBusy, setApplyThresholdsBusy] = useState(false);
  const [applyThresholdsMsg, setApplyThresholdsMsg] = useState<string | null>(null);
  const localeOverride = getLocaleOverride();
  // tauriHost is computed once at mount — the host never changes
  // mid-session. Settings rows that depend on Tauri-side bridges
  // (autostart, reveal data dir, save_agent_config) hide themselves
  // when this is false so the browser-served React build doesn't
  // surface buttons that just throw NotInTauriError on click.
  const tauriHost = isTauriHost();

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

  // Devices load only kicks in once we know the mode is "agent" — combined
  // mode is single-machine by definition and the section stays hidden.
  // Reload whenever vaultMode flips (e.g. after pair/unpair restarts the
  // daemon and the new about response arrives).
  useEffect(() => {
    if (vaultMode !== "agent") {
      setDevices(null);
      setDevicesError(null);
      return;
    }
    apiClient
      .listDevices()
      .then((d) => {
        setDevices(d);
        setDevicesError(null);
      })
      .catch((e) => {
        setDevices([]);
        setDevicesError(e instanceof Error ? e.message : String(e));
      });
  }, [vaultMode]);

  const onUnpair = async () => {
    if (unpairBusy) return;
    setUnpairBusy(true);
    setUnpairError(null);
    try {
      await clearAgentConfig();
      // restart so detectModeFromConfig picks up the now-missing
      // agent-config.json on the next launch and boots in combined.
      await restartDaemon();
      // Bounce the window so cached port + about state re-hydrate from
      // the newly-spawned sidecar (mirrors onResetData's reload).
      setTimeout(() => window.location.reload(), 800);
    } catch (e) {
      setUnpairError(e instanceof Error ? e.message : String(e));
      setUnpairBusy(false);
      setConfirmUnpair(false);
    }
  };

  const onApplyThresholds = async () => {
    if (applyThresholdsBusy) return;
    setApplyThresholdsBusy(true);
    setApplyThresholdsMsg(null);
    try {
      // Persist the current form values first so the server applies exactly
      // what's shown — the apply endpoint reads the saved settings, not the
      // request body — then push them across every account.
      await apiClient.setSettings({
        default_five_hour: settings.default_five_hour,
        default_seven_day: settings.default_seven_day,
        default_seven_day_sonnet: settings.default_seven_day_sonnet,
      });
      const r = await apiClient.applyThresholdDefaults();
      setApplyThresholdsMsg(
        tf("settings.threshold_default.applied", { count: r.updated }),
      );
    } catch (e) {
      setApplyThresholdsMsg(e instanceof Error ? e.message : String(e));
    } finally {
      setApplyThresholdsBusy(false);
    }
  };

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
            {tauriHost && (
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
            )}

            {tauriHost && (
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
            )}

            {tauriHost && (
              <div className="settings-row">
                <div className="settings-row-text">
                  <div className="settings-row-label">{t("settings.update.auto.label")}</div>
                  <div className="settings-row-sub text-meta">
                    {t("settings.update.auto.sub")}
                  </div>
                </div>
                <button
                  type="button"
                  role="switch"
                  aria-checked={autoUpdateCheck}
                  aria-label={t("settings.update.auto.aria")}
                  className={`toggle ${autoUpdateCheck ? "on" : "off"}`}
                  onClick={() => {
                    const next = !autoUpdateCheck;
                    setAutoUpdateCheck(next);
                    try {
                      localStorage.setItem(
                        AUTO_UPDATE_CHECK_KEY,
                        next ? "true" : "false",
                      );
                    } catch {
                      // ignore — preference just won't persist across
                      // launches in this sandboxed storage env
                    }
                  }}
                >
                  <span className="toggle-thumb" aria-hidden />
                </button>
              </div>
            )}

            {tauriHost && (
              <div className="settings-row">
                <div className="settings-row-text">
                  <div className="settings-row-label">{t("settings.update.check_now.label")}</div>
                  <div className="settings-row-sub text-meta">
                    {t("settings.update.check_now.sub")}
                  </div>
                </div>
                <button
                  type="button"
                  className="btn"
                  onClick={() => {
                    // UpdateNotice listens for this DOM event and runs a
                    // forced check; routing through the event keeps banner
                    // state in one place instead of duplicating it here.
                    window.dispatchEvent(new Event("foxy:check-updates"));
                  }}
                >
                  {t("settings.update.check_now.button")}
                </button>
              </div>
            )}

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

        {autoSwitch && onAutoSwitchToggle && onAutoSwitchChange && (
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
        )}

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
                <div className="settings-row-label">
                  {t("settings.marker_state.label")}
                </div>
                <div className="settings-row-sub text-meta">
                  {t("settings.marker_state.sub")}
                </div>
              </div>
              <span
                className={`pill ${
                  markerState === "intact" ? "active-pill" : ""
                }`}
              >
                {t(
                  `settings.marker_state.value.${
                    (markerState || "unknown").replace("-", "_")
                  }`,
                )}
              </span>
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

            {/* threshold_default writes to vault state in agent mode;
                hide so the user isn't led to a 405 path. The vault
                admin web UI is the right place for cross-agent
                threshold defaults. */}
            {vaultMode !== "agent" && (
              <>
                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">
                      {t("settings.threshold_default.five_hour")}
                    </div>
                    <div className="settings-row-sub text-meta">
                      {t("settings.threshold_default.sub")}
                    </div>
                  </div>
                  <NumberStepper
                    value={settings.default_five_hour}
                    min={50}
                    max={100}
                    step={5}
                    suffix="%"
                    onChange={(v) => onSettingsChange({ default_five_hour: v })}
                  />
                </div>

                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">
                      {t("settings.threshold_default.seven_day")}
                    </div>
                  </div>
                  <NumberStepper
                    value={settings.default_seven_day}
                    min={50}
                    max={100}
                    step={5}
                    suffix="%"
                    onChange={(v) => onSettingsChange({ default_seven_day: v })}
                  />
                </div>

                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">
                      {t("settings.threshold_default.seven_day_sonnet")}
                    </div>
                  </div>
                  <NumberStepper
                    value={settings.default_seven_day_sonnet}
                    min={50}
                    max={100}
                    step={5}
                    suffix="%"
                    onChange={(v) =>
                      onSettingsChange({ default_seven_day_sonnet: v })
                    }
                  />
                </div>

                <div className="settings-row">
                  <div className="settings-row-text">
                    <div className="settings-row-label">
                      {t("settings.threshold_default.apply.label")}
                    </div>
                    <div className="settings-row-sub text-meta">
                      {applyThresholdsMsg ??
                        t("settings.threshold_default.apply.sub")}
                    </div>
                  </div>
                  <button
                    type="button"
                    className="btn"
                    disabled={applyThresholdsBusy}
                    onClick={onApplyThresholds}
                  >
                    {applyThresholdsBusy
                      ? t("settings.threshold_default.apply.busy")
                      : t("settings.threshold_default.apply.button")}
                  </button>
                </div>
              </>
            )}
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
                      {about?.vault_url ? (
                        <a
                          href={about.vault_url}
                          onClick={(e) => {
                            e.preventDefault();
                            void openExternal(about.vault_url!);
                          }}
                        >
                          {about.vault_url}
                        </a>
                      ) : (
                        ""
                      )}
                    </div>
                  </div>
                  {about?.vault_url && (
                    <button
                      type="button"
                      className="btn"
                      onClick={() => void openExternal(about.vault_url!)}
                    >
                      {t("settings.vault.manage.btn")}
                    </button>
                  )}
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
                {tauriHost && (
                  <div className="settings-row">
                    <div className="settings-row-text">
                      <div className="settings-row-label">
                        {t("settings.vault.unpair.label")}
                      </div>
                      <div className="settings-row-sub text-meta">
                        {t("settings.vault.unpair.sub")}
                      </div>
                      {unpairError && (
                        <div className="text-meta" style={{ color: "#c33" }}>
                          {tf("settings.vault.unpair.failed", {
                            message: unpairError,
                          })}
                        </div>
                      )}
                    </div>
                    {confirmUnpair ? (
                      <div style={{ display: "flex", gap: 8 }}>
                        <button
                          type="button"
                          className="btn"
                          onClick={() => setConfirmUnpair(false)}
                          disabled={unpairBusy}
                        >
                          {t("settings.vault.unpair.cancel")}
                        </button>
                        <button
                          type="button"
                          className="btn btn-danger"
                          onClick={onUnpair}
                          disabled={unpairBusy}
                        >
                          {unpairBusy
                            ? t("settings.vault.unpair.busy")
                            : t("settings.vault.unpair.confirm")}
                        </button>
                      </div>
                    ) : (
                      <button
                        type="button"
                        className="btn"
                        onClick={() => setConfirmUnpair(true)}
                      >
                        {t("settings.vault.unpair.btn")}
                      </button>
                    )}
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
              <div style={{ display: "flex", gap: 8 }}>
                <button type="button" className="btn" onClick={onCopyPairCmd}>
                  {pairCmdCopied
                    ? t("settings.vault.pair.copied")
                    : t("settings.vault.pair.copy")}
                </button>
                {tauriHost && (
                  <button
                    type="button"
                    className="btn btn-primary"
                    onClick={() => setPairOpen(true)}
                  >
                    {t("settings.vault.pair.start")}
                  </button>
                )}
              </div>
            </div>
          </div>
          <PairVaultModal
            open={pairOpen}
            onClose={() => setPairOpen(false)}
            defaultUrl={about?.vault_url}
          />
        </section>

        {vaultMode === "agent" && (
          <section className="settings-group">
            <h3 className="settings-group-title">
              {t("settings.group.devices")}
            </h3>
            <div className="settings-card">
              <div className="settings-row">
                <div className="settings-row-text">
                  <div className="settings-row-sub text-meta">
                    {t("settings.devices.sub")}
                  </div>
                  {devicesError && (
                    <div className="text-meta" style={{ color: "#c33" }}>
                      {tf("settings.devices.error", { message: devicesError })}
                    </div>
                  )}
                </div>
              </div>
              {devices === null ? (
                <div className="settings-row">
                  <div className="settings-row-sub text-meta">
                    {t("settings.devices.loading")}
                  </div>
                </div>
              ) : devices.length === 0 ? (
                <div className="settings-row">
                  <div className="settings-row-sub text-meta">
                    {t("settings.devices.empty")}
                  </div>
                </div>
              ) : (
                devices.map((d) => {
                  const isThis = about?.device_id && d.id === about.device_id;
                  return (
                    <div className="settings-row" key={d.id}>
                      <div className="settings-row-text">
                        <div className="settings-row-label">
                          {d.name || d.hostname || d.id.slice(0, 8)}
                          {d.hostname && d.hostname !== d.name && (
                            <span
                              className="text-meta"
                              style={{ marginLeft: 8, fontWeight: "normal" }}
                            >
                              {d.hostname}
                            </span>
                          )}
                        </div>
                        <div className="settings-row-sub text-meta">
                          {[
                            (d.os || d.os_version) &&
                              `${t("settings.devices.os")}: ${[d.os, d.os_version].filter(Boolean).join(" ")}`,
                            d.arch &&
                              `${t("settings.devices.arch")}: ${d.arch}`,
                            d.model &&
                              `${t("settings.devices.model")}: ${d.model}`,
                            d.app_version &&
                              `${t("settings.devices.app")}: ${d.app_version}`,
                            d.client_type &&
                              `${t("settings.devices.client_type")}: ${d.client_type}`,
                          ]
                            .filter(Boolean)
                            .join(" · ")}
                        </div>
                        <div className="settings-row-sub text-meta">
                          {t("settings.devices.paired_at")}:{" "}
                          {fmtDeviceTime(d.created_at)} ·{" "}
                          {t("settings.devices.last_seen")}:{" "}
                          {d.last_seen_at
                            ? fmtDeviceTime(d.last_seen_at)
                            : t("settings.devices.never")}
                        </div>
                      </div>
                      {isThis && (
                        <span className="pill active-pill">
                          {t("settings.devices.this_device")}
                        </span>
                      )}
                    </div>
                  );
                })
              )}
            </div>
          </section>
        )}

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
