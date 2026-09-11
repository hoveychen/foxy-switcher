import { useCallback, useEffect, useState } from "react";
import { useIsMobile } from "./useIsMobile";
import { Icon, BrandMark } from "./Icon";
import {
  ICON_DASHBOARD,
  ICON_USERS,
  ICON_PULSE,
  ICON_GEAR,
  ICON_CHEVRON_RIGHT,
  ICON_DEVICE,
  ICON_LINK,
  ICON_KEY,
  ICON_DOTS,
  ICON_POWER,
} from "./icons";
import { t } from "../i18n";

export type Route =
  | "dashboard"
  | "accounts"
  | "activity"
  | "settings"
  | "devices"
  | "pair"
  | "password";

const NAV: Array<{ key: Route; labelKey: string; icon: string }> = [
  { key: "dashboard", labelKey: "nav.dashboard", icon: ICON_DASHBOARD },
  { key: "accounts", labelKey: "nav.accounts", icon: ICON_USERS },
  { key: "activity", labelKey: "nav.activity", icon: ICON_PULSE },
  { key: "settings", labelKey: "nav.settings", icon: ICON_GEAR },
];

// ADMIN_NAV is rendered only when the App runs in a browser at the
// vault server origin and the user is signed in as admin. Tauri desktop
// hides this section entirely — admin actions live on the vault, not
// on the local agent (see vault-app-admin-merge in TASKS.md).
const ADMIN_NAV: Array<{ key: Route; labelKey: string; icon: string }> = [
  { key: "devices", labelKey: "admin.nav.devices", icon: ICON_DEVICE },
  { key: "pair", labelKey: "admin.nav.pair", icon: ICON_LINK },
  { key: "password", labelKey: "admin.nav.password", icon: ICON_KEY },
];

export function Sidebar({
  current,
  onNavigate,
  daemonOk,
  collapsed,
  onToggleCollapse,
  showAdminNav,
  onLogout,
  hideDaemonStatus,
}: {
  current: Route;
  onNavigate: (r: Route) => void;
  daemonOk: boolean;
  collapsed: boolean;
  onToggleCollapse: () => void;
  showAdminNav?: boolean;
  onLogout?: () => void;
  // hideDaemonStatus: when true (vault-only mode), the bottom-left
  // "守护进程" badge is suppressed because there is no local agent
  // process to monitor — the browser is talking to a remote vault.
  hideDaemonStatus?: boolean;
}) {
  const isMobile = useIsMobile();
  const [moreOpen, setMoreOpen] = useState(false);
  const closeMore = useCallback(() => setMoreOpen(false), []);

  // Leaving the phone breakpoint (rotation, desktop resize) must drop the
  // sheet — otherwise it stays pinned over a layout that already shows
  // every admin link in the rail.
  useEffect(() => {
    if (!isMobile) setMoreOpen(false);
  }, [isMobile]);

  useEffect(() => {
    if (!moreOpen) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setMoreOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [moreOpen]);

  const adminRoute =
    current === "devices" || current === "pair" || current === "password";
  const showMoreTab = isMobile && !!showAdminNav;

  const navItem = (item: { key: Route; labelKey: string; icon: string }) => {
    const label = t(item.labelKey);
    return (
      <li key={item.key}>
        <button
          type="button"
          className={`sidebar-link ${current === item.key ? "active" : ""}`}
          onClick={() => {
            setMoreOpen(false);
            onNavigate(item.key);
          }}
          aria-current={current === item.key ? "page" : undefined}
          title={collapsed ? label : undefined}
        >
          <Icon d={item.icon} size={16} />
          <span>{label}</span>
        </button>
      </li>
    );
  };

  return (
    <>
      <nav
        className={`sidebar ${collapsed ? "collapsed" : ""}`}
        aria-label={t("sidebar.aria")}
      >
        <div className="sidebar-brand">
          <span className="sidebar-mark">
            <BrandMark size={28} />
          </span>
          <span className="brand-name">Foxy Switcher</span>
        </div>

        <ul className="sidebar-nav">
          {NAV.map(navItem)}
          {showMoreTab ? (
            <li>
              <button
                type="button"
                className={`sidebar-link ${adminRoute || moreOpen ? "active" : ""}`}
                onClick={() => setMoreOpen((v) => !v)}
                aria-expanded={moreOpen}
                aria-haspopup="menu"
              >
                <Icon d={ICON_DOTS} size={16} />
                <span>{t("nav.more")}</span>
              </button>
            </li>
          ) : (
            showAdminNav && (
              <>
                <li className="sidebar-section-label" aria-hidden>
                  <span>{t("sidebar.admin.section")}</span>
                </li>
                {ADMIN_NAV.map(navItem)}
                {onLogout && (
                  <li>
                    <button
                      type="button"
                      className="sidebar-link sidebar-link-logout"
                      onClick={onLogout}
                      title={collapsed ? t("admin.nav.logout") : undefined}
                    >
                      <Icon d={ICON_POWER} size={16} />
                      <span>{t("admin.nav.logout")}</span>
                    </button>
                  </li>
                )}
              </>
            )
          )}
        </ul>

        <div className="sidebar-footer">
          {!hideDaemonStatus && (
            <span
              className={`sidebar-health ${daemonOk ? "ok" : "danger"}`}
              aria-label={daemonOk ? t("sidebar.health.ok") : t("sidebar.health.down")}
            >
              <span className="dot" />
              <span className="sidebar-health-label">
                {daemonOk ? t("sidebar.health.label.ok") : t("sidebar.health.label.down")}
              </span>
            </span>
          )}
          <button
            type="button"
            className="sidebar-collapse-btn"
            onClick={onToggleCollapse}
            aria-label={collapsed ? t("sidebar.expand") : t("sidebar.collapse")}
            aria-pressed={collapsed}
            title={collapsed ? t("sidebar.expand") : t("sidebar.collapse")}
          >
            <Icon
              d={ICON_CHEVRON_RIGHT}
              size={14}
              className={`sidebar-collapse-icon ${collapsed ? "" : "flipped"}`}
            />
          </button>
        </div>
      </nav>

      {showMoreTab && moreOpen && (
        <>
          <div className="nav-sheet-backdrop" onClick={closeMore} />
          <div className="nav-sheet" role="menu" aria-label={t("admin.nav.aria")}>
            <div className="nav-sheet-title">{t("sidebar.admin.section")}</div>
            {ADMIN_NAV.map((item) => (
              <button
                key={item.key}
                type="button"
                role="menuitem"
                className={`nav-sheet-item ${current === item.key ? "active" : ""}`}
                onClick={() => {
                  setMoreOpen(false);
                  onNavigate(item.key);
                }}
              >
                <Icon d={item.icon} size={16} />
                <span>{t(item.labelKey)}</span>
              </button>
            ))}
            {onLogout && (
              <button
                type="button"
                role="menuitem"
                className="nav-sheet-item nav-sheet-item-danger"
                onClick={() => {
                  setMoreOpen(false);
                  onLogout();
                }}
              >
                <Icon d={ICON_POWER} size={16} />
                <span>{t("admin.nav.logout")}</span>
              </button>
            )}
          </div>
        </>
      )}
    </>
  );
}
