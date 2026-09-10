package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/deepseek"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// deepseek.go is the admin surface for DeepSeek accounts: create one with a
// console-issued key, rotate that key, and re-check it.
//
// It is deliberately smaller than openrouter.go next door, and the difference
// is the provider's, not a shortcut. DeepSeek has no key-minting API, so there
// is no derivation template to edit — no per-key model allowlist, no spend cap,
// no workspace. An account is a name plus a key.
//
// The key is write-only across this API. It goes in, but no handler ever reads
// it back out; the UI renders a "configured / not configured" flag instead.

// deepSeekView is the admin-visible shape of a DeepSeek account. Note the
// absence of the key itself.
type deepSeekView struct {
	// HasAPIKey is how the UI shows configured-ness without the secret.
	HasAPIKey bool `json:"has_api_key"`
	// Balance is the account balance as last polled. Nil when never polled —
	// the UI must show "unknown" rather than "0", which would read as broke.
	Balance *deepSeekBalanceView `json:"balance,omitempty"`
	// OutOfBalance mirrors the grant service's own verdict, so the card's badge
	// and the routing decision can't disagree.
	OutOfBalance bool `json:"out_of_balance"`
	// Shared is always true and says so explicitly: every authorised device is
	// served this same key, because DeepSeek issues keys only from its console.
	// Stated in the payload rather than assumed by the UI so the badge can't
	// drift from the behaviour.
	Shared bool `json:"shared"`
}

// deepSeekBalanceView is an account's balance as the admin UI shows it.
type deepSeekBalanceView struct {
	// Available is DeepSeek's own is_available flag — the eligibility signal.
	Available bool `json:"available"`
	// Amount / Currency are for display. Currency is "CNY" or "USD" depending
	// on how the account bills, which is exactly why there is no local
	// threshold on Amount anywhere in this feature.
	Amount    float64 `json:"amount"`
	Currency  string  `json:"currency"`
	CheckedAt int64   `json:"checked_at"`
}

// deepSeekConfigFor builds the view for one account.
func (s *Server) deepSeekConfigFor(ctx context.Context, a store.Account) (*deepSeekView, error) {
	if a.Provider != store.ProviderDeepSeek {
		return nil, nil
	}
	view := &deepSeekView{Shared: true}
	cred, err := s.Store.DeepSeekCredential(ctx, a.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// No key yet. Leave Balance nil so the UI says "unknown", not "0".
	case err != nil:
		return nil, err
	default:
		view.HasAPIKey = true
		view.OutOfBalance = !cred.HasBalance()
		if cred.BalanceCheckedAt != 0 {
			view.Balance = &deepSeekBalanceView{
				Available: cred.BalanceAvailable,
				Amount:    cred.BalanceAmount,
				Currency:  cred.BalanceCurrency,
				CheckedAt: cred.BalanceCheckedAt,
			}
		}
	}
	return view, nil
}

// deepSeekAccountReq is the create/update payload. APIKey is required on
// create and optional on update ("" = leave the stored one alone).
type deepSeekAccountReq struct {
	Name   string `json:"name"`
	APIKey string `json:"api_key"`
}

