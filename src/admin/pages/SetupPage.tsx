import { FormEvent, useState } from "react";
import { adminApi, AdminApiError } from "../api";
import { t } from "../../i18n";

interface Props {
  onDone: () => void;
}

export function SetupPage({ onDone }: Props) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    if (!password || password !== confirm) {
      setError(t("admin.setup.error.mismatch"));
      return;
    }
    setError(null);
    setBusy(true);
    try {
      await adminApi.setup(password, confirm);
      onDone();
    } catch (err) {
      const code = err instanceof AdminApiError ? err.code : String(err);
      setError(
        code === "already_set_up"
          ? t("admin.setup.error.already")
          : t("admin.setup.error.generic"),
      );
      setBusy(false);
    }
  }

  return (
    <div className="admin-content">
      <div className="admin-page" style={{ maxWidth: 460 }}>
        <div>
          <h1 className="admin-page__title">{t("admin.setup.title")}</h1>
          <p className="admin-page__subtitle">{t("admin.setup.subtitle")}</p>
        </div>
        <form className="admin-card admin-form" onSubmit={onSubmit}>
          <label className="admin-form__field">
            <span className="admin-form__label">
              {t("admin.setup.password")}
            </span>
            <input
              className="admin-input"
              type="password"
              autoFocus
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
            />
          </label>
          <label className="admin-form__field">
            <span className="admin-form__label">
              {t("admin.setup.confirm")}
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
          <div className="admin-actions">
            <button
              type="submit"
              className="admin-button admin-button--primary"
              disabled={busy || !password || !confirm}
              aria-busy={busy}
            >
              {busy ? t("admin.setup.saving") : t("admin.setup.submit")}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
