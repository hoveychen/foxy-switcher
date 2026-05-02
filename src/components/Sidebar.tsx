import { Icon, BrandMark } from "./Icon";
import {
  ICON_DASHBOARD,
  ICON_USERS,
  ICON_PULSE,
  ICON_GEAR,
  ICON_CHEVRON_RIGHT,
} from "./icons";
import { t } from "../i18n";

export type Route = "dashboard" | "accounts" | "activity" | "settings";

const NAV: Array<{ key: Route; labelKey: string; icon: string }> = [
  { key: "dashboard", labelKey: "nav.dashboard", icon: ICON_DASHBOARD },
  { key: "accounts", labelKey: "nav.accounts", icon: ICON_USERS },
  { key: "activity", labelKey: "nav.activity", icon: ICON_PULSE },
  { key: "settings", labelKey: "nav.settings", icon: ICON_GEAR },
];

export function Sidebar({
  current,
  onNavigate,
  daemonOk,
  collapsed,
  onToggleCollapse,
}: {
  current: Route;
  onNavigate: (r: Route) => void;
  daemonOk: boolean;
  collapsed: boolean;
  onToggleCollapse: () => void;
}) {
  return (
    <nav
      className={`sidebar ${collapsed ? "collapsed" : ""}`}
      aria-label={t("sidebar.aria")}
    >
      <div className="sidebar-brand">
        <span className="brand-mark sidebar-mark">
          <BrandMark size={16} />
        </span>
        <span className="brand-name">Foxy Switcher</span>
      </div>

      <ul className="sidebar-nav">
        {NAV.map((item) => {
          const label = t(item.labelKey);
          return (
            <li key={item.key}>
              <button
                type="button"
                className={`sidebar-link ${current === item.key ? "active" : ""}`}
                onClick={() => onNavigate(item.key)}
                aria-current={current === item.key ? "page" : undefined}
                title={collapsed ? label : undefined}
              >
                <Icon d={item.icon} size={16} />
                <span>{label}</span>
              </button>
            </li>
          );
        })}
      </ul>

      <div className="sidebar-footer">
        <span
          className={`sidebar-health ${daemonOk ? "ok" : "danger"}`}
          aria-label={daemonOk ? t("sidebar.health.ok") : t("sidebar.health.down")}
        >
          <span className="dot" />
          <span className="sidebar-health-label">
            {daemonOk ? t("sidebar.health.label.ok") : t("sidebar.health.label.down")}
          </span>
        </span>
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
  );
}
