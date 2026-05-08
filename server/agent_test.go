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

// runDashboardPatch is a small harness for the InUseSelf patch tests:
// builds the *http.Response with the supplied JSON body + dashboard
// path, runs the hook, and returns the decoded "kpis.in_use" list +
// any non-kpis sentinel field for invariant checks. Centralised so each
// case below stays focused on its specific assertion.
func runDashboardPatch(t *testing.T, hook func(*http.Response) error, body string) (in_use []map[string]any, poolSize int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://agent.local/api/dashboard", nil)
	resp := &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{},
		Body:          io.NopCloser(bytes.NewReader([]byte(body))),
		ContentLength: int64(len(body)),
		Request:       req,
	}
	if err := hook(resp); err != nil {
		t.Fatalf("hook: %v", err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read patched: %v", err)
	}
	var doc struct {
		KPIs struct {
			InUse    []map[string]any `json:"in_use"`
			PoolSize int              `json:"pool_size"`
		} `json:"kpis"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode: %v / raw=%s", err, out)
	}
	if got := resp.Header.Get("Content-Length"); got == "" {
		t.Errorf("Content-Length not updated: %q", got)
	}
	return doc.KPIs.InUse, doc.KPIs.PoolSize
}

// TestPatchDashboardInUseSelf_NilCoordinatorPreservesList covers the
// no-Cred case: the patch reads cc.Status().ManagedAccountID (0 on nil)
// and cc.DeviceID() (empty on nil), so it has nothing to inject; the
// upstream in_use[] list must round-trip verbatim, including pool_size
// and any other-device entries.
func TestPatchDashboardInUseSelf_NilCoordinatorPreservesList(t *testing.T) {
	hook := patchDashboardInUseSelf("", func() int64 { return 0 }) // no local view → nothing to inject
	body := `{"kpis":{"pool_size":3,"in_use":[{"account_id":7,"device_id":"dev-other","device_name":"Other","mine":false,"expires_at":1234}]},"trend":[]}`
	inUse, pool := runDashboardPatch(t, hook, body)
	if pool != 3 {
		t.Errorf("pool_size lost: got %d want 3", pool)
	}
	if len(inUse) != 1 || inUse[0]["device_id"] != "dev-other" {
		t.Errorf("other-device entry mangled: %+v", inUse)
	}
}

// TestPatchDashboardInUseSelf_NoMineEntryAppends covers case (a):
// vault hasn't yet committed our lease (race window between local
// AcquireLease and the next tick), but the local Coordinator already
// shows ManagedAccountID — patch must append a mine=true entry so the
// desktop's "in use" highlight renders without flicker.
func TestPatchDashboardInUseSelf_NoMineEntryAppends(t *testing.T) {
	hook := patchDashboardInUseSelf("dev-self", func() int64 { return 42 })
	body := `{"kpis":{"pool_size":3,"in_use":[{"account_id":7,"device_id":"dev-other","device_name":"Other","mine":false,"expires_at":1234}]},"trend":[]}`
	inUse, _ := runDashboardPatch(t, hook, body)
	if len(inUse) != 2 {
		t.Fatalf("expected 2 entries (other + appended self), got %d: %+v", len(inUse), inUse)
	}
	// Other entry must be untouched.
	var found_other, found_self bool
	for _, e := range inUse {
		if e["device_id"] == "dev-other" {
			found_other = true
			if e["mine"] == true {
				t.Errorf("other entry's mine=true flipped (must stay false): %+v", e)
			}
		}
		if e["mine"] == true {
			found_self = true
			if asInt64(e["account_id"]) != 42 {
				t.Errorf("appended self entry account_id: got %v want 42", e["account_id"])
			}
			if e["device_id"] != "dev-self" {
				t.Errorf("appended self entry device_id: got %v want dev-self", e["device_id"])
			}
		}
	}
	if !found_other {
		t.Error("other-device entry lost during append")
	}
	if !found_self {
		t.Error("self entry never appended")
	}
}

// TestPatchDashboardInUseSelf_MineEntryAccountIDOverridden covers case
// (b): the list already has our mine entry but its account_id is stale
// (vault sees the previous lease before our most recent rotation
// landed). Patch must overwrite mine.account_id with the local truth.
func TestPatchDashboardInUseSelf_MineEntryAccountIDOverridden(t *testing.T) {
	hook := patchDashboardInUseSelf("dev-self", func() int64 { return 99 })
	body := `{"kpis":{"pool_size":2,"in_use":[
		{"account_id":11,"device_id":"dev-self","device_name":"Mine","mine":true,"expires_at":1234},
		{"account_id":7,"device_id":"dev-other","device_name":"Other","mine":false,"expires_at":5678}
	]},"trend":[]}`
	inUse, _ := runDashboardPatch(t, hook, body)
	if len(inUse) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(inUse), inUse)
	}
	var minePatched bool
	for _, e := range inUse {
		if e["mine"] == true {
			minePatched = true
			if asInt64(e["account_id"]) != 99 {
				t.Errorf("mine entry account_id: got %v want 99 (local Coordinator truth)", e["account_id"])
			}
		}
		if e["device_id"] == "dev-other" && asInt64(e["account_id"]) != 7 {
			t.Errorf("other entry account_id mutated: got %v want 7", e["account_id"])
		}
	}
	if !minePatched {
		t.Error("mine entry not located")
	}
}

// TestPatchDashboardInUseSelf_NoOpWhenInSync covers case (c): mine
// entry already matches local truth — patch must be a no-op semantically
// (other entries unchanged, mine entry's account_id and device_id
// preserved).
func TestPatchDashboardInUseSelf_NoOpWhenInSync(t *testing.T) {
	hook := patchDashboardInUseSelf("dev-self", func() int64 { return 42 })
	body := `{"kpis":{"pool_size":1,"in_use":[
		{"account_id":42,"device_id":"dev-self","device_name":"Mine","mine":true,"expires_at":1234}
	]},"trend":[]}`
	inUse, _ := runDashboardPatch(t, hook, body)
	if len(inUse) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(inUse), inUse)
	}
	e := inUse[0]
	if asInt64(e["account_id"]) != 42 || e["device_id"] != "dev-self" || e["mine"] != true {
		t.Errorf("in-sync mine entry mutated: %+v", e)
	}
}

// TestPatchDashboardInUseSelf_NonDashboardPathPasses checks the path
// guard: a /api/accounts response must come back unchanged so the
// patch doesn't accidentally mangle every JSON response that flows
// through the proxy.
func TestPatchDashboardInUseSelf_NonDashboardPathPasses(t *testing.T) {
	hook := patchDashboardInUseSelf("", func() int64 { return 0 })
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

// asInt64 unwraps a JSON-decoded number (always float64 in interface{})
// to int64 for compactly comparing account_id assertions.
func asInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	}
	return 0
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
