package vault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

func seedCodexAccount(t *testing.T, st *store.Store, name string) *store.Account {
	t.Helper()
	a := &store.Account{
		Provider:       store.ProviderCodex,
		Name:           name,
		Email:          name + "@cx",
		AccessToken:    "at-" + name,
		RefreshToken:   "rt-" + name,
		ExpiresAt:      time.Now().Add(2 * time.Hour).UnixMilli(),
		Status:         "active",
		CredentialJSON: `{"tokens":{"access_token":"x"}}`,
	}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("upsert codex: %v", err)
	}
	return a
}

// TestPickProviderForDevice_GatesOnAllowlist is the core enforcement: the vault
// must refuse to hand a device a provider its per-device allowlist doesn't
// grant. A claude-only device asking for a Codex account gets ErrNoAvailable
// (the agent's Codex manager then restores the local creds and injects
// nothing), while a codex-enabled device — and combined mode (deviceID=="") —
// pick it normally.
func TestPickProviderForDevice_GatesOnAllowlist(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()

	codex := seedCodexAccount(t, st, "cx")
	claude := seedAccount(t, st, "cl") // provider defaults to claude

	// claude-only device: allowed claude, denied codex.
	if err := st.InsertDevice(ctx, store.Device{
		ID: "claude-dev", Name: "C", TokenHash: "h1", AllowClaude: true, AllowCodex: false,
	}); err != nil {
		t.Fatalf("insert claude-dev: %v", err)
	}
	if _, err := svc.PickProviderForDevice(ctx, time.Now(), "claude-dev", store.ProviderCodex); !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("claude-only device picking codex: err=%v, want ErrNoAvailable", err)
	}
	// ...but it can still pick claude.
	got, err := svc.PickForDevice(ctx, time.Now(), "claude-dev")
	if err != nil || got == nil || got.ID != claude.ID {
		t.Fatalf("claude-only device picking claude: got=%+v err=%v", got, err)
	}

	// codex-enabled device can pick the codex account.
	if err := st.InsertDevice(ctx, store.Device{
		ID: "codex-dev", Name: "X", TokenHash: "h2", AllowClaude: true, AllowCodex: true,
	}); err != nil {
		t.Fatalf("insert codex-dev: %v", err)
	}
	got, err = svc.PickProviderForDevice(ctx, time.Now(), "codex-dev", store.ProviderCodex)
	if err != nil || got == nil || got.ID != codex.ID {
		t.Fatalf("codex device picking codex: got=%+v err=%v", got, err)
	}

	// Combined mode (no device identity) is not gated.
	got, err = svc.PickProviderForDevice(ctx, time.Now(), "", store.ProviderCodex)
	if err != nil || got == nil || got.ID != codex.ID {
		t.Fatalf("combined (no device) picking codex: got=%+v err=%v", got, err)
	}
}
