package httpclient

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpserver"
)

// fakeORAPI is a stand-in for OpenRouter's management API so the roundtrip
// exercises the real store → OpenRouterKeys → httpserver → httpclient chain
// without reaching the network.
type fakeORAPI struct{ n int }

func (f *fakeORAPI) DeriveDeviceKey(_ context.Context, spec openrouter.DeriveSpec) (openrouter.Key, error) {
	f.n++
	return openrouter.Key{Hash: "kh", Secret: "sk-or-runtime"}, nil
}

func (f *fakeORAPI) RevokeDerivedKey(context.Context, string) error { return nil }

// newORRoundtrip wires the full chain, mirroring newRoundtripFixture but with
// the OpenRouter derivation service attached.
func newORRoundtrip(t *testing.T, grantDevice bool) (*store.Store, *Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	acc := &store.Account{
		Provider: store.ProviderOpenRouter, Name: "pool", AccountUUID: "or-1",
		Status: store.StatusActive,
	}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.SetOpenRouterConfig(ctx, acc.ID, store.OpenRouterAccountConfig{
		AllowedModels: []string{"deepseek/deepseek-v4-flash"}, LimitUSD: 10,
	}); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := st.SetOpenRouterManagementKey(ctx, acc.ID, "sk-or-mgmt"); err != nil {
		t.Fatalf("mgmt: %v", err)
	}

	token := vaultauth.NewToken()
	if err := st.InsertDevice(ctx, store.Device{
		ID: vaultauth.NewID(), Name: "test-device", TokenHash: vaultauth.HashToken(token),
		AllowClaude: true, AllowOpenRouter: grantDevice,
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	keys := vault.NewOpenRouterKeys(st, nil)
	keys.SetClientFactory(func(string) vault.OpenRouterAPI { return &fakeORAPI{} })
	srv := httpserver.New(vault.NewInProc(st), st)
	srv.OpenRouter = keys
	tsrv := httptest.NewServer(srv.Handler())
	t.Cleanup(tsrv.Close)

	c := New(tsrv.URL)
	c.SetToken(token)
	return st, c
}

// TestOpenRouterConfigRoundTrip proves the wire format and the in-process
// semantics agree: what the vault derives is what the agent decodes.
func TestOpenRouterConfigRoundTrip(t *testing.T) {
	_, c := newORRoundtrip(t, true)

	grant, err := c.OpenRouterConfig(context.Background())
	if err != nil {
		t.Fatalf("OpenRouterConfig: %v", err)
	}
	if grant.APIKey != "sk-or-runtime" {
		t.Fatalf("APIKey = %q", grant.APIKey)
	}
	if grant.BaseURL != vault.DefaultOpenRouterBaseURL {
		t.Fatalf("BaseURL = %q", grant.BaseURL)
	}
	if len(grant.AllowedModels) != 1 || grant.AllowedModels[0] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("AllowedModels = %v", grant.AllowedModels)
	}
	if grant.AccountName != "pool" {
		t.Fatalf("AccountName = %q", grant.AccountName)
	}
}

// An ungranted device gets the same not-available signal the Claude/Codex pools
// use, so the agent has one branch to handle rather than a per-provider zoo.
func TestOpenRouterConfigNotGrantedMapsToErrNoAvailable(t *testing.T) {
	_, c := newORRoundtrip(t, false)

	_, err := c.OpenRouterConfig(context.Background())
	if !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("err = %v, want selector.ErrNoAvailable", err)
	}
}
