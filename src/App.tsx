import { useCallback, useEffect, useRef, useState } from "react";
import { ask } from "@tauri-apps/plugin-dialog";
import { listen, type UnlistenFn } from "@tauri-apps/api/event";
import {
  Account,
  ActivityEvent,
  AboutResponse,
  AutoSwitchSettings,
  DaemonMode,
  Settings,
  ThresholdInput,
  apiClient,
  getDaemonMode,
  isTauriHost,
  restartDaemon,
} from "./api";
import { adminApi, type AdminMe } from "./admin/api";
import { AppShell } from "./components/AppShell";
import { AccountDrawer } from "./components/AccountDrawer";
import { UpdateNotice } from "./components/UpdateNotice";
import type { Route } from "./components/Sidebar";
import { notifyForEvents } from "./notify";
import { DashboardPage } from "./pages/DashboardPage";
import { AccountsPage } from "./pages/AccountsPage";
import { ActivityPage } from "./pages/ActivityPage";
import { SettingsPage } from "./pages/SettingsPage";
import { DevicesPage } from "./admin/pages/DevicesPage";
import { PairPage } from "./admin/pages/PairPage";
import { PasswordPage } from "./admin/pages/PasswordPage";
import { OnboardingOverlay } from "./onboarding/OnboardingOverlay";
import { t, tf } from "./i18n";

const ONBOARDING_SEEN_KEY = "foxy.onboarding.seen.v1";

// Tauri's ask() throws in the browser (no IPC), so anywhere this
// shared App.tsx runs in vault web (browser) we'd silently get
// `undefined → falsy` and the user's intent would be dropped. Fall
// back to window.confirm() in that path.
async function confirmAction(message: string, title: string): Promise<boolean> {
  if (isTauriHost()) {
    return ask(message, { title, kind: "warning" });
  }
  if (typeof window === "undefined" || !window.confirm) return false;
  return window.confirm(`${title}\n\n${message}`);
}

// ROUTE_KEYS lists every route the in-app navigation surfaces — the
// native menu's "menu:navigate" event payload validates against this
// list. Admin routes (devices/pair/password) are included so a deep
// link in the browser (vault server / + path) opens the right page;
// the menu doesn't expose them since native menus only show on Tauri.
const ROUTE_KEYS: Route[] = [
  "dashboard",
  "accounts",
  "activity",
  "settings",
  "devices",
  "pair",
  "password",
];

// initialRouteFromPath reads window.location.pathname so a user who
// landed at /devices (because the vault server 301-redirected from
// /admin/devices, or they bookmarked the URL) sees the matching
// sidebar item active on first paint. Tauri always loads from "/" so
// it falls through to "dashboard".
function initialRouteFromPath(): Route {
  if (typeof window === "undefined") return "dashboard";
  const p = window.location.pathname;
  switch (p) {
    case "/accounts":
      return "accounts";
    case "/activity":
      return "activity";
    case "/settings":
      return "settings";
    case "/devices":
      return "devices";
    case "/pair":
      return "pair";
    case "/password":
      return "password";
    default:
      return "dashboard";
  }
}

