import { useCallback, useEffect, useState } from "react";
import type { ReactNode } from "react";
import { Sidebar, type Route } from "./Sidebar";
import { Icon } from "./Icon";
import {
  ICON_WIN_MINIMIZE,
  ICON_WIN_MAXIMIZE,
  ICON_WIN_RESTORE,
  ICON_X,
} from "./icons";

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

// Windows-only caption-button actions. Each lazily imports the window
// module so browser builds don't pull it in; close goes through the
// existing CloseRequested -> hide-to-tray path in lib.rs.
async function captionMinimize() {
  try {
    const { getCurrentWindow } = await import("@tauri-apps/api/window");
    await getCurrentWindow().minimize();
  } catch {
    /* no-op outside Tauri */
  }
}

async function captionToggleMaximize() {
  try {
    const { getCurrentWindow } = await import("@tauri-apps/api/window");
    await getCurrentWindow().toggleMaximize();
  } catch {
    /* no-op outside Tauri */
  }
}

async function captionClose() {
  try {
    const { getCurrentWindow } = await import("@tauri-apps/api/window");
    await getCurrentWindow().close();
  } catch {
    /* no-op outside Tauri */
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
  const [maximized, setMaximized] = useState(false);

  useEffect(() => {
    try {
      window.localStorage.setItem(LS_COLLAPSED_KEY, collapsed ? "1" : "0");
    } catch {
      // ignore: persistence is best-effort
    }
  }, [collapsed]);

  // Keep the max / restore caption-button glyph in sync with the actual
  // window state. Resize fires for both user drags and toggleMaximize;
  // we also seed once on mount for the case where the window opens
  // maximized. Listener is no-op in browser mode (window module is
  // lazy-loaded and isMaximized throws there).
  useEffect(() => {
    let unlisten: (() => void) | undefined;
    let cancelled = false;
    void (async () => {
      try {
        const { getCurrentWindow } = await import("@tauri-apps/api/window");
        const w = getCurrentWindow();
        const seed = await w.isMaximized();
        if (!cancelled) setMaximized(seed);
        unlisten = await w.onResized(async () => {
          try {
            const m = await w.isMaximized();
            if (!cancelled) setMaximized(m);
          } catch {
            /* ignore transient errors */
          }
        });
      } catch {
        /* browser mode — caption buttons are hidden anyway */
      }
    })();
    return () => {
      cancelled = true;
      unlisten?.();
    };
  }, []);

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
      {/* Windows caption buttons (min / max / close). Sit in the top-right
          inside the 28px drag strip; CSS gates visibility on
          html.tauri-host.os-windows so macOS keeps its native traffic
          lights. Each button is no-drag and z-index 200 so the OS
          treats clicks as clicks, not as drag handoff. Close goes
          through CloseRequested -> hide-to-tray (lib.rs) so the sidecar
          stays alive; the tray menu still owns real quit. */}
      <div className="tauri-caption-buttons" aria-hidden={false}>
        <button
          type="button"
          className="tauri-caption-btn tauri-caption-min"
          onClick={captionMinimize}
          aria-label="Minimize"
          title="Minimize"
        >
          <Icon d={ICON_WIN_MINIMIZE} size={10} strokeWidth={1} />
        </button>
        <button
          type="button"
          className="tauri-caption-btn tauri-caption-max"
          onClick={captionToggleMaximize}
          aria-label={maximized ? "Restore" : "Maximize"}
          title={maximized ? "Restore" : "Maximize"}
        >
          <Icon
            d={maximized ? ICON_WIN_RESTORE : ICON_WIN_MAXIMIZE}
            size={10}
            strokeWidth={1}
          />
        </button>
        <button
          type="button"
          className="tauri-caption-btn tauri-caption-close"
          onClick={captionClose}
          aria-label="Close"
          title="Close"
        >
          <Icon d={ICON_X} size={10} strokeWidth={1} />
        </button>
      </div>
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
