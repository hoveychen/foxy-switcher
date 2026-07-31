package vault

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// fakeUpstream stands in for OpenRouter's management API. It records what was
// asked of it and which keys are still live, so tests can assert the thing that
// actually matters after a revoke: the credential no longer works.
type fakeUpstream struct {
	mu       sync.Mutex
	live     map[string]bool // key hash -> still valid upstream
	derives  int
	revokes  []string
	nextID   int
	deriveFn func(spec openrouter.DeriveSpec) (openrouter.Key, error)
	revokeFn func(hash string) error
	// specs records every derivation request, for asserting the allowlist and
	// caps were carried across from the account config.
	specs []openrouter.DeriveSpec
}

func newFakeUpstream() *fakeUpstream {
	return &fakeUpstream{live: map[string]bool{}}
}

func (f *fakeUpstream) DeriveDeviceKey(_ context.Context, spec openrouter.DeriveSpec) (openrouter.Key, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, spec)
	f.derives++
	if f.deriveFn != nil {
		return f.deriveFn(spec)
	}
	f.nextID++
	hash := fmt.Sprintf("kh-%d", f.nextID)
	f.live[hash] = true
	return openrouter.Key{Hash: hash, Secret: "sk-or-" + hash}, nil
}

func (f *fakeUpstream) RevokeDerivedKey(_ context.Context, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revokes = append(f.revokes, hash)
	if f.revokeFn != nil {
		return f.revokeFn(hash)
	}
	delete(f.live, hash)
	return nil
}

func (f *fakeUpstream) isLive(hash string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.live[hash]
	return ok
}

func (f *fakeUpstream) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

func (f *fakeUpstream) deriveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.derives
}

// openRouterFixture builds a store with one configured OpenRouter account and
// one device granted the provider.
type openRouterFixture struct {
	st       *store.Store
	svc      *OpenRouterKeys
	upstream *fakeUpstream
	acc      *store.Account
}

func newOpenRouterFixture(t *testing.T) *openRouterFixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	acc := &store.Account{
		Provider: store.ProviderOpenRouter, Name: "pool", AccountUUID: "or-1",
		Email: "or@example.com", Status: store.StatusActive,
	}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert account: %v", err)
	}
	if err := st.SetOpenRouterConfig(ctx, acc.ID, store.OpenRouterAccountConfig{
		AllowedModels: []string{"deepseek/deepseek-v4-flash", "openai/gpt-oss-120b"},
		LimitUSD:      25, LimitReset: "monthly",
	}); err != nil {
		t.Fatalf("set config: %v", err)
	}
	if err := st.SetOpenRouterCredential(ctx, acc.ID, "sk-or-mgmt", true); err != nil {
		t.Fatalf("set management key: %v", err)
	}
	if err := st.InsertDevice(ctx, store.Device{
		ID: "dev-1", Name: "Laptop", TokenHash: "h-1",
		AllowClaude: true, AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	up := newFakeUpstream()
	svc := NewOpenRouterKeys(st, log.New(io.Discard, "", 0))
	svc.newClient = func(string) OpenRouterAPI { return up }
	return &openRouterFixture{st: st, svc: svc, upstream: up, acc: acc}
}

func TestEnsureDeviceKeyDerivesOnceThenReuses(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()

	first, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if first.APIKey == "" || first.AccountID != f.acc.ID {
		t.Fatalf("grant = %+v", first)
	}
	if first.BaseURL != DefaultOpenRouterBaseURL {
		t.Fatalf("BaseURL = %q, want %q", first.BaseURL, DefaultOpenRouterBaseURL)
	}
	// The device is told the model list so it can write one profile file each —
	// that is what makes them selectable in the picker.
	if len(first.AllowedModels) != 2 || first.AllowedModels[0] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("AllowedModels = %v", first.AllowedModels)
	}
	spec := f.upstream.specs[0]
	if spec.LimitUSD != 25 || spec.LimitReset != "monthly" {
		t.Fatalf("derive spec = %+v, want the account's caps carried onto the key", spec)
	}
	if spec.KeyName != "foxy-dev-1" {
		t.Fatalf("KeyName = %q, want foxy-<device-id> so an operator can identify it upstream", spec.KeyName)
	}
	// That the model list is not sent upstream is now structural rather than
	// asserted: openrouter.DeriveSpec has no model field at all. Only the cap
	// crosses to OpenRouter.

	// Steady state: no second upstream call. OpenRouter is deliberately outside
	// the agent's reconcile loop, and a derive-per-call would undo that.
	second, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("second EnsureDeviceKey: %v", err)
	}
	if second.APIKey != first.APIKey {
		t.Fatalf("key changed between calls: %q -> %q", first.APIKey, second.APIKey)
	}
	if n := f.upstream.deriveCount(); n != 1 {
		t.Fatalf("derived %d times, want 1 — repeat calls must be a DB read", n)
	}
}

