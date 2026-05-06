import { FormEvent, useState } from "react";
import { adminApi, AdminApiError } from "../api";
import { t } from "../../i18n";

interface Props {
  next?: string | null;
  onSignedIn: () => void;
}

export function LoginPage({ next, onSignedIn }: Props) {
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function onSubmit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await adminApi.login(password);
      onSignedIn();
    } catch (err) {
      const code = err instanceof AdminApiError ? err.code : String(err);
      setError(
        code === "wrong password"
          ? t("admin.login.error.wrong")
          : t("admin.login.error.generic"),
      );
      setBusy(false);
    }
  }

  return (
    <div className="admin-content">
      <div className="admin-page" style={{ maxWidth: 420 }}>
        <div>
          <h1 className="admin-page__title">{t("admin.login.title")}</h1>
          <p className="admin-page__subtitle">
            {next
              ? t("admin.login.subtitle.with_next")
              : t("admin.login.subtitle")}
          </p>
        </div>
        <form className="admin-card admin-form" onSubmit={onSubmit}>
          <label className="admin-form__field">
            <span className="admin-form__label">
              {t("admin.login.password")}
            </span>
            <input
              className="admin-input"
              type="password"
              autoFocus
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>
          {error && <p className="admin-alert admin-alert--error">{error}</p>}
          <div className="admin-actions">
            <button
              type="submit"
              className="admin-button admin-button--primary"
              disabled={busy || !password}
              aria-busy={busy}
            >
              {busy ? t("admin.login.signing_in") : t("admin.login.submit")}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}
