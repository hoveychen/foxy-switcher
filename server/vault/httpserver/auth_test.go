package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// authFixture is the parallel of httpclient/roundtrip.go's fixture but
// driven from the server side — drives raw HTTP against the auth surface
// so we can assert on response shape and store-side state.
type authFixture struct {
	st     *store.Store
	server *httptest.Server
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	svc := vault.NewInProc(st)
	mux := http.NewServeMux()
	srv := New(svc, st)
	mux.Handle("/agent/v1/", srv.Handler())
	srv.RegisterWebRoutes(mux)
	srv.RegisterAPIRoutes(mux)
	tsrv := httptest.NewServer(mux)
	t.Cleanup(tsrv.Close)
	return &authFixture{st: st, server: tsrv}
}

// TestPairFlow_HappyPath drives the agent's pair-init / pair-poll +
// the Web UI's approve POST end-to-end. The point is to lock down the
// "agent eventually receives a working token" contract.
func TestPairFlow_HappyPath(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()

	// Set the password so login + approval can happen.
	hash, _ := vaultauth.HashPassword("hunter2")
	if err := f.st.SetPasswordHash(ctx, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}

	// 1. Agent: pair-init.
	nonce := vaultauth.NewID()
	body := map[string]string{"client_nonce": nonce, "device_name": "laptop"}
	buf, _ := json.Marshal(body)
	resp, err := http.Post(f.server.URL+"/agent/v1/devices/pair-init",
		"application/json", strings.NewReader(string(buf)))
	if err != nil {
		t.Fatalf("pair-init: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pair-init status %s", resp.Status)
	}
	var initOut pairInitResp
	if err := json.NewDecoder(resp.Body).Decode(&initOut); err != nil {
		t.Fatalf("pair-init decode: %v", err)
	}
	resp.Body.Close()
	if initOut.UserCode == "" || initOut.VerificationURL == "" {
		t.Fatalf("pair-init missing fields: %+v", initOut)
	}

	// 2. Pre-approval: pair-poll says pending.
	pollBody, _ := json.Marshal(map[string]string{"client_nonce": nonce})
	resp, err = http.Post(f.server.URL+"/agent/v1/devices/pair-poll",
		"application/json", strings.NewReader(string(pollBody)))
	if err != nil {
		t.Fatalf("pair-poll pre-approve: %v", err)
	}
	var pollOut pairPollResp
	_ = json.NewDecoder(resp.Body).Decode(&pollOut)
	resp.Body.Close()
	if pollOut.Status != "pending" {
		t.Fatalf("pre-approve status: got %q want pending", pollOut.Status)
	}

	// 3. Admin SPA: log in via JSON API, approve via JSON API.
	jar := newCookieJar(t)
	loginResp := postJSON(t, f.server.URL+"/admin/api/login", jar,
		map[string]string{"password": "hunter2"})
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("login status: got %s want 200", loginResp.Status)
	}
	approve := postJSON(t, f.server.URL+"/admin/api/pair", jar, map[string]string{
		"code":   initOut.UserCode,
		"action": "approve",
	})
	approve.Body.Close()
	if approve.StatusCode != http.StatusOK {
		t.Fatalf("approve status: got %s", approve.Status)
	}

	// 4. Post-approval: pair-poll returns the token.
	resp, err = http.Post(f.server.URL+"/agent/v1/devices/pair-poll",
		"application/json", strings.NewReader(string(pollBody)))
	if err != nil {
		t.Fatalf("pair-poll post-approve: %v", err)
	}
	pollOut = pairPollResp{}
	_ = json.NewDecoder(resp.Body).Decode(&pollOut)
	resp.Body.Close()
	if pollOut.Status != "approved" || pollOut.DeviceToken == "" || pollOut.DeviceID == "" {
		t.Fatalf("post-approve: %+v", pollOut)
	}

	// 5. Token works on a protected route.
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/agent/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+pollOut.DeviceToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("authenticated GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET status %s", resp.Status)
	}

	// 6. Bogus token gets 401.
	req, _ = http.NewRequest(http.MethodGet, f.server.URL+"/agent/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer not-a-real-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("bogus GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus token: got %s want 401", resp.Status)
	}
}

func TestPairFlow_DeniedShortCircuits(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	_ = f.st.SetPasswordHash(ctx, hash)

	nonce := vaultauth.NewID()
	buf, _ := json.Marshal(map[string]string{"client_nonce": nonce, "device_name": "laptop"})
	resp, _ := http.Post(f.server.URL+"/agent/v1/devices/pair-init",
		"application/json", strings.NewReader(string(buf)))
	var init pairInitResp
	_ = json.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()

	jar := newCookieJar(t)
	postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "pw"}).Body.Close()
	postJSON(t, f.server.URL+"/admin/api/pair", jar, map[string]string{
		"code": init.UserCode, "action": "deny",
	}).Body.Close()

	pollBody, _ := json.Marshal(map[string]string{"client_nonce": nonce})
	resp, _ = http.Post(f.server.URL+"/agent/v1/devices/pair-poll",
		"application/json", strings.NewReader(string(pollBody)))
	var poll pairPollResp
	_ = json.NewDecoder(resp.Body).Decode(&poll)
	resp.Body.Close()
	if poll.Status != "denied" {
		t.Errorf("denied status: got %q", poll.Status)
	}
	// And no device row materialised.
	devs, _ := f.st.ListDevices(ctx)
	if len(devs) != 0 {
		t.Errorf("denied flow created device rows: %d", len(devs))
	}
}