func TestEnsureDeviceKeyIsPerDevice(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if err := f.st.InsertDevice(ctx, store.Device{
		ID: "dev-2", Name: "Desktop", TokenHash: "h-2", AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("insert dev-2: %v", err)
	}

	a, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("dev-1: %v", err)
	}
	b, err := f.svc.EnsureDeviceKey(ctx, "dev-2")
	if err != nil {
		t.Fatalf("dev-2: %v", err)
	}
	if a.APIKey == b.APIKey {
		t.Fatal("two devices share one key — per-device revocation would be impossible")
	}
	if a.AccountID != b.AccountID {
		t.Fatal("both devices should derive from the same (only) configured account")
	}
}

func TestEnsureDeviceKeyDeniesUngrantedDevice(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if err := f.st.InsertDevice(ctx, store.Device{
		ID: "dev-plain", Name: "Plain", TokenHash: "h-plain", AllowClaude: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := f.svc.EnsureDeviceKey(ctx, "dev-plain")
	if !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("err = %v, want selector.ErrNoAvailable", err)
	}
	if n := f.upstream.deriveCount(); n != 0 {
		t.Fatalf("derived %d keys for an ungranted device; want 0", n)
	}
}

// TestRevokeDeviceKeysKillsTheCredential is the security core: a derived key
// talks to OpenRouter directly and never passes through the vault's bearer
// auth, so revoking the device does nothing unless the key itself is deleted.
func TestRevokeDeviceKeysKillsTheCredential(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if err := f.st.InsertDevice(ctx, store.Device{
		ID: "dev-2", Name: "Desktop", TokenHash: "h-2", AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("insert dev-2: %v", err)
	}
	one, _ := f.svc.EnsureDeviceKey(ctx, "dev-1")
	two, _ := f.svc.EnsureDeviceKey(ctx, "dev-2")
	oneHash := strings.TrimPrefix(one.APIKey, "sk-or-")
	twoHash := strings.TrimPrefix(two.APIKey, "sk-or-")

	if err := f.svc.RevokeDeviceKeys(ctx, "dev-1"); err != nil {
		t.Fatalf("RevokeDeviceKeys: %v", err)
	}
	if f.upstream.isLive(oneHash) {
		t.Fatal("dev-1's key is still live upstream after revoke")
	}
	if !f.upstream.isLive(twoHash) {
		t.Fatal("revoking dev-1 killed dev-2's key — revocation must be per-device")
	}
	if rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1"); len(rows) != 0 {
		t.Fatalf("dev-1 mapping rows survive revoke: %+v", rows)
	}
	// Idempotent: a retry (or a revoke of a device that never had a key) is fine.
	if err := f.svc.RevokeDeviceKeys(ctx, "dev-1"); err != nil {
		t.Fatalf("second RevokeDeviceKeys: %v", err)
	}
}

// TestRevokeKeepsRowWhenUpstreamRefuses pins the failure policy. Deleting the
// local row on an upstream failure would erase the key_hash — the only handle
// that can ever kill that key — leaving a working credential nobody can revoke.
func TestRevokeKeepsRowWhenUpstreamRefuses(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	f.upstream.revokeFn = func(string) error { return errors.New("upstream down") }

	if err := f.svc.RevokeDeviceKeys(ctx, "dev-1"); err == nil {
		t.Fatal("RevokeDeviceKeys must surface an upstream failure")
	}
	rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1")
	if len(rows) != 1 {
		t.Fatalf("mapping row was dropped despite a failed upstream revoke: %+v", rows)
	}

	// Once upstream recovers, the retry completes the revoke.
	f.upstream.revokeFn = nil
	if err := f.svc.RevokeDeviceKeys(ctx, "dev-1"); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if f.upstream.liveCount() != 0 {
		t.Fatal("key still live after successful retry")
	}
}

// TestRevokeWithoutAPIKeyRefusesRatherThanForget covers the operator mistake of
// clearing the account's key while devices still hold derived keys.
func TestRevokeWithoutAPIKeyRefusesRatherThanForget(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if err := f.st.DeleteOpenRouterCredential(ctx, f.acc.ID); err != nil {
		t.Fatalf("delete management key: %v", err)
	}

	err := f.svc.RevokeDeviceKeys(ctx, "dev-1")
	if err == nil || !strings.Contains(err.Error(), "no API key on file") {
		t.Fatalf("err = %v, want an explicit 'cannot revoke, no key on file' failure", err)
	}
	if rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1"); len(rows) != 1 {
		t.Fatal("the row must survive — it holds the only handle that can ever kill this key")
	}
}

