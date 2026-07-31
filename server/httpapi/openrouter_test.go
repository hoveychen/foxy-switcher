package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// recordingKeys captures which accounts had their derived keys revoked, so the
// tests can pin exactly when a policy edit invalidates outstanding keys.
type recordingKeys struct {
	revoked []int64
	err     error
}

func (r *recordingKeys) RevokeAccountKeys(_ context.Context, accountID int64) error {
	if r.err != nil {
		return r.err
	}
	r.revoked = append(r.revoked, accountID)
	return nil
}

func newOpenRouterServer(t *testing.T) (*Server, *recordingKeys) {
	t.Helper()
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	keys := &recordingKeys{}
	srv.SetOpenRouterKeys(keys)
	return srv, keys
}

func doJSON(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

func decodeAccount(t *testing.T, w *httptest.ResponseRecorder) accountView {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Account accountView `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out.Account
}

func createAccount(t *testing.T, srv *Server) accountView {
	t.Helper()
	return decodeAccount(t, doJSON(t, srv, http.MethodPost, "/api/accounts/openrouter", map[string]any{
		"name":           "pool",
		"allowed_models": []string{"openai/gpt-oss-120b", "deepseek/deepseek-v4-flash"},
		"limit_usd":      25,
		"limit_reset":    "monthly",
		"management_key": "sk-or-mgmt",
	}))
}

func TestCreateOpenRouterAccount(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	got := createAccount(t, srv)

	if got.Provider != store.ProviderOpenRouter || got.Name != "pool" {
		t.Fatalf("account = %+v", got)
	}
	if got.OpenRouter == nil {
		t.Fatal("response carries no openrouter config")
	}
	// Normalised on the way in: sorted, so the stored document is stable and an
	// unchanged allowlist doesn't look like an edit.
	want := []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"}
	if strings.Join(got.OpenRouter.AllowedModels, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowedModels = %v, want %v", got.OpenRouter.AllowedModels, want)
	}
	if !got.OpenRouter.HasManagementKey {
		t.Fatal("HasManagementKey = false after supplying one")
	}
	if got.OpenRouter.LimitUSD != 25 || got.OpenRouter.LimitReset != "monthly" {
		t.Fatalf("limits = %+v", got.OpenRouter)
	}
}

// TestManagementKeyIsWriteOnlyOverTheAPI is the security regression test for
// the admin surface: the key can mint and revoke runtime keys for every device,
// so no read path may ever return it.
func TestManagementKeyIsWriteOnlyOverTheAPI(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	created := createAccount(t, srv)

	list := doJSON(t, srv, http.MethodGet, "/api/accounts", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list status = %d: %s", list.Code, list.Body.String())
	}
	for name, body := range map[string]string{
		"create response": "",
		"account list":    list.Body.String(),
	} {
		if name == "create response" {
			raw, _ := json.Marshal(created)
			body = string(raw)
		}
		if strings.Contains(body, "sk-or-mgmt") {
			t.Fatalf("%s leaks the management key: %s", name, body)
		}
	}

	// It must nonetheless be usable internally.
	key, err := srv.Store.OpenRouterManagementKey(context.Background(), created.ID)
	if err != nil || key != "sk-or-mgmt" {
		t.Fatalf("stored key = %q, %v", key, err)
	}
}

func TestCreateOpenRouterAccountValidation(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	for name, body := range map[string]map[string]any{
		"no name":           {"management_key": "k"},
		"no management key": {"name": "pool"},
		"bad limit reset":   {"name": "pool", "management_key": "k", "limit_reset": "hourly"},
		"negative limit":    {"name": "pool", "management_key": "k", "limit_usd": -1},
	} {
		t.Run(name, func(t *testing.T) {
			w := doJSON(t, srv, http.MethodPost, "/api/accounts/openrouter", body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
			}
		})
	}
}

// TestPolicyEditRevokesOutstandingKeys: every derived key has the old allowlist
// and cap baked into its own upstream guardrail, so an edit that didn't revoke
// them would silently apply to new devices only.
func TestPolicyEditRevokesOutstandingKeys(t *testing.T) {
	srv, keys := newOpenRouterServer(t)
	acc := createAccount(t, srv)
	path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10) + "/openrouter"

	for name, body := range map[string]map[string]any{
		"models changed": {
			"allowed_models": []string{"deepseek/deepseek-v4-flash"},
			"limit_usd":      25, "limit_reset": "monthly",
		},
		"cap changed": {
			"allowed_models": []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"},
			"limit_usd":      50, "limit_reset": "monthly",
		},
		"reset changed": {
			"allowed_models": []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"},
			"limit_usd":      25, "limit_reset": "weekly",
		},
		"workspace changed": {
			"allowed_models": []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"},
			"limit_usd":      25, "limit_reset": "monthly", "workspace_id": "ws-2",
		},
		"management key rotated": {
			"allowed_models": []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"},
			"limit_usd":      25, "limit_reset": "monthly", "management_key": "sk-or-mgmt-2",
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv, keys := newOpenRouterServer(t)
			acc := createAccount(t, srv)
			path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10) + "/openrouter"
			if w := doJSON(t, srv, http.MethodPost, path, body); w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if len(keys.revoked) != 1 || keys.revoked[0] != acc.ID {
				t.Fatalf("revoked = %v, want [%d]", keys.revoked, acc.ID)
			}
		})
	}

	// A no-op save (same policy, models re-pasted in a different order) must NOT
	// churn every device's key.
	if w := doJSON(t, srv, http.MethodPost, path, map[string]any{
		"allowed_models": []string{"openai/gpt-oss-120b", "deepseek/deepseek-v4-flash"},
		"limit_usd":      25, "limit_reset": "monthly",
	}); w.Code != http.StatusOK {
		t.Fatalf("no-op save status = %d: %s", w.Code, w.Body.String())
	}
	if len(keys.revoked) != 0 {
		t.Fatalf("a no-op save revoked keys: %v", keys.revoked)
	}
}

// If the outstanding keys can't be killed, the edit must not land — otherwise
// the stored policy and the live keys silently disagree.
func TestPolicyEditAbortsWhenRevocationFails(t *testing.T) {
	srv, keys := newOpenRouterServer(t)
	acc := createAccount(t, srv)
	keys.err = errAlwaysFails{}

	path := "/api/accounts/" + strconv.FormatInt(acc.ID, 10) + "/openrouter"
	w := doJSON(t, srv, http.MethodPost, path, map[string]any{
		"allowed_models": []string{"only/one"}, "limit_usd": 25, "limit_reset": "monthly",
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	cfg, err := srv.Store.OpenRouterConfig(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(cfg.AllowedModels) != 2 {
		t.Fatalf("policy was written despite the failed revoke: %v", cfg.AllowedModels)
	}
}

type errAlwaysFails struct{}

func (errAlwaysFails) Error() string { return "upstream unreachable" }

func TestUpdateRejectsNonOpenRouterAccount(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	claude := &store.Account{Name: "claude", AccessToken: "at", RefreshToken: "rt", Email: "c@example.com"}
	if err := srv.Store.Upsert(context.Background(), claude); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	w := doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(claude.ID, 10)+"/openrouter",
		map[string]any{"allowed_models": []string{"a/b"}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestCheckRequiresAManagementKey(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	acc := &store.Account{
		Provider: store.ProviderOpenRouter, Name: "bare", AccountUUID: "openrouter:bare",
	}
	if err := srv.Store.Upsert(context.Background(), acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	w := doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/openrouter/check", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
}

// Non-OpenRouter accounts must not grow an openrouter block in the list — the
// frontend keys its provider-specific UI off its presence.
func TestAccountListOmitsOpenRouterBlockForOtherProviders(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	claude := &store.Account{Name: "claude", AccessToken: "at", RefreshToken: "rt", Email: "c@example.com"}
	if err := srv.Store.Upsert(context.Background(), claude); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	createAccount(t, srv)

	w := doJSON(t, srv, http.MethodGet, "/api/accounts", nil)
	var out struct {
		Accounts []accountView `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Accounts) != 2 {
		t.Fatalf("accounts = %d, want 2", len(out.Accounts))
	}
	for _, a := range out.Accounts {
		switch a.Provider {
		case store.ProviderOpenRouter:
			if a.OpenRouter == nil {
				t.Fatal("OpenRouter account has no openrouter block")
			}
		default:
			if a.OpenRouter != nil {
				t.Fatalf("%s account carries an openrouter block: %+v", a.Provider, a.OpenRouter)
			}
		}
	}
}

// A spend cap with no reset interval is a policy OpenRouter cannot express:
// /guardrails rejects it outright (verified live). Catching it at save time
// means the operator learns immediately, instead of every device's derivation
// failing later with a raw upstream validation error.
func TestSpendCapWithoutResetIntervalIsRejectedAtSaveTime(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	w := doJSON(t, srv, http.MethodPost, "/api/accounts/openrouter", map[string]any{
		"name": "pool", "management_key": "sk-or-mgmt",
		"allowed_models": []string{"a/b"},
		"limit_usd":      25, "limit_reset": "",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "limit_reset") {
		t.Fatalf("error should name the field to fix: %s", w.Body.String())
	}

	// No cap at all is fine without a reset interval.
	if w := doJSON(t, srv, http.MethodPost, "/api/accounts/openrouter", map[string]any{
		"name": "pool2", "management_key": "sk-or-mgmt",
		"allowed_models": []string{"a/b"},
	}); w.Code != http.StatusOK {
		t.Fatalf("uncapped account rejected: %d %s", w.Code, w.Body.String())
	}
}