func TestSetup_FirstRunGate(t *testing.T) {
	f := newAuthFixture(t)
	// Without a password set, GET / must redirect to the SPA setup
	// route, NOT /admin/login.
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(f.server.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: got %s want 303", resp.Status)
	}
	if !strings.HasSuffix(resp.Header.Get("Location"), "/admin/setup") {
		t.Errorf("redirect target: got %q want /admin/setup", resp.Header.Get("Location"))
	}
}

// TestPairCORS_PreflightAndPost covers the Step 8 contract the Tauri
// Settings → Pair modal depends on: pair-init / pair-poll respond to
// OPTIONS preflight and a real POST with permissive
// Access-Control-Allow-Origin headers, so the React webview can drive
// the device flow cross-origin without a local proxy.
func TestPairCORS_PreflightAndPost(t *testing.T) {
	f := newAuthFixture(t)

	preflight, _ := http.NewRequest(http.MethodOptions, f.server.URL+"/agent/v1/devices/pair-init", nil)
	preflight.Header.Set("Origin", "https://random-origin.local")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status: got %s want 204", resp.Status)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("preflight CORS origin: got %q want *", got)
	}

	body, _ := json.Marshal(map[string]string{
		"client_nonce": "nonce-x",
		"device_name":  "tauri-modal",
	})
	resp2, err := http.Post(f.server.URL+"/agent/v1/devices/pair-init",
		"application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("post status: got %s want 200", resp2.Status)
	}
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("post CORS origin: got %q want *", got)
	}
}

// TestPairCORS_BearerRoutesNotExposed pins down the negative side of
// pairCORS — Bearer-protected routes deliberately stay un-CORS'd so a
// malicious origin can't trick a logged-in browser into hitting
// /agent/v1/accounts. Without this assertion, a "while we're at it"
// future patch could quietly expand pairCORS to cover everything and
// open the surface back up.
func TestPairCORS_BearerRoutesNotExposed(t *testing.T) {
	f := newAuthFixture(t)
	req, _ := http.NewRequest(http.MethodOptions, f.server.URL+"/agent/v1/accounts", nil)
	req.Header.Set("Origin", "https://hostile.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got == "*" {
		t.Errorf("/agent/v1/accounts must not advertise wildcard CORS, got %q", got)
	}
}

func TestRevokeDevice_TokenStopsWorking(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()

	token := vaultauth.NewToken()
	dev := store.Device{
		ID:        vaultauth.NewID(),
		Name:      "to-be-revoked",
		TokenHash: vaultauth.HashToken(token),
	}
	if err := f.st.InsertDevice(ctx, dev); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	// Initially the token works.
	req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/agent/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pre-revoke status: got %s", resp.Status)
	}

	if err := f.st.DeleteDevice(ctx, dev.ID); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}

	req, _ = http.NewRequest(http.MethodGet, f.server.URL+"/agent/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked token: got %s want 401", resp.Status)
	}
}

// TestSuspendDevice_TokenStopsThenResumes locks the core suspend contract:
// a suspended device's token 401s (temporary kick) but the SAME token works
// again after resume — no re-pair, because the row and token_hash survive.
func TestSuspendDevice_TokenStopsThenResumes(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()

	token := vaultauth.NewToken()
	dev := store.Device{
		ID:        vaultauth.NewID(),
		Name:      "to-be-suspended",
		TokenHash: vaultauth.HashToken(token),
	}
	if err := f.st.InsertDevice(ctx, dev); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	call := func() int {
		req, _ := http.NewRequest(http.MethodGet, f.server.URL+"/agent/v1/accounts", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if got := call(); got != http.StatusOK {
		t.Fatalf("pre-suspend status: got %d want 200", got)
	}

	if err := f.st.SetDeviceDisabled(ctx, dev.ID, true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if got := call(); got != http.StatusUnauthorized {
		t.Errorf("suspended token: got %d want 401", got)
	}

	if err := f.st.SetDeviceDisabled(ctx, dev.ID, false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if got := call(); got != http.StatusOK {
		t.Errorf("resumed token: got %d want 200", got)
	}
}

// --- helpers -------------------------------------------------------------

type cookieJar struct {
	cookies []*http.Cookie
}

func newCookieJar(t *testing.T) *cookieJar { t.Helper(); return &cookieJar{} }

func (j *cookieJar) update(resp *http.Response) {
	for _, c := range resp.Cookies() {
		j.cookies = append(j.cookies, c)
	}
}

func (j *cookieJar) attach(req *http.Request) {
	for _, c := range j.cookies {
		req.AddCookie(c)
	}
}

// time.Sleep is intentionally avoided in these tests; the polling tests
// drive the state transitions explicitly via Web UI POSTs and call
// pair-poll directly. This helper is unused but documents the choice.
var _ = time.Second
