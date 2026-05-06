package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAgentAPIWhitelist_AllowsLeaseRoutes locks down the lease-friendly
// surface: GETs on the read endpoints and POST refresh/select on a
// specific account flow through to the inner handler.
func TestAgentAPIWhitelist_AllowsLeaseRoutes(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/accounts"},
		{http.MethodGet, "/api/dashboard"},
		{http.MethodGet, "/api/activity"},
		{http.MethodGet, "/api/activity/stream"},
		{http.MethodGet, "/api/devices"},
		{http.MethodGet, "/api/about"},
		{http.MethodGet, "/api/cred/status"},
		{http.MethodPost, "/api/accounts/42/refresh"},
		{http.MethodPost, "/api/accounts/42/select"},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTeapot) // sentinel: request reached inner handler
			})
			h := agentAPIWhitelist(inner)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusTeapot {
				t.Fatalf("%s %s: want pass-through (418), got %d body=%s",
					tc.method, tc.path, w.Code, w.Body.String())
			}
		})
	}
}

// TestAgentAPIWhitelist_BlocksAdminWrites is the regression guard: the
// admin write surface (login/callback/delete/pause/resume/thresholds/
// auto-switch write/reset/settings write/pair) must come back 405 with
// the same JSON error shape, regardless of which method/path variant
// the caller tries.
func TestAgentAPIWhitelist_BlocksAdminWrites(t *testing.T) {
	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/accounts/login"},
		{http.MethodPost, "/api/accounts/callback"},
		{http.MethodDelete, "/api/accounts/42"},
		{http.MethodPost, "/api/accounts/42/pause"},
		{http.MethodPost, "/api/accounts/42/resume"},
		{http.MethodPost, "/api/accounts/42/thresholds"},
		{http.MethodPost, "/api/auto-switch"},
		{http.MethodPost, "/api/reset"},
		{http.MethodPut, "/api/settings"},
		{http.MethodPost, "/api/pair/init"},
		{http.MethodPost, "/api/pair/poll"},
		// Nonsense paths also reject (default-deny posture).
		{http.MethodGet, "/api/totally/made/up"},
		{http.MethodPost, "/api/accounts/42"},
	}
	for _, tc := range cases {
		t.Run(tc.method+"_"+tc.path, func(t *testing.T) {
			innerCalled := false
			inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				innerCalled = true
			})
			h := agentAPIWhitelist(inner)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if innerCalled {
				t.Fatalf("%s %s: inner handler ran but admin route should be blocked", tc.method, tc.path)
			}
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s: want 405, got %d body=%s",
					tc.method, tc.path, w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Fatalf("%s %s: want JSON content-type, got %q", tc.method, tc.path, ct)
			}
			var body map[string]string
			if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
				t.Fatalf("%s %s: decode body: %v", tc.method, tc.path, err)
			}
			if body["error"] == "" {
				t.Fatalf("%s %s: empty error message", tc.method, tc.path)
			}
		})
	}
}
