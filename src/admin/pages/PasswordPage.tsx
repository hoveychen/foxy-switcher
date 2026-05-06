import { FormEvent, useState } from "react";
import { adminApi, AdminApiError } from "../api";
import { t } from "../../i18n";

interface Props {
  onUnauthorized: () => void;
}

export function PasswordPage({ onUnauthorized }: Props) {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [success, setSuccess] = useState(false);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!next || next !== confirm) {
      setError(t("admin.password.error.mismatch"));
      return;
    }
    setError(null);
    setSuccess(false);
    setBusy(true);
    try {
      await adminApi.changePassword(current, next, confirm);
      setSuccess(true);
      setCurrent("");
      setNext("");
      setConfirm("");
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        if (err.code === "current password is wrong") {
          setError(t("admin.password.error.wrong"));
        } else {
          onUnauthorized();
          return;
        }
      } else {
        setError(t("admin.password.error.generic"));
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="admin-content">
      <div className="admin-page" style={{ maxWidth: 460 }}>
        <div>
          <h1 className="admin-page__title">{t("admin.password.title")}</h1>
          <p className="admin-page__subtitle">{t("admin.password.subtitle")}</p>
        </div>
        <form className="admin-card admin-form" onSubmit={onSubmit}>
          <label className="admin-form__field">
            <span className="admin-form__label">
              {t("admin.password.current")}
            </span>
            <input
              className="admin-input"
              type="password"
              autoFocus
              required
              value={current}
              onChange={(e) => setCurrent(e.target.value)}
              autoComplete="current-password"
            />
          </label>
          <label className="admin-form__field">
            <span className="admin-form__label">
              {t("admin.password.next")}
            </span>
            <input
              className="admin-input"
              type="password"
              required
              value={next}
              onChange={(e) => setNext(e.target.value)}
              autoComplete="new-password"
            />
          </label>
          <label className="admin-form__field">
            <span className="admin-form__label">
              {t("admin.password.confirm")}
            </span>
            <input
              className="admin-input"
              type="password"
              required
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
            />
          </label>
          {error && <p className="admin-alert admin-alert--error">{error}</p>}
          {success && (
            <p className="admin-alert admin-alert--success">
              {t("admin.password.success")}
            </p>
          )}
          <div className="admin-actions">
            <button
              type="submit"
              className="admin-button admin-button--primary"
              disabled={busy || !current || !next || !confirm}
              aria-busy={busy}
            >
              {busy ? t("admin.password.updating") : t("admin.password.submit")}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
