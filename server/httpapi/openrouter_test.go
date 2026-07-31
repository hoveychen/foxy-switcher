package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
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

// fakeORKeyReader stands in for OpenRouter so key-kind detection needs no
// network. Without it these tests would reach openrouter.ai for real.
type fakeORKeyReader struct {
	provisioning bool
	err          error
	caps         *openrouter.Capabilities
	seenKeys     *[]string
}

func (f *fakeORKeyReader) KeySelf(context.Context) (openrouter.KeyInfo, error) {
	if f.err != nil {
		return openrouter.KeyInfo{}, f.err
	}
	return openrouter.KeyInfo{IsProvisioning: f.provisioning}, nil
}

func (f *fakeORKeyReader) CheckKey(context.Context) (openrouter.Capabilities, error) {
	if f.err != nil {
		return openrouter.Capabilities{}, f.err
	}
	if f.caps != nil {
		return *f.caps, nil
	}
	return openrouter.Capabilities{
		KeyValid: true, CanMintKeys: f.provisioning, Detail: "ok",
	}, nil
}

// orEnv bundles the knobs a test needs over the fake upstream.
type orEnv struct {
	keys   *recordingKeys
	reader *fakeORKeyReader
	// keysSeen records every key detection ran against.
	keysSeen []string
}

func newOpenRouterServer(t *testing.T) (*Server, *recordingKeys) {
	srv, env := newOpenRouterServerEnv(t)
	return srv, env.keys
}

