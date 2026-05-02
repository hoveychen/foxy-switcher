package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAbout_ReflectsModeAndVaultURL covers the Step 7 Settings → Vault
// surface. The frontend's vault card branches on AboutResponse.mode, so
// regressing this field would silently render the wrong card on existing
// installs.
func TestAbout_ReflectsModeAndVaultURL(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	srv.Mode = "vault"
	srv.VaultURL = "https://vault.example.com"

	req := httptest.NewRequest(http.MethodGet, "/api/about", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	var got AboutResponse
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != "vault" {
		t.Errorf("mode: got %q want vault", got.Mode)
	}
	if got.VaultURL != "https://vault.example.com" {
		t.Errorf("vault_url: got %q", got.VaultURL)
	}
}

// TestAbout_DefaultsAreEmptyStrings verifies a server with the fields
// unset (a test fixture or a misconfigured deployment) emits "" rather
// than crashing or producing unexpected JSON. The frontend already
// treats empty as "combined", so this is the contract.
func TestAbout_DefaultsAreEmptyStrings(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/about", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var got AboutResponse
	_ = json.NewDecoder(w.Body).Decode(&got)
	if got.Mode != "" {
		t.Errorf("default mode: got %q want \"\"", got.Mode)
	}
	if got.VaultURL != "" {
		t.Errorf("default vault_url: got %q", got.VaultURL)
	}
}
