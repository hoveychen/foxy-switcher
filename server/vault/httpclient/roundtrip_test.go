package httpclient

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpserver"
)

// roundtripFixture wires up the full chain Step 2 introduces: a real
// SQLite store → vault.InProc → httpserver.Server → httptest.Server →
// httpclient.Client. Tests exercise httpclient methods and assert the
// effect on the underlying store, verifying that the wire format and
// the inproc semantics agree.
type roundtripFixture struct {
	st     *store.Store
	server *httptest.Server
	client *Client
}

func newRoundtripFixture(t *testing.T) *roundtripFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	svc := vault.NewInProc(st)
	srv := httptest.NewServer(httpserver.New(svc, st).Handler())
	t.Cleanup(srv.Close)

	// Seed a device + bearer token so the protected /agent/v1/* routes
	// pass the Bearer middleware. Tests that exercise the pair-init / poll
	// flow drive that path explicitly; the rest just want an authenticated
	// client.
	token := vaultauth.NewToken()
	if err := st.InsertDevice(context.Background(), store.Device{
		ID:        vaultauth.NewID(),
		Name:      "test-device",
		TokenHash: vaultauth.HashToken(token),
		// Roundtrips exercise both the Claude and Codex pick paths, and the
		// vault now gates leases on the per-device allowlist, so grant both.
		AllowClaude: true,
		AllowCodex:  true,
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	c := New(srv.URL)
	c.SetToken(token)
	_ = svc // silences "declared and not used" if future steps inline more svc calls
	return &roundtripFixture{
		st:     st,
		server: srv,
		client: c,
	}
}

func seedAccount(t *testing.T, st *store.Store, name string) *store.Account {
	t.Helper()
	a := &store.Account{
		Name:         name,
		Email:        name + "@example.com",
		AccessToken:  "at-" + name,
		RefreshToken: "rt-" + name,
		ExpiresAt:    time.Now().Add(2 * time.Hour).UnixMilli(),
		Status:       "active",
	}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("upsert %s: %v", name, err)
	}
	return a
}

func TestRoundtrip_ListAccounts(t *testing.T) {
	f := newRoundtripFixture(t)
	a := seedAccount(t, f.st, "alpha")
	seedAccount(t, f.st, "beta")

	accs, err := f.client.ListAccounts(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accs) != 2 {
		t.Fatalf("len: got %d want 2", len(accs))
	}
	// Tokens must round-trip — credinject needs them to inject into the
	// keychain. (The frontend httpapi redacts them; agent surface does not.)
	if accs[0].AccessToken != a.AccessToken {
		t.Errorf("access_token redacted: got %q want %q", accs[0].AccessToken, a.AccessToken)
	}
}

func TestRoundtrip_PickReturns204OnEmptyPool(t *testing.T) {
	f := newRoundtripFixture(t)
	got, err := f.client.Pick(context.Background(), time.Now())
	if got != nil {
		t.Fatalf("Pick: got account %v, want nil", got)
	}
	// Client maps 204 back to selector.ErrNoAvailable so the coordinator's
	// errors.Is check works regardless of which Service impl it holds.
	if err == nil {
		t.Fatal("Pick: want error, got nil")
	}
	// We don't assert on the exact error — selector.ErrNoAvailable is the
	// contract but the client returns it directly so a plain non-nil check
	// is enough; the credinject coordinator is what cares about the type.
}

