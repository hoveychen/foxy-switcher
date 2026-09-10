package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeepSeekCredentialRoundTrip covers the whole credential lifecycle:
// store, read back, replace, delete. The balance fields are the poller's
// business and must survive a plain read, but must reset when the key is
// replaced — a different key may be looking at a different account.
func TestDeepSeekCredentialRoundTrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	acc := &Account{Provider: ProviderDeepSeek, Name: "ds-a", AccountUUID: "ds-a"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	id := acc.ID

	if _, err := st.DeepSeekCredential(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeepSeekCredential before set = %v, want ErrNotFound", err)
	}
	if has, err := st.HasDeepSeekCredential(ctx, id); err != nil || has {
		t.Fatalf("HasDeepSeekCredential before set = %v, %v; want false, nil", has, err)
	}

	if err := st.SetDeepSeekCredential(ctx, id, "  sk-ds-one  "); err != nil {
		t.Fatalf("SetDeepSeekCredential: %v", err)
	}
	cred, err := st.DeepSeekCredential(ctx, id)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if cred.APIKey != "sk-ds-one" {
		t.Fatalf("APIKey = %q, want trimmed %q", cred.APIKey, "sk-ds-one")
	}
	if cred.BalanceCheckedAt != 0 {
		t.Fatalf("BalanceCheckedAt = %d, want 0 (never polled)", cred.BalanceCheckedAt)
	}

	if err := st.SetDeepSeekBalance(ctx, id, true, 42.5, "CNY"); err != nil {
		t.Fatalf("SetDeepSeekBalance: %v", err)
	}
	cred, err = st.DeepSeekCredential(ctx, id)
	if err != nil {
		t.Fatalf("DeepSeekCredential after poll: %v", err)
	}
	if !cred.BalanceAvailable || cred.BalanceAmount != 42.5 || cred.BalanceCurrency != "CNY" {
		t.Fatalf("balance = %+v, want available/42.5/CNY", cred)
	}
	if cred.BalanceCheckedAt == 0 {
		t.Fatal("BalanceCheckedAt must be stamped by a successful poll")
	}

	// Replacing the key resets the balance: the new key may belong to a
	// different DeepSeek account entirely, so keeping the old figure would
	// report someone else's money.
	if err := st.SetDeepSeekCredential(ctx, id, "sk-ds-two"); err != nil {
		t.Fatalf("replace key: %v", err)
	}
	cred, err = st.DeepSeekCredential(ctx, id)
	if err != nil {
		t.Fatalf("DeepSeekCredential after replace: %v", err)
	}
	if cred.APIKey != "sk-ds-two" || cred.BalanceCheckedAt != 0 ||
		cred.BalanceAmount != 0 || cred.BalanceAvailable {
		t.Fatalf("after replace = %+v, want fresh key with reset balance", cred)
	}

	// An empty key un-configures the account without deleting it.
	if err := st.SetDeepSeekCredential(ctx, id, "   "); err != nil {
		t.Fatalf("SetDeepSeekCredential empty: %v", err)
	}
	if has, err := st.HasDeepSeekCredential(ctx, id); err != nil || has {
		t.Fatalf("HasDeepSeekCredential after blanking = %v, %v; want false, nil", has, err)
	}
	if _, err := st.Get(ctx, id); err != nil {
		t.Fatalf("account must survive credential deletion: %v", err)
	}
	// Delete is idempotent.
	if err := st.DeleteDeepSeekCredential(ctx, id); err != nil {
		t.Fatalf("DeleteDeepSeekCredential twice: %v", err)
	}
}