export default function App() {
  const [route, setRoute] = useState<Route>(initialRouteFromPath);
  // adminMe is only meaningful in the browser (the vault server's web
  // UI). Tauri leaves it null so the admin sidebar section stays hidden.
  // Populated by GET /admin/api/me on mount; revoked when adminApi
  // returns 401 from any inner page.
  const [adminMe, setAdminMe] = useState<AdminMe | null>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [managedAccountId, setManagedAccountId] = useState<number>(0);
  const [markerState, setMarkerState] = useState<string>("");
  const [now, setNow] = useState<number>(Date.now());
  const [error, setError] = useState<string | null>(null);
  const [daemonOk, setDaemonOk] = useState<boolean>(true);
  const [daemonMode, setDaemonMode] = useState<DaemonMode | null>(null);
  // about is fetched once on mount and on every restart, so the
  // Accounts/Settings pages can branch on vaultMode without refetching
  // per page. The agent-mode guard hides admin-write UI elements that
  // would 405 against the agent's local proxy anyway.
  const [about, setAbout] = useState<AboutResponse | null>(null);
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
  // First-launch intro: read once on mount; localStorage access can throw in
  // sandboxed contexts so swallow and default to "already seen" to avoid
  // surprising users with a video that can't be dismissed.
  const [showOnboarding, setShowOnboarding] = useState<boolean>(() => {
    try {
      return localStorage.getItem(ONBOARDING_SEEN_KEY) !== "1";
    } catch {
      return false;
    }
  });
  const dismissOnboarding = useCallback(() => {
    try {
      localStorage.setItem(ONBOARDING_SEEN_KEY, "1");
    } catch {
      // ignore — user just won't be persisted, overlay still closes
    }
    setShowOnboarding(false);
  }, []);
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
    default_five_hour: 95,
    default_seven_day: 95,
    default_seven_day_sonnet: 95,
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
    // allSettled, NOT Promise.all: in agent mode listAccounts / listActivity
    // proxy to a remote vault, so a single transient hiccup (network blip,
    // SSO, cold proxy) used to reject the whole batch and skip every setter —
    // freezing the accounts list (and its lease badges) on the last good
    // snapshot until a later poll happened to succeed. Settling each call
    // independently lets the pieces that did succeed update, and keeps the
    // last-known value for the ones that didn't.
    const [listR, credR, eventsR, autoR] = await Promise.allSettled([
      apiClient.listAccounts(),
      apiClient.credStatus(),
      // Dashboard only renders the top 5; ask for a small slice so the JSON
      // payload stays tight on every poll.
      apiClient.listActivity({ limit: 10 }),
      // Polled so TUI / other client edits to the policy show up here.
      apiClient.getAutoSwitch(),
    ]);
    if (listR.status === "fulfilled") setAccounts(listR.value);
    if (credR.status === "fulfilled") {
      setManagedAccountId(credR.value.managed_account_id);
      setMarkerState(credR.value.marker_state ?? "");
    }
    // Only touch recentEvents when the fetch succeeded — keep the last-known
    // timeline on a transient failure rather than blanking it.
    if (eventsR.status === "fulfilled") {
      const events = eventsR.value;
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
    }
    if (
      autoR.status === "fulfilled" &&
      autoR.value &&
      !autoSwitchWritingRef.current
    ) {
      setAutoSwitchState(autoR.value);
    }
    // Connectivity is keyed off credStatus, not listAccounts: credStatus is
    // served locally even in agent mode (the agent owns /api/cred/status),
    // so it's the truest "is the local daemon up" probe. A remote
    // listAccounts failure must NOT trip the disconnect banner when the
    // local daemon is perfectly healthy.
    if (credR.status === "fulfilled") {
      setDaemonOk(true);
      setError(null);
    } else {
      setDaemonOk(false);
      setError(String(credR.reason));
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
    apiClient
      .getAbout()
      .then(setAbout)
      .catch(() => {});
  }, []);

  // Auto-dismiss the onboarding overlay for users who already have a
  // working vault: paired agents (mode="agent" with a vault_url) and
  // combined-mode users with accounts in the pool. Without this hook a
  // user upgrading from a pre-onboarding-wizard build would be forced
  // through "choose local or cloud" even though their setup is fine.
  // Fresh installs (mode=combined, zero accounts, no localStorage flag)
  // still walk the wizard.
  //
  // Vault-only mode (the cloud server's own web admin UI, opened in a
  // browser at the vault origin) must also skip the wizard: the "store
  // your account locally or in the cloud" choice is meaningless when
  // you are already looking at the cloud vault, and a brand-new vault
  // has zero accounts so the listAccounts branch below would otherwise
  // leave the overlay stuck open over the admin UI.
  //
  // Agent-mode dismissal must NOT depend on listAccounts succeeding —
  // a paired vault behind an SSO that gates /api/* (whitelisting only
  // /agent/v1/*) makes listAccounts throw, and a Promise.all that
  // bundled the two would reject on that throw, leave the overlay
  // open, and trap the user behind the intro video on every restart.
  useEffect(() => {
    if (!showOnboarding) return;
    let canceled = false;
    (async () => {
      try {
        const about = await apiClient.getAbout();
        if (canceled) return;
        if (about.mode === "agent" && about.vault_url) {
          dismissOnboarding();
          return;
        }
        if (about.mode === "vault") {
          dismissOnboarding();
          return;
        }
        const list = await apiClient.listAccounts();
        if (canceled) return;
        if (list.length > 0) dismissOnboarding();
      } catch {
        // daemon not ready yet — leave overlay open; the next mount or
        // refresh tick will re-evaluate.
      }
    })();
    return () => {
      canceled = true;
    };
  }, [showOnboarding, dismissOnboarding]);

  const onRestartDaemon = useCallback(async () => {
    setRestarting(true);
    try {
      await restartDaemon();
      // Give the new sidecar a beat to bind before the next refresh polls.
      // 400ms covers the typical launch — refresh will retry on its own
      // 5s tick if this still races.
      await new Promise((r) => setTimeout(r, 400));
      await refresh();
      // Mode can flip across a restart (pair / unpair); re-pull about so
      // agent-mode UI guards stay aligned with the new daemon.
      apiClient
        .getAbout()
        .then(setAbout)
        .catch(() => {});
    } catch (e) {
      setError(tf("app.error.restart_failed", { error: String(e) }));
    } finally {
      setRestarting(false);
    }
  }, [refresh]);

  // disableAdminActions reflects "this daemon is an agent talking to a
  // remote vault". Account CRUD belongs to the vault admin web UI in
  // that topology — see TASKS.md plan agent-lease-only — so the desktop
  // hides Add / Pause / Resume / Delete / Threshold UI to avoid leading
  // the user to operations the agent will 405.
  const disableAdminActions = about?.mode === "agent";
  // isVaultOnly is the dual: this UI is being served by a vault-only daemon
  // (browser at vault origin, no local agent). Per-agent-local features
  // — auto-switch toggle, daemon-process health badge — have no meaning
  // here and are hidden, mirroring how agent mode hides admin actions.
  const isVaultOnly = about?.mode === "vault";

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

  // Force a refresh the moment the window becomes visible / regains focus.
  // FoxySwitcher is a tray app — closing the window only hides it (the
  // sidecar keeps running), so the window sits hidden most of the time.
  // WKWebView throttles/suspends the 5s setInterval above while hidden (and
  // the OS pauses it entirely across sleep), so on re-open the accounts list
  // — including each account's lease badge — would otherwise show a stale
  // snapshot from whenever the timer last fired. In agent mode that's how a
  // pool account another device leased while you were away still looks free
  // until you act on it. Re-fetching on show closes that gap.
  useEffect(() => {
    const onShow = () => {
      if (document.visibilityState === "visible") void refresh();
    };
    document.addEventListener("visibilitychange", onShow);
    window.addEventListener("focus", onShow);
    return () => {
      document.removeEventListener("visibilitychange", onShow);
      window.removeEventListener("focus", onShow);
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
    async (a: Account) => {
      // Going-to-pause on a foreign-leased account evicts the other
      // device's lease and 401s their CC session. Resume is harmless
      // (the other device still holds the lease), so only confirm
      // when transitioning active → paused.
      const goingToPause = a.status === "active";
      const fLease = a.lease && !a.lease.mine ? a.lease : null;
      if (goingToPause && fLease) {
        const ok = await confirmAction(
          tf("app.foreign_lease.pause.prompt", {
            device: fLease.device_name || fLease.device_id || "—",
          }),
          t("app.foreign_lease.pause.title"),
        );
        if (!ok) return;
      }
      await wrapAction(a.id, () => apiClient.setPaused(a.id, goingToPause));
    },
    [wrapAction],
  );
  const onSetThresholds = useCallback(
    (id: number, t: ThresholdInput) =>
      wrapAction(id, () => apiClient.setThresholds(id, t)),
    [wrapAction],
  );
  const onDelete = useCallback(
    async (id: number) => {
      // Surface a sharper warning for foreign-leased accounts: deleting
      // them yanks the row out from under the other device mid-session.
      const target = accounts.find((a) => a.id === id);
      const fLease = target?.lease && !target.lease.mine ? target.lease : null;
      const ok = fLease
        ? await confirmAction(
            tf("app.foreign_lease.delete.prompt", {
              device: fLease.device_name || fLease.device_id || "—",
            }),
            t("app.foreign_lease.delete.title"),
          )
        : await confirmAction(
            t("app.delete_account.prompt"),
            t("app.delete_account.title"),
          );
      if (!ok) return;
      setSelectedAccountId((curr) => (curr === id ? null : curr));
      await wrapAction(id, () => apiClient.deleteAccount(id));
    },
    [accounts, wrapAction],
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
    // Browser host: keep window.location in sync so a refresh re-enters
    // the same sidebar section (and bookmarks behave). Tauri's webview
    // is stuck on file:// (or the dev URL) so syncing has no value
    // there and pushState would burn history entries.
    if (typeof window === "undefined" || isTauriHost()) return;
    const target = r === "dashboard" ? "/" : "/" + r;
    if (window.location.pathname !== target) {
      window.history.pushState({}, "", target);
    }
  }, []);

  // Browser host: bootstrap admin state once on mount. Tauri leaves
  // adminMe null forever — admin actions don't make sense from the
  // local desktop, so the sidebar's admin section stays hidden.
  useEffect(() => {
    if (isTauriHost()) return;
    let canceled = false;
    (async () => {
      try {
        const me = await adminApi.me();
        if (!canceled) setAdminMe(me);
      } catch {
        // /admin/api/me only exists on the vault server. In dev (npm
        // run dev pointed at a non-vault daemon) this 404s — harmless,
        // adminMe stays null and the admin sidebar items don't render.
      }
    })();
    return () => {
      canceled = true;
    };
  }, []);

  // Sync route with browser back/forward so the sidebar follows the
  // URL when the user navigates via history. No-op in Tauri.
  useEffect(() => {
    if (isTauriHost()) return;
    const onPop = () => setRoute(initialRouteFromPath());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  const onAdminLogout = useCallback(async () => {
    try {
      await adminApi.logout();
    } catch {
      // Best-effort: even if the server rejects, drop the local state
      // so the UI stops offering admin actions.
    }
    setAdminMe(null);
    // Bounce to the login page so the next admin action starts fresh.
    if (typeof window !== "undefined" && !isTauriHost()) {
      window.location.assign("/admin/login");
    }
  }, []);

  // onAdminUnauthorized fires when an admin page sees a 401 — usually
  // because the session timed out. Drop adminMe and route the browser
  // to /admin/login so the user can sign back in.
  const onAdminUnauthorized = useCallback(() => {
    setAdminMe(null);
    if (typeof window !== "undefined" && !isTauriHost()) {
      const next = encodeURIComponent(window.location.pathname);
      window.location.assign(`/admin/login?next=${next}`);
    }
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
        // Agent mode proxies admin writes to the vault and gets 405 back.
        // Don't even surface the modal — the vault admin web UI is where
        // account login belongs. Mirrors the disableAdminActions guards
        // on the in-app + button. Native menu items don't carry app
        // state so the gate has to live here.
        if (disableAdminActions) return;
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
  }, [refresh, disableAdminActions]);

  // Show the admin sidebar section when the App runs in a browser at
  // the vault server origin and the visitor is signed in as admin.
  // Tauri desktop never shows it (admin is the vault owner's surface),
  // and signed-out browser visitors don't either (they 'd hit /admin/login
  // before reaching here for /devices et al, but the sidebar still
  // shouldn't tease links to pages they can't open).
  const showAdminNav = !isTauriHost() && !!adminMe?.signed_in;

  return (
    <AppShell
      current={route}
      onNavigate={onNavigate}
      daemonOk={daemonOk}
      showAdminNav={showAdminNav}
      onAdminLogout={showAdminNav ? onAdminLogout : undefined}
      hideDaemonStatus={isVaultOnly}
      drawer={
        route === "accounts" && selectedAccount ? (
          <AccountDrawer
            account={selectedAccount}
            nowMs={now}
            isInUse={selectedAccount.id === managedAccountId}
            busy={busyAccountId === selectedAccount.id}
            disableAdminActions={disableAdminActions}
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
      <UpdateNotice />
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
          autoSwitch={isVaultOnly ? undefined : autoSwitch}
          onAutoSwitchToggle={isVaultOnly ? undefined : onAutoSwitchToggle}
          recentEvents={recentEvents}
          stale={!daemonOk}
          vaultMode={isVaultOnly}
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
          autoSwitch={isVaultOnly ? undefined : autoSwitch}
          onAutoSwitchToggle={isVaultOnly ? undefined : onAutoSwitchToggle}
          onSelectRow={setSelectedAccountId}
          onUseNow={onUseNow}
          onRefreshAccount={onRefreshAccount}
          onTogglePause={onTogglePause}
          onDelete={onDelete}
          onRefresh={refresh}
          onError={setError}
          stale={!daemonOk}
          disableAdminActions={disableAdminActions}
          vaultMode={isVaultOnly}
        />
      )}
      {route === "activity" && (
        <ActivityPage
          autoSwitch={isVaultOnly ? undefined : autoSwitch}
          onAutoSwitchToggle={isVaultOnly ? undefined : onAutoSwitchToggle}
        />
      )}
      {route === "settings" && (
        <SettingsPage
          autoSwitch={isVaultOnly ? undefined : autoSwitch}
          onAutoSwitchToggle={isVaultOnly ? undefined : onAutoSwitchToggle}
          onAutoSwitchChange={isVaultOnly ? undefined : setAutoSwitch}
          settings={settings}
          onSettingsChange={persistSettings}
          markerState={markerState}
        />
      )}
      {route === "devices" && showAdminNav && (
        <DevicesPage onUnauthorized={onAdminUnauthorized} />
      )}
      {route === "pair" && showAdminNav && (
        <PairPage
          initialCode={
            typeof window === "undefined"
              ? null
              : new URLSearchParams(window.location.search).get("code")
          }
          onUnauthorized={onAdminUnauthorized}
        />
      )}
      {route === "password" && showAdminNav && (
        <PasswordPage onUnauthorized={onAdminUnauthorized} />
      )}
      {showOnboarding && <OnboardingOverlay onDismiss={dismissOnboarding} />}
    </AppShell>
  );
}