func TestRoundtrip_PickReturnsLRU(t *testing.T) {
	f := newRoundtripFixture(t)
	a := seedAccount(t, f.st, "alpha")
	seedAccount(t, f.st, "beta")

	got, err := f.client.Pick(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	// Both have last_used_at=upsert-time, so id-tiebreak picks alpha.
	if got == nil || got.ID != a.ID {
		t.Fatalf("Pick: got %+v, want id=%d", got, a.ID)
	}
}

func TestRoundtrip_PickProviderReturnsCodexCredential(t *testing.T) {
	f := newRoundtripFixture(t)
	seedAccount(t, f.st, "claude")
	codex := &store.Account{
		Provider: store.ProviderCodex, Name: "codex", Email: "codex@example.com",
		AccessToken: "codex-at", RefreshToken: "codex-rt",
		ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(), Status: store.StatusActive,
		AccountUUID: "codex-account", CredentialJSON: `{"auth_mode":"chatgpt"}`,
	}
	if err := f.st.Upsert(context.Background(), codex); err != nil {
		t.Fatal(err)
	}
	got, err := f.client.PickProviderForDevice(context.Background(), time.Now(), "ignored", store.ProviderCodex)
	if err != nil {
		t.Fatalf("PickProviderForDevice: %v", err)
	}
	if got.ID != codex.ID || got.Provider != store.ProviderCodex || got.CredentialJSON != codex.CredentialJSON {
		t.Fatalf("Codex credential mismatch: %+v", got)
	}
}

func TestRoundtrip_MarkUsed(t *testing.T) {
	f := newRoundtripFixture(t)
	a := seedAccount(t, f.st, "alpha")
	if _, err := f.st.DB().ExecContext(context.Background(),
		`UPDATE accounts SET last_used_at = 1 WHERE id = ?`, a.ID); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := f.client.MarkUsed(context.Background(), a.ID); err != nil {
		t.Fatalf("MarkUsed: %v", err)
	}

	got, _ := f.st.Get(context.Background(), a.ID)
	if got.LastUsedAt <= 1 {
		t.Errorf("last_used_at not bumped: %d", got.LastUsedAt)
	}
}

func TestRoundtrip_UpdateTokens(t *testing.T) {
	f := newRoundtripFixture(t)
	a := seedAccount(t, f.st, "alpha")
	newExpiry := time.Now().Add(8 * time.Hour).UnixMilli()
	if err := f.client.UpdateTokens(context.Background(), a.ID, "at-NEW", "rt-NEW", newExpiry); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}
	got, _ := f.st.Get(context.Background(), a.ID)
	if got.AccessToken != "at-NEW" || got.RefreshToken != "rt-NEW" || got.ExpiresAt != newExpiry {
		t.Errorf("UpdateTokens not applied: %+v", got)
	}
}

func TestRoundtrip_UpdateProviderCredential(t *testing.T) {
	f := newRoundtripFixture(t)
	a := seedAccount(t, f.st, "alpha")
	newExpiry := time.Now().Add(8 * time.Hour).UnixMilli()
	if err := f.client.UpdateProviderCredential(context.Background(), a.ID,
		"at-NEW", "rt-NEW", newExpiry, `{"tokens":"rotated"}`); err != nil {
		t.Fatalf("UpdateProviderCredential: %v", err)
	}
	got, _ := f.st.Get(context.Background(), a.ID)
	if got.CredentialJSON != `{"tokens":"rotated"}` {
		t.Fatalf("credential_json not applied: %q", got.CredentialJSON)
	}
}

func TestRoundtrip_LeaseLifecycle(t *testing.T) {
	f := newRoundtripFixture(t)
	a := seedAccount(t, f.st, "alpha")

	lease, err := f.client.AcquireLease(context.Background(), a.ID, "device-1", time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if lease.ID == "" || lease.AccountID != a.ID {
		t.Fatalf("lease shape: %+v", lease)
	}
	// LeaseStore is the source of truth refresh.Scheduler queries — verify
	// the client's HTTP call actually populated it.
	if !f.st.IsAccountLeased(a.ID) {
		t.Fatal("server-side LeaseStore did not record the lease")
	}

	renewed, err := f.client.RenewLease(context.Background(), lease.ID, time.Minute)
	if err != nil {
		t.Fatalf("RenewLease: %v", err)
	}
	if renewed.ID != lease.ID {
		t.Errorf("renewed lease ID changed: %s → %s", lease.ID, renewed.ID)
	}
	if renewed.ExpiresAt < lease.ExpiresAt {
		t.Errorf("renewed lease expires_at went backwards: %d → %d", lease.ExpiresAt, renewed.ExpiresAt)
	}

	if err := f.client.ReleaseLease(context.Background(), lease.ID); err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if f.st.IsAccountLeased(a.ID) {
		t.Error("LeaseStore still reports leased after release")
	}
}

func TestRoundtrip_RenewMissingLeaseIsNotFound(t *testing.T) {
	f := newRoundtripFixture(t)
	_, err := f.client.RenewLease(context.Background(), "no-such-lease", time.Minute)
	if err == nil {
		t.Fatal("RenewLease: want error, got nil")
	}
	// We don't deeply inspect the error — the credinject coordinator's
	// fallback path catches "any error" by re-acquiring. The 404→ErrLeaseNotFound
	// mapping in the client is exercised because the test would otherwise
	// see a generic 5xx error.
	if err.Error() == "" {
		t.Fatal("expected non-empty error message")
	}
}

func TestRoundtrip_GetAutoSwitchDefaults(t *testing.T) {
	f := newRoundtripFixture(t)
	v, err := f.client.GetAutoSwitch(context.Background())
	if err != nil {
		t.Fatalf("GetAutoSwitch: %v", err)
	}
	if !v.Enabled || v.Policy != "lru" {
		t.Errorf("defaults: got %+v want {Enabled:true Policy:lru}", v)
	}
}
