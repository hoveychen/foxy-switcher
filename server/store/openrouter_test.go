package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestOpenRouterConfigNormaliseAndRoundTrip(t *testing.T) {
	cfg := OpenRouterAccountConfig{
		AllowedModels: []string{
			"  openai/gpt-oss-120b ", "deepseek/deepseek-v4-flash",
			"openai/gpt-oss-120b", "", "   ",
		},
		LimitUSD:   25,
		LimitReset: "monthly",
	}
	raw, err := cfg.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseOpenRouterConfig(raw)
	if err != nil {
		t.Fatalf("ParseOpenRouterConfig: %v", err)
	}
	want := []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"}
	if len(got.AllowedModels) != len(want) {
		t.Fatalf("AllowedModels = %v, want %v (trimmed, de-duped, sorted)", got.AllowedModels, want)
	}
	for i := range want {
		if got.AllowedModels[i] != want[i] {
			t.Fatalf("AllowedModels = %v, want %v", got.AllowedModels, want)
		}
	}
	if got.LimitUSD != 25 || got.LimitReset != "monthly" {
		t.Fatalf("limits = %v/%q, want 25/monthly", got.LimitUSD, got.LimitReset)
	}

	// Marshal is stable: the same logical allowlist in a different input order
	// must produce byte-identical JSON, or every admin save would look like a
	// change and needlessly re-derive every device's key.
	shuffled := OpenRouterAccountConfig{
		AllowedModels: []string{"openai/gpt-oss-120b", "deepseek/deepseek-v4-flash"},
		LimitUSD:      25, LimitReset: "monthly",
	}
	raw2, err := shuffled.Marshal()
	if err != nil {
		t.Fatalf("Marshal shuffled: %v", err)
	}
	if raw != raw2 {
		t.Fatalf("Marshal not order-stable:\n %q\n %q", raw, raw2)
	}
}

func TestParseOpenRouterConfigEmptyIsZeroNotError(t *testing.T) {
	for _, in := range []string{"", "   "} {
		cfg, err := ParseOpenRouterConfig(in)
		if err != nil {
			t.Fatalf("ParseOpenRouterConfig(%q) = %v, want nil (unconfigured is valid)", in, err)
		}
		if len(cfg.AllowedModels) != 0 {
			t.Fatalf("ParseOpenRouterConfig(%q) models = %v, want empty", in, cfg.AllowedModels)
		}
	}
	if _, err := ParseOpenRouterConfig("{not json"); err == nil {
		t.Fatal("malformed credential_json must error, not silently yield an empty allowlist")
	}
}

func TestSetOpenRouterConfigRefusesForeignProvider(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	// A Codex row keeps its entire auth.json in credential_json. Writing an
	// OpenRouter template over it would destroy a working credential.
	codex := &Account{
		Provider: ProviderCodex, Name: "codex", AccountUUID: "u-1",
		AccessToken: "at", RefreshToken: "rt", CredentialJSON: `{"tokens":{"access_token":"at"}}`,
	}
	if err := st.Upsert(ctx, codex); err != nil {
		t.Fatalf("upsert codex: %v", err)
	}
	err := st.SetOpenRouterConfig(ctx, codex.ID, OpenRouterAccountConfig{
		AllowedModels: []string{"deepseek/deepseek-v4-flash"},
	})
	if err == nil {
		t.Fatal("SetOpenRouterConfig on a codex row must fail")
	}
	back, err := st.Get(ctx, codex.ID)
	if err != nil {
		t.Fatalf("get codex: %v", err)
	}
	if back.CredentialJSON != `{"tokens":{"access_token":"at"}}` {
		t.Fatalf("codex credential_json was clobbered: %q", back.CredentialJSON)
	}
}

func TestSetOpenRouterConfigRoundTripsThroughStore(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	acc := newOpenRouterAccount(t, st, "pool-a")
	cfg := OpenRouterAccountConfig{
		AllowedModels: []string{"z/model", "a/model"},
		LimitUSD:      10, LimitReset: "weekly", WorkspaceID: "ws-1",
	}
	if err := st.SetOpenRouterConfig(ctx, acc.ID, cfg); err != nil {
		t.Fatalf("SetOpenRouterConfig: %v", err)
	}
	got, err := st.OpenRouterConfig(ctx, acc.ID)
	if err != nil {
		t.Fatalf("OpenRouterConfig: %v", err)
	}
	if len(got.AllowedModels) != 2 || got.AllowedModels[0] != "a/model" {
		t.Fatalf("AllowedModels = %v, want sorted [a/model z/model]", got.AllowedModels)
	}
	if got.WorkspaceID != "ws-1" || got.LimitUSD != 10 || got.LimitReset != "weekly" {
		t.Fatalf("config = %+v", got)
	}
}

