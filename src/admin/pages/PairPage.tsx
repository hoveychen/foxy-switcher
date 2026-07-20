import { FormEvent, useEffect, useState } from "react";
import { adminApi, AdminApiError, AdminPairLookup } from "../api";
import { t } from "../../i18n";

interface Props {
  initialCode: string | null;
  onUnauthorized: () => void;
}

type ResolveResult = "approved" | "denied";

export function PairPage({ initialCode, onUnauthorized }: Props) {
  const [code, setCode] = useState(initialCode ?? "");
  const [lookup, setLookup] = useState<AdminPairLookup | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [resolved, setResolved] = useState<ResolveResult | null>(null);
  // Provider allowlist for this device, chosen at approval. Default matches
  // the backend: Claude on, Codex off.
  const [allowClaude, setAllowClaude] = useState(true);
  const [allowCodex, setAllowCodex] = useState(false);

  // Auto-lookup if URL came in with ?code=…; user can also type one.
  useEffect(() => {
    if (initialCode) {
      void doLookup(initialCode);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function doLookup(c: string) {
    setError(null);
    setLookup(null);
    setBusy(true);
    try {
      const r = await adminApi.lookupPair(c.trim());
      setLookup(r);
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      if (err instanceof AdminApiError && err.status === 404) {
        setError(t("admin.pair.error.not_found"));
      } else {
        setError(t("admin.pair.error.generic"));
      }
    } finally {
      setBusy(false);
    }
  }

  async function onLookupSubmit(e: FormEvent) {
    e.preventDefault();
    if (!code.trim()) return;
    void doLookup(code);
  }

  async function resolve(action: "approve" | "deny") {
    if (!lookup) return;
    setBusy(true);
    setError(null);
    try {
      const r = await adminApi.resolvePair(
        lookup.code,
        action,
        action === "approve"
          ? { allow_claude: allowClaude, allow_codex: allowCodex }
          : undefined,
      );
      setResolved(r.result);
    } catch (err) {
      if (err instanceof AdminApiError && err.status === 401) {
        onUnauthorized();
        return;
      }
      if (err instanceof AdminApiError && err.status === 404) {
        setError(t("admin.pair.error.expired"));
      } else {
        setError(t("admin.pair.error.generic"));
      }
    } finally {
      setBusy(false);
    }
  }

  function reset() {
    setLookup(null);
    setCode("");
    setResolved(null);
    setError(null);
  }

  if (resolved) {
    return (
      <div className="admin-content">
        <div className="admin-page" style={{ maxWidth: 460 }}>
          <h1 className="admin-page__title">{t("admin.pair.title")}</h1>
          <div className="admin-card">
            <p
              className={
                resolved === "approved"
                  ? "admin-alert admin-alert--success"
                  : "admin-alert admin-alert--info"
              }
            >
              {resolved === "approved"
                ? t("admin.pair.result.approved")
                : t("admin.pair.result.denied")}
            </p>
            <div className="admin-actions">
              <button type="button" className="admin-button" onClick={reset}>
                {t("admin.pair.pair_another")}
              </button>
              <a className="admin-button" href="/admin/devices">
                {t("admin.pair.back_to_devices")}
              </a>
            </div>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="admin-content">
      <div className="admin-page" style={{ maxWidth: 460 }}>
        <div>
          <h1 className="admin-page__title">{t("admin.pair.title")}</h1>
          <p className="admin-page__subtitle">{t("admin.pair.subtitle")}</p>
        </div>
        {!lookup ? (
          <form className="admin-card admin-form" onSubmit={onLookupSubmit}>
            <label className="admin-form__field">
              <span className="admin-form__label">{t("admin.pair.code")}</span>
              <input
                className="admin-input"
                type="text"
                autoFocus
                required
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="ABCD-1234"
                autoCapitalize="characters"
              />
            </label>
            {error && <p className="admin-alert admin-alert--error">{error}</p>}
            <div className="admin-actions">
              <button
                type="submit"
                className="admin-button admin-button--primary"
                disabled={busy || !code.trim()}
                aria-busy={busy}
              >
                {busy ? t("admin.pair.looking_up") : t("admin.pair.lookup")}
              </button>
            </div>
          </form>
        ) : (
          <div className="admin-card">
            <p className="admin-page__subtitle">
              {t("admin.pair.confirm_intro")}{" "}
              <strong>{lookup.device_name}</strong>{" "}
              {t("admin.pair.confirm_using_code")}{" "}
              <code>{lookup.code}</code>.
            </p>
            <p className="admin-page__subtitle">
              {t("admin.pair.confirm_warning")}
            </p>
            <fieldset className="admin-providers">
              <legend>{t("admin.pair.providers_label")}</legend>
              <label className="admin-checkbox">
                <input
                  type="checkbox"
                  checked={allowClaude}
                  onChange={(e) => setAllowClaude(e.target.checked)}
                  disabled={busy}
                />
                {t("admin.pair.provider_claude")}
              </label>
              <label className="admin-checkbox">
                <input
                  type="checkbox"
                  checked={allowCodex}
                  onChange={(e) => setAllowCodex(e.target.checked)}
                  disabled={busy}
                />
                {t("admin.pair.provider_codex")}
              </label>
            </fieldset>
            {error && <p className="admin-alert admin-alert--error">{error}</p>}
            <div className="admin-actions">
              <button
                type="button"
                className="admin-button admin-button--primary"
                onClick={() => resolve("approve")}
                disabled={busy || (!allowClaude && !allowCodex)}
                aria-busy={busy}
              >
                {t("admin.pair.approve")}
              </button>
              <button
                type="button"
                className="admin-button admin-button--danger"
                onClick={() => resolve("deny")}
                disabled={busy}
                aria-busy={busy}
              >
                {t("admin.pair.deny")}
              </button>
              <button
                type="button"
                className="admin-button"
                onClick={reset}
                disabled={busy}
              >
                {t("admin.pair.cancel")}
              </button>
            </div>
          </div>
        )}
      </div>
    </div>
  );
}
