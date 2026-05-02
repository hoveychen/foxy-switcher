import { Icon, BrandMark } from "./Icon";
import {
  ICON_DASHBOARD,
  ICON_USERS,
  ICON_PULSE,
  ICON_GEAR,
  ICON_CHEVRON_RIGHT,
} from "./icons";

export type Route = "dashboard" | "accounts" | "activity" | "settings";

const NAV: Array<{ key: Route; label: string; icon: string }> = [
  { key: "dashboard", label: "Dashboard", icon: ICON_DASHBOARD },
  { key: "accounts", label: "Accounts", icon: ICON_USERS },
  { key: "activity", label: "Activity", icon: ICON_PULSE },
  { key: "settings", label: "Settings", icon: ICON_GEAR },
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
      aria-label="Primary"
    >
      <div className="sidebar-brand">
        <span className="brand-mark sidebar-mark">
          <BrandMark size={16} />
        </span>
        <span className="brand-name">Foxy Switcher</span>
      </div>

      <ul className="sidebar-nav">
        {NAV.map((item) => (
          <li key={item.key}>
            <button
              type="button"
              className={`sidebar-link ${current === item.key ? "active" : ""}`}
              onClick={() => onNavigate(item.key)}
              aria-current={current === item.key ? "page" : undefined}
              title={collapsed ? item.label : undefined}
            >
              <Icon d={item.icon} size={16} />
              <span>{item.label}</span>
            </button>
          </li>
        ))}
      </ul>

      <div className="sidebar-footer">
        <span
          className={`sidebar-health ${daemonOk ? "ok" : "danger"}`}
          aria-label={daemonOk ? "Daemon healthy" : "Daemon unreachable"}
        >
          <span className="dot" />
          <span className="sidebar-health-label">
            {daemonOk ? "Daemon" : "Offline"}
          </span>
        </span>
        <button
          type="button"
          className="sidebar-collapse-btn"
          onClick={onToggleCollapse}
          aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
          aria-pressed={collapsed}
          title={collapsed ? "Expand sidebar" : "Collapse sidebar"}
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
