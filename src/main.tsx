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

// Tag <html> with tauri-host before first paint so shell.css can scope
// frameless-window styles (top drag strip, sidebar top padding to clear
// the macOS traffic lights) without flashing the browser-mode layout.
// Mirrors api.ts's inTauri probe — duplicated here to keep main.tsx's
// import graph minimal.
if (
  typeof window !== "undefined" &&
  ("__TAURI_INTERNALS__" in window || "__TAURI__" in window)
) {
  document.documentElement.classList.add("tauri-host");
}

// os-* class lets shell.css gate Windows-only chrome — specifically the
// 4-edge + 4-corner resize handles AppShell renders. macOS keeps native
// resize via the OS chrome, so handles stay hidden there. UA sniffing
// against the system webview is fine here: Tauri's wry uses the OS-default
// engine (WebView2 on Windows, WebKit on macOS) so "Windows" / "Mac"
// substrings are reliable.
if (typeof navigator !== "undefined") {
  const ua = navigator.userAgent;
  const cls = document.documentElement.classList;
  if (/Windows/i.test(ua)) cls.add("os-windows");
  else if (/Macintosh|Mac OS X/i.test(ua)) cls.add("os-macos");
  else cls.add("os-linux");
}

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
