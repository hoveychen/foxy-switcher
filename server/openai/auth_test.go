package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

func fakeJWT(t *testing.T, accountID, email, name, plan string, exp int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, err := json.Marshal(map[string]any{
		"email": email, "name": name, "exp": exp,
		authClaimNamespace: map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func authJSON(t *testing.T, accountID, accessSuffix string) []byte {
	t.Helper()
	return authJSONExpiring(t, accountID, accessSuffix, time.Now().Add(time.Hour))
}

// authJSONExpiring builds an auth.json whose access_token expires at
// accessExp, so tests can model a device holding a credential the pool has
// already rotated past (accessExp in the past) versus a genuinely fresher one.
func authJSONExpiring(t *testing.T, accountID, accessSuffix string, accessExp time.Time) []byte {
	t.Helper()
	id := fakeJWT(t, accountID, accountID+"@example.com", "Codex User", "pro", accessExp.Add(time.Hour).Unix())
	access := fakeJWT(t, accountID, "", "", "", accessExp.Unix()) + accessSuffix
	raw, err := json.Marshal(map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": id, "access_token": access,
			"refresh_token": "refresh-" + accountID, "account_id": accountID,
		},
		"last_refresh": "2026-07-17T00:00:00Z",
		"future_field": map[string]any{"preserve": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseAuthFileBuildsCodexAccountAndPreservesUnknownFields(t *testing.T) {
	auth, err := ParseAuthFile(authJSON(t, "acct-1", ""))
	if err != nil {
		t.Fatalf("ParseAuthFile: %v", err)
	}
	account, err := auth.Account()
	if err != nil {
		t.Fatalf("Account: %v", err)
	}
	if account.Provider != store.ProviderCodex || account.AccountUUID != "acct-1" {
		t.Fatalf("identity mismatch: %+v", account)
	}
	if account.Email != "acct-1@example.com" || account.Plan != "Codex Pro" {
		t.Fatalf("profile mismatch: %+v", account)
	}
	var roundTrip map[string]json.RawMessage
	raw, _ := auth.Marshal()
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if _, ok := roundTrip["future_field"]; !ok {
		t.Fatal("unknown auth.json field was dropped")
	}
}

func TestRefreshAndFetchUsageFollowCodexProtocol(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != codexOAuthClientID || body["grant_type"] != "refresh_token" || body["refresh_token"] != "refresh-acct-1" {
				t.Errorf("refresh body: %+v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token":  fakeJWT(t, "acct-1", "", "", "", time.Now().Add(3*time.Hour).Unix()),
				"refresh_token": "rotated-refresh",
			})
		case "/usage":
			if r.Header.Get("Authorization") == "" || r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
				t.Errorf("usage headers: %+v", r.Header)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plan_type": "plus",
				"rate_limit": map[string]any{
					"primary_window":   map[string]any{"used_percent": 42.0, "reset_at": int64(1900000000)},
					"secondary_window": map[string]any{"used_percent": 7.0, "reset_at": int64(1900003600)},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldTokenURL, oldUsageURL, oldClient := TokenURL, UsageURL, httpClient
	TokenURL, UsageURL, httpClient = server.URL+"/oauth/token", server.URL+"/usage", server.Client()
	defer func() { TokenURL, UsageURL, httpClient = oldTokenURL, oldUsageURL, oldClient }()

	auth, _ := ParseAuthFile(authJSON(t, "acct-1", ""))
	if err := Refresh(context.Background(), auth); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if auth.Tokens.RefreshToken != "rotated-refresh" || auth.LastRefresh == "" {
		t.Fatalf("refresh not applied: %+v", auth.Tokens)
	}
	usage, err := FetchUsage(context.Background(), auth.Tokens.AccessToken, auth.Tokens.AccountID)
	if err != nil {
		t.Fatalf("FetchUsage: %v", err)
	}
	if usage.Primary == nil || usage.Primary.UsedPercent != 42 || usage.Secondary == nil || usage.Secondary.UsedPercent != 7 {
		t.Fatalf("usage mismatch: %+v", usage)
	}
}

func TestManagerInjectsReverseSyncsAndRestoresNativeAuth(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	native := authJSON(t, "native", "")
	if err := os.WriteFile(authPath, native, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	managedAuth, _ := ParseAuthFile(authJSON(t, "managed", ""))
	managed, _ := managedAuth.Account()
	if err := st.Upsert(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	m := NewManager(st, authPath, nil)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	injected, _ := os.ReadFile(authPath)
	parsed, err := ParseAuthFile(injected)
	if err != nil || parsed.Tokens.AccountID != "managed" {
		t.Fatalf("injected auth = %q, %v", parsed.Tokens.AccountID, err)
	}

	rotated := authJSON(t, "managed", "-rotated")
	if err := os.WriteFile(authPath, rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reverse-sync reconcile: %v", err)
	}
	stored, _ := st.Get(context.Background(), managed.ID)
	rotatedAuth, _ := ParseAuthFile(rotated)
	if stored.AccessToken != rotatedAuth.Tokens.AccessToken {
		t.Fatal("Codex CLI token rotation was not reverse-synced")
	}

	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, _ := os.Open(authPath)
	defer restored.Close()
	got, _ := io.ReadAll(restored)
	var gotJSON, nativeJSON any
	_ = json.Unmarshal(got, &gotJSON)
	_ = json.Unmarshal(native, &nativeJSON)
	if !jsonEqual(gotJSON, nativeJSON) {
		t.Fatal("native auth.json was not restored")
	}
}

func TestManagerRestoresDirectKeyringCredentials(t *testing.T) {
	dir := t.TempDir()
	kr := &memoryKeyring{}
	storage := &directKeyringStorage{
		codexHome: dir, authPath: filepath.Join(dir, "auth.json"), keyring: kr,
	}
	native := authJSON(t, "native-keyring", "")
	if err := storage.Save(native); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	managedAuth, _ := ParseAuthFile(authJSON(t, "managed-keyring", ""))
	managed, _ := managedAuth.Account()
	if err := st.Upsert(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	m := NewManagerWithStorage(st, storage, nil)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	injected, found, err := storage.Load()
	if err != nil || !found {
		t.Fatalf("load injected: %v, %v", found, err)
	}
	parsed, _ := ParseAuthFile(injected)
	if parsed.Tokens.AccountID != "managed-keyring" {
		t.Fatalf("injected account = %q", parsed.Tokens.AccountID)
	}
	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, found, err := storage.Load()
	if err != nil || !found {
		t.Fatalf("load restored: %v, %v", found, err)
	}
	parsed, _ = ParseAuthFile(restored)
	if parsed.Tokens.AccountID != "native-keyring" {
		t.Fatalf("restored account = %q", parsed.Tokens.AccountID)
	}
}

func jsonEqual(a, b any) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(aa) == string(bb)
}
