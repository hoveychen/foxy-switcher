package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// postJSON drives an admin /api/* call. Returns the response with the
// fixture's cookie jar updated, mirroring postForm but with a JSON body.
func postJSON(t *testing.T, url string, jar *cookieJar, body any) *http.Response {
	t.Helper()
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	jar.attach(req)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	jar.update(resp)
	return resp
}

func getWith(t *testing.T, url string, jar *cookieJar) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	jar.attach(req)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	jar.update(resp)
	return resp
}

// TestAPILogin_SuccessAndFailure pins down the JSON login surface: wrong
// password → 401 JSON without a session cookie; right password → 200 with
// cookie that subsequently authenticates /admin/api/devices.
func TestAPILogin_SuccessAndFailure(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("hunter2")
	if err := f.st.SetPasswordHash(ctx, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}

	// Wrong password → 401.
	jar := newCookieJar(t)
	resp := postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "wrong"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: got %s want 401", resp.Status)
	}
	if got := resp.Header.Get("Set-Cookie"); got != "" {
		t.Errorf("wrong password set a cookie: %q", got)
	}

	// Right password → 200 + cookie.
	jar = newCookieJar(t)
	resp = postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "hunter2"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("right password: got %s want 200", resp.Status)
	}
	hasSession := false
	for _, c := range jar.cookies {
		if c.Name == SessionCookieName && c.Value != "" {
			hasSession = true
		}
	}
	if !hasSession {
		t.Fatalf("login did not set %s cookie", SessionCookieName)
	}

	// Cookie authenticates /admin/api/devices.
	resp = getWith(t, f.server.URL+"/admin/api/devices", jar)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/admin/api/devices with cookie: got %s want 200", resp.Status)
	}
}

// TestAPIMe_ReportsBootstrapState anchors the SPA's bootstrap query: it
// must distinguish "fresh vault" (has_password=false) from "logged out
// vault" (has_password=true, signed_in=false) from "logged in".
func TestAPIMe_ReportsBootstrapState(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	jar := newCookieJar(t)

	// Fresh vault.
	resp := getWith(t, f.server.URL+"/admin/api/me", jar)
	var me apiMeResp
	_ = json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if me.HasPassword || me.SignedIn {
		t.Errorf("fresh: %+v", me)
	}

	// After setup-via-API.
	resp = postJSON(t, f.server.URL+"/admin/api/setup", jar, map[string]string{
		"password": "hunter2", "confirm": "hunter2",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("setup: got %s want 200", resp.Status)
	}
	resp = getWith(t, f.server.URL+"/admin/api/me", jar)
	me = apiMeResp{}
	_ = json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if !me.HasPassword || !me.SignedIn {
		t.Errorf("after setup: %+v", me)
	}

	// Setup is one-shot — second call is rejected with 409.
	resp = postJSON(t, f.server.URL+"/admin/api/setup", jar, map[string]string{
		"password": "again", "confirm": "again",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("re-setup: got %s want 409", resp.Status)
	}

	_ = ctx
}

// TestAPIDevices_ListAndRevoke covers the canonical admin flow: log in,
// list paired devices, revoke one — confirming the JSON shape and the
// store-side delete.
func TestAPIDevices_ListAndRevoke(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	_ = f.st.SetPasswordHash(ctx, hash)

	// Insert two devices.
	d1 := store.Device{ID: vaultauth.NewID(), Name: "alpha", TokenHash: vaultauth.HashToken(vaultauth.NewToken())}
	d2 := store.Device{ID: vaultauth.NewID(), Name: "beta", TokenHash: vaultauth.HashToken(vaultauth.NewToken())}
	_ = f.st.InsertDevice(ctx, d1)
	_ = f.st.InsertDevice(ctx, d2)

	jar := newCookieJar(t)
	resp := postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "pw"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %s", resp.Status)
	}

	// List.
	resp = getWith(t, f.server.URL+"/admin/api/devices", jar)
	var list apiDevicesResp
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Devices) != 2 {
		t.Fatalf("list len: got %d want 2", len(list.Devices))
	}

	// Revoke d1.
	resp = postJSON(t, f.server.URL+"/admin/api/devices/revoke", jar, map[string]string{"id": d1.ID})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: got %s want 204", resp.Status)
	}
	devs, _ := f.st.ListDevices(ctx)
	if len(devs) != 1 || devs[0].ID != d2.ID {
		t.Errorf("after revoke: %+v", devs)
	}
}

