// Bootstrap surface for unauthenticated visitors to the cloud vault.
// After vault-app-admin-merge this root only handles two pages:
//   - /admin/setup — first-run password creation (vault has no password)
//   - /admin/login — sign-in for an existing vault
//
// Everything else (devices/pair/password + the App's own pages) is
// served by App.tsx at top-level paths once the visitor is signed in.
// SetupPage / LoginPage success → window.location.assign("/") so the
// next paint is the merged App shell with the admin sidebar items
// visible.

import { useEffect, useState } from "react";
import { adminApi, AdminApiError, AdminMe } from "./api";
import { LoginPage } from "./pages/LoginPage";
import { SetupPage } from "./pages/SetupPage";
import { t } from "../i18n";
import "./admin.css";

function defaultPostAuthTarget(query: URLSearchParams): string {
  const next = query.get("next");
  // Only follow same-origin paths to avoid open-redirects. Anything
  // else falls back to the App root.
  if (next && next.startsWith("/") && !next.startsWith("//")) {
    return next;
  }
  return "/";
}

export function AdminApp() {
  const [me, setMe] = useState<AdminMe | null>(null);
  const [bootError, setBootError] = useState<string | null>(null);

  // Bootstrap: who are we?
  useEffect(() => {
    void refreshMe();
  }, []);

  async function refreshMe() {
    try {
      const r = await adminApi.me();
      setMe(r);
      setBootError(null);
    } catch (err) {
      const code = err instanceof AdminApiError ? err.code : String(err);
      setBootError(code);
    }
  }

  if (bootError) {
    return (
      <main className="admin-loading">
        <div className="admin-card" style={{ maxWidth: 360 }}>
          <h2 className="admin-page__title">{t("admin.boot.error.title")}</h2>
          <p className="admin-page__subtitle">{t("admin.boot.error.body")}</p>
          <code style={{ font: "var(--font-mono)" }}>{bootError}</code>
          <div className="admin-actions">
            <button
              type="button"
              className="admin-button admin-button--primary"
              onClick={() => {
                setBootError(null);
                void refreshMe();
              }}
            >
              {t("admin.boot.retry")}
            </button>
          </div>
        </div>
      </main>
    );
  }

  if (!me) {
    return (
      <main className="admin-loading">
        <p>{t("admin.common.loading")}</p>
      </main>
    );
  }

  // First run: vault has no password yet → render setup. The router on
  // the server side ensures this URL is what fresh visitors land on.
  if (!me.has_password) {
    return (
      <SetupPage
        onDone={() => {
          // Setup auto-signs the admin in; jump straight into the App.
          window.location.assign("/");
        }}
      />
    );
  }

  // Already signed in but somehow on /admin/setup or /admin/login —
  // bounce back to the App root so the shell takes over.
  if (me.signed_in) {
    if (typeof window !== "undefined") {
      window.location.replace("/");
    }
    return (
      <main className="admin-loading">
        <p>{t("admin.common.loading")}</p>
      </main>
    );
  }

  // Signed-out path: render login. Post-login, route to ?next= if the
  // server supplied one (from /admin/login?next=…), otherwise the App
  // root.
  const query =
    typeof window === "undefined"
      ? new URLSearchParams()
      : new URLSearchParams(window.location.search);
  return (
    <LoginPage
      next={query.get("next")}
      onSignedIn={() => {
        window.location.assign(defaultPostAuthTarget(query));
      }}
    />
  );
}
