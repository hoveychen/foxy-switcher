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

// grantingDeepSeek answers EnsureDeviceGrant with a per-device grant so the
// endpoint test can assert *which* device the server asked about.
type grantingDeepSeek struct {
	asked []string
	grant vault.DeepSeekGrant
	err   error
}

func (g *grantingDeepSeek) EnsureDeviceGrant(_ context.Context, deviceID string) (vault.DeepSeekGrant, error) {
	g.asked = append(g.asked, deviceID)
	if g.err != nil {
		return vault.DeepSeekGrant{}, g.err
	}
	out := g.grant
	out.APIKey = "sk-ds-for-" + deviceID
	return out, nil
}

func newDeepSeekFixture(t *testing.T, ds DeepSeekGrantService) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := New(vault.NewInProc(st), st)
	srv.DeepSeek = ds
	mux := http.NewServeMux()
	mux.Handle("/agent/v1/", srv.Handler())
	tsrv := httptest.NewServer(mux)
	t.Cleanup(tsrv.Close)
	return st, tsrv
}

func pairedDeepSeekDevice(t *testing.T, st *store.Store, id string) string {
	t.Helper()
	token := vaultauth.NewToken()
	if err := st.InsertDevice(context.Background(), store.Device{
		ID: id, Name: id, TokenHash: vaultauth.HashToken(token), AllowDeepSeek: true,
	}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	return token
}

// The device id comes from the bearer token, never from the request, so a
// device can only ever fetch its own grant.
func TestDeepSeekConfigServesTheBearersOwnGrant(t *testing.T) {
	svc := &grantingDeepSeek{grant: vault.DeepSeekGrant{
		AccountID: 7, AccountName: "pool", BaseURL: vault.DefaultDeepSeekBaseURL,
	}}
	st, tsrv := newDeepSeekFixture(t, svc)
	token := pairedDeepSeekDevice(t, st, "dev-a")
	_ = pairedDeepSeekDevice(t, st, "dev-b")

	resp := getWithBearer(t, tsrv.URL+"/agent/v1/deepseek/config", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var grant vault.DeepSeekGrant
	if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if grant.APIKey != "sk-ds-for-dev-a" {
		t.Fatalf("APIKey = %q, want the bearer's own key", grant.APIKey)
	}
	if grant.AccountID != 7 || grant.BaseURL != vault.DefaultDeepSeekBaseURL {
		t.Fatalf("grant = %+v", grant)
	}
	if len(svc.asked) != 1 || svc.asked[0] != "dev-a" {
		t.Fatalf("service was asked about %v, want [dev-a]", svc.asked)
	}
}

// Both "not granted" and "no account configured" collapse to 204: the device's
// action is identical, and an unprivileged device learns nothing about whether
// the vault has DeepSeek accounts at all.
func TestDeepSeekConfigNoGrantIs204(t *testing.T) {
	for name, err := range map[string]error{
		"not granted":  selector.ErrNoAvailable,
		"no account":   vault.ErrNoDeepSeekAccount,
		"wrapped":      errors.Join(vault.ErrNoDeepSeekAccount, errors.New("2 skipped")),
		"unconfigured": nil, // service left nil entirely
	} {
		t.Run(name, func(t *testing.T) {
			var svc DeepSeekGrantService
			if err != nil {
				svc = &grantingDeepSeek{err: err}
			}
			st, tsrv := newDeepSeekFixture(t, svc)
			token := pairedDeepSeekDevice(t, st, "dev-a")
			resp := getWithBearer(t, tsrv.URL+"/agent/v1/deepseek/config", token)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", resp.StatusCode)
			}
		})
	}
}

// A real fault must not masquerade as "nothing for you": the device would
// silently run without DeepSeek and nobody would know why.
func TestDeepSeekConfigRealFaultIs502(t *testing.T) {
	st, tsrv := newDeepSeekFixture(t, &grantingDeepSeek{err: errors.New("database is locked")})
	token := pairedDeepSeekDevice(t, st, "dev-a")
	resp := getWithBearer(t, tsrv.URL+"/agent/v1/deepseek/config", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestDeepSeekConfigRequiresBearer(t *testing.T) {
	_, tsrv := newDeepSeekFixture(t, &grantingDeepSeek{})
	resp := getWithBearer(t, tsrv.URL+"/agent/v1/deepseek/config", "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
}