func newOpenRouterServerEnv(t *testing.T) (*Server, *orEnv) {
	t.Helper()
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	keys := &recordingKeys{}
	srv.SetOpenRouterKeys(keys)
	env := &orEnv{keys: keys, reader: &fakeORKeyReader{provisioning: true}}
	srv.SetOpenRouterClientFactory(func(apiKey string) openRouterKeyReader {
		env.keysSeen = append(env.keysSeen, apiKey)
		return env.reader
	})
	return srv, env
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
		"api_key":        "sk-or-mgmt",
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
	if !got.OpenRouter.HasAPIKey {
		t.Fatal("HasAPIKey = false after supplying one")
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
	cred, err := srv.Store.OpenRouterCredential(context.Background(), created.ID)
	if err != nil || cred.APIKey != "sk-or-mgmt" {
		t.Fatalf("stored key = %+v, %v", cred, err)
	}
}

func TestCreateOpenRouterAccountValidation(t *testing.T) {
	srv, _ := newOpenRouterServer(t)
	for name, body := range map[string]map[string]any{
		"no name":           {"api_key": "k"},
		"no management key": {"name": "pool"},
		"bad limit reset":   {"name": "pool", "api_key": "k", "limit_reset": "hourly"},
		"negative limit":    {"name": "pool", "api_key": "k", "limit_usd": -1},
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
			"limit_usd":      25, "limit_reset": "monthly", "api_key": "sk-or-mgmt-2",
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

// --- key-kind detection ----------------------------------------------------

// The admin pastes a key; OpenRouter says what it is. Making the admin declare
// it would let a wrong answer through, and a wrong answer means either deriving
// against a key that can't mint, or handing out a provisioning key.
func TestKeyKindIsDetectedNotDeclared(t *testing.T) {
	for name, provisioning := range map[string]bool{
		"provisioning": true,
		"ordinary":     false,
	} {
		t.Run(name, func(t *testing.T) {
			srv, env := newOpenRouterServerEnv(t)
			env.reader.provisioning = provisioning

			got := decodeAccount(t, doJSON(t, srv, http.MethodPost, "/api/accounts/openrouter", map[string]any{
				"name": "pool", "api_key": "sk-or-pasted",
				"allowed_models": []string{"a/b"}, "limit_usd": 5, "limit_reset": "monthly",
			}))
			if got.OpenRouter.IsProvisioning != provisioning {
				t.Fatalf("IsProvisioning = %v, want %v (detected)", got.OpenRouter.IsProvisioning, provisioning)
			}
			cred, err := srv.Store.OpenRouterCredential(context.Background(), got.ID)
			if err != nil || cred.IsProvisioning != provisioning {
				t.Fatalf("stored cred = %+v, %v", cred, err)
			}
			// Detection must run against the key being saved.
			if len(env.keysSeen) == 0 || env.keysSeen[0] != "sk-or-pasted" {
				t.Fatalf("detection ran against %v", env.keysSeen)
			}
		})
	}
}

// A typo must fail at save time, not at every device's first request.
func TestRejectedKeyFailsAtSaveTime(t *testing.T) {
	srv, env := newOpenRouterServerEnv(t)
	env.reader.err = openrouter.ErrUnauthorized

	w := doJSON(t, srv, http.MethodPost, "/api/accounts/openrouter", map[string]any{
		"name": "pool", "api_key": "sk-or-typo", "allowed_models": []string{"a/b"},
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rejected this key") {
		t.Fatalf("body = %s, want a message the admin can act on", w.Body.String())
	}
	// And nothing half-configured is left behind.
	list := doJSON(t, srv, http.MethodGet, "/api/accounts", nil)
	var out struct{ Accounts []accountView }
	_ = json.Unmarshal(list.Body.Bytes(), &out)
	for _, a := range out.Accounts {
		if a.OpenRouter != nil && a.OpenRouter.HasAPIKey {
			t.Fatalf("a rejected key was stored anyway: %+v", a.OpenRouter)
		}
	}
}

// Rotating the key re-detects: the replacement may be a different kind.
func TestRotatingTheKeyReDetectsItsKind(t *testing.T) {
	srv, env := newOpenRouterServerEnv(t)
	env.reader.provisioning = true
	acc := createAccount(t, srv)
	if !acc.OpenRouter.IsProvisioning {
		t.Fatal("setup: expected a provisioning key")
	}

	env.reader.provisioning = false
	w := doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/openrouter", map[string]any{
			"allowed_models": []string{"openai/gpt-oss-120b", "deepseek/deepseek-v4-flash"},
			"limit_usd":      25, "limit_reset": "monthly", "api_key": "sk-or-plain",
		})
	got := decodeAccount(t, w)
	if got.OpenRouter.IsProvisioning {
		t.Fatal("the replacement key's kind was not re-detected")
	}
}

// --- balance in the view ---------------------------------------------------

// A never-polled balance must read as unknown, not $0 — $0 says "broke".
func TestUnpolledBalanceIsOmittedNotZero(t *testing.T) {
	srv, _ := newOpenRouterServerEnv(t)
	acc := createAccount(t, srv)
	if acc.OpenRouter.Credit != nil {
		t.Fatalf("Credit = %+v, want nil until polled", acc.OpenRouter.Credit)
	}
	if acc.OpenRouter.OutOfCredit {
		t.Fatal("an unpolled account must not be flagged out of credit")
	}
}

func TestPolledBalanceAndOutOfCreditFlag(t *testing.T) {
	srv, _ := newOpenRouterServerEnv(t)
	ctx := context.Background()
	acc := createAccount(t, srv)

	if err := srv.Store.SetOpenRouterCredit(ctx, acc.ID, 510, 23.94); err != nil {
		t.Fatalf("credit: %v", err)
	}
	list := doJSON(t, srv, http.MethodGet, "/api/accounts", nil)
	var out struct{ Accounts []accountView }
	if err := json.Unmarshal(list.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	or := out.Accounts[0].OpenRouter
	if or.Credit == nil || or.Credit.Remaining != 23.94 || or.Credit.Total != 510 {
		t.Fatalf("Credit = %+v", or.Credit)
	}
	if or.OutOfCredit {
		t.Fatal("$23.94 is not out of credit")
	}

	// Drain it: the badge must agree with the selector's own verdict, or the UI
	// and the routing decision disagree.
	if err := srv.Store.SetOpenRouterCredit(ctx, acc.ID, 510, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	list = doJSON(t, srv, http.MethodGet, "/api/accounts", nil)
	_ = json.Unmarshal(list.Body.Bytes(), &out)
	if !out.Accounts[0].OpenRouter.OutOfCredit {
		t.Fatal("a drained account must be flagged out of credit")
	}
}

// The probe is the admin's "is this working?" button, so it refreshes both the
// detected kind and the balance — a stale figure beside a green tick is
// confusing, and a stale kind means deriving against a key that can't mint.
func TestProbeRefreshesKindAndBalance(t *testing.T) {
	srv, env := newOpenRouterServerEnv(t)
	ctx := context.Background()
	env.reader.provisioning = false
	acc := createAccount(t, srv)
	if acc.OpenRouter.IsProvisioning {
		t.Fatal("setup: expected an ordinary key")
	}

	// The same key now reports as provisioning (upgraded upstream).
	env.reader.caps = &openrouter.Capabilities{
		KeyValid: true, CanMintKeys: true,
		CreditKnown: true, CreditTotal: 100, CreditRemaining: 42, Detail: "ok",
	}
	w := doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/openrouter/check", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var probe map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &probe); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if probe["is_provisioning"] != true || probe["credit_remaining"] != 42.0 {
		t.Fatalf("probe = %+v", probe)
	}
	cred, _ := srv.Store.OpenRouterCredential(ctx, acc.ID)
	if !cred.IsProvisioning {
		t.Fatal("the probe did not re-record the upgraded kind")
	}
	if cred.CreditRemaining != 42 || cred.CreditTotal != 100 {
		t.Fatalf("the probe did not refresh the balance: %+v", cred)
	}
}
