package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// deviceWithAllowlist pairs a device whose provider grants the caller chooses,
// returning its bearer token. pairedDevice always grants OpenRouter, which is
// exactly the axis these tests vary.
func deviceWithAllowlist(t *testing.T, st *store.Store, id string, claude, codex, openrouter bool) string {
	t.Helper()
	token := vaultauth.NewToken()
	if err := st.InsertDevice(context.Background(), store.Device{
		ID: id, Name: id, TokenHash: vaultauth.HashToken(token),
		AllowClaude: claude, AllowCodex: codex, AllowOpenRouter: openrouter,
	}); err != nil {
		t.Fatalf("InsertDevice(%s): %v", id, err)
	}
	return token
}

func seedProviderAccount(t *testing.T, st *store.Store, provider, name string) *store.Account {
	t.Helper()
	a := &store.Account{
		Provider:    provider,
		Name:        name,
		AccountUUID: provider + ":" + name,
		Status:      store.StatusActive,
	}
	if provider == store.ProviderClaude {
		a.AccessToken = "at-" + name
		a.RefreshToken = "rt-" + name
		a.ExpiresAt = time.Now().Add(2 * time.Hour).UnixMilli()
	}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("Upsert(%s/%s): %v", provider, name, err)
	}
	return a
}

func agentAccountProviders(t *testing.T, url, token string) map[string]int {
	t.Helper()
	resp := getWithBearer(t, url+"/agent/v1/accounts", token)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /agent/v1/accounts: status %s, want 200", resp.Status)
	}
	var out struct {
		Accounts []struct {
			Provider string `json:"provider"`
		} `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	byProvider := map[string]int{}
	for _, a := range out.Accounts {
		byProvider[a.Provider]++
	}
	return byProvider
}

// TestAgentAccountsGatesOnProviderAllowlist reproduces the live "peixi switches
// account every 5s" incident from 2026-08-18.
//
// The vault handed every device the WHOLE pool, every provider included. An
// active OpenRouter row with last_used_at == 0 (its token never expires, so it
// stays eligible) then looked like the legacy global "Use now" pin sentinel to
// the agent's Claude coordinator, which defeated sticky selection on every 5s
// reconcile tick and made equally-ranked Claude accounts ping-pong forever.
// Agent-side that regression is fixed by choose()'s provider filter, but only
// for agents new enough to have it — the field runs builds as old as 1.1.9.
//
// So the vault must not ship a device rows for providers its allowlist denies:
// PickProviderForDevice already refuses to lease them, and the OpenRouter
// config route already hides their existence, making the unfiltered account
// list the odd one out.
func TestAgentAccountsGatesOnProviderAllowlist(t *testing.T) {
	st, tsrv := newGrantFixture(t, nil)
	seedProviderAccount(t, st, store.ProviderClaude, "cl")
	seedProviderAccount(t, st, store.ProviderCodex, "cx")
	seedProviderAccount(t, st, store.ProviderOpenRouter, "or")

	// A claude-only device (peixi's shape) must see Claude rows only.
	claudeOnly := deviceWithAllowlist(t, st, "claude-only", true, false, false)
	got := agentAccountProviders(t, tsrv.URL, claudeOnly)
	if got[store.ProviderOpenRouter] != 0 || got[store.ProviderCodex] != 0 {
		t.Errorf("claude-only device saw disallowed providers: %v", got)
	}
	if got[store.ProviderClaude] != 1 {
		t.Errorf("claude-only device lost its own provider: %v", got)
	}

	// A codex-enabled device must still see both of its granted pools — the
	// filter has to gate, not blanket-hide every non-Claude row.
	multi := deviceWithAllowlist(t, st, "multi", true, true, false)
	got = agentAccountProviders(t, tsrv.URL, multi)
	if got[store.ProviderClaude] != 1 || got[store.ProviderCodex] != 1 {
		t.Errorf("codex-enabled device lost a granted provider: %v", got)
	}
	if got[store.ProviderOpenRouter] != 0 {
		t.Errorf("codex-enabled device saw denied OpenRouter row: %v", got)
	}
}
