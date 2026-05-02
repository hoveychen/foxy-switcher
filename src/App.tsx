import { useCallback, useEffect, useRef, useState } from "react";
import { ask } from "@tauri-apps/plugin-dialog";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import {
  Account,
  ActivityEvent,
  AutoSwitchSettings,
  DaemonMode,
  Settings,
  ThresholdInput,
  apiClient,
  getDaemonMode,
  restartDaemon,
} from "./api";
import { AppShell } from "./components/AppShell";
import { AccountDrawer } from "./components/AccountDrawer";
import type { Route } from "./components/Sidebar";
import { notifyForEvents } from "./notify";
import { DashboardPage } from "./pages/DashboardPage";
import { AccountsPage } from "./pages/AccountsPage";
import { ActivityPage } from "./pages/ActivityPage";
import { SettingsPage } from "./pages/SettingsPage";
import { t, tf } from "./i18n";

const ROUTE_KEYS: Route[] = ["dashboard", "accounts", "activity", "settings"];

export default function App() {
  const [route, setRoute] = useState<Route>("accounts");
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [managedAccountId, setManagedAccountId] = useState<number>(0);
  const [now, setNow] = useState<number>(Date.now());
  const [error, setError] = useState<string | null>(null);
  const [daemonOk, setDaemonOk] = useState<boolean>(true);
  const [daemonMode, setDaemonMode] = useState<DaemonMode | null>(null);
  const [restarting, setRestarting] = useState(false);
  const [selectedAccountId, setSelectedAccountId] = useState<number | null>(
    null,
  );
  const [busyAccountId, setBusyAccountId] = useState<number | null>(null);
  const [autoSwitch, setAutoSwitchState] = useState<AutoSwitchSettings>({
    enabled: true,
    policy: "lru",
  });
  const [addAccountTick, setAddAccountTick] = useState(0);
  // Dashboard's "Recent activity" lives in App so its 5s refresh shares the
  // same listAccounts cadence — one daemon round-trip per tick instead of
  // every page mounting its own poller.
  const [recentEvents, setRecentEvents] = useState<ActivityEvent[]>([]);
  // User prefs are loaded once, then driven by Settings page edits. We don't
  // re-poll: the daemon never mutates these on its own, so the local copy
  // can't drift unless another window writes — and that's a v0.3 problem.
  const [settings, setSettings] = useState<Settings>({
    theme: "system",
    sidebar_mode: "auto",
    usage_poll_interval_sec: 60,
    default_threshold_percent: 95,
    restore_native_on_quit: true,
  });

  // Skip applying GET'd auto-switch state while a local write is in flight,
  // so a refresh tick that races a POST doesn't briefly revert the toggle to
  // the pre-write server value.
  const autoSwitchWritingRef = useRef(false);
  // Highwater mark for desktop notifications. We seed it from the FIRST
  // refresh that returns events (prevents a flood of stale alerts at app
  // launch), then only notify for events with id > this value.
  const lastNotifiedIDRef = useRef<number | null>(null);

  const refresh = useCallback(async () => {
    try {
      const [list, cred, events, auto] = await Promise.all([
        apiClient.listAccounts(),
        apiClient.credStatus(),
        // Dashboard only renders the top 5; ask for a small slice so the JSON
        // payload stays tight on every poll.
        apiClient.listActivity({ limit: 10 }).catch(() => [] as ActivityEvent[]),
        // Polled so TUI / other client edits to the policy show up here.
        apiClient.getAutoSwitch().catch(() => null),
      ]);
      setAccounts(list);
      setManagedAccountId(cred.managed_account_id);
      setRecentEvents(events);
      if (events.length > 0) {
        const newest = events[0].id; // events are newest-first
        if (lastNotifiedIDRef.current === null) {
          // First poll: seed without firing so backlog doesn't spam.
          lastNotifiedIDRef.current = newest;
        } else if (newest > lastNotifiedIDRef.current) {
          const fresh = events
            .filter((e) => e.id > (lastNotifiedIDRef.current ?? 0))
            .reverse(); // chronological order
          lastNotifiedIDRef.current = newest;
          void notifyForEvents(fresh);
        }
      }
      if (auto && !autoSwitchWritingRef.current) {
        setAutoSwitchState(auto);
      }
      setDaemonOk(true);
      setError(null);
    } catch (e) {
      setDaemonOk(false);
      setError(String(e));
    }
  }, []);

  useEffect(() => {
    apiClient
      .getSettings()
      .then(setSettings)
      .catch(() => {
        // Older daemon without /api/settings — keep optimistic defaults.
      });
    // Daemon mode is fixed at sidecar spawn time, so we only fetch it once.
    // Drives whether the disconnect banner offers a Restart button (owned)
    // or just a Retry (attached, since we don't own the lifecycle).
    getDaemonMode()
      .then(setDaemonMode)
      .catch(() => {});
  }, []);

  const onRestartDaemon = useCallback(async () => {
    setRestarting(true);
    try {
      await restartDaemon();
      // Give the new sidecar a beat to bind before the next refresh polls.
      // 400ms covers the typical launch — refresh will retry on its own
      // 5s tick if this still races.
      await new Promise((r) => setTimeout(r, 400));
      await refresh();
    } catch (e) {
      setError(tf("app.error.restart_failed", { error: String(e) }));
    } finally {
      setRestarting(false);
    }
  }, [refresh]);

  // Apply theme to <html data-theme>. CSS tokens.css already keys its
  // dark-mode block off both `prefers-color-scheme: dark` (system) and
  // `[data-theme="dark"]` so we only need to set the attribute.
  useEffect(() => {
    const root = document.documentElement;
    if (settings.theme === "system") {
      root.removeAttribute("data-theme");
    } else {
      root.setAttribute("data-theme", settings.theme);
    }
  }, [settings.theme]);

  const persistSettings = useCallback(async (patch: Partial<Settings>) => {
    setSettings((prev) => ({ ...prev, ...patch })); // optimistic
    try {
      const echoed = await apiClient.setSettings(patch);
      setSettings(echoed);
    } catch (e) {
      setError(String(e));
      apiClient.getSettings().then(setSettings).catch(() => {});
    }
  }, []);

  const persistAutoSwitch = useCallback(async (next: AutoSwitchSettings) => {
    autoSwitchWritingRef.current = true;
    setAutoSwitchState(next); // optimistic
    try {
      const echoed = await apiClient.setAutoSwitch(next);
      setAutoSwitchState(echoed);
    } catch (e) {
      setError(String(e));
      // Re-pull authoritative state so the toggle visually reverts on failure.
      apiClient.getAutoSwitch().then(setAutoSwitchState).catch(() => {});
    } finally {
      autoSwitchWritingRef.current = false;
    }
  }, []);

  const setAutoSwitch = useCallback(
    (v: AutoSwitchSettings) => {
      void persistAutoSwitch(v);
    },
    [persistAutoSwitch],
  );

  useEffect(() => {
    refresh();
    const i = setInterval(refresh, 5000);
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => {
      clearInterval(i);
      clearInterval(t);
    };
  }, [refresh]);

  // Default-select the currently injected account once it becomes known,
  // so the Drawer reveals it without a click. Subsequent user closes stick.
  const defaultedRef = useRef(false);
  useEffect(() => {
    if (defaultedRef.current) return;
    if (managedAccountId === 0) return;
    if (!accounts.some((a) => a.id === managedAccountId)) return;
    defaultedRef.current = true;
    setSelectedAccountId((prev) => prev ?? managedAccountId);
  }, [accounts, managedAccountId]);

  const wrapAction = useCallback(
    async (id: number, fn: () => Promise<unknown>) => {
      setBusyAccountId(id);
      try {
        await fn();
        await refresh();
      } catch (e) {
        setError(String(e));
      } finally {
        setBusyAccountId((curr) => (curr === id ? null : curr));
      }
    },
    [refresh],
  );

  const onUseNow = useCallback(
    (id: number) => wrapAction(id, () => apiClient.selectAccount(id)),
    [wrapAction],
  );
  const onRefreshAccount = useCallback(
    (id: number) => wrapAction(id, () => apiClient.refreshAccount(id)),
    [wrapAction],
  );
  const onTogglePause = useCallback(
    (a: Account) =>
      wrapAction(a.id, () => apiClient.setPaused(a.id, a.status === "active")),
    [wrapAction],
  );
  const onSetThresholds = useCallback(
    (id: number, t: ThresholdInput) =>
      wrapAction(id, () => apiClient.setThresholds(id, t)),
    [wrapAction],
  );
  const onDelete = useCallback(
    async (id: number) => {
      const ok = await ask(t("app.delete_account.prompt"), {
        title: t("app.delete_account.title"),
        kind: "warning",
      });
      if (!ok) return;
      setSelectedAccountId((curr) => (curr === id ? null : curr));
      await wrapAction(id, () => apiClient.deleteAccount(id));
    },
    [wrapAction],
  );

  const onAutoSwitchToggle = useCallback(() => {
    void persistAutoSwitch({ ...autoSwitch, enabled: !autoSwitch.enabled });
  }, [autoSwitch, persistAutoSwitch]);

  const selectedAccount =
    selectedAccountId !== null
      ? accounts.find((a) => a.id === selectedAccountId) ?? null
      : null;

  const onNavigate = useCallback((r: Route) => {
    setRoute(r);
    setSelectedAccountId(null);
  }, []);

  // Native menu (lib.rs build_app_menu) emits these events. They duplicate
  // the in-webview keyboard shortcuts below — the OS menu's accelerator
  // intercepts the keystroke before it reaches the webview, so the menu
  // path is what actually fires when an accelerator is bound.
  useEffect(() => {
    const subs: Promise<UnlistenFn>[] = [
      listen<string>("menu:navigate", (e) => {
        const r = e.payload as Route;
        if (ROUTE_KEYS.includes(r)) {
          setRoute(r);
          setSelectedAccountId(null);
        }
      }),
      listen<void>("menu:add-account", () => {
        setRoute("accounts");
        setSelectedAccountId(null);
        setAddAccountTick((t) => t + 1);
      }),
      listen<void>("menu:refresh", () => {
        void refresh();
      }),
    ];
    return () => {
      subs.forEach((p) => void p.then((u) => u()));
    };
  }, [refresh]);

  return (
    <AppShell
      current={route}
      onNavigate={onNavigate}
      daemonOk={daemonOk}
      drawer={
        route === "accounts" && selectedAccount ? (
          <AccountDrawer
            account={selectedAccount}
            nowMs={now}
            isInUse={selectedAccount.id === managedAccountId}
            busy={busyAccountId === selectedAccount.id}
            onClose={() => setSelectedAccountId(null)}
            onSelect={() => onUseNow(selectedAccount.id)}
            onRefresh={() => onRefreshAccount(selectedAccount.id)}
            onTogglePause={() => onTogglePause(selectedAccount)}
            onSetThresholds={(t) => onSetThresholds(selectedAccount.id, t)}
            onDelete={() => onDelete(selectedAccount.id)}
          />
        ) : undefined
      }
    >
      {!daemonOk && (
        <div className="banner banner-disconnect">
          <div className="banner-body">
            <strong>{t("banner.disconnect.title")}</strong>
            <span className="text-meta">
              {daemonMode === "attached"
                ? t("banner.disconnect.attached")
                : t("banner.disconnect.owned")}
            </span>
          </div>
          <div className="banner-actions">
            <button
              type="button"
              className="btn btn-ghost"
              onClick={() => void refresh()}
              disabled={restarting}
            >
              {t("banner.disconnect.retry")}
            </button>
            {daemonMode !== "attached" && (
              <button
                type="button"
                className="btn btn-primary"
                onClick={onRestartDaemon}
                disabled={restarting}
              >
                {restarting
                  ? t("banner.disconnect.restarting")
                  : t("banner.disconnect.restart")}
              </button>
            )}
          </div>
        </div>
      )}
      {error && (
        <div className="banner err">
          <span>{error}</span>
          <button className="btn btn-ghost" onClick={() => setError(null)}>
            {t("banner.dismiss")}
          </button>
        </div>
      )}
      {route === "dashboard" && (
        <DashboardPage
          accounts={accounts}
          managedAccountId={managedAccountId}
          nowMs={now}
          onNavigate={setRoute}
          autoSwitch={autoSwitch}
          onAutoSwitchChange={setAutoSwitch}
          onAutoSwitchToggle={onAutoSwitchToggle}
          recentEvents={recentEvents}
          stale={!daemonOk}
        />
      )}
      {route === "accounts" && (
        <AccountsPage
          accounts={accounts}
          managedAccountId={managedAccountId}
          nowMs={now}
          selectedAccountId={selectedAccountId}
          busyAccountId={busyAccountId}
          addAccountTick={addAccountTick}
          onAddAccountConsumed={() => setAddAccountTick(0)}
          autoSwitch={autoSwitch}
          onAutoSwitchToggle={onAutoSwitchToggle}
          onSelectRow={setSelectedAccountId}
          onUseNow={onUseNow}
          onRefreshAccount={onRefreshAccount}
          onTogglePause={onTogglePause}
          onDelete={onDelete}
          onRefresh={refresh}
          onError={setError}
          stale={!daemonOk}
        />
      )}
      {route === "activity" && (
        <ActivityPage
          autoSwitch={autoSwitch}
          onAutoSwitchToggle={onAutoSwitchToggle}
        />
      )}
      {route === "settings" && (
        <SettingsPage
          autoSwitch={autoSwitch}
          onAutoSwitchToggle={onAutoSwitchToggle}
          settings={settings}
          onSettingsChange={persistSettings}
        />
      )}
    </AppShell>
  );
}