// handleCreateDeepSeekAccount adds a DeepSeek pool account. Like OpenRouter
// and unlike Claude / Codex there is no OAuth dance: the admin pastes a key
// from the DeepSeek console.
func (s *Server) handleCreateDeepSeekAccount(w http.ResponseWriter, r *http.Request) {
	var req deepSeekAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		http.Error(w, "api_key required — the vault needs a key to hand devices",
			http.StatusBadRequest)
		return
	}
	// Validate before storing, so a typo fails at save time instead of at
	// every device's first request.
	bal, err := s.checkDeepSeekKey(r.Context(), req.APIKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	acc := &store.Account{
		Provider: store.ProviderDeepSeek,
		Name:     req.Name,
		// Dedup keys must be non-empty or two accounts collapse onto one row
		// via the partial unique indexes. DeepSeek gives us no account identity
		// through this endpoint, so synthesise one from the admin's name.
		AccountUUID:      "deepseek:" + req.Name,
		SubscriptionType: "payg",
		Plan:             "DeepSeek",
		Status:           store.StatusActive,
	}
	if err := s.Store.Upsert(r.Context(), acc); err != nil {
		http.Error(w, "save DeepSeek account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetDeepSeekCredential(r.Context(), acc.ID, req.APIKey); err != nil {
		http.Error(w, "save API key: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// The validation call already told us the balance; record it so the card
	// shows a real figure immediately instead of "unknown" until the poller's
	// first tick up to fifteen minutes later.
	if err := s.Store.SetDeepSeekBalance(r.Context(), acc.ID, bal.Available, bal.Amount, bal.Currency); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountAdded, acc.ID,
		fmt.Sprintf("Added DeepSeek account %s", acc.Name))
	s.writeDeepSeekAccount(w, r.Context(), *acc)
}

// handleUpdateDeepSeekAccount rotates an existing account's key.
//
// Nothing has to be revoked first: the vault mints nothing, so the only thing
// a rotation changes is which key devices are served on their next config
// fetch. Name is create-only for the same reason as OpenRouter's — it seeds
// account_uuid, the dedup key.
func (s *Server) handleUpdateDeepSeekAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	acc, err := s.Store.Get(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if acc.Provider != store.ProviderDeepSeek {
		http.Error(w, "not a DeepSeek account", http.StatusBadRequest)
		return
	}
	var req deepSeekAccountReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.APIKey) == "" {
		http.Error(w, "api_key required", http.StatusBadRequest)
		return
	}
	bal, err := s.checkDeepSeekKey(r.Context(), req.APIKey)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.Store.SetDeepSeekCredential(r.Context(), id, req.APIKey); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.Store.SetDeepSeekBalance(r.Context(), id, bal.Available, bal.Amount, bal.Currency); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountUpdated, id,
		fmt.Sprintf("Rotated the API key for DeepSeek account %s", acc.Name))
	s.writeDeepSeekAccount(w, r.Context(), *acc)
}

// handleCheckDeepSeekAccount is the admin's "is this working?" button. It
// re-reads the balance with the stored key, which both proves the key is still
// valid and refreshes the figure on the card.
func (s *Server) handleCheckDeepSeekAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	cred, err := s.Store.DeepSeekCredential(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "no API key on file for this account", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bal, err := s.newDeepSeekClient(cred.APIKey).Balance(r.Context())
	if err != nil {
		if errors.Is(err, deepseek.ErrUnauthorized) {
			// Report it rather than 502: the key being rejected is an answer,
			// and it is the answer the button exists to find out.
			writeJSON(w, http.StatusOK, map[string]any{
				"key_valid": false,
				"detail":    "DeepSeek rejected this key — check it was copied in full, or that it still exists in the console",
			})
			return
		}
		http.Error(w, "probe DeepSeek: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.Store.SetDeepSeekBalance(r.Context(), id, bal.Available, bal.Amount, bal.Currency); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key_valid": true,
		"available": bal.Available,
		"amount":    bal.Amount,
		"currency":  bal.Currency,
	})
}

// checkDeepSeekKey validates a key by reading its balance, and returns that
// balance so the caller can record it without a second call.
func (s *Server) checkDeepSeekKey(ctx context.Context, apiKey string) (deepseek.Balance, error) {
	bal, err := s.newDeepSeekClient(apiKey).Balance(ctx)
	if errors.Is(err, deepseek.ErrUnauthorized) {
		return deepseek.Balance{}, errors.New("DeepSeek rejected this key — check it was copied in full")
	}
	if err != nil {
		return deepseek.Balance{}, fmt.Errorf("verify key with DeepSeek: %w", err)
	}
	return bal, nil
}

// deepSeekBalanceReader is the slice of the deepseek client the admin surface
// needs.
type deepSeekBalanceReader interface {
	Balance(ctx context.Context) (deepseek.Balance, error)
}

// newDeepSeekClient builds a client for one key. Overridden in tests.
func (s *Server) newDeepSeekClient(apiKey string) deepSeekBalanceReader {
	if s.deepSeekClientFor != nil {
		return s.deepSeekClientFor(apiKey)
	}
	return &deepseek.Client{APIKey: apiKey}
}

// SetDeepSeekClientFactory swaps the client constructor. Tests only.
func (s *Server) SetDeepSeekClientFactory(f func(apiKey string) deepSeekBalanceReader) {
	s.deepSeekClientFor = f
}

func (s *Server) writeDeepSeekAccount(w http.ResponseWriter, ctx context.Context, acc store.Account) {
	view := toView(acc)
	cfg, err := s.deepSeekConfigFor(ctx, acc)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view.DeepSeek = cfg
	writeJSON(w, http.StatusOK, map[string]any{"account": view})
}
