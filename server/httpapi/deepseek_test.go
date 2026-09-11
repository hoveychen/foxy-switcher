package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/deepseek"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// fakeDSReader stands in for the DeepSeek platform so key validation needs no
// network. Without it these tests would reach api.deepseek.com for real.
type fakeDSReader struct {
	bal deepseek.Balance
	err error
}

func (f *fakeDSReader) Balance(context.Context) (deepseek.Balance, error) {
	return f.bal, f.err
}

type dsEnv struct {
	reader *fakeDSReader
	// keysSeen records every key the server validated.
	keysSeen []string
}

func newDeepSeekServer(t *testing.T) (*Server, *store.Store, *dsEnv) {
	t.Helper()
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	env := &dsEnv{reader: &fakeDSReader{
		bal: deepseek.Balance{Available: true, Amount: 110.5, Currency: "CNY"},
	}}
	srv.SetDeepSeekClientFactory(func(apiKey string) deepSeekBalanceReader {
		env.keysSeen = append(env.keysSeen, apiKey)
		return env.reader
	})
	return srv, st, env
}

func createDeepSeekAccount(t *testing.T, srv *Server) accountView {
	t.Helper()
	return decodeAccount(t, doJSON(t, srv, http.MethodPost, "/api/accounts/deepseek", map[string]any{
		"name":    "ds-pool",
		"api_key": "sk-ds-console",
	}))
}

