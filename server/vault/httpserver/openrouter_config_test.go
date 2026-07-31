package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// grantingOpenRouter answers EnsureDeviceKey with a per-device grant so the
// endpoint test can assert *which* device the server asked about.
type grantingOpenRouter struct {
	asked []string
	grant vault.OpenRouterGrant
	err   error
}

func (g *grantingOpenRouter) EnsureDeviceKey(_ context.Context, deviceID string) (vault.OpenRouterGrant, error) {
	g.asked = append(g.asked, deviceID)
	if g.err != nil {
		return vault.OpenRouterGrant{}, g.err
	}
	out := g.grant
	out.APIKey = "sk-or-for-" + deviceID
	return out, nil
}

func (g *grantingOpenRouter) RevokeDeviceKeys(context.Context, string) error { return nil }

func newGrantFixture(t *testing.T, or OpenRouterKeyService) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := New(vault.NewInProc(st), st)
	srv.OpenRouter = or
	mux := http.NewServeMux()
	mux.Handle("/agent/v1/", srv.Handler())
	tsrv := httptest.NewServer(mux)
	t.Cleanup(tsrv.Close)
	return st, tsrv
}

func pairedDevice(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	token := vaultauth.NewToken()
	if err := st.InsertDevice(context.Background(), store.Device{
		ID: id, Name: id, TokenHash: vaultauth.HashToken(token), AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	return token
}

func getWithBearer(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// TestOpenRouterConfigIdentifiesTheDeviceFromItsToken is the authorisation
// core: the endpoint takes no device parameter, so an agent can only ever
// receive the key minted for the token it presented.
func TestOpenRouterConfigIdentifiesTheDeviceFromItsToken(t *testing.T) {
	or := &grantingOpenRouter{grant: vault.OpenRouterGrant{
		AccountID: 7, AccountName: "pool", BaseURL: vault.DefaultOpenRouterBaseURL,
		AllowedModels: []string{"deepseek/deepseek-v4-flash"}, GuardrailEnforced: true,
	}}
	st, tsrv := newGrantFixture(t, or)
	tokenA := pairedDevice(t, st, "dev-a")
	pairedDevice(t, st, "dev-b")

	resp := getWithBearer(t, tsrv.URL+"/agent/v1/openrouter/config", tokenA)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %s, want 200", resp.Status)
	}
	var grant vault.OpenRouterGrant
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if grant.APIKey != "sk-or-for-dev-a" {
		t.Fatalf("APIKey = %q, want the key for the token's own device", grant.APIKey)
	}
	if len(or.asked) != 1 || or.asked[0] != "dev-a" {
		t.Fatalf("server asked about %v, want [dev-a]", or.asked)
	}
	if len(grant.AllowedModels) != 1 || !grant.GuardrailEnforced || grant.BaseURL == "" {
		t.Fatalf("grant = %+v, want models + enforcement flag + base url on the wire", grant)
	}
}

func TestOpenRouterConfigRequiresABearerToken(t *testing.T) {
	or := &grantingOpenRouter{}
	_, tsrv := newGrantFixture(t, or)

	resp := getWithBearer(t, tsrv.URL+"/agent/v1/openrouter/config", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %s, want 401", resp.Status)
	}
	if len(or.asked) != 0 {
		t.Fatalf("unauthenticated request reached the derivation service: %v", or.asked)
	}
}

// 204 covers both "not granted" and "no account configured": the agent's
// action is identical (write nothing / tear down what it wrote), and merging
// them avoids telling an unprivileged device whether OpenRouter accounts exist.
func TestOpenRouterConfigReturns204ForNothingAvailable(t *testing.T) {
	for name, err := range map[string]error{
		"device not granted":    selector.ErrNoAvailable,
		"no account configured": vault.ErrNoOpenRouterAccount,
	} {
		t.Run(name, func(t *testing.T) {
			or := &grantingOpenRouter{err: err}
			st, tsrv := newGrantFixture(t, or)
			token := pairedDevice(t, st, "dev-a")

			resp := getWithBearer(t, tsrv.URL+"/agent/v1/openrouter/config", token)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %s, want 204", resp.Status)
			}
		})
	}
}

// A real fault (upstream down, guardrails refused, no management key) must NOT
// look like "nothing for you" — the device would silently run without
// OpenRouter and nobody would know why.
func TestOpenRouterConfigSurfacesRealFaults(t *testing.T) {
	or := &grantingOpenRouter{err: errors.New("openrouter unreachable")}
	st, tsrv := newGrantFixture(t, or)
	token := pairedDevice(t, st, "dev-a")

	resp := getWithBearer(t, tsrv.URL+"/agent/v1/openrouter/config", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %s, want 502", resp.Status)
	}
}

func TestOpenRouterConfigIs204WhenProviderIsNotConfigured(t *testing.T) {
	st, tsrv := newGrantFixture(t, nil) // srv.OpenRouter left nil
	token := pairedDevice(t, st, "dev-a")

	resp := getWithBearer(t, tsrv.URL+"/agent/v1/openrouter/config", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}
}
