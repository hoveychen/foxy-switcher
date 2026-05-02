import { useEffect } from "react";
import type { ReactNode } from "react";
import { Icon } from "./Icon";
import { ICON_X } from "./icons";

export function Modal({
  open,
  title,
  subtitle,
  onClose,
  onRequestClose,
  children,
  footer,
  size = "md",
}: {
  open: boolean;
  title: string;
  subtitle?: string;
  onClose: () => void;
  onRequestClose?: () => void;
  children: ReactNode;
  footer?: ReactNode;
  size?: "md" | "lg";
}) {
  const requestClose = onRequestClose ?? onClose;

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.stopPropagation();
        requestClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [open, requestClose]);

  if (!open) return null;

  return (
    <div className="modal-backdrop" onClick={requestClose} role="presentation">
      <div
        className={`modal modal-${size}`}
        role="dialog"
        aria-modal="true"
        aria-label={title}
        onClick={(e) => e.stopPropagation()}
      >
        <header className="modal-header">
          <div className="modal-header-text">
            <h2 className="modal-title">{title}</h2>
            {subtitle && <p className="modal-subtitle">{subtitle}</p>}
          </div>
          <button
            type="button"
            className="btn-icon"
            aria-label="Close"
            onClick={requestClose}
          >
            <Icon d={ICON_X} />
          </button>
        </header>
        <div className="modal-body">{children}</div>
        {footer && <footer className="modal-footer">{footer}</footer>}
      </div>
    </div>
  );
}
