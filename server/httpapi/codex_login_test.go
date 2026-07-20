package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// mockCodexIssuer points the exported openai OAuth endpoints at a mock server
// for the duration of the test.
func mockCodexIssuer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	restore := struct{ authorize, token string }{openai.CodexAuthorizeURL, openai.TokenURL}
	openai.CodexAuthorizeURL = srv.URL + "/oauth/authorize"
	openai.TokenURL = srv.URL + "/oauth/token"
	t.Cleanup(func() {
		openai.CodexAuthorizeURL, openai.TokenURL = restore.authorize, restore.token
		srv.Close()
	})
	return srv
}

func TestCodexLoginStoresAccountInVaultMode(t *testing.T) {
	mockCodexIssuer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
	srv.Mode = "vault" // must work where import-codex is rejected

	// start -> authorize_url + state
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%q", w.Code, w.Body.String())
	}
	var start struct {
		AuthorizeURL string `json:"authorize_url"`
		State        string `json:"state"`
	}
	if err := json.NewDecoder(w.Body).Decode(&start); err != nil {
		t.Fatal(err)
	}
	if start.State == "" {
		t.Fatalf("start payload missing state: %+v", start)
	}
	// The authorize URL must round-trip our state and target the mock issuer.
	u, err := url.Parse(start.AuthorizeURL)
	if err != nil || u.Query().Get("state") != start.State {
		t.Fatalf("authorize_url = %q err=%v", start.AuthorizeURL, err)
	}

	// callback with the pasted loopback URL -> account stored
	pasted := "http://localhost:1455/auth/callback?code=the-code&state=" + start.State
	body, _ := json.Marshal(map[string]string{"state": start.State, "pasted": pasted})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login/callback", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("callback code=%d body=%q", rr.Code, rr.Body.String())
	}
	var got struct {
		Account accountView `json:"account"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Account.Provider != store.ProviderCodex || got.Account.AccountUUID != "acct-x" {
		t.Fatalf("callback payload: %+v", got)
	}

	rows, err := st.ListProvider(context.Background(), store.ProviderCodex)
	if err != nil || len(rows) != 1 || rows[0].CredentialJSON == "" {
		t.Fatalf("stored Codex rows=%+v err=%v", rows, err)
	}

	// state is single-use: replaying the same callback is rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login/callback", bytes.NewReader(body))
	req2.Header.Set("content-type", "application/json")
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("reused state code=%d, want 400", rr2.Code)
	}
}

func TestCodexLoginCallbackStateMismatch(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login", nil))
	var start struct {
		State string `json:"state"`
	}
	_ = json.NewDecoder(w.Body).Decode(&start)

	// Pasted state does not match the issued state.
	pasted := "http://localhost:1455/auth/callback?code=c&state=other-attempt"
	body, _ := json.Marshal(map[string]string{"state": start.State, "pasted": pasted})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login/callback", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q, want 400", rr.Code, rr.Body.String())
	}
}

func TestCodexLoginCallbackUnknownSession(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	body, _ := json.Marshal(map[string]string{"state": "nope", "pasted": "code=c&state=nope"})
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/codex-login/callback", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}