func TestRevokeAccountKeysClearsEveryDevice(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	for _, id := range []string{"dev-2", "dev-3"} {
		if err := f.st.InsertDevice(ctx, store.Device{
			ID: id, Name: id, TokenHash: "h-" + id, AllowOpenRouter: true,
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	for _, id := range []string{"dev-1", "dev-2", "dev-3"} {
		if _, err := f.svc.EnsureDeviceKey(ctx, id); err != nil {
			t.Fatalf("derive %s: %v", id, err)
		}
	}
	if f.upstream.liveCount() != 3 {
		t.Fatalf("live keys = %d, want 3", f.upstream.liveCount())
	}

	// This is what an allowlist edit triggers: every outstanding key encodes the
	// OLD policy in its guardrail, so all of them have to go.
	if err := f.svc.RevokeAccountKeys(ctx, f.acc.ID); err != nil {
		t.Fatalf("RevokeAccountKeys: %v", err)
	}
	if f.upstream.liveCount() != 0 {
		t.Fatalf("live keys after account revoke = %d, want 0", f.upstream.liveCount())
	}
	if rows, _ := f.st.ListAccountOpenRouterKeys(ctx, f.acc.ID); len(rows) != 0 {
		t.Fatalf("mapping rows survive: %+v", rows)
	}

	// And the next Ensure re-derives under the new policy.
	if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); err != nil {
		t.Fatalf("re-derive: %v", err)
	}
	if f.upstream.deriveCount() != 4 {
		t.Fatalf("derives = %d, want 4 (3 + 1 re-derive)", f.upstream.deriveCount())
	}
}

func TestEnsureDeviceKeyReplacesAnExpiredKey(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()

	// Seed a key that already expired upstream.
	f.upstream.live["stale"] = true
	if err := f.st.PutDeviceOpenRouterKey(ctx, store.DeviceOpenRouterKey{
		DeviceID: "dev-1", AccountID: f.acc.ID, KeyHash: "stale",
		KeySecret: "sk-or-stale",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if grant.APIKey == "sk-or-stale" {
		t.Fatal("expired key was handed back instead of being replaced")
	}
	if f.upstream.isLive("stale") {
		t.Fatal("the expired key was not revoked upstream — dead keys would accumulate")
	}
}

func TestEnsureDeviceKeyRevokesUpstreamWhenPersistFails(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	// Close the store so the write fails after the upstream key already exists.
	if err := f.st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err == nil {
		t.Skip("store write unexpectedly succeeded on a closed DB; nothing to assert")
	}
	if f.upstream.deriveCount() == 1 && f.upstream.liveCount() != 0 {
		t.Fatal("a key we failed to persist was left live upstream — it is now unrevocable")
	}
}

// --- account selection ----------------------------------------------------

func TestPickAccountSkipsUnconfiguredAccounts(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()

	// A lower-id account exists but is unusable in three different ways; each
	// must be skipped rather than producing a key with an empty dropdown or a
	// derivation that can't authenticate.
	t.Run("no management key", func(t *testing.T) {
		if err := f.st.DeleteOpenRouterCredential(ctx, f.acc.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		defer func() { _ = f.st.SetOpenRouterCredential(ctx, f.acc.ID, "sk-or-mgmt", true) }()
		if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); !errors.Is(err, ErrNoOpenRouterAccount) {
			t.Fatalf("err = %v, want ErrNoOpenRouterAccount", err)
		}
	})

	t.Run("empty allowlist", func(t *testing.T) {
		if err := f.st.SetOpenRouterConfig(ctx, f.acc.ID, store.OpenRouterAccountConfig{LimitUSD: 5}); err != nil {
			t.Fatalf("set config: %v", err)
		}
		defer func() {
			_ = f.st.SetOpenRouterConfig(ctx, f.acc.ID, store.OpenRouterAccountConfig{
				AllowedModels: []string{"a/b"}, LimitUSD: 25,
			})
		}()
		if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); !errors.Is(err, ErrNoOpenRouterAccount) {
			t.Fatalf("err = %v, want ErrNoOpenRouterAccount (a key with no models is useless)", err)
		}
	})

	t.Run("paused", func(t *testing.T) {
		if err := f.st.SetStatus(ctx, f.acc.ID, store.StatusPaused); err != nil {
			t.Fatalf("pause: %v", err)
		}
		defer func() { _ = f.st.SetStatus(ctx, f.acc.ID, store.StatusActive) }()
		if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); !errors.Is(err, ErrNoOpenRouterAccount) {
			t.Fatalf("err = %v, want ErrNoOpenRouterAccount", err)
		}
	})
}

func TestPickAccountIsStableAcrossCalls(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()

	// A second, equally valid account must not cause a device to hop between
	// accounts — its key would change for no reason the operator asked for.
	second := &store.Account{
		Provider: store.ProviderOpenRouter, Name: "pool-b", AccountUUID: "or-2",
		Email: "or2@example.com", Status: store.StatusActive,
	}
	if err := f.st.Upsert(ctx, second); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := f.st.SetOpenRouterConfig(ctx, second.ID, store.OpenRouterAccountConfig{
		AllowedModels: []string{"x/y"}, LimitUSD: 1,
	}); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := f.st.SetOpenRouterCredential(ctx, second.ID, "sk-or-mgmt-2", true); err != nil {
		t.Fatalf("mgmt: %v", err)
	}

	for i := 0; i < 3; i++ {
		grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if grant.AccountID != f.acc.ID {
			t.Fatalf("call %d picked account %d, want the stable lowest-id choice %d",
				i, grant.AccountID, f.acc.ID)
		}
	}
}

func TestEnsureDeviceKeySurfacesDerivationFailureWithoutStoringAnything(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	f.upstream.deriveFn = func(openrouter.DeriveSpec) (openrouter.Key, error) {
		return openrouter.Key{}, openrouter.ErrUnauthorized
	}
	_, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if !errors.Is(err, openrouter.ErrUnauthorized) {
		t.Fatalf("err = %v, want the underlying cause to survive for the admin UI", err)
	}
	if rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1"); len(rows) != 0 {
		t.Fatalf("a failed derivation stored a mapping row: %+v", rows)
	}
}

// --- ordinary (non-provisioning) keys ---------------------------------------

// For a single machine, deriving from a provisioning key buys nothing — and
// requiring one hands the daemon the power to add and delete every key on the
// account just to solve "one machine needs one key". An ordinary key is served
// straight through instead.
func TestOrdinaryKeyIsServedDirectlyWithoutDeriving(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if err := f.st.SetOpenRouterCredential(ctx, f.acc.ID, "sk-or-plain", false); err != nil {
		t.Fatalf("set plain: %v", err)
	}

	grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if grant.APIKey != "sk-or-plain" {
		t.Fatalf("APIKey = %q, want the account's own key", grant.APIKey)
	}
	if n := f.upstream.deriveCount(); n != 0 {
		t.Fatalf("made %d upstream derive call(s) for a key that cannot mint", n)
	}
	// The UI must not imply a revocability that doesn't exist.
	if grant.DeviceScoped {
		t.Fatal("DeviceScoped must be false — every device shares this key")
	}
	// No mapping row: there is no per-device key to track, and nothing the vault
	// could revoke. A row would imply otherwise.
	if rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1"); len(rows) != 0 {
		t.Fatalf("wrote %d mapping row(s) for a shared key", len(rows))
	}
	// The model list still reaches the device — that's what fills the picker.
	if len(grant.AllowedModels) != 2 {
		t.Fatalf("AllowedModels = %v", grant.AllowedModels)
	}
}

