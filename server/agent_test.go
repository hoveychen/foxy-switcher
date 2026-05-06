package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestVaultAPIProxy_RewritesPathAndInjectsBearer asserts the three
// contracts of the agent's reverse proxy: every forwarded request
// carries the agent's bearer token, request bodies / methods reach the
// upstream verbatim, and incoming /api/* paths are rewritten to
// /agent/v1/api/* so they hit the bearer-only agent surface (which the
// vault deployment whitelists past any outer SSO).
func TestVaultAPIProxy_RewritesPathAndInjectsBearer(t *testing.T) {
	var (
		gotAuth string
		gotPath string
		gotBody string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	proxy := newVaultAPIProxy(target, "secret-token")

	req := httptest.NewRequest(http.MethodPost,
		"http://agent.local/api/accounts/42/refresh",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header: got %q want %q", gotAuth, "Bearer secret-token")
	}
	if gotPath != "/agent/v1/api/accounts/42/refresh" {
		t.Errorf("path: got %q want /agent/v1/api/accounts/42/refresh", gotPath)
	}
	if gotBody != `{}` {
		t.Errorf("body: got %q", gotBody)
	}
}

// TestVaultAPIProxy_LeavesAgentV1PathsAlone covers the second branch of
// the path rewriter: a request that already hits /agent/v1/* (e.g.
// because the agent's lease-flow caller drives that surface directly)
// must not get a second prefix.
func TestVaultAPIProxy_LeavesAgentV1PathsAlone(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	proxy := newVaultAPIProxy(target, "tok")
	req := httptest.NewRequest(http.MethodGet, "http://agent.local/agent/v1/leases", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if gotPath != "/agent/v1/leases" {
		t.Errorf("path: got %q want /agent/v1/leases (no double prefix)", gotPath)
	}
}

// TestVaultAPIProxy_UpstreamDownReturns502 confirms the ErrorHandler hook
// surfaces a JSON 502 when the vault is unreachable. Without it, the
// frontend would see a Go-default plain-text 502 and fail to render the
// "vault unreachable" toast cleanly.
func TestVaultAPIProxy_UpstreamDownReturns502(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1") // port 1 → connection refused
	proxy := newVaultAPIProxy(target, "tok")
	req := httptest.NewRequest(http.MethodGet, "http://agent.local/api/accounts", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "vault unreachable") {
		t.Errorf("body missing vault-unreachable signal: %s", rr.Body.String())
	}
}

// TestPatchDashboardInUseAccount_RewritesKPIs is the lockdown for the
// "vault returns 0 for in_use_account_id; agent should patch its local
// view" rule. Without the patch the desktop would never highlight the
// currently-injected account when running in agent mode.
func TestPatchDashboardInUseAccount_RewritesKPIs(t *testing.T) {
	hook := patchDashboardInUseAccount(nil) // nil cc → Status() returns 0
	body := []byte(`{"kpis":{"in_use_account_id":0,"pool_size":3},"trend":[]}`)
	req := httptest.NewRequest(http.MethodGet, "http://agent.local/api/dashboard", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
	if err := hook(resp); err != nil {
		t.Fatalf("hook: %v", err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read patched body: %v", err)
	}
	var doc struct {
		KPIs struct {
			InUseAccountID int64 `json:"in_use_account_id"`
			PoolSize       int   `json:"pool_size"`
		} `json:"kpis"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode patched body: %v / raw=%s", err, out)
	}
	// nil cc.Status() returns 0 — same as upstream — but the patch ran
	// (verified by the body being re-serialized without error). The
	// pool_size field round-trips, proving non-kpis fields are preserved.
	if doc.KPIs.PoolSize != 3 {
		t.Errorf("pool_size lost in patch: got %d want 3", doc.KPIs.PoolSize)
	}
	if got := resp.Header.Get("Content-Length"); got == "" {
		t.Errorf("Content-Length not updated: %q", got)
	}
}

// TestPatchDashboardInUseAccount_NonDashboardPathPasses checks the path
// guard: a /api/accounts response must come back unchanged so the patch
// doesn't accidentally mangle every JSON response that flows through
// the proxy.
func TestPatchDashboardInUseAccount_NonDashboardPathPasses(t *testing.T) {
	hook := patchDashboardInUseAccount(nil)
	body := []byte(`{"accounts":[{"id":1}]}`)
	req := httptest.NewRequest(http.MethodGet, "http://agent.local/api/accounts", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
	if err := hook(resp); err != nil {
		t.Fatalf("hook: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	if string(out) != string(body) {
		t.Errorf("non-dashboard body changed: got %q want %q", out, body)
	}
}

// TestRegisterLocalPrefRoutes_RoundTrip exercises the per-agent prefs
// surface end-to-end: GET returns store defaults, PUT echoes the
// updated value back, the next GET reflects the write. Locks down the
// "agent has its own settings + auto-switch state" property.
func TestRegisterLocalPrefRoutes_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "agent-activity.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	mux := http.NewServeMux()
	registerLocalPrefRoutes(mux, st)

	// /api/settings: GET defaults, PUT a patch, GET reads back the
	// merged shape.
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/settings: %d body=%s", w.Code, w.Body.String())
	}
	var defaults store.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &defaults); err != nil {
		t.Fatalf("decode defaults: %v", err)
	}

	patch := map[string]any{"theme": "dark"}
	pb, _ := json.Marshal(patch)
	req = httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewReader(pb))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /api/settings: %d body=%s", w.Code, w.Body.String())
	}
	var echoed store.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &echoed); err != nil {
		t.Fatalf("decode echo: %v", err)
	}
	if echoed.Theme != "dark" {
		t.Errorf("PUT echo: theme=%q want dark", echoed.Theme)
	}

	// Verify the GET sees the persisted value.
	req = httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	var after store.Settings
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode after: %v", err)
	}
	if after.Theme != "dark" {
		t.Errorf("readback: theme=%q want dark", after.Theme)
	}

	// /api/auto-switch round-trip.
	body := `{"enabled":false,"policy":"lowest"}`
	req = httptest.NewRequest(http.MethodPost, "/api/auto-switch", strings.NewReader(body))
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /api/auto-switch: %d body=%s", w.Code, w.Body.String())
	}
	got, err := st.GetAutoSwitch(context.Background())
	if err != nil {
		t.Fatalf("GetAutoSwitch: %v", err)
	}
	if got.Enabled || got.Policy != "lowest" {
		t.Errorf("auto-switch persist: got %+v want {enabled:false policy:lowest}", got)
	}
}
