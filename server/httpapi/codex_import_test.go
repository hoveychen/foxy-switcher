package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

func TestImportCodexAddsProviderScopedAccount(t *testing.T) {
	st, dir := newTestStore(t)
	codexHome := filepath.Join(dir, "codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", codexHome)
	jwt := testCodexJWT(t, "codex-account", "codex@example.com", "plus")
	raw, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token": jwt, "access_token": jwt,
			"refresh_token": "refresh", "account_id": "codex-account",
		},
	})
	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := New(st, nil, nil, "")
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/import-codex", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	var response struct {
		Account accountView `json:"account"`
	}
	if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if response.Account.Provider != store.ProviderCodex || response.Account.AccountUUID != "codex-account" {
		t.Fatalf("response account: %+v", response.Account)
	}
	rows, err := st.ListProvider(req.Context(), store.ProviderCodex)
	if err != nil || len(rows) != 1 || rows[0].CredentialJSON == "" {
		t.Fatalf("stored Codex rows=%+v err=%v", rows, err)
	}
}

func TestImportCodexRejectedInVaultMode(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	srv.Mode = "vault"
	req := httptest.NewRequest(http.MethodPost, "/api/accounts/import-codex", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func testCodexJWT(t *testing.T, accountID, email, plan string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		"email": email, "name": "Codex User", "exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
