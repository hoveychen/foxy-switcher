package vault

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// deepSeekFixture builds a store with one configured DeepSeek account and one
// device granted the provider.
type deepSeekFixture struct {
	st  *store.Store
	svc *DeepSeekGrants
	acc *store.Account
}

func newDeepSeekFixture(t *testing.T) *deepSeekFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	acc := &store.Account{
		Provider: store.ProviderDeepSeek, Name: "ds-pool", AccountUUID: "ds-1",
		Email: "ds@example.com", Status: store.StatusActive,
	}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := st.SetDeepSeekCredential(ctx, acc.ID, "sk-ds-one"); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	if err := st.InsertDevice(ctx, store.Device{
		ID: "dev-1", Name: "Laptop", TokenHash: "h-1",
		AllowClaude: true, AllowDeepSeek: true,
	}); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	return &deepSeekFixture{
		st:  st,
		svc: NewDeepSeekGrants(st, log.New(io.Discard, "", 0)),
		acc: acc,
	}
}

func TestDeepSeekGrantServesTheAccountKey(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()

	grant, err := f.svc.EnsureDeviceGrant(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceGrant: %v", err)
	}
	if grant.APIKey != "sk-ds-one" {
		t.Fatalf("APIKey = %q, want the account's own key", grant.APIKey)
	}
	if grant.AccountID != f.acc.ID || grant.AccountName != "ds-pool" {
		t.Fatalf("grant identifies the wrong account: %+v", grant)
	}
	if grant.BaseURL != DefaultDeepSeekBaseURL {
		t.Fatalf("BaseURL = %q, want %q", grant.BaseURL, DefaultDeepSeekBaseURL)
	}

	// Repeat calls are stable: the agent polls this, and a key that changed on
	// every tick would rewrite the device's credential file endlessly.
	again, err := f.svc.EnsureDeviceGrant(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceGrant again: %v", err)
	}
	if again != grant {
		t.Fatalf("grant is not stable across calls: %+v vs %+v", again, grant)
	}
}

func TestDeepSeekGrantGatedByDevice(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()

	if err := f.st.InsertDevice(ctx, store.Device{
		ID: "dev-2", Name: "Desktop", TokenHash: "h-2", AllowClaude: true,
	}); err != nil {
		t.Fatalf("insert device: %v", err)
	}
	if _, err := f.svc.EnsureDeviceGrant(ctx, "dev-2"); !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("ungranted device = %v, want selector.ErrNoAvailable", err)
	}

	// Withdrawing the grant from a device that had it takes effect immediately.
	if err := f.st.SetDeviceProviders(ctx, "dev-1", true, false, false, false); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if _, err := f.svc.EnsureDeviceGrant(ctx, "dev-1"); !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("withdrawn device = %v, want selector.ErrNoAvailable", err)
	}
}

// Combined (local) mode generates a device id but never inserts a devices row,
// and is deliberately un-gated.
func TestDeepSeekGrantUngatedForUnpairedDevice(t *testing.T) {
	f := newDeepSeekFixture(t)
	if _, err := f.svc.EnsureDeviceGrant(context.Background(), "local-only"); err != nil {
		t.Fatalf("combined-mode device must not be gated: %v", err)
	}
	if _, err := f.svc.EnsureDeviceGrant(context.Background(), ""); err == nil {
		t.Fatal("an empty device id must be rejected outright")
	}
}

// The pool rolls onto the next funded account when DeepSeek reports one as
// unavailable, and keeps a stable lowest-id-first order otherwise.
func TestDeepSeekGrantSkipsDrainedAccounts(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()

	second := &store.Account{
		Provider: store.ProviderDeepSeek, Name: "ds-backup", AccountUUID: "ds-2",
		Status: store.StatusActive,
	}
	if err := f.st.Upsert(ctx, second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	if err := f.st.SetDeepSeekCredential(ctx, second.ID, "sk-ds-two"); err != nil {
		t.Fatalf("set second credential: %v", err)
	}

	// Both funded (never polled) — lowest id wins.
	grant, err := f.svc.EnsureDeviceGrant(ctx, "dev-1")
	if err != nil || grant.AccountID != f.acc.ID {
		t.Fatalf("grant = %+v err=%v, want the lowest-id account", grant, err)
	}

	// The first account drains; the pool must roll onto the second.
	if err := f.st.SetDeepSeekBalance(ctx, f.acc.ID, false, 0, "CNY"); err != nil {
		t.Fatalf("SetDeepSeekBalance: %v", err)
	}
	grant, err = f.svc.EnsureDeviceGrant(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceGrant after drain: %v", err)
	}
	if grant.AccountID != second.ID || grant.APIKey != "sk-ds-two" {
		t.Fatalf("grant = %+v, want the funded backup account", grant)
	}

	// Both drained: the error must say the money ran out, not "nothing
	// configured" — the operator fix is completely different.
	if err := f.st.SetDeepSeekBalance(ctx, second.ID, false, 0, "CNY"); err != nil {
		t.Fatalf("SetDeepSeekBalance second: %v", err)
	}
	_, err = f.svc.EnsureDeviceGrant(ctx, "dev-1")
	if !errors.Is(err, ErrNoDeepSeekAccount) {
		t.Fatalf("err = %v, want ErrNoDeepSeekAccount", err)
	}
	if !strings.Contains(err.Error(), "insufficient balance") {
		t.Fatalf("error must name the reason, got %q", err)
	}
}

func TestDeepSeekGrantSkipsUnusableAccounts(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()

	// Paused.
	if err := f.st.SetStatus(ctx, f.acc.ID, store.StatusPaused); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if _, err := f.svc.EnsureDeviceGrant(ctx, "dev-1"); !errors.Is(err, ErrNoDeepSeekAccount) {
		t.Fatalf("paused account = %v, want ErrNoDeepSeekAccount", err)
	}
	if err := f.st.SetStatus(ctx, f.acc.ID, store.StatusActive); err != nil {
		t.Fatalf("SetStatus back: %v", err)
	}

	// No key on file.
	if err := f.st.DeleteDeepSeekCredential(ctx, f.acc.ID); err != nil {
		t.Fatalf("DeleteDeepSeekCredential: %v", err)
	}
	if _, err := f.svc.EnsureDeviceGrant(ctx, "dev-1"); !errors.Is(err, ErrNoDeepSeekAccount) {
		t.Fatalf("keyless account = %v, want ErrNoDeepSeekAccount", err)
	}
}

// A Claude or Codex account in the pool must never be mistaken for a DeepSeek
// one — its tokens would be handed out as an API key.
func TestDeepSeekGrantIgnoresOtherProviders(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()
	if err := f.st.DeleteDeepSeekCredential(ctx, f.acc.ID); err != nil {
		t.Fatalf("DeleteDeepSeekCredential: %v", err)
	}
	if err := f.st.Upsert(ctx, &store.Account{
		Provider: store.ProviderClaude, Name: "claude", AccountUUID: "c-1",
		AccessToken: "at-secret", Status: store.StatusActive,
	}); err != nil {
		t.Fatalf("upsert claude: %v", err)
	}
	if _, err := f.svc.EnsureDeviceGrant(ctx, "dev-1"); !errors.Is(err, ErrNoDeepSeekAccount) {
		t.Fatalf("err = %v, want ErrNoDeepSeekAccount", err)
	}
}
