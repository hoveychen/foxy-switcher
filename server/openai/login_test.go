package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// withOAuthEndpoints points the authorize + exchange vars at a mock server and
// restores them (and httpClient) on cleanup.
func withOAuthEndpoints(t *testing.T, srv *httptest.Server) {
	t.Helper()
	old := struct {
		authorize, token string
		client           *http.Client
	}{CodexAuthorizeURL, TokenURL, httpClient}
	CodexAuthorizeURL = srv.URL + "/oauth/authorize"
	TokenURL = srv.URL + "/oauth/token"
	httpClient = srv.Client()
	t.Cleanup(func() {
		CodexAuthorizeURL, TokenURL, httpClient = old.authorize, old.token, old.client
	})
}

func TestAuthorizeURLParams(t *testing.T) {
	raw := AuthorizeURL("chal-abc", "state-xyz")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse authorize url: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  codexOAuthClientID,
		"redirect_uri":               CodexRedirectURI,
		"scope":                      codexOAuthScopes,
		"code_challenge":             "chal-abc",
		"code_challenge_method":      "S256",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"state":                      "state-xyz",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("authorize %s = %q, want %q", k, got, v)
		}
	}
}

func TestNewPKCEPairChallengeIsSHA256(t *testing.T) {
	v, c, err := NewPKCEPair()
	if err != nil {
		t.Fatal(err)
	}
	if v == "" || c == "" || v == c {
		t.Fatalf("verifier/challenge look wrong: v=%q c=%q", v, c)
	}
}

func TestParsePastedCode(t *testing.T) {
	cases := []struct {
		name, in, code, state string
		wantErr               bool
	}{
		{name: "full url", in: "http://localhost:1455/auth/callback?code=abc123&state=st1", code: "abc123", state: "st1"},
		{name: "trailing space", in: "  http://localhost:1455/auth/callback?code=abc&state=st  ", code: "abc", state: "st"},
		{name: "bare query", in: "code=xyz&state=st2", code: "xyz", state: "st2"},
		{name: "code#state form", in: "cccc#ssss", code: "cccc", state: "ssss"},
		{name: "empty", in: "", wantErr: true},
		{name: "no code", in: "http://localhost:1455/auth/callback?state=only", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, state, err := ParsePastedCode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got code=%q state=%q", code, state)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if code != tc.code || state != tc.state {
				t.Fatalf("got code=%q state=%q, want code=%q state=%q", code, state, tc.code, tc.state)
			}
		})
	}
}

func TestCompleteLoginExchangesAndBuildsAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
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
				r.Form.Get("redirect_uri") != CodexRedirectURI ||
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
	withOAuthEndpoints(t, srv)

	account, err := CompleteLogin(context.Background(), "verifier-xyz", "auth-code-abc")
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
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

func TestCompleteLoginMissingArgs(t *testing.T) {
	if _, err := CompleteLogin(context.Background(), "", "code"); err == nil {
		t.Fatal("want error for empty verifier")
	}
	if _, err := CompleteLogin(context.Background(), "verifier", ""); err == nil {
		t.Fatal("want error for empty code")
	}
}
