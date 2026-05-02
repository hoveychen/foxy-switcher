import type { ReactNode } from "react";
import { t } from "../i18n";

type AutoSwitchValue = {
  enabled: boolean;
  policy: "lru" | "lowest" | "rr";
};

export function Topbar({
  title,
  status,
  actions,
  autoSwitch,
  onAutoSwitchToggle,
}: {
  title: string;
  status?: { label: string; tone: "ok" | "muted" | "warn" | "danger" };
  actions?: ReactNode;
  autoSwitch?: AutoSwitchValue;
  onAutoSwitchToggle?: () => void;
}) {
  const manual = autoSwitch && !autoSwitch.enabled;
  return (
    <header className="topbar">
      <div className="topbar-left">
        <h1 className="topbar-title">{title}</h1>
        {manual && (
          <span className="topbar-manual" title={t("topbar.manual.title")}>
            <span className="dot" />
            {t("topbar.manual.label")}
          </span>
        )}
        {status && (
          <span className={`topbar-status ${status.tone}`}>
            <span className="dot" />
            {status.label}
          </span>
        )}
      </div>
      <div className="topbar-actions">
        {actions}
        {autoSwitch && onAutoSwitchToggle && (
          <button
            type="button"
            role="switch"
            aria-checked={autoSwitch.enabled}
            aria-label={t("topbar.auto.aria")}
            className={`topbar-auto ${autoSwitch.enabled ? "on" : "off"}`}
            onClick={onAutoSwitchToggle}
            title={
              autoSwitch.enabled
                ? t("topbar.auto.title.on")
                : t("topbar.auto.title.off")
            }
          >
            <span className="topbar-auto-label">{t("topbar.auto.label")}</span>
            <span className={`toggle ${autoSwitch.enabled ? "on" : "off"}`}>
              <span className="toggle-thumb" aria-hidden />
            </span>
          </button>
        )}
      </div>
    </header>
  );
}
