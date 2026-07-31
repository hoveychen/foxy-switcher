package httpserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// fakeOpenRouter records revocations so the lifecycle tests can assert that
// each admin action actually kills the device's third-party credential.
type fakeOpenRouter struct {
	revoked []string
	err     error
}

func (f *fakeOpenRouter) EnsureDeviceKey(context.Context, string) (vault.OpenRouterGrant, error) {
	return vault.OpenRouterGrant{}, errors.New("not used in these tests")
}

func (f *fakeOpenRouter) RevokeDeviceKeys(_ context.Context, deviceID string) error {
	if f.err != nil {
		return f.err
	}
	f.revoked = append(f.revoked, deviceID)
	return nil
}

type openRouterFixture struct {
	st     *store.Store
	server *httptest.Server
	or     *fakeOpenRouter
}

func newOpenRouterAdminFixture(t *testing.T) *openRouterFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	or := &fakeOpenRouter{}
	srv := New(vault.NewInProc(st), st)
	srv.OpenRouter = or
	mux := http.NewServeMux()
	mux.Handle("/agent/v1/", srv.Handler())
	srv.RegisterAPIRoutes(mux)
	tsrv := httptest.NewServer(mux)
	t.Cleanup(tsrv.Close)

	hash, _ := vaultauth.HashPassword("pw")
	if err := st.SetPasswordHash(context.Background(), hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	return &openRouterFixture{st: st, server: tsrv, or: or}
}

func (f *openRouterFixture) signIn(t *testing.T) *cookieJar {
	t.Helper()
	jar := newCookieJar(t)
	postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "pw"}).Body.Close()
	return jar
}