// TestDeepSeekBalanceOnMissingRow: the poller must not silently create a
// credential row for an account whose key was removed mid-poll.
func TestDeepSeekBalanceOnMissingRow(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	acc := &Account{Provider: ProviderDeepSeek, Name: "ds-b", AccountUUID: "ds-b"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	id := acc.ID
	if err := st.SetDeepSeekBalance(ctx, id, true, 10, "CNY"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDeepSeekBalance with no credential = %v, want ErrNotFound", err)
	}
}

// TestDeepSeekHasBalance pins the "unknown is not broke" rule. A never-polled
// account stays usable; only DeepSeek's own is_available=false takes an
// account out of the pool.
func TestDeepSeekHasBalance(t *testing.T) {
	if !(DeepSeekCredential{}).HasBalance() {
		t.Fatal("a never-polled credential must count as usable")
	}
	polledBroke := DeepSeekCredential{BalanceCheckedAt: 1, BalanceAvailable: false}
	if polledBroke.HasBalance() {
		t.Fatal("is_available=false from a real poll must take the account out of the pool")
	}
	polledOK := DeepSeekCredential{BalanceCheckedAt: 1, BalanceAvailable: true, BalanceAmount: 0.01}
	if !polledOK.HasBalance() {
		t.Fatal("is_available=true is DeepSeek's own answer; do not second-guess it with a floor")
	}
}

// TestDeepSeekProviderAllowlistGating covers the fourth allowlist column: off
// by default (including for rows written before it existed), togglable, and
// honoured by DeviceAllowsProvider.
func TestDeepSeekProviderAllowlistGating(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.InsertDevice(ctx, Device{
		ID: "dev-ds", Name: "DS", TokenHash: "h-ds",
		AllowClaude: true, AllowDeepSeek: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertDevice(ctx, Device{
		ID: "dev-plain", Name: "Plain", TokenHash: "h-plain", AllowClaude: true,
	}); err != nil {
		t.Fatalf("insert plain: %v", err)
	}
	assertAllows(t, st, "dev-ds", ProviderDeepSeek, true)
	assertAllows(t, st, "dev-plain", ProviderDeepSeek, false)

	d, err := st.FindDevice(ctx, "dev-ds")
	if err != nil || !d.AllowDeepSeek {
		t.Fatalf("FindDevice(dev-ds) = %+v err=%v", d, err)
	}
	devices, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	var seen bool
	for _, dd := range devices {
		if dd.ID == "dev-ds" {
			seen = dd.AllowDeepSeek
		}
	}
	if !seen {
		t.Fatal("ListDevices must carry allow_deepseek")
	}

	if err := st.SetDeviceProviders(ctx, "dev-plain", true, false, false, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	assertAllows(t, st, "dev-plain", ProviderDeepSeek, true)
	if err := st.SetDeviceProviders(ctx, "dev-plain", true, false, false, false); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	assertAllows(t, st, "dev-plain", ProviderDeepSeek, false)

	// A row written without the allow_* columns (pre-feature device) must not
	// inherit DeepSeek access.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, 0)`,
		"legacy", "Legacy", "h-legacy", time.Now().UnixMilli()); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	assertAllows(t, st, "legacy", ProviderDeepSeek, false)
}

// TestApprovePairingCarriesDeepSeekChoice: the admin's DeepSeek tick at
// approval has to reach the pairing row, since pair-poll copies it onto the
// device.
func TestApprovePairingCarriesDeepSeekChoice(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	if err := st.InsertPairing(ctx, Pairing{
		ClientNonce: "n-ds", UserCode: "CODE-DS", DeviceName: "laptop",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("InsertPairing: %v", err)
	}
	if err := st.ApprovePairing(ctx, "CODE-DS", "dev-ds", "tok-ds", true, false, false, true); err != nil {
		t.Fatalf("ApprovePairing: %v", err)
	}
	p, err := st.FindPairingByNonce(ctx, "n-ds")
	if err != nil {
		t.Fatalf("FindPairingByNonce: %v", err)
	}
	if !p.AllowClaude || p.AllowCodex || p.AllowOpenRouter || !p.AllowDeepSeek {
		t.Fatalf("pairing flags = %v/%v/%v/%v, want claude+deepseek only",
			p.AllowClaude, p.AllowCodex, p.AllowOpenRouter, p.AllowDeepSeek)
	}
}