func TestCreateDeepSeekAccount(t *testing.T) {
	srv, st, env := newDeepSeekServer(t)
	acc := createDeepSeekAccount(t, srv)

	if acc.Provider != store.ProviderDeepSeek || acc.Name != "ds-pool" {
		t.Fatalf("account = %+v", acc)
	}
	if acc.DeepSeek == nil {
		t.Fatal("the response must carry the deepseek view")
	}
	if !acc.DeepSeek.HasAPIKey || !acc.DeepSeek.Shared {
		t.Fatalf("deepseek view = %+v, want a configured, shared key", acc.DeepSeek)
	}
	// The validation call already knew the balance, so the card must show a
	// real figure rather than "unknown" until the poller's first tick.
	if acc.DeepSeek.Balance == nil || acc.DeepSeek.Balance.Amount != 110.5 ||
		acc.DeepSeek.Balance.Currency != "CNY" || !acc.DeepSeek.Balance.Available {
		t.Fatalf("balance = %+v, want the figure from the validation call", acc.DeepSeek.Balance)
	}
	if acc.DeepSeek.OutOfBalance {
		t.Fatal("a funded account must not be flagged out of balance")
	}
	if len(env.keysSeen) != 1 || env.keysSeen[0] != "sk-ds-console" {
		t.Fatalf("validated %v, want the pasted key exactly once", env.keysSeen)
	}

	// The key is stored, and never echoed back.
	cred, err := st.DeepSeekCredential(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if cred.APIKey != "sk-ds-console" {
		t.Fatalf("stored key = %q", cred.APIKey)
	}
	if strings.Contains(mustJSON(t, acc), "sk-ds-console") {
		t.Fatal("the API key must never be echoed back to the UI")
	}
}

// A key DeepSeek rejects must fail at save time, not at every device's first
// request.
func TestCreateDeepSeekAccountRejectsBadKey(t *testing.T) {
	srv, st, env := newDeepSeekServer(t)
	env.reader.err = deepseek.ErrUnauthorized

	w := doJSON(t, srv, http.MethodPost, "/api/accounts/deepseek", map[string]any{
		"name": "ds-pool", "api_key": "sk-typo",
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rejected this key") {
		t.Fatalf("error must name the cause: %s", w.Body.String())
	}
	accs, err := st.ListProvider(context.Background(), store.ProviderDeepSeek)
	if err != nil {
		t.Fatalf("ListProvider: %v", err)
	}
	if len(accs) != 0 {
		t.Fatalf("a rejected key must leave no account behind, got %d", len(accs))
	}
}

func TestCreateDeepSeekAccountRequiresNameAndKey(t *testing.T) {
	srv, _, _ := newDeepSeekServer(t)
	for _, body := range []map[string]any{
		{"api_key": "sk-ds"},
		{"name": "ds-pool"},
		{"name": "  ", "api_key": "sk-ds"},
		{"name": "ds-pool", "api_key": "   "},
	} {
		w := doJSON(t, srv, http.MethodPost, "/api/accounts/deepseek", body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%v: status = %d, want 400", body, w.Code)
		}
	}
}

func TestUpdateDeepSeekAccountRotatesTheKey(t *testing.T) {
	srv, st, env := newDeepSeekServer(t)
	acc := createDeepSeekAccount(t, srv)

	env.reader.bal = deepseek.Balance{Available: true, Amount: 5, Currency: "USD"}
	w := doJSON(t, srv, http.MethodPost, "/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/deepseek",
		map[string]any{"api_key": "sk-ds-rotated"})
	updated := decodeAccount(t, w)
	if updated.DeepSeek == nil || updated.DeepSeek.Balance == nil ||
		updated.DeepSeek.Balance.Currency != "USD" {
		t.Fatalf("rotation must refresh the balance too: %+v", updated.DeepSeek)
	}
	cred, err := st.DeepSeekCredential(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if cred.APIKey != "sk-ds-rotated" {
		t.Fatalf("stored key = %q, want the rotated one", cred.APIKey)
	}
}

func TestUpdateDeepSeekAccountRefusesForeignProvider(t *testing.T) {
	srv, st, _ := newDeepSeekServer(t)
	claude := &store.Account{
		Provider: store.ProviderClaude, Name: "claude", AccountUUID: "c-1",
		AccessToken: "at", Status: store.StatusActive,
	}
	if err := st.Upsert(context.Background(), claude); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	w := doJSON(t, srv, http.MethodPost, "/api/accounts/"+strconv.FormatInt(claude.ID, 10)+"/deepseek",
		map[string]any{"api_key": "sk-ds"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// The check button is an answer, not a failure: a rejected key reports
// key_valid=false with a 200 rather than a 502 the UI would render as "the
// probe broke".
func TestCheckDeepSeekAccount(t *testing.T) {
	srv, st, env := newDeepSeekServer(t)
	acc := createDeepSeekAccount(t, srv)

	env.reader.bal = deepseek.Balance{Available: false, Amount: 0, Currency: "CNY"}
	w := doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/deepseek/check", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["key_valid"] != true || out["available"] != false {
		t.Fatalf("probe = %v", out)
	}
	// The probe must persist what it learned, or the card shows a stale
	// figure next to a fresh tick.
	cred, err := st.DeepSeekCredential(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if cred.HasBalance() {
		t.Fatal("the probe's is_available=false must reach the store")
	}

	env.reader.err = deepseek.ErrUnauthorized
	w = doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/deepseek/check", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("a rejected key is an answer, not a fault: status = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["key_valid"] != false {
		t.Fatalf("probe = %v, want key_valid=false", out)
	}
}

func TestCheckDeepSeekAccountWithoutKey(t *testing.T) {
	srv, st, _ := newDeepSeekServer(t)
	acc := &store.Account{
		Provider: store.ProviderDeepSeek, Name: "bare", AccountUUID: "deepseek:bare",
		Status: store.StatusActive,
	}
	if err := st.Upsert(context.Background(), acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	w := doJSON(t, srv, http.MethodPost,
		"/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/deepseek/check", nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
}

// The account list has to carry the same view the create response did, or the
// card renders differently after a refresh.
func TestListAccountsCarriesDeepSeekView(t *testing.T) {
	srv, _, _ := newDeepSeekServer(t)
	created := createDeepSeekAccount(t, srv)

	w := doJSON(t, srv, http.MethodGet, "/api/accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Accounts []accountView `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *accountView
	for i := range out.Accounts {
		if out.Accounts[i].ID == created.ID {
			found = &out.Accounts[i]
		}
	}
	if found == nil {
		t.Fatal("the created account is missing from the list")
	}
	if found.DeepSeek == nil || !found.DeepSeek.HasAPIKey || found.DeepSeek.Balance == nil {
		t.Fatalf("list view = %+v, want the same shape as the create response", found.DeepSeek)
	}
	if strings.Contains(w.Body.String(), "sk-ds-console") {
		t.Fatal("the account list must never carry the API key")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}
