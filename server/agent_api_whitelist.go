package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// agentAPIWhitelist gates which `/api/*` routes a remote agent is allowed
// to invoke through the `/agent/v1/api/` mount the vault exposes for
// agents whose deployment hides `/api/*` behind an outer SSO. The vault
// already authenticates the device's bearer token at the BearerAuth
// middleware layer; this filter enforces the lease/admin boundary on top
// of that — agents may *use* (read views, refresh tokens, select an
// account for themselves) but may not *administer* (add/delete accounts,
// pause/resume, change thresholds, reset state).
//
// Path is what `httpapi.Server` sees AFTER the `/agent/v1` prefix has
// been stripped (e.g. `/api/accounts`). Rejection writes
// `{"error":"agent mode is read-only; use the vault admin web UI"}` with
// 405 — the same shape the agent's own proxy returns for blocked routes,
// so the desktop UI surfaces a single consistent error string regardless
// of which layer caught the call.
func agentAPIWhitelist(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !agentLeaseAllowed(r.Method, r.URL.Path) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "agent mode is read-only; use the vault admin web UI",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// agentLeaseAllowed returns true for methods/paths an agent may invoke.
// Lease-friendly = view + per-account use ops:
//   - GET reads on accounts/dashboard/activity/devices/about/cred-status,
//     plus the SSE stream
//   - POST refresh and select on a specific account (lease "use it now")
//
// Everything else (login, callback, delete, pause/resume, thresholds,
// auto-switch write, reset, settings write, pair) is admin or
// per-machine-only and stays 405.
func agentLeaseAllowed(method, path string) bool {
	switch method {
	case http.MethodGet:
		switch path {
		case "/api/accounts",
			"/api/dashboard",
			"/api/activity",
			"/api/activity/stream",
			"/api/devices",
			"/api/about",
			"/api/cred/status":
			return true
		}
	case http.MethodPost:
		// /api/accounts/{id}/refresh and /api/accounts/{id}/select are
		// lease ops; ParseInt-safe matching is overkill for a whitelist
		// — the underlying handler validates the id and returns 400 on
		// garbage anyway.
		if strings.HasPrefix(path, "/api/accounts/") {
			if strings.HasSuffix(path, "/refresh") || strings.HasSuffix(path, "/select") {
				return true
			}
		}
	}
	return false
}
