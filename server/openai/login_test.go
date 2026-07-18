package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// withDeviceEndpoints points the device-code + exchange vars at a mock server
// and restores them (and httpClient) on cleanup.
func withDeviceEndpoints(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := struct {
		uc, tok, redirect, verify, token string
		client                           *http.Client
	}{DeviceUserCodeURL, DeviceTokenURL, DeviceRedirectURI, DeviceVerificionURL, TokenURL, httpClient}
	DeviceUserCodeURL = srv.URL + "/api/accounts/deviceauth/usercode"
	DeviceTokenURL = srv.URL + "/api/accounts/deviceauth/token"
	DeviceRedirectURI = srv.URL + "/deviceauth/callback"
	DeviceVerificionURL = srv.URL + "/codex/device"
	TokenURL = srv.URL + "/oauth/token"
	httpClient = srv.Client()
	t.Cleanup(func() {
		DeviceUserCodeURL, DeviceTokenURL, DeviceRedirectURI, DeviceVerificionURL, TokenURL, httpClient =
			old.uc, old.tok, old.redirect, old.verify, old.token, old.client
	})
}

func TestDeviceLoginHappyPath(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != codexOAuthClientID {
				t.Errorf("usercode client_id = %q", body["client_id"])
			}
			// interval is a STRING on the wire (mirrors codex-rs).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"device_auth_id": "dev-123",
				"user_code":      "WXYZ-1234",
				"interval":       "5",
			})
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["device_auth_id"] != "dev-123" || body["user_code"] != "WXYZ-1234" {
				t.Errorf("poll body = %+v", body)
			}
			polls++
			if polls < 2 {
				// Not approved yet — codex-rs treats 403 as "keep polling".
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_code": "auth-code-abc",
				"code_challenge":     "chal",
				"code_verifier":      "verifier-xyz",
			})
		case "/oauth/token":
			if err := r.ParseForm(); err != nil {
				t.Fatalf("parse exchange form: %v", err)
			}
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
				t.Errorf("exchange content-type = %q", r.Header.Get("Content-Type"))
			}
			if r.Form.Get("grant_type") != "authorization_code" ||
				r.Form.Get("code") != "auth-code-abc" ||
				r.Form.Get("code_verifier") != "verifier-xyz" ||
				r.Form.Get("client_id") != codexOAuthClientID {
				t.Errorf("exchange form = %+v", r.Form)
			}
			id := fakeJWT(t, "acct-dev", "dev@example.com", "Dev User", "pro", time.Now().Add(2*time.Hour).Unix())
			access := fakeJWT(t, "acct-dev", "", "", "", time.Now().Add(time.Hour).Unix())
			_ = json.NewEncoder(w).Encode(map[string]string{
				"id_token":      id,
				"access_token":  access,
				"refresh_token": "refresh-dev",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	withDeviceEndpoints(t, srv)

	da, err := StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if da.UserCode != "WXYZ-1234" || da.DeviceAuthID != "dev-123" || da.Interval != 5 {
		t.Fatalf("device auth mismatch: %+v", da)
	}
	if da.VerificationURL != srv.URL+"/codex/device" {
		t.Fatalf("verification url = %q", da.VerificationURL)
	}

	// First poll: pending.
	if _, err := PollDeviceLogin(context.Background(), da); !errors.Is(err, ErrAuthorizationPending) {
		t.Fatalf("first poll err = %v, want ErrAuthorizationPending", err)
	}
	// Second poll: approved -> exchange -> account.
	account, err := PollDeviceLogin(context.Background(), da)
	if err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if account.Provider != store.ProviderCodex {
		t.Fatalf("provider = %v", account.Provider)
	}
	if account.AccountUUID != "acct-dev" || account.Email != "dev@example.com" || account.Plan != "Codex Pro" {
		t.Fatalf("account identity mismatch: %+v", account)
	}
	if account.AccessToken == "" || account.RefreshToken != "refresh-dev" {
		t.Fatalf("account tokens mismatch: %+v", account)
	}
	// CredentialJSON must be a valid Codex auth.json so injection can write it back.
	if _, err := ParseAuthFile([]byte(account.CredentialJSON)); err != nil {
		t.Fatalf("CredentialJSON is not a valid auth.json: %v", err)
	}
}

func TestStartDeviceLoginUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // 404 == device flow disabled on this issuer
	}))
	defer srv.Close()
	withDeviceEndpoints(t, srv)

	if _, err := StartDeviceLogin(context.Background()); !errors.Is(err, ErrDeviceCodeUnsupported) {
		t.Fatalf("err = %v, want ErrDeviceCodeUnsupported", err)
	}
}
