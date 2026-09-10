package httpclient

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpserver"
)

// newDSRoundtrip wires the full store → DeepSeekGrants → httpserver →
// httpclient chain so the test proves the wire format and the in-process
// semantics agree.
func newDSRoundtrip(t *testing.T, grantDevice bool) (*store.Store, *Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	acc := &store.Account{
		Provider: store.ProviderDeepSeek, Name: "ds-pool", AccountUUID: "ds-1",
		Status: store.StatusActive,
	}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.SetDeepSeekCredential(ctx, acc.ID, "sk-ds-console"); err != nil {
		t.Fatalf("credential: %v", err)
	}

	token := vaultauth.NewToken()
	if err := st.InsertDevice(ctx, store.Device{
		ID: vaultauth.NewID(), Name: "test-device", TokenHash: vaultauth.HashToken(token),
		AllowClaude: true, AllowDeepSeek: grantDevice,
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	srv := httpserver.New(vault.NewInProc(st), st)
	srv.DeepSeek = vault.NewDeepSeekGrants(st, nil)
	tsrv := httptest.NewServer(srv.Handler())
	t.Cleanup(tsrv.Close)

	c := New(tsrv.URL)
	c.SetToken(token)
	return st, c
}

func TestDeepSeekConfigRoundTrip(t *testing.T) {
	_, c := newDSRoundtrip(t, true)

	grant, err := c.DeepSeekConfig(context.Background())
	if err != nil {
		t.Fatalf("DeepSeekConfig: %v", err)
	}
	if grant.APIKey != "sk-ds-console" {
		t.Fatalf("APIKey = %q, want the account's console key", grant.APIKey)
	}
	if grant.BaseURL != vault.DefaultDeepSeekBaseURL {
		t.Fatalf("BaseURL = %q", grant.BaseURL)
	}
	if grant.AccountName != "ds-pool" {
		t.Fatalf("AccountName = %q", grant.AccountName)
	}
}

// An ungranted device gets the same not-available signal the Claude/Codex
// pools use, so the agent has one branch to handle.
func TestDeepSeekConfigNotGrantedMapsToErrNoAvailable(t *testing.T) {
	_, c := newDSRoundtrip(t, false)

	_, err := c.DeepSeekConfig(context.Background())
	if !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("err = %v, want selector.ErrNoAvailable", err)
	}
}
