import { useCallback, useEffect, useRef, useState } from "react";
import { ask } from "@tauri-apps/plugin-dialog";
import {
  Account,
  AutoSwitchSettings,
  ThresholdInput,
  apiClient,
} from "./api";
import { AppShell } from "./components/AppShell";
import { AccountDrawer } from "./components/AccountDrawer";
import type { Route } from "./components/Sidebar";
import { DashboardPage } from "./pages/DashboardPage";
import { AccountsPage } from "./pages/AccountsPage";
import { ActivityPage } from "./pages/ActivityPage";
import { SettingsPage } from "./pages/SettingsPage";

const ROUTE_KEYS: Route[] = ["dashboard", "accounts", "activity", "settings"];

export default function App() {
  const [route, setRoute] = useState<Route>("accounts");
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [managedAccountId, setManagedAccountId] = useState<number>(0);
  const [now, setNow] = useState<number>(Date.now());
  const [error, setError] = useState<string | null>(null);
  const [daemonOk, setDaemonOk] = useState<boolean>(true);
  const [selectedAccountId, setSelectedAccountId] = useState<number | null>(
    null,
  );
  const [busyAccountId, setBusyAccountId] = useState<number | null>(null);
  const [autoSwitch, setAutoSwitchState] = useState<AutoSwitchSettings>({
    enabled: true,
    policy: "lru",
  });
  const [addAccountTick, setAddAccountTick] = useState(0);

  const refresh = useCallback(async () => {
    try {
      const [list, cred] = await Promise.all([
        apiClient.listAccounts(),
        apiClient.credStatus(),
      ]);
      setAccounts(list);
      setManagedAccountId(cred.managed_account_id);
      setDaemonOk(true);
      setError(null);
    } catch (e) {
      setDaemonOk(false);
      setError(String(e));
    }
  }, []);

  // One-shot: hydrate auto-switch from the daemon. The 5s refresh loop doesn't
  // re-fetch this — toggles are driver, not driven, so we only resync on explicit
  // user action (or after a write echoes back the persisted state).
  useEffect(() => {
    apiClient
      .getAutoSwitch()
      .then((v) => setAutoSwitchState(v))
      .catch(() => {
        // Daemon not up yet or older daemon — keep optimistic defaults.
      });
  }, []);

  const persistAutoSwitch = useCallback(async (next: AutoSwitchSettings) => {
    setAutoSwitchState(next); // optimistic
    try {
      const echoed = await apiClient.setAutoSwitch(next);
      setAutoSwitchState(echoed);
    } catch (e) {
      setError(String(e));
      // Re-pull authoritative state so the toggle visually reverts on failure.
      apiClient.getAutoSwitch().then(setAutoSwitchState).catch(() => {});
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
      const ok = await ask("Delete this account from the pool?", {
        title: "Confirm delete",
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

  const triggerAddAccount = useCallback(() => {
    setRoute("accounts");
    setAddAccountTick((t) => t + 1);
  }, []);

  const selectedAccount =
    selectedAccountId !== null
      ? accounts.find((a) => a.id === selectedAccountId) ?? null
      : null;

  const onNavigate = useCallback((r: Route) => {
    setRoute(r);
    setSelectedAccountId(null);
  }, []);

  // Global keyboard shortcuts (PRD §4.3)
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const mod = e.metaKey || e.ctrlKey;
      if (!mod) return;
      const target = e.target as HTMLElement | null;
      // Allow native Cmd-A/C/V/X/Z inside text inputs.
      if (
        target &&
        (target.tagName === "INPUT" ||
          target.tagName === "TEXTAREA" ||
          target.isContentEditable)
      ) {
        if (!(e.key === "," || (e.key >= "1" && e.key <= "4"))) return;
      }
      switch (e.key) {
        case "n":
        case "N":
          e.preventDefault();
          triggerAddAccount();
          return;
        case ",":
          e.preventDefault();
          setRoute("settings");
          setSelectedAccountId(null);
          return;
        case "r":
        case "R":
          e.preventDefault();
          refresh();
          return;
      }
      if (e.key >= "1" && e.key <= "4") {
        e.preventDefault();
        const idx = Number(e.key) - 1;
        setRoute(ROUTE_KEYS[idx]);
        setSelectedAccountId(null);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [triggerAddAccount, refresh]);

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
      {error && (
        <div className="banner err">
          <span>{error}</span>
          <button className="btn btn-ghost" onClick={() => setError(null)}>
            Dismiss
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
          autoSwitch={autoSwitch}
          onAutoSwitchToggle={onAutoSwitchToggle}
          onSelectRow={setSelectedAccountId}
          onUseNow={onUseNow}
          onRefreshAccount={onRefreshAccount}
          onTogglePause={onTogglePause}
          onDelete={onDelete}
          onRefresh={refresh}
          onError={setError}
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
        />
      )}
    </AppShell>
  );
}