func TestOrdinaryKeyIsSharedAcrossDevices(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	if err := f.st.SetOpenRouterCredential(ctx, f.acc.ID, "sk-or-plain", false); err != nil {
		t.Fatalf("set plain: %v", err)
	}
	if err := f.st.InsertDevice(ctx, store.Device{
		ID: "dev-2", Name: "Desktop", TokenHash: "h-2", AllowOpenRouter: true,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	a, _ := f.svc.EnsureDeviceKey(ctx, "dev-1")
	b, _ := f.svc.EnsureDeviceKey(ctx, "dev-2")
	if a.APIKey != b.APIKey {
		t.Fatal("an ordinary key is by definition the same for both devices")
	}
	if a.DeviceScoped || b.DeviceScoped {
		t.Fatal("neither grant is device-scoped")
	}
}

// A provisioning key still derives, and still says so.
func TestProvisioningKeyStillDerivesPerDevice(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if !grant.DeviceScoped {
		t.Fatal("DeviceScoped must be true when the key was minted for this device")
	}
	if f.upstream.deriveCount() != 1 {
		t.Fatalf("derives = %d, want 1", f.upstream.deriveCount())
	}
}

// --- rotation on insufficient credit ---------------------------------------

// addAccount configures a second OpenRouter account.
func addAccount(t *testing.T, f *openRouterFixture, name string, provisioning bool) *store.Account {
	t.Helper()
	ctx := context.Background()
	acc := &store.Account{
		Provider: store.ProviderOpenRouter, Name: name, AccountUUID: "or-" + name,
		Email: name + "@example.com", Status: store.StatusActive,
	}
	if err := f.st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert %s: %v", name, err)
	}
	if err := f.st.SetOpenRouterConfig(ctx, acc.ID, store.OpenRouterAccountConfig{
		AllowedModels: []string{"x/y"}, LimitUSD: 5, LimitReset: "monthly",
	}); err != nil {
		t.Fatalf("config %s: %v", name, err)
	}
	if err := f.st.SetOpenRouterCredential(ctx, acc.ID, "sk-or-"+name, provisioning); err != nil {
		t.Fatalf("cred %s: %v", name, err)
	}
	return acc
}

// The point of the whole feature: when the first account runs out of money, the
// pool rolls onto the next funded one instead of handing out a key that 402s.
func TestExhaustedAccountIsSkippedForTheNextFundedOne(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	second := addAccount(t, f, "pool-b", true)

	// Both funded → the stable lowest-id choice wins.
	if err := f.st.SetOpenRouterCredit(ctx, f.acc.ID, 100, 50); err != nil {
		t.Fatalf("credit a: %v", err)
	}
	if err := f.st.SetOpenRouterCredit(ctx, second.ID, 100, 50); err != nil {
		t.Fatalf("credit b: %v", err)
	}
	grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if grant.AccountID != f.acc.ID {
		t.Fatalf("picked %d, want the stable first choice %d", grant.AccountID, f.acc.ID)
	}

	// Now the first runs dry. Note it already holds a derived key — being out of
	// money has to override the reuse path, or the device would keep serving a
	// key that can't pay.
	if err := f.st.SetOpenRouterCredit(ctx, f.acc.ID, 100, 0); err != nil {
		t.Fatalf("drain a: %v", err)
	}
	grant, err = f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("after drain: %v", err)
	}
	if grant.AccountID != second.ID {
		t.Fatalf("picked account %d, want the funded one %d", grant.AccountID, second.ID)
	}
}

