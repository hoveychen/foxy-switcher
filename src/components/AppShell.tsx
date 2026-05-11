import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Sidebar, type Route } from "./Sidebar";

const LS_COLLAPSED_KEY = "fx.sidebar.collapsed";

// startResize hands the mouse off to Tauri so the OS can drive the
// resize loop natively. Only fired by the Windows-only resize handles
// (shell.css gates visibility on html.tauri-host.os-windows), but we
// still guard against import-time evaluation in browser mode by
// loading the window module lazily.
type ResizeDirection =
  | "East"
  | "North"
  | "NorthEast"
  | "NorthWest"
  | "South"
  | "SouthEast"
  | "SouthWest"
  | "West";

async function startResize(direction: ResizeDirection) {
  try {
    const { getCurrentWindow } = await import("@tauri-apps/api/window");
    await getCurrentWindow().startResizeDragging(direction);
  } catch {
    // Browser mode (vault Web UI) — handles aren't visible there, so
    // this only runs if someone toggles the CSS class. Fail silently.
  }
}

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
    <>
      {/* Frameless top drag strip + Windows resize handles.
          They MUST be siblings of `.app-shell`, not nested inside it.
          The grid container holds `.sidebar` (position:sticky;
          height:100vh) which establishes a stacking context covering
          the left 220×100vh — including the top-left of the drag
          strip. With the bar nested inside the same grid, hit-testing
          for the top-left 220×28px lands on the sticky sidebar and
          `-webkit-app-region: drag` is never read (the window simply
          won't drag, even though the bar paints fine).
          netferry / claude-fleet both hoist their drag bar to a root
          sibling for exactly this reason. */}
      <div className="tauri-titlebar" data-tauri-drag-region />
      {/* Windows resize handles — same reasoning. CSS gates visibility
          on html.tauri-host.os-windows; on macOS the OS chrome already
          provides resize, so they stay hidden there. mousedown hands
          the drag loop off to Tauri's startResizeDragging so the user
          gets the native cursor + behavior. */}
      <div
        className="tauri-resize tauri-resize-n"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("North");
        }}
      />
      <div
        className="tauri-resize tauri-resize-s"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("South");
        }}
      />
      <div
        className="tauri-resize tauri-resize-w"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("West");
        }}
      />
      <div
        className="tauri-resize tauri-resize-e"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("East");
        }}
      />
      <div
        className="tauri-resize tauri-resize-nw"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("NorthWest");
        }}
      />
      <div
        className="tauri-resize tauri-resize-ne"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("NorthEast");
        }}
      />
      <div
        className="tauri-resize tauri-resize-sw"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("SouthWest");
        }}
      />
      <div
        className="tauri-resize tauri-resize-se"
        onMouseDown={(e) => {
          e.preventDefault();
          void startResize("SouthEast");
        }}
      />
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
    </>
  );
}
