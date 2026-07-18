package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// mockCodexIssuer points the exported openai device-code endpoints at a mock
// server for the duration of the test.
func mockCodexIssuer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	restore := struct{ uc, tok, redir, verify, token string }{
		openai.DeviceUserCodeURL, openai.DeviceTokenURL, openai.DeviceRedirectURI, openai.DeviceVerificionURL, openai.TokenURL,
	}
	openai.DeviceUserCodeURL = srv.URL + "/api/accounts/deviceauth/usercode"
	openai.DeviceTokenURL = srv.URL + "/api/accounts/deviceauth/token"
	openai.DeviceRedirectURI = srv.URL + "/deviceauth/callback"
	openai.DeviceVerificionURL = srv.URL + "/codex/device"
	openai.TokenURL = srv.URL + "/oauth/token"
	t.Cleanup(func() {
		openai.DeviceUserCodeURL, openai.DeviceTokenURL, openai.DeviceRedirectURI, openai.DeviceVerificionURL, openai.TokenURL =
			restore.uc, restore.tok, restore.redir, restore.verify, restore.token
		srv.Close()
	})
	return srv
}

func TestCodexDeviceLoginStoresAccountInVaultMode(t *testing.T) {
	polls := 0
	mockCodexIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "dev-1", "user_code": "ABCD-1234", "interval": "1",
			})
		case "/api/accounts/deviceauth/token":
			polls++
			if polls < 2 {
				w.WriteHeader(http.StatusForbidden) // not approved yet
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "code", "code_challenge": "c", "code_verifier": "v",
			})
		case "/oauth/token":
			jwt := testCodexJWT(t, "acct-x", "x@example.com", "pro")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id_token": jwt, "access_token": jwt, "refresh_token": "r",
			})
		default:
			http.NotFound(w, r)
		}
	})

	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	srv.Mode = "vault" // device-code must work where import-codex is rejected

	// start
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%q", w.Code, w.Body.String())
	}
	var start struct {
		Session         string `json:"session"`
		UserCode        string `json:"user_code"`
		VerificationURL string `json:"verification_url"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(w.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start.Session == "" || start.UserCode != "ABCD-1234" || start.Interval != 1 {
		t.Fatalf("start payload: %+v", start)
	}

	poll := func() *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"session": start.Session})
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login/poll", bytes.NewReader(body))
		req.Header.Set("content-type", "application/json")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}

	// first poll -> pending
	rr := poll()
	var p1 struct {
		Status string `json:"status"`
	}
	_ = json.NewDecoder(rr.Body).Decode(&p1)
	if rr.Code != http.StatusOK || p1.Status != "pending" {
		t.Fatalf("poll1 code=%d status=%q body=%q", rr.Code, p1.Status, rr.Body.String())
	}

	// second poll -> complete + account stored
	rr = poll()
	if rr.Code != http.StatusOK {
		t.Fatalf("poll2 code=%d body=%q", rr.Code, rr.Body.String())
	}
	var p2 struct {
		Status  string      `json:"status"`
		Account accountView `json:"account"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&p2); err != nil {
		t.Fatal(err)
	}
	if p2.Status != "complete" || p2.Account.Provider != store.ProviderCodex || p2.Account.AccountUUID != "acct-x" {
		t.Fatalf("poll2 payload: %+v", p2)
	}

	rows, err := st.ListProvider(context.Background(), store.ProviderCodex)
	if err != nil || len(rows) != 1 || rows[0].CredentialJSON == "" {
		t.Fatalf("stored Codex rows=%+v err=%v", rows, err)
	}

	// session is single-use: polling again is rejected.
	if rr := poll(); rr.Code != http.StatusBadRequest {
		t.Fatalf("reused session code=%d, want 400", rr.Code)
	}
}

func TestCodexLoginPollUnknownSession(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	body, _ := json.Marshal(map[string]string{"session": "nope"})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login/poll", bytes.NewReader(body))
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}
