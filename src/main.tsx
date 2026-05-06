import React from "react";
import ReactDOM from "react-dom/client";
import "./styles/tokens.css";
import "./styles/typography.css";
import "./styles/shell.css";
import "./styles.css";

// Two roots share this bundle: the desktop App (Tauri runtime + browser
// fallback at /app/*), and the cloud vault admin SPA (browser-only at
// /admin/*). Pick by URL — Tauri loads pages off "/" so it always lands
// on the desktop root. Each root is dynamically imported so the
// non-active surface doesn't tax the active one's first paint.
const root = ReactDOM.createRoot(document.getElementById("root")!);
const path = typeof window !== "undefined" ? window.location.pathname : "/";
const isAdmin = path === "/admin" || path.startsWith("/admin/");

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
