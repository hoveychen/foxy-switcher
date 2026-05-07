import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Sidebar, type Route } from "./Sidebar";

const LS_COLLAPSED_KEY = "fx.sidebar.collapsed";

function readInitialCollapsed(): boolean {
  try {
    return window.localStorage.getItem(LS_COLLAPSED_KEY) === "1";
  } catch {
    return false;
  }
}

export function AppShell({
  current,
  onNavigate,
  daemonOk,
  children,
  drawer,
  showAdminNav,
  onAdminLogout,
  hideDaemonStatus,
}: {
  current: Route;
  onNavigate: (r: Route) => void;
  daemonOk: boolean;
  children: ReactNode;
  drawer?: ReactNode;
  showAdminNav?: boolean;
  onAdminLogout?: () => void;
  hideDaemonStatus?: boolean;
}) {
  const [collapsed, setCollapsed] = useState<boolean>(readInitialCollapsed);

  useEffect(() => {
    try {
      window.localStorage.setItem(LS_COLLAPSED_KEY, collapsed ? "1" : "0");
    } catch {
      // ignore: persistence is best-effort
    }
  }, [collapsed]);

  const onToggleCollapse = useCallback(() => setCollapsed((v) => !v), []);

  return (
    <div
      className={`app-shell ${drawer ? "has-drawer" : ""} ${
        collapsed ? "sidebar-collapsed" : ""
      }`}
    >
      <Sidebar
        current={current}
        onNavigate={onNavigate}
        daemonOk={daemonOk}
        collapsed={collapsed}
        onToggleCollapse={onToggleCollapse}
        showAdminNav={showAdminNav}
        onLogout={onAdminLogout}
        hideDaemonStatus={hideDaemonStatus}
      />
      <main className="app-main">{children}</main>
      {drawer}
    </div>
  );
}
