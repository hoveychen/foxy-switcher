import { useEffect } from "react";
import type { ReactNode } from "react";
import { Icon } from "./Icon";
import { ICON_X } from "./icons";
import { t } from "../i18n";

export function Drawer({
  open,
  title,
  onClose,
  children,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <aside className="drawer" role="dialog" aria-label={title}>
      <header className="drawer-header">
        <h2 className="drawer-title">{title}</h2>
        <button
          type="button"
          className="btn-icon"
          aria-label={t("drawer.close")}
          onClick={onClose}
        >
          <Icon d={ICON_X} />
        </button>
      </header>
      <div className="drawer-body">{children}</div>
    </aside>
  );
}