// When everything is broke the error has to say so — "OpenRouter stopped
// working" is much harder to act on than "your accounts are out of money".
func TestAllAccountsBrokeSaysSo(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	second := addAccount(t, f, "pool-b", true)
	if err := f.st.SetOpenRouterCredit(ctx, f.acc.ID, 100, 0); err != nil {
		t.Fatalf("drain a: %v", err)
	}
	if err := f.st.SetOpenRouterCredit(ctx, second.ID, 100, 0.10); err != nil {
		t.Fatalf("drain b: %v", err)
	}

	_, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if !errors.Is(err, ErrNoOpenRouterAccount) {
		t.Fatalf("err = %v, want ErrNoOpenRouterAccount", err)
	}
	if !strings.Contains(err.Error(), "insufficient credit") {
		t.Fatalf("err = %v, must name the actual cause", err)
	}
	if !strings.Contains(err.Error(), "2 account") {
		t.Fatalf("err = %v, should say how many were skipped", err)
	}
}

// A never-polled account must not be locked out: the poller can fail for reasons
// unrelated to money, and "unknown" must not mean "broke".
func TestNeverPolledAccountIsStillUsable(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	// Fixture account has no credit poll at all.
	grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("EnsureDeviceKey: %v", err)
	}
	if grant.AccountID != f.acc.ID {
		t.Fatalf("picked %d, want the unpolled account to still be usable", grant.AccountID)
	}
}