func (f *openRouterFixture) addDevice(t *testing.T, id string) {
	t.Helper()
	if err := f.st.InsertDevice(context.Background(), store.Device{
		ID: id, Name: id, TokenHash: vaultauth.HashToken(vaultauth.NewToken()),
		AllowClaude: true, AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
}

// An OpenRouter runtime key never presents the device token — it talks to
// OpenRouter directly. So none of the vault's own auth changes (deleting the
// row, stamping disabled_at, dropping the grant) affect it. Each of these
// actions must explicitly revoke, or the credential outlives the action that
// was supposed to stop it.

func TestDeviceRevokeKillsOpenRouterKeys(t *testing.T) {
	f := newOpenRouterAdminFixture(t)
	f.addDevice(t, "dev-1")
	jar := f.signIn(t)

	resp := postJSON(t, f.server.URL+"/admin/api/devices/revoke", jar, map[string]string{"id": "dev-1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke status = %s, want 204", resp.Status)
	}
	if len(f.or.revoked) != 1 || f.or.revoked[0] != "dev-1" {
		t.Fatalf("revoked = %v, want [dev-1]", f.or.revoked)
	}
}

func TestDeviceSuspendKillsOpenRouterKeys(t *testing.T) {
	f := newOpenRouterAdminFixture(t)
	f.addDevice(t, "dev-1")
	jar := f.signIn(t)

	resp := postJSON(t, f.server.URL+"/admin/api/devices/suspend", jar, map[string]string{"id": "dev-1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("suspend status = %s, want 204", resp.Status)
	}
	if len(f.or.revoked) != 1 || f.or.revoked[0] != "dev-1" {
		t.Fatalf("revoked = %v, want [dev-1] — a suspended device must stop spending", f.or.revoked)
	}
}

func TestWithdrawingOpenRouterGrantKillsTheKey(t *testing.T) {
	f := newOpenRouterAdminFixture(t)
	f.addDevice(t, "dev-1")
	jar := f.signIn(t)

	resp := postJSON(t, f.server.URL+"/admin/api/devices/providers", jar, map[string]any{
		"id": "dev-1", "allow_claude": true, "allow_codex": false, "allow_openrouter": false,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("providers status = %s, want 204", resp.Status)
	}
	if len(f.or.revoked) != 1 {
		t.Fatalf("revoked = %v, want the grant withdrawal to kill the key", f.or.revoked)
	}
	d, err := f.st.FindDevice(context.Background(), "dev-1")
	if err != nil || d.AllowOpenRouter {
		t.Fatalf("device = %+v err=%v, want allow_openrouter cleared", d, err)
	}
}

func TestKeepingOpenRouterGrantDoesNotRevoke(t *testing.T) {
	f := newOpenRouterAdminFixture(t)
	f.addDevice(t, "dev-1")
	jar := f.signIn(t)

	// Toggling an unrelated provider must not disturb a live OpenRouter key.
	resp := postJSON(t, f.server.URL+"/admin/api/devices/providers", jar, map[string]any{
		"id": "dev-1", "allow_claude": true, "allow_codex": true, "allow_openrouter": true,
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}
	if len(f.or.revoked) != 0 {
		t.Fatalf("revoked = %v, want none — the grant was kept", f.or.revoked)
	}
}

// TestProvidersPayloadWithoutOpenRouterPreservesTheGrant guards against a
// DevicesPage build that predates the OpenRouter toggle silently revoking every
// device's key the first time an admin flips Codex.
func TestProvidersPayloadWithoutOpenRouterPreservesTheGrant(t *testing.T) {
	f := newOpenRouterAdminFixture(t)
	f.addDevice(t, "dev-1")
	jar := f.signIn(t)

	resp := postJSON(t, f.server.URL+"/admin/api/devices/providers", jar, map[string]any{
		"id": "dev-1", "allow_claude": true, "allow_codex": true, // no allow_openrouter
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}
	if len(f.or.revoked) != 0 {
		t.Fatalf("an omitted allow_openrouter revoked the key: %v", f.or.revoked)
	}
	d, err := f.st.FindDevice(context.Background(), "dev-1")
	if err != nil || !d.AllowOpenRouter {
		t.Fatalf("device = %+v err=%v, want allow_openrouter preserved", d, err)
	}
}

// TestDeviceRevokeAbortsWhenTheKeyCannotBeKilled: deleting the device row would
// erase the only record of which upstream keys belong to it, so a failed revoke
// must stop the whole operation rather than strand a live credential.
func TestDeviceRevokeAbortsWhenTheKeyCannotBeKilled(t *testing.T) {
	f := newOpenRouterAdminFixture(t)
	f.addDevice(t, "dev-1")
	f.or.err = errors.New("openrouter unreachable")
	jar := f.signIn(t)

	resp := postJSON(t, f.server.URL+"/admin/api/devices/revoke", jar, map[string]string{"id": "dev-1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %s, want 500", resp.Status)
	}
	if _, err := f.st.FindDevice(context.Background(), "dev-1"); err != nil {
		t.Fatalf("device row was deleted despite the failed revoke: %v", err)
	}
}

// A build with no OpenRouter service configured must still be able to run the
// device lifecycle — nothing could have been derived, so there is nothing to
// revoke.
func TestDeviceLifecycleWorksWithoutOpenRouterConfigured(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := New(vault.NewInProc(st), st) // srv.OpenRouter left nil
	mux := http.NewServeMux()
	srv.RegisterAPIRoutes(mux)
	tsrv := httptest.NewServer(mux)
	t.Cleanup(tsrv.Close)

	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	if err := st.SetPasswordHash(ctx, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if err := st.InsertDevice(ctx, store.Device{
		ID: "dev-1", Name: "d", TokenHash: vaultauth.HashToken(vaultauth.NewToken()),
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	jar := newCookieJar(t)
	postJSON(t, tsrv.URL+"/admin/api/login", jar, map[string]string{"password": "pw"}).Body.Close()

	resp := postJSON(t, tsrv.URL+"/admin/api/devices/revoke", jar, map[string]string{"id": "dev-1"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %s, want 204", resp.Status)
	}
}
