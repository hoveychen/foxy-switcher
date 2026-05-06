// Cloud vault admin SPA root. Mounted by main.tsx when window.location.
// pathname starts with /admin. Bootstraps via GET /admin/api/me and
// dispatches to setup → login → devices/pair/password based on auth
// state and the current URL path. Shares LURA tokens (src/styles/*)
// and the i18n module with the desktop App.

import { useCallback, useEffect, useState } from "react";
import { adminApi, AdminApiError, AdminMe } from "./api";
import { LoginPage } from "./pages/LoginPage";
import { SetupPage } from "./pages/SetupPage";
import { DevicesPage } from "./pages/DevicesPage";
import { PairPage } from "./pages/PairPage";
import { PasswordPage } from "./pages/PasswordPage";
import { t } from "../i18n";
import "./admin.css";

type Route = "devices" | "pair" | "password" | "login" | "setup";

const NAV_ROUTES: Route[] = ["devices", "pair", "password"];
const NAV_LABEL: Record<Route, string> = {
  devices: "admin.nav.devices",
  pair: "admin.nav.pair",
  password: "admin.nav.password",
  login: "admin.nav.login",
  setup: "admin.nav.setup",
};

function readPath(): { route: Route; query: URLSearchParams } {
  const path = typeof window === "undefined" ? "/admin/devices" : window.location.pathname;
  const search = typeof window === "undefined" ? "" : window.location.search;
  const query = new URLSearchParams(search);
  if (path.startsWith("/admin/setup")) return { route: "setup", query };
  if (path.startsWith("/admin/login")) return { route: "login", query };
  if (path.startsWith("/admin/pair")) return { route: "pair", query };
  if (path.startsWith("/admin/password")) return { route: "password", query };
  return { route: "devices", query };
}

export function AdminApp() {
  const [{ route, query }, setRouteState] = useState(readPath);
  const [me, setMe] = useState<AdminMe | null>(null);
  const [bootError, setBootError] = useState<string | null>(null);

  // Resync route when the user uses browser back/forward.
  useEffect(() => {
    const onPop = () => setRouteState(readPath());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

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

  const navigate = useCallback((to: Route, qs?: string) => {
    const url = `/admin/${to}` + (qs ? `?${qs}` : "");
    window.history.pushState({}, "", url);
    setRouteState(readPath());
  }, []);

  const onUnauthorized = useCallback(() => {
    setMe((cur) => (cur ? { ...cur, signed_in: false } : cur));
    navigate("login");
  }, [navigate]);

  async function onLogout() {
    try {
      await adminApi.logout();
    } catch {
      // Best-effort: even if the server rejects the call, we still
      // want the UI to forget the session locally.
    }
    setMe((cur) => (cur ? { ...cur, signed_in: false } : cur));
    navigate("login");
  }

  // --- bootstrap states --------------------------------------------------

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

  // --- gates -------------------------------------------------------------

  if (!me.has_password) {
    return (
      <SetupPage
        onDone={() => {
          setMe({ has_password: true, signed_in: true });
          navigate("devices");
        }}
      />
    );
  }

  if (!me.signed_in) {
    return (
      <LoginPage
        next={query.get("next")}
        onSignedIn={() => {
          setMe({ has_password: true, signed_in: true });
          const next = query.get("next");
          if (next && next.startsWith("/admin/")) {
            window.location.assign(next);
          } else {
            navigate("devices");
          }
        }}
      />
    );
  }

  // --- signed-in shell ---------------------------------------------------

  // Treat /admin/login or /admin/setup hits while already signed in as
  // "go home" — the SPA only shows those pages when the gate above
  // selects them.
  const activeRoute: Route =
    route === "login" || route === "setup" ? "devices" : route;

  return (
    <div className="admin-shell">
      <header className="admin-topbar">
        <span className="admin-topbar__brand">{t("admin.brand")}</span>
        <nav className="admin-topbar__nav" aria-label={t("admin.nav.aria")}>
          {NAV_ROUTES.map((r) => (
            <a
              key={r}
              href={`/admin/${r}`}
              className="admin-nav-link"
              aria-current={activeRoute === r ? "page" : undefined}
              onClick={(e) => {
                if (e.metaKey || e.ctrlKey || e.shiftKey || e.button !== 0) {
                  return;
                }
                e.preventDefault();
                navigate(r);
              }}
            >
              {t(NAV_LABEL[r])}
            </a>
          ))}
          <button
            type="button"
            className="admin-nav-link"
            onClick={onLogout}
          >
            {t("admin.nav.logout")}
          </button>
        </nav>
      </header>
      {activeRoute === "devices" && (
        <DevicesPage onUnauthorized={onUnauthorized} />
      )}
      {activeRoute === "pair" && (
        <PairPage
          initialCode={query.get("code")}
          onUnauthorized={onUnauthorized}
        />
      )}
      {activeRoute === "password" && (
        <PasswordPage onUnauthorized={onUnauthorized} />
      )}
    </div>
  );
}