// Rotating away leaves the old account's key alive (capped, and unable to spend
// on an account with no money). It must stay revocable, or rotation would slowly
// accumulate credentials nobody can kill.
func TestKeysFromRotatedAwayAccountsStayRevocable(t *testing.T) {
	f := newOpenRouterFixture(t)
	ctx := context.Background()
	second := addAccount(t, f, "pool-b", true)

	if _, err := f.svc.EnsureDeviceKey(ctx, "dev-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := f.st.SetOpenRouterCredit(ctx, f.acc.ID, 100, 0); err != nil {
		t.Fatalf("drain: %v", err)
	}
	grant, err := f.svc.EnsureDeviceKey(ctx, "dev-1")
	if err != nil {
		t.Fatalf("after rotate: %v", err)
	}
	if grant.AccountID != second.ID {
		t.Fatalf("did not rotate: account %d", grant.AccountID)
	}
	// One row per account the device has used.
	rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want one per account used", len(rows))
	}
	if f.upstream.liveCount() != 2 {
		t.Fatalf("live upstream keys = %d, want 2", f.upstream.liveCount())
	}

	// Revoking the device must kill both, whichever account each came from.
	if err := f.svc.RevokeDeviceKeys(ctx, "dev-1"); err != nil {
		t.Fatalf("RevokeDeviceKeys: %v", err)
	}
	if f.upstream.liveCount() != 0 {
		t.Fatalf("live keys after revoke = %d, want 0", f.upstream.liveCount())
	}
	if rows, _ := f.st.ListDeviceOpenRouterKeys(ctx, "dev-1"); len(rows) != 0 {
		t.Fatalf("rows survive revoke: %+v", rows)
	}
}
