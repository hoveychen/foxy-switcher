import React from "react";
import ReactDOM from "react-dom/client";
import "./styles/tokens.css";
import "./styles/typography.css";
import "./styles/shell.css";
import "./styles.css";
// admin.css holds the .admin-* classnames the device/pair/password
// pages use. After vault-app-admin-merge those pages render inside the
// browser App shell, so the styles need to be available globally — not
// just to the AdminApp bootstrap surface.
import "./admin/admin.css";

// Two roots share this bundle:
//   - AdminApp: the unauthenticated bootstrap surface (setup + login)
//     served at /admin/setup and /admin/login. Bare /admin still lands
//     here so a typo or stale bookmark hits the bootstrap router.
//   - App: everything else — desktop Tauri at "/" and the cloud-vault
//     web UI at /, /devices, /pair, /password (App now hosts the admin
//     sidebar items after vault-app-admin-merge).
//
// /admin/devices, /admin/pair, /admin/password are 301-redirected by
// the server (web.go) to their top-level paths so they don't reach
// this dispatch.
const root = ReactDOM.createRoot(document.getElementById("root")!);
const path = typeof window !== "undefined" ? window.location.pathname : "/";
const isAdmin =
  path === "/admin" ||
  path === "/admin/" ||
  path.startsWith("/admin/setup") ||
  path.startsWith("/admin/login");

if (isAdmin) {
  void import("./admin/AdminApp").then(({ AdminApp }) => {
    root.render(
      <React.StrictMode>
        <AdminApp />
      </React.StrictMode>,
    );
  });
} else {
  void import("./App").then(({ default: App }) => {
    root.render(
      <React.StrictMode>
        <App />
      </React.StrictMode>,
    );
  });
}