// TestOpenRouterManagementKeyNeverRidesOnAccount is the security regression
// test for the whole storage split: the management key can mint and revoke
// runtime keys for every device, and GET /agent/v1/accounts serialises
// store.Account verbatim to every paired device. So the key must be reachable
// only through its own table, and must not appear anywhere in an Account.
func TestOpenRouterAPIKeyNeverRidesOnAccount(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	acc := newOpenRouterAccount(t, st, "pool-a")
	const mgmt = "sk-or-v1-MANAGEMENT-KEY-DO-NOT-LEAK"
	if err := st.SetOpenRouterCredential(ctx, acc.ID, mgmt, true); err != nil {
		t.Fatalf("SetOpenRouterManagementKey: %v", err)
	}
	if err := st.SetOpenRouterConfig(ctx, acc.ID, OpenRouterAccountConfig{
		AllowedModels: []string{"deepseek/deepseek-v4-flash"}, LimitUSD: 5,
	}); err != nil {
		t.Fatalf("SetOpenRouterConfig: %v", err)
	}

	// Every account read path an agent can reach must be clean.
	one, err := st.Get(ctx, acc.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	all, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for name, payload := range map[string]any{"Get": one, "List": all} {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		if strings.Contains(string(raw), mgmt) {
			t.Fatalf("%s serialisation leaks the management key: %s", name, raw)
		}
	}

	// And it must still be readable through its own accessor.
	cred, err := st.OpenRouterCredential(ctx, acc.ID)
	if err != nil || cred.APIKey != mgmt {
		t.Fatalf("OpenRouterCredential = %+v, %v; want the stored key", cred, err)
	}
	has, err := st.HasOpenRouterCredential(ctx, acc.ID)
	if err != nil || !has {
		t.Fatalf("HasOpenRouterManagementKey = %v, %v; want true", has, err)
	}
}

func TestOpenRouterCredentialLifecycle(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	acc := newOpenRouterAccount(t, st, "pool-a")

	if _, err := st.OpenRouterCredential(ctx, acc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset key = %v, want ErrNotFound", err)
	}
	if has, err := st.HasOpenRouterCredential(ctx, acc.ID); err != nil || has {
		t.Fatalf("HasOpenRouterManagementKey on unset = %v, %v; want false, nil", has, err)
	}
	if err := st.SetOpenRouterCredential(ctx, acc.ID, "k1", true); err != nil {
		t.Fatalf("set k1: %v", err)
	}
	// Replacing (rotating the management key) updates in place, not inserts.
	if err := st.SetOpenRouterCredential(ctx, acc.ID, "k2", true); err != nil {
		t.Fatalf("set k2: %v", err)
	}
	if got, _ := st.OpenRouterCredential(ctx, acc.ID); got.APIKey != "k2" {
		t.Fatalf("after rotate = %q, want k2", got.APIKey)
	}
	// An empty value un-configures the account rather than storing "".
	if err := st.SetOpenRouterCredential(ctx, acc.ID, "  ", true); err != nil {
		t.Fatalf("set empty: %v", err)
	}
	if _, err := st.OpenRouterCredential(ctx, acc.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after blanking = %v, want ErrNotFound", err)
	}
	// Delete is idempotent.
	if err := st.DeleteOpenRouterCredential(ctx, acc.ID); err != nil {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestDeviceOpenRouterKeyMapping(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	accA := newOpenRouterAccount(t, st, "pool-a")
	accB := newOpenRouterAccount(t, st, "pool-b")

	mk := func(dev string, acc int64, hash, secret string) DeviceOpenRouterKey {
		return DeviceOpenRouterKey{
			DeviceID: dev, AccountID: acc, KeyHash: hash, KeySecret: secret,
			CreatedAt: time.Now().UnixMilli(),
		}
	}
	for _, k := range []DeviceOpenRouterKey{
		mk("dev-1", accA.ID, "h1", "sk-or-1"),
		mk("dev-2", accA.ID, "h2", "sk-or-2"),
		mk("dev-1", accB.ID, "h3", "sk-or-3"),
	} {
		if err := st.PutDeviceOpenRouterKey(ctx, k); err != nil {
			t.Fatalf("put %+v: %v", k, err)
		}
	}

	got, err := st.DeviceOpenRouterKeyFor(ctx, "dev-1", accA.ID)
	if err != nil {
		t.Fatalf("DeviceOpenRouterKeyFor: %v", err)
	}
	if got.KeyHash != "h1" || got.KeySecret != "sk-or-1" {
		t.Fatalf("key = %+v", got)
	}
	if _, err := st.DeviceOpenRouterKeyFor(ctx, "dev-3", accA.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown device = %v, want ErrNotFound", err)
	}

	byDevice, err := st.ListDeviceOpenRouterKeys(ctx, "dev-1")
	if err != nil || len(byDevice) != 2 {
		t.Fatalf("ListDeviceOpenRouterKeys(dev-1) = %+v err=%v, want 2", byDevice, err)
	}
	byAccount, err := st.ListAccountOpenRouterKeys(ctx, accA.ID)
	if err != nil || len(byAccount) != 2 {
		t.Fatalf("ListAccountOpenRouterKeys(A) = %+v err=%v, want 2", byAccount, err)
	}

	// Re-deriving replaces the row rather than inserting a second one — two live
	// rows for one (device, account) would mean one key we can no longer revoke.
	if err := st.PutDeviceOpenRouterKey(ctx, mk("dev-1", accA.ID, "h1-new", "sk-or-1-new")); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	byDevice, err = st.ListDeviceOpenRouterKeys(ctx, "dev-1")
	if err != nil || len(byDevice) != 2 {
		t.Fatalf("after re-derive = %+v err=%v, want still 2 rows", byDevice, err)
	}
	got, _ = st.DeviceOpenRouterKeyFor(ctx, "dev-1", accA.ID)
	if got.KeyHash != "h1-new" || got.KeySecret != "sk-or-1-new" {
		t.Fatalf("re-derived key = %+v, want h1-new", got)
	}

	// Revoking one device leaves the other device's key for the same account.
	if err := st.DeleteDeviceOpenRouterKeys(ctx, "dev-1"); err != nil {
		t.Fatalf("DeleteDeviceOpenRouterKeys: %v", err)
	}
	if rows, _ := st.ListDeviceOpenRouterKeys(ctx, "dev-1"); len(rows) != 0 {
		t.Fatalf("dev-1 keys after revoke = %+v, want none", rows)
	}
	if rows, _ := st.ListDeviceOpenRouterKeys(ctx, "dev-2"); len(rows) != 1 {
		t.Fatalf("dev-2 keys after revoking dev-1 = %+v, want 1 (revoke must be per-device)", rows)
	}

	// Deleting the account's keys clears the rest.
	if err := st.DeleteAccountOpenRouterKeys(ctx, accA.ID); err != nil {
		t.Fatalf("DeleteAccountOpenRouterKeys: %v", err)
	}
	if rows, _ := st.ListAccountOpenRouterKeys(ctx, accA.ID); len(rows) != 0 {
		t.Fatalf("account A keys after delete = %+v, want none", rows)
	}
}

func TestPutDeviceOpenRouterKeyRequiresIdentifiers(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	for _, k := range []DeviceOpenRouterKey{
		{AccountID: 1, KeyHash: "h"},
		{DeviceID: "d", KeyHash: "h"},
		{DeviceID: "d", AccountID: 1},
	} {
		if err := st.PutDeviceOpenRouterKey(ctx, k); err == nil {
			t.Fatalf("PutDeviceOpenRouterKey(%+v) = nil, want error", k)
		}
	}
}

func TestDeviceOpenRouterKeyExpired(t *testing.T) {
	now := time.Now()
	if (DeviceOpenRouterKey{}).Expired(now) {
		t.Fatal("ExpiresAt==0 means no expiry was requested; must not read as expired")
	}
	if !(DeviceOpenRouterKey{ExpiresAt: now.Add(-time.Second).UnixMilli()}).Expired(now) {
		t.Fatal("a past ExpiresAt must read as expired")
	}
	if (DeviceOpenRouterKey{ExpiresAt: now.Add(time.Hour).UnixMilli()}).Expired(now) {
		t.Fatal("a future ExpiresAt must not read as expired")
	}
}

// TestOpenRouterProviderAllowlistGating covers the third allowlist column:
// off by default (including for rows written before it existed), togglable, and
// honoured by DeviceAllowsProvider.
func TestOpenRouterProviderAllowlistGating(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.InsertDevice(ctx, Device{
		ID: "dev-or", Name: "OR", TokenHash: "h-or",
		AllowClaude: true, AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := st.InsertDevice(ctx, Device{
		ID: "dev-plain", Name: "Plain", TokenHash: "h-plain", AllowClaude: true,
	}); err != nil {
		t.Fatalf("insert plain: %v", err)
	}
	assertAllows(t, st, "dev-or", ProviderOpenRouter, true)
	assertAllows(t, st, "dev-plain", ProviderOpenRouter, false)

	d, err := st.FindDevice(ctx, "dev-or")
	if err != nil || !d.AllowOpenRouter {
		t.Fatalf("FindDevice(dev-or) = %+v err=%v", d, err)
	}
	if _, err := st.FindDevice(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindDevice(unknown) = %v, want ErrNotFound", err)
	}

	// Grant, then withdraw.
	if err := st.SetDeviceProviders(ctx, "dev-plain", true, false, true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	assertAllows(t, st, "dev-plain", ProviderOpenRouter, true)
	if err := st.SetDeviceProviders(ctx, "dev-plain", true, false, false); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	assertAllows(t, st, "dev-plain", ProviderOpenRouter, false)

	// A row written without the allow_* columns (pre-feature device) must not
	// inherit OpenRouter access.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, 0)`,
		"legacy", "Legacy", "h-legacy", time.Now().UnixMilli()); err != nil {
		t.Fatalf("raw insert: %v", err)
	}
	assertAllows(t, st, "legacy", ProviderOpenRouter, false)
}

// newOpenRouterAccount inserts a provider="openrouter" pool row. Per the
// design it carries no secret at all: no access/refresh token, no expiry.
func newOpenRouterAccount(t *testing.T, st *Store, name string) *Account {
	t.Helper()
	acc := &Account{
		Provider:         ProviderOpenRouter,
		Name:             name,
		AccountUUID:      "or-" + name,
		Email:            name + "@openrouter.local",
		SubscriptionType: "payg",
		Plan:             "OpenRouter",
	}
	if err := st.Upsert(context.Background(), acc); err != nil {
		t.Fatalf("upsert openrouter account %q: %v", name, err)
	}
	return acc
}

func TestOpenRouterCredentialKindAndCredit(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	acc := newOpenRouterAccount(t, st, "pool-a")

	// An ordinary inference key: nothing to derive from, so it is served to
	// devices as-is.
	if err := st.SetOpenRouterCredential(ctx, acc.ID, "sk-or-plain", false); err != nil {
		t.Fatalf("set plain: %v", err)
	}
	cred, err := st.OpenRouterCredential(ctx, acc.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cred.APIKey != "sk-or-plain" || cred.IsProvisioning {
		t.Fatalf("cred = %+v, want a non-provisioning key", cred)
	}
	// Never polled, so the balance is unknown — which must read as usable.
	if cred.CreditCheckedAt != 0 || !cred.HasCredit() {
		t.Fatalf("cred = %+v; an unpolled account must not be treated as broke", cred)
	}

	if err := st.SetOpenRouterCredit(ctx, acc.ID, 510, 23.94); err != nil {
		t.Fatalf("SetOpenRouterCredit: %v", err)
	}
	cred, _ = st.OpenRouterCredential(ctx, acc.ID)
	if cred.CreditTotal != 510 || cred.CreditRemaining != 23.94 {
		t.Fatalf("credit = %+v", cred)
	}
	if cred.CreditCheckedAt == 0 {
		t.Fatal("a successful poll must stamp checked_at, or HasCredit stays in 'unknown'")
	}
	if !cred.HasCredit() {
		t.Fatal("$23.94 should be usable")
	}
}

func TestHasCreditThreshold(t *testing.T) {
	polled := func(remaining float64) OpenRouterCredential {
		return OpenRouterCredential{CreditRemaining: remaining, CreditCheckedAt: 1}
	}
	// A small buffer rather than zero: the poller samples periodically, so aiming
	// at exactly empty guarantees some requests land after the money is gone.
	if polled(0).HasCredit() {
		t.Fatal("an empty account must not be handed out")
	}
	if polled(MinUsableCredit - 0.01).HasCredit() {
		t.Fatal("below the floor must not be handed out")
	}
	if !polled(MinUsableCredit).HasCredit() {
		t.Fatal("exactly at the floor should still be usable")
	}
	if polled(-5).HasCredit() {
		t.Fatal("an overdrawn account must not be handed out")
	}
	// Unknown (never polled) stays usable — a failed poll must not lock an
	// operator out of their own pool.
	if !(OpenRouterCredential{CreditRemaining: 0}).HasCredit() {
		t.Fatal("never-polled must read as usable")
	}
}

// Replacing the key resets the balance: a different key may belong to a
// different OpenRouter account entirely, so carrying the old figure over would
// make an empty account look funded (or vice versa).
func TestReplacingTheKeyResetsCreditState(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()
	acc := newOpenRouterAccount(t, st, "pool-a")

	if err := st.SetOpenRouterCredential(ctx, acc.ID, "sk-or-a", true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := st.SetOpenRouterCredit(ctx, acc.ID, 100, 90); err != nil {
		t.Fatalf("credit: %v", err)
	}
	if err := st.SetOpenRouterCredential(ctx, acc.ID, "sk-or-b", false); err != nil {
		t.Fatalf("replace: %v", err)
	}
	cred, _ := st.OpenRouterCredential(ctx, acc.ID)
	if cred.CreditCheckedAt != 0 || cred.CreditRemaining != 0 {
		t.Fatalf("credit state survived a key swap: %+v", cred)
	}
	if cred.IsProvisioning {
		t.Fatal("the kind must follow the new key")
	}
}

func TestSetOpenRouterCreditRequiresACredential(t *testing.T) {
	st := openTempStore(t)
	if err := st.SetOpenRouterCredit(context.Background(), 999, 10, 5); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound rather than a silent no-op", err)
	}
}

// TestMigrationFromManagementKeysTable covers an install that ran the earlier
// build: rows in openrouter_management_keys must come across, flagged
// provisioning (that table could only ever hold provisioning keys), and the old
// table must survive — dropping the only copy of a credential is the one
// mistake here that can't be undone.
func TestMigrationFromManagementKeysTable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/legacy.db"

	st := openStoreAt(t, path)
	ctx := context.Background()
	acc := newOpenRouterAccount(t, st, "pool-a")
	// Simulate the old build's state: a row in the legacy table, none in the new.
	if _, err := st.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS openrouter_management_keys (
		   account_id INTEGER PRIMARY KEY, management_key TEXT NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO openrouter_management_keys (account_id, management_key, updated_at) VALUES (?, ?, ?)`,
		acc.ID, "sk-or-legacy", 12345); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := st.DeleteOpenRouterCredential(ctx, acc.ID); err != nil {
		t.Fatalf("clear new table: %v", err)
	}
	st.Close()

	// Re-open: Open runs the migration.
	st2 := openStoreAt(t, path)
	cred, err := st2.OpenRouterCredential(ctx, acc.ID)
	if err != nil {
		t.Fatalf("after migration: %v", err)
	}
	if cred.APIKey != "sk-or-legacy" {
		t.Fatalf("APIKey = %q, want the legacy key carried over", cred.APIKey)
	}
	if !cred.IsProvisioning {
		t.Fatal("a legacy management key must be flagged provisioning")
	}
	var legacyCount int
	if err := st2.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM openrouter_management_keys`).Scan(&legacyCount); err != nil {
		t.Fatalf("count legacy: %v", err)
	}
	if legacyCount != 1 {
		t.Fatalf("legacy row count = %d, want it left in place", legacyCount)
	}

	// Idempotent, and must not clobber a newer value.
	if err := st2.SetOpenRouterCredential(ctx, acc.ID, "sk-or-current", false); err != nil {
		t.Fatalf("update: %v", err)
	}
	st2.Close()
	st3 := openStoreAt(t, path)
	cred, _ = st3.OpenRouterCredential(ctx, acc.ID)
	if cred.APIKey != "sk-or-current" || cred.IsProvisioning {
		t.Fatalf("re-running the migration clobbered the current credential: %+v", cred)
	}
}

func openStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open(%s): %v", path, err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