// TestAPIPair_LookupAndApprove walks the device-flow: agent pair-init
// produces a code, admin SPA looks it up via GET /admin/api/pair, approves
// via POST /admin/api/pair, and the agent's pair-poll then sees an approved
// status with a token. This is the JSON parallel of
// TestPairFlow_HappyPath that the SPA admin will drive.
func TestAPIPair_LookupAndApprove(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	_ = f.st.SetPasswordHash(ctx, hash)

	// 1. Agent pair-init.
	nonce := vaultauth.NewID()
	initBody, _ := json.Marshal(map[string]string{"client_nonce": nonce, "device_name": "laptop"})
	resp, err := http.Post(f.server.URL+"/agent/v1/devices/pair-init",
		"application/json", strings.NewReader(string(initBody)))
	if err != nil {
		t.Fatalf("pair-init: %v", err)
	}
	var init pairInitResp
	_ = json.NewDecoder(resp.Body).Decode(&init)
	resp.Body.Close()

	// 2. Admin login + lookup via /admin/api/pair?code=.
	jar := newCookieJar(t)
	postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "pw"}).Body.Close()
	resp = getWith(t, f.server.URL+"/admin/api/pair?code="+init.UserCode, jar)
	var look apiPairLookupResp
	_ = json.NewDecoder(resp.Body).Decode(&look)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup: got %s want 200 — %+v", resp.Status, look)
	}
	if look.DeviceName != "laptop" || look.Status != store.PairingPending {
		t.Errorf("lookup: %+v", look)
	}

	// 3. Approve via POST /admin/api/pair.
	resp = postJSON(t, f.server.URL+"/admin/api/pair", jar, map[string]string{
		"code": init.UserCode, "action": "approve",
	})
	var ack apiPairResolveResp
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	resp.Body.Close()
	if ack.Result != store.PairingApproved {
		t.Errorf("approve result: %q", ack.Result)
	}

	// 4. Agent pair-poll now sees approved + a token.
	pollBody, _ := json.Marshal(map[string]string{"client_nonce": nonce})
	resp, err = http.Post(f.server.URL+"/agent/v1/devices/pair-poll",
		"application/json", strings.NewReader(string(pollBody)))
	if err != nil {
		t.Fatalf("pair-poll: %v", err)
	}
	var poll pairPollResp
	_ = json.NewDecoder(resp.Body).Decode(&poll)
	resp.Body.Close()
	if poll.Status != store.PairingApproved || poll.DeviceToken == "" {
		t.Errorf("pair-poll post-approve: %+v", poll)
	}

	// 5. Lookup of an unknown code is 404.
	resp = getWith(t, f.server.URL+"/admin/api/pair?code=ZZZZ-9999", jar)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown lookup: got %s want 404", resp.Status)
	}
}

// TestAPIPassword_ChangeLockOldOpenNew confirms that POST /admin/api/password
// succeeds with the right `current`, fails with a wrong one (401), and
// that the old password no longer logs in afterwards.
func TestAPIPassword_ChangeLockOldOpenNew(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("old-pw")
	_ = f.st.SetPasswordHash(ctx, hash)

	jar := newCookieJar(t)
	postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "old-pw"}).Body.Close()

	// Wrong current → 401.
	resp := postJSON(t, f.server.URL+"/admin/api/password", jar, map[string]string{
		"current": "nope", "next": "new-pw", "confirm": "new-pw",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bad current: got %s want 401", resp.Status)
	}

	// Mismatch confirm → 400.
	resp = postJSON(t, f.server.URL+"/admin/api/password", jar, map[string]string{
		"current": "old-pw", "next": "new-pw", "confirm": "typo",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("mismatch confirm: got %s want 400", resp.Status)
	}

	// Right current + matching confirm → 200.
	resp = postJSON(t, f.server.URL+"/admin/api/password", jar, map[string]string{
		"current": "old-pw", "next": "new-pw", "confirm": "new-pw",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("change: got %s want 200", resp.Status)
	}

	// Old password no longer logs in.
	jar2 := newCookieJar(t)
	resp = postJSON(t, f.server.URL+"/admin/api/login", jar2, map[string]string{"password": "old-pw"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("old pw still works: got %s", resp.Status)
	}

	// New password works.
	resp = postJSON(t, f.server.URL+"/admin/api/login", jar2, map[string]string{"password": "new-pw"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("new pw doesn't work: got %s", resp.Status)
	}
	_ = ctx
}

// TestAPI_UnauthenticatedReturnsJSON401 covers the contract the SPA
// depends on: protected routes return a 401 JSON body, NOT a 302
// redirect, when there's no session cookie. (web.go's HTML handlers do
// the redirect — that's deliberately different.)
func TestAPI_UnauthenticatedReturnsJSON401(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	_ = f.st.SetPasswordHash(ctx, hash)

	jar := newCookieJar(t)
	resp := getWith(t, f.server.URL+"/admin/api/devices", jar)
	body, _ := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: got %s want 401", resp.Status)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type: got %q want application/json", ct)
	}
	if !strings.Contains(body, "unauthorized") {
		t.Errorf("body: %q", body)
	}
	_ = ctx
}

// TestAPI_FreshVaultReturnsSetupRequired ensures protected routes don't
// just 401 on a brand-new vault — they return 409 setup_required so the
// SPA can route the user to the setup form rather than the login form.
func TestAPI_FreshVaultReturnsSetupRequired(t *testing.T) {
	f := newAuthFixture(t)
	jar := newCookieJar(t)
	resp := getWith(t, f.server.URL+"/admin/api/devices", jar)
	body, _ := readBody(resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status: got %s want 409", resp.Status)
	}
	if !strings.Contains(body, "setup_required") {
		t.Errorf("body: %q want setup_required", body)
	}
}

// TestLegacyWebRoutesRedirectToAdmin pins down the bookmark-friendly
// redirects that replaced the deleted server-rendered admin pages.
// The agent's pair-init verification URL still says "/pair", so a user
// clicking that link must end up on the SPA's /admin/pair?code=…
// route with the query string preserved.
func TestLegacyWebRoutesRedirectToAdmin(t *testing.T) {
	f := newAuthFixture(t)
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	cases := []struct {
		from string
		to   string
	}{
		{"/setup", "/admin/setup"},
		{"/login", "/admin/login"},
		{"/login?next=/devices", "/admin/login?next=/devices"},
		{"/devices", "/admin/devices"},
		{"/pair", "/admin/pair"},
		{"/pair?code=ABCD-1234", "/admin/pair?code=ABCD-1234"},
		{"/password", "/admin/password"},
	}
	for _, tc := range cases {
		resp, err := c.Get(f.server.URL + tc.from)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.from, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("%s status: got %s want 301", tc.from, resp.Status)
		}
		if got := resp.Header.Get("Location"); got != tc.to {
			t.Errorf("%s redirect: got %q want %q", tc.from, got, tc.to)
		}
	}
}

func readBody(resp *http.Response) (string, error) {
	var b strings.Builder
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			b.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return b.String(), nil
}
