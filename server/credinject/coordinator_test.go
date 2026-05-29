package credinject

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// fakeBackend is an in-memory Backend used to drive Coordinator unit tests
// without touching the macOS keychain or real filesystem state.
type fakeBackend struct {
	mu         sync.Mutex
	oauthBlob  []byte
	hasOAuth   bool
	apiKey     string
	hasAPIKey  bool
	writes     int
	deletes    int
	apiDeletes int
}

func (b *fakeBackend) ReadOAuthBlob() ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hasOAuth {
		return nil, false, nil
	}
	out := make([]byte, len(b.oauthBlob))
	copy(out, b.oauthBlob)
	return out, true, nil
}
func (b *fakeBackend) WriteOAuthBlob(blob []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.oauthBlob = append([]byte{}, blob...)
	b.hasOAuth = true
	b.writes++
	return nil
}
func (b *fakeBackend) DeleteOAuthBlob() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.oauthBlob = nil
	b.hasOAuth = false
	b.deletes++
	return nil
}
func (b *fakeBackend) ReadManagedAPIKey() (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.apiKey, b.hasAPIKey, nil
}
func (b *fakeBackend) WriteManagedAPIKey(k string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.apiKey = k
	b.hasAPIKey = true
	return nil
}
func (b *fakeBackend) DeleteManagedAPIKey() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.apiKey = ""
	b.hasAPIKey = false
	b.apiDeletes++
	return nil
}

// newCoord builds a Coordinator with an in-memory store and fake backend.
// dataDir is a t.TempDir so state.json / native-cred-backup.json don't leak.
func newCoord(t *testing.T) (*Coordinator, *fakeBackend, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	be := &fakeBackend{}
	c := New(vault.NewInProc(st), be, dir, log.New(io.Discard, "", 0), "")
	return c, be, st, dir
}

func seedActive(t *testing.T, st *store.Store, name, accessToken string) int64 {
	t.Helper()
	a := store.Account{
		Name:             name,
		AccessToken:      accessToken,
		RefreshToken:     accessToken + "-refresh",
		ExpiresAt:        time.Now().Add(2 * time.Hour).UnixMilli(),
		Scopes:           "user:inference user:profile",
		SubscriptionType: "max",
		OrganizationUUID: name + "-org-uuid",
		Status:           "active",
	}
	if err := st.Upsert(context.Background(), &a); err != nil {
		t.Fatalf("upsert %s: %v", name, err)
	}
	return a.ID
}

func TestReconcile_InjectsFreshAccount(t *testing.T) {
	c, be, st, _ := newCoord(t)
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	c.reconcile(context.Background())

	if c.CurrentAccountID() != id {
		t.Fatalf("CurrentAccountID: got %d want %d", c.CurrentAccountID(), id)
	}
	if !be.hasOAuth {
		t.Fatal("expected OAuth blob written")
	}
	if be.writes != 1 {
		t.Fatalf("writes: got %d want 1", be.writes)
	}
	if be.apiDeletes < 1 {
		t.Fatalf("expected managed-api-key delete, got %d", be.apiDeletes)
	}
	got := extractAccessToken(be.oauthBlob)
	if got != "sk-ant-oat01-alpha" {
		t.Errorf("injected token mismatch: got %q", got)
	}
}

// TestReconcile_SyncsClaudeJSONProfile is the end-to-end P3 check: a switch
// writes the account's oauthAccount + hasCompletedOnboarding into ~/.claude.json
// (creating nothing it shouldn't, clobbering no unrelated field), and shutdown
// restores the user's pre-foxy identity symmetrically.
func TestReconcile_SyncsClaudeJSONProfile(t *testing.T) {
	c, _, st, dir := newCoord(t)
	cfgPath := filepath.Join(dir, ".claude.json")

	// Pre-existing user identity: an oauthAccount but no onboarding flag, plus
	// an unrelated field that must survive the whole cycle.
	if err := writeConfig(cfgPath, map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "user@home.com"},
		"numStartups":  float64(7),
	}); err != nil {
		t.Fatalf("seed cfg: %v", err)
	}
	c.SetClaudeConfigPath(cfgPath)

	a := store.Account{
		Name: "alpha", AccessToken: "sk-ant-oat01-alpha", RefreshToken: "r",
		ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli(),
		Scopes:    "user:inference user:profile", SubscriptionType: "max", Status: "active",
		Email: "alpha@corp.com", AccountUUID: "alpha-uuid", FullName: "Alpha", OrganizationName: "Corp",
	}
	if err := st.Upsert(context.Background(), &a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	c.reconcile(context.Background())

	cfg := readJSON(t, cfgPath)
	if cfg[onboardingKey] != true {
		t.Errorf("hasCompletedOnboarding not set after inject: %#v", cfg[onboardingKey])
	}
	acct := cfg[oauthAccountKey].(map[string]any)
	if acct["emailAddress"] != "alpha@corp.com" {
		t.Errorf("oauthAccount not synced to switched account: %#v", acct)
	}
	if cfg["numStartups"] != float64(7) {
		t.Errorf("unrelated field clobbered: %#v", cfg["numStartups"])
	}

	// Shutdown: restore the user's original identity. oauthAccount goes back to
	// user@home.com (present at snapshot); the foxy-added onboarding flag is
	// removed (absent at snapshot).
	if err := c.RestoreOnShutdown(); err != nil {
		t.Fatalf("RestoreOnShutdown: %v", err)
	}
	cfg = readJSON(t, cfgPath)
	acct = cfg[oauthAccountKey].(map[string]any)
	if acct["emailAddress"] != "user@home.com" {
		t.Errorf("identity not restored on shutdown: %#v", acct)
	}
	if _, ok := cfg[onboardingKey]; ok {
		t.Errorf("foxy-added onboarding should be removed on restore: %#v", cfg[onboardingKey])
	}
	if cfg["numStartups"] != float64(7) {
		t.Errorf("unrelated field lost across restore: %#v", cfg["numStartups"])
	}
}

func TestReconcile_NoOpIfAlreadyInjected(t *testing.T) {
	c, be, st, _ := newCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	c.reconcile(context.Background())
	first := be.writes
	c.reconcile(context.Background())
	c.reconcile(context.Background())

	if be.writes != first {
		t.Fatalf("expected no further writes after first inject; got %d → %d", first, be.writes)
	}
}

func TestReconcile_SwitchesAccount(t *testing.T) {
	c, be, st, _ := newCoord(t)
	idA := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	seedActive(t, st, "beta", "sk-ant-oat01-beta")

	// First pick is LRU; both have last_used_at=0 so account A (lower id) wins.
	c.reconcile(context.Background())
	if c.CurrentAccountID() != idA {
		t.Fatalf("first pick: got %d want %d", c.CurrentAccountID(), idA)
	}

	// Pause account A → next reconcile must switch to beta.
	if err := st.SetStatus(context.Background(), idA, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	c.reconcile(context.Background())
	got := extractAccessToken(be.oauthBlob)
	if got != "sk-ant-oat01-beta" {
		t.Errorf("after pause A: injected token %q (expected beta)", got)
	}
	if c.CurrentAccountID() == idA {
		t.Errorf("CurrentAccountID still A after pause")
	}
}

// TestReconcile_NoAvailable_HoldsKeychain pins the rate-limit-storm fix:
// when the only account is paused (or all accounts cross threshold) the
// reconcile loop must KEEP the current keychain blob in place, not fall back
// to native creds. Restoring native used to wedge running CC sessions on
// macOS — see [keychain-credentials-pool.md §4.3] and the comment on
// [handleNoAvailable]. A native backup is still pre-populated here to verify
// it is intentionally ignored on this code path; RestoreOnShutdown remains
// the only writer that consumes it.
func TestReconcile_NoAvailable_HoldsKeychain(t *testing.T) {
	c, be, st, dir := newCoord(t)

	if err := writeBackup(dir, backupFile{
		OAuthBlob:     []byte(`{"native":"yes"}`),
		ManagedAPIKey: "sk-ant-api03-native",
		SnapshotAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("writeBackup: %v", err)
	}

	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())
	if c.CurrentAccountID() != id {
		t.Fatalf("inject: CurrentAccountID=%d", c.CurrentAccountID())
	}
	injectedBlob := append([]byte(nil), be.oauthBlob...)
	injectedDeletes := be.deletes

	if err := st.SetStatus(context.Background(), id, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	c.reconcile(context.Background())

	if c.CurrentAccountID() != id {
		t.Errorf("after no-available: CurrentAccountID=%d, want %d (account should still be marked as last-injected)",
			c.CurrentAccountID(), id)
	}
	if !be.hasOAuth {
		t.Error("keychain blob was deleted; expected hold")
	}
	if string(be.oauthBlob) != string(injectedBlob) {
		t.Errorf("keychain blob changed: got %q want %q", string(be.oauthBlob), string(injectedBlob))
	}
	if be.deletes != injectedDeletes {
		t.Errorf("DeleteOAuthBlob called: got %d want %d", be.deletes, injectedDeletes)
	}
}

func TestReverseSync_DetectsExternalRotation(t *testing.T) {
	c, be, st, _ := newCoord(t)
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())

	// Simulate Claude Code rotating the keychain entry behind us.
	rotatedExpiry := time.Now().Add(8 * time.Hour).UnixMilli()
	rotatedBlob, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "sk-ant-oat01-NEW",
			"refreshToken": "sk-ant-ort01-NEW",
			"expiresAt":    rotatedExpiry,
		},
	})
	be.mu.Lock()
	be.oauthBlob = rotatedBlob
	be.mu.Unlock()

	c.reverseSync(context.Background())

	a, err := st.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if a.AccessToken != "sk-ant-oat01-NEW" {
		t.Errorf("store access_token not updated: got %q", a.AccessToken)
	}
	if a.RefreshToken != "sk-ant-ort01-NEW" {
		t.Errorf("store refresh_token not updated: got %q", a.RefreshToken)
	}
	if a.ExpiresAt != rotatedExpiry {
		t.Errorf("store expires_at not updated: got %d want %d", a.ExpiresAt, rotatedExpiry)
	}
}

func TestSnapshot_DetectsForeignBlob(t *testing.T) {
	c, be, st, dir := newCoord(t)
	// Pre-existing blob in the keychain that does NOT match any store account.
	be.oauthBlob = []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-NATIVE"}}`)
	be.hasOAuth = true
	be.apiKey = "sk-ant-api03-native"
	be.hasAPIKey = true

	seedActive(t, st, "alpha", "sk-ant-oat01-alpha") // not the foreign token
	c.reconcile(context.Background())

	bf, ok, err := readBackup(dir)
	if err != nil {
		t.Fatalf("readBackup: %v", err)
	}
	if !ok {
		t.Fatal("expected backup file to be written")
	}
	if extractAccessToken(bf.OAuthBlob) != "sk-ant-oat01-NATIVE" {
		t.Errorf("backup blob mismatch")
	}
	if bf.ManagedAPIKey != "sk-ant-api03-native" {
		t.Errorf("backup api key not captured")
	}
}

func TestSnapshot_SkipsFoxyOwnedBlob(t *testing.T) {
	c, be, st, dir := newCoord(t)
	// Pre-existing blob whose accessToken IS one of our stored accounts —
	// e.g. daemon restarted while still owning the keychain.
	be.oauthBlob = []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-alpha"}}`)
	be.hasOAuth = true

	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())

	if _, ok, _ := readBackup(dir); ok {
		t.Fatal("expected no backup written for foxy-owned blob")
	}
}

func TestRestoreOnShutdown_NoBackup_ClearsKeychain(t *testing.T) {
	c, be, st, _ := newCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())
	if !be.hasOAuth {
		t.Fatal("setup: expected blob present before shutdown")
	}

	if err := c.RestoreOnShutdown(); err != nil {
		t.Fatalf("RestoreOnShutdown: %v", err)
	}
	// The very first reconcile snapshotted "no native login" (since the
	// keychain was empty before our write) — restore from that snapshot is a
	// blank state, i.e. clear the keychain entries.
	if be.hasOAuth {
		t.Errorf("expected oauth cleared on shutdown")
	}
	if be.hasAPIKey {
		t.Errorf("expected api key cleared on shutdown")
	}
}

// TestReconcile_StickyAcrossRepeatedTicks pins down the regression where
// repeated reconciles ping-ponged between two equally-eligible accounts
// because LRU + MarkUsed kept inverting which one looked least-recently-used.
func TestReconcile_StickyAcrossRepeatedTicks(t *testing.T) {
	c, be, st, _ := newCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	seedActive(t, st, "beta", "sk-ant-oat01-beta")

	// Drain initial last_used_at=0 sentinels by reconciling enough times
	// that both accounts have been touched at least once.
	c.reconcile(context.Background())
	c.reconcile(context.Background())
	settled := c.CurrentAccountID()
	if settled == 0 {
		t.Fatal("settle: no current account picked")
	}
	settledWrites := be.writes

	// Once settled, reconcile must be a no-op: no flip, no keychain writes.
	for i := 0; i < 6; i++ {
		c.reconcile(context.Background())
		if got := c.CurrentAccountID(); got != settled {
			t.Fatalf("reconcile #%d flipped: got %d want %d", i+1, got, settled)
		}
	}
	if be.writes != settledWrites {
		t.Errorf("unexpected keychain writes during sticky window: %d → %d",
			settledWrites, be.writes)
	}
}

// TestReconcile_RespectsManualSelect verifies that stickiness yields when an
// account has been pinned via store.MarkForNextPick (the path /api/accounts/
// {id}/select takes).
func TestReconcile_RespectsManualSelect(t *testing.T) {
	c, _, st, _ := newCoord(t)
	idA := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	idB := seedActive(t, st, "beta", "sk-ant-oat01-beta")

	c.reconcile(context.Background())
	c.reconcile(context.Background())
	current := c.CurrentAccountID()
	other := idA
	if current == idA {
		other = idB
	}

	if err := st.MarkForNextPick(context.Background(), other); err != nil {
		t.Fatalf("MarkForNextPick: %v", err)
	}
	c.reconcile(context.Background())

	if got := c.CurrentAccountID(); got != other {
		t.Errorf("after MarkForNextPick(%d): CurrentAccountID=%d", other, got)
	}
}

// TestReconcile_ManualMode_StaysOnCurrentAccount covers the auto-switch=off
// invariant: once an account is injected, the coordinator must not rotate to
// another pool member just because LRU prefers it. Without this, toggling
// "Manual" in the UI would still produce the ping-pong behavior the user is
// explicitly opting out of.
func TestReconcile_ManualMode_StaysOnCurrentAccount(t *testing.T) {
	c, be, st, _ := newCoord(t)
	idA := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	idB := seedActive(t, st, "beta", "sk-ant-oat01-beta")

	// First reconcile picks LRU winner (lower id wins ties).
	c.reconcile(context.Background())
	if c.CurrentAccountID() != idA {
		t.Fatalf("setup: expected idA injected first, got %d", c.CurrentAccountID())
	}

	if err := st.SetAutoSwitch(context.Background(), store.AutoSwitch{Enabled: false, Policy: "lru"}); err != nil {
		t.Fatalf("SetAutoSwitch: %v", err)
	}

	// Drive idA's last_used_at backwards (older than idB's) so LRU would prefer
	// idB if auto-switch were on. Manual mode must ignore this and stay on idA.
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE accounts SET last_used_at = 1 WHERE id = ?`, idA); err != nil {
		t.Fatalf("hand-rewind last_used_at: %v", err)
	}
	if _, err := st.DB().ExecContext(context.Background(),
		`UPDATE accounts SET last_used_at = 2 WHERE id = ?`, idB); err != nil {
		t.Fatalf("hand-rewind beta: %v", err)
	}

	priorWrites := be.writes
	for i := 0; i < 3; i++ {
		c.reconcile(context.Background())
		if got := c.CurrentAccountID(); got != idA {
			t.Fatalf("manual mode rotated on reconcile #%d: got %d want %d", i+1, got, idA)
		}
	}
	if be.writes != priorWrites {
		t.Errorf("manual mode produced %d extra keychain writes", be.writes-priorWrites)
	}
}

// TestReconcile_ManualMode_RespectsExplicitPin verifies that "Use now" still
// works in manual mode — clicking it on another account must switch to it
// even though Auto Switch is off.
func TestReconcile_ManualMode_RespectsExplicitPin(t *testing.T) {
	c, _, st, _ := newCoord(t)
	idA := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	idB := seedActive(t, st, "beta", "sk-ant-oat01-beta")

	c.reconcile(context.Background())
	if c.CurrentAccountID() != idA {
		t.Fatalf("setup: expected idA, got %d", c.CurrentAccountID())
	}
	if err := st.SetAutoSwitch(context.Background(), store.AutoSwitch{Enabled: false, Policy: "lru"}); err != nil {
		t.Fatalf("SetAutoSwitch: %v", err)
	}

	if err := st.MarkForNextPick(context.Background(), idB); err != nil {
		t.Fatalf("MarkForNextPick: %v", err)
	}
	c.reconcile(context.Background())
	if got := c.CurrentAccountID(); got != idB {
		t.Errorf("manual + MarkForNextPick(idB): CurrentAccountID=%d want %d", got, idB)
	}
}

// TestReconcile_ManualMode_HoldsCurrentWhenIneligible covers the safety net:
// if the account currently injected becomes ineligible (paused, cooldown,
// over-threshold), manual mode must NOT silently rotate to a different one
// AND must NOT fall back to native creds — it holds the current keychain so
// running CC sessions don't get rugged out from under by stale native creds
// (see TestReconcile_NoAvailable_HoldsKeychain for the auto-mode twin).
func TestReconcile_ManualMode_HoldsCurrentWhenIneligible(t *testing.T) {
	c, be, st, dir := newCoord(t)
	if err := writeBackup(dir, backupFile{
		OAuthBlob:  []byte(`{"native":"yes"}`),
		SnapshotAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("writeBackup: %v", err)
	}

	idA := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	seedActive(t, st, "beta", "sk-ant-oat01-beta")
	c.reconcile(context.Background())
	if c.CurrentAccountID() != idA {
		t.Fatalf("setup: expected idA, got %d", c.CurrentAccountID())
	}
	injectedBlob := append([]byte(nil), be.oauthBlob...)

	if err := st.SetAutoSwitch(context.Background(), store.AutoSwitch{Enabled: false, Policy: "lru"}); err != nil {
		t.Fatalf("SetAutoSwitch: %v", err)
	}
	if err := st.SetStatus(context.Background(), idA, "paused"); err != nil {
		t.Fatalf("pause idA: %v", err)
	}

	c.reconcile(context.Background())

	if c.CurrentAccountID() != idA {
		t.Errorf("expected hold of idA; got CurrentAccountID=%d", c.CurrentAccountID())
	}
	if got := extractAccessToken(be.oauthBlob); got == "sk-ant-oat01-beta" {
		t.Errorf("manual mode auto-rotated to beta; expected hold of idA")
	}
	if string(be.oauthBlob) != string(injectedBlob) {
		t.Errorf("keychain blob changed: got %q want %q", string(be.oauthBlob), string(injectedBlob))
	}
}

func TestNilCoordinator_TriggerAndShutdownAreSafe(t *testing.T) {
	var c *Coordinator
	c.Trigger() // must not panic
	if got := c.CurrentAccountID(); got != 0 {
		t.Errorf("nil CurrentAccountID: got %d", got)
	}
	if err := c.RestoreOnShutdown(); err != nil {
		t.Errorf("nil RestoreOnShutdown: %v", err)
	}
}

// TestBootstrap_ExternalRotationDoesNotForceSwitch guards the cold-start race:
// while the daemon was off, Claude Code (or another machine) rotated the
// previously-injected account's keychain blob — fresh access_token + expiry.
// Our store still holds the old expires_at, which is now in the past. If
// bootstrap reconciles before reverse-syncing, choose() reads the stale
// store, decides A is "expired" via IsEligible, and switches to B —
// clobbering the externally-rotated A blob in the keychain.
//
// Expected after fix: bootstrap calls reverseSync first, so the store
// catches up with the rotated tokens and A is again the current,
// untouched, in-use account.
func TestBootstrap_ExternalRotationDoesNotForceSwitch(t *testing.T) {
	ctx := context.Background()
	c, be, st, dir := newCoord(t)

	idA := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	seedActive(t, st, "beta", "sk-ant-oat01-beta")

	// A's stored tokens look stale to the selector (token "expired" 1h ago).
	staleAccess := "sk-ant-oat01-alpha"
	staleRefresh := "sk-ant-oat01-alpha-refresh"
	if err := st.UpdateTokens(ctx, idA, staleAccess, staleRefresh,
		time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatalf("UpdateTokens stale A: %v", err)
	}

	// injected.json says A is the current injection (matches a real shutdown).
	if err := writeState(dir, stateFile{
		AccountID:  idA,
		AccessHash: hashToken(staleAccess),
		InjectedAt: time.Now().Add(-2 * time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("writeState: %v", err)
	}

	// Keychain holds A's externally-rotated blob: new tokens, fresh expiry.
	rotatedAccess := "sk-ant-oat01-alpha-NEW"
	rotatedRefresh := "sk-ant-ort01-alpha-NEW"
	rotatedExpiry := time.Now().Add(8 * time.Hour).UnixMilli()
	rotatedBlob, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  rotatedAccess,
			"refreshToken": rotatedRefresh,
			"expiresAt":    rotatedExpiry,
		},
	})
	be.mu.Lock()
	be.oauthBlob = rotatedBlob
	be.hasOAuth = true
	be.mu.Unlock()

	c.bootstrap(ctx)

	if got := c.CurrentAccountID(); got != idA {
		t.Fatalf("bootstrap switched off A despite the rotation belonging to A; CurrentAccountID=%d want %d",
			got, idA)
	}
	if got := extractAccessToken(be.oauthBlob); got != rotatedAccess {
		t.Fatalf("bootstrap clobbered the externally-rotated A blob in the keychain; got %q want %q",
			got, rotatedAccess)
	}
	a, err := st.Get(ctx, idA)
	if err != nil {
		t.Fatalf("st.Get: %v", err)
	}
	if a.AccessToken != rotatedAccess {
		t.Errorf("store did not catch up with rotated A access_token; got %q", a.AccessToken)
	}
	if a.RefreshToken != rotatedRefresh {
		t.Errorf("store did not catch up with rotated A refresh_token; got %q", a.RefreshToken)
	}
}

// TestNew_DeviceIDPersistsAcrossRestarts is the regression test for the
// "agent restart blanks the keychain for ~minute" symptom. Two coordinators
// constructed against the same dataDir must reuse the same deviceID so the
// vault's leases table sees the new process as the same device and the
// AcquireLease can renew (instead of returning ErrLeaseLocked until the stale
// lease's TTL lapses).
func TestNew_DeviceIDPersistsAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	be := &fakeBackend{}
	logger := log.New(io.Discard, "", 0)

	c1 := New(vault.NewInProc(st), be, dir, logger, "")
	if c1.deviceID == "" {
		t.Fatal("c1.deviceID is empty; New must produce one")
	}

	c2 := New(vault.NewInProc(st), be, dir, logger, "")
	if c2.deviceID != c1.deviceID {
		t.Fatalf("deviceID not persisted across restarts: c1=%q c2=%q", c1.deviceID, c2.deviceID)
	}
}

// TestHandleNoAvailable_StartupGraceKeepsCurrent is the regression test
// for the multi-device boot case: machine B is currently leasing the only
// eligible account, so machine A's first reconcile gets ErrNoAvailable.
// Without a grace window, handleNoAvailable would restoreLocked and
// blank the user's keychain — the same symptom we already fixed for the
// single-device stale-lease case via deviceID persistence. Inside the
// grace window the keychain must be left alone.
func TestHandleNoAvailable_StartupGraceKeepsCurrent(t *testing.T) {
	c, be, st, _ := newCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	// Prime an injection so currentAccountID != 0 and the keychain has
	// the foxy blob — exactly the state a returning agent boots into.
	c.reconcile(context.Background())
	if c.CurrentAccountID() == 0 || !be.hasOAuth {
		t.Fatalf("setup failed: expected an injection (currentID=%d hasOAuth=%v)",
			c.CurrentAccountID(), be.hasOAuth)
	}
	initialID := c.CurrentAccountID()
	initialBlob := string(be.oauthBlob)

	// Pretend bootstrap just set the grace window. Frozen clock + future
	// startupGraceUntil keeps us inside it for the duration of the call.
	c.startupGraceUntil = c.clock().Add(time.Minute)

	c.handleNoAvailable()

	if c.CurrentAccountID() != initialID {
		t.Fatalf("startup grace ignored: currentAccountID went from %d to %d",
			initialID, c.CurrentAccountID())
	}
	if string(be.oauthBlob) != initialBlob {
		t.Fatalf("startup grace ignored: keychain blob mutated")
	}
	if !be.hasOAuth {
		t.Fatalf("startup grace ignored: keychain blob was deleted")
	}
}

// TestSetAutoSwitchSource_OverridesSvc is the regression test for the
// agent-mode auto-switch inconsistency: the desktop's /api/auto-switch
// writes to the agent-local store, but until this wiring landed the
// Coordinator's choose() called svc.GetAutoSwitch (the remote vault).
// With auto-switch=disabled in the override and currentID=0, chooseManual
// must return ErrNoAvailable and no inject happens — even when the
// remote svc still says enabled.
func TestSetAutoSwitchSource_OverridesSvc(t *testing.T) {
	c, be, st, _ := newCoord(t)
	// Underlying svc (vault.InProc) defaults to auto-switch enabled. Seed
	// an active account so an enabled-path reconcile would inject it.
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	// Override says auto-switch is OFF. With currentID=0 and no pinned
	// account (LastUsedAt > 0 from Upsert defaults), chooseManual returns
	// ErrNoAvailable → handleNoAvailable does not inject.
	c.SetAutoSwitchSource(func(_ context.Context) (vault.AutoSwitch, error) {
		return vault.AutoSwitch{Enabled: false, Policy: "lru"}, nil
	})

	c.reconcile(context.Background())

	if be.hasOAuth {
		t.Fatalf("auto-switch override (off) was ignored: blob written despite manual mode + no pin")
	}
	if c.CurrentAccountID() != 0 {
		t.Fatalf("CurrentAccountID: got %d want 0 (no inject expected)", c.CurrentAccountID())
	}
}

// TestNew_HonoursExplicitDeviceID covers the agent-mode path: the caller
// passes the pair-assigned cfg.DeviceID, and the coordinator must use it
// verbatim (not generate or load a different one). This is what keeps the
// vault's devices table and leases table coherent for the same physical
// agent across restarts.
func TestNew_HonoursExplicitDeviceID(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	be := &fakeBackend{}
	logger := log.New(io.Discard, "", 0)

	const explicit = "device-from-pair-flow"
	c := New(vault.NewInProc(st), be, dir, logger, explicit)
	if c.deviceID != explicit {
		t.Fatalf("deviceID: got %q want %q", c.deviceID, explicit)
	}
}

// TestReconcile_EmitsReinjectOnTokenRotation pins the activity surface for the
// "same account, new token" reconcile path. Without this emit, agent mode is
// silent on every token rotation — making it impossible to tell from the
// activity timeline whether a reverseSync race overwrote a freshly-rotated
// keychain blob with the vault's stale copy.
func TestReconcile_EmitsReinjectOnTokenRotation(t *testing.T) {
	c, _, st, _ := newCoord(t)

	bus, err := activity.NewBus(st.DB(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("activity bus: %v", err)
	}
	c.SetBus(bus)

	id := seedActive(t, st, "alpha", "sk-ant-oat01-V1")

	// First reconcile: prev==0 path emits "Injected alpha" — the baseline
	// we already cover elsewhere; sanity-counting it lets the assertion
	// below distinguish the *new* re-inject event from this one.
	c.reconcile(context.Background())
	if c.CurrentAccountID() != id {
		t.Fatalf("first inject: CurrentAccountID=%d", c.CurrentAccountID())
	}

	// Simulate vault-side token rotation for the same account: the access
	// token (and hash) changes, but the account ID does not.
	newExpiry := time.Now().Add(2 * time.Hour).UnixMilli()
	if err := st.UpdateTokens(context.Background(), id,
		"sk-ant-oat01-V2", "sk-ant-ort01-V2", newExpiry); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}

	// Second reconcile detects the rotated token and re-injects.
	c.reconcile(context.Background())

	var found bool
	for _, ev := range bus.List(activity.Filter{}) {
		if ev.Type != activity.TypeCredInjected || ev.AccountID != id {
			continue
		}
		if strings.Contains(strings.ToLower(ev.Message), "re-injected") ||
			strings.Contains(strings.ToLower(ev.Message), "rotated") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected cred.injected re-inject/rotated event for account %d after token rotation; got events: %+v",
			id, bus.List(activity.Filter{}))
	}
}

// TestHandleNoAvailable_HoldsKeychainAfterGrace is the regression test for
// the rate-limit-storm 401 reported by users on macOS. Before the fix, when
// every account simultaneously crossed its 95% threshold (or got paused),
// handleNoAvailable wrote the user's pre-foxy native credentials back into
// the keychain. Running CC sessions on macOS still had the previously-
// injected token cached in lodash.memoize; the next 401 retry re-read the
// keychain, picked up the stale native creds, and surfaced "Failed to
// authenticate. API Error: 401 Invalid authentication credentials" to the
// user. The fix: outside startup grace, hold the current keychain in place
// instead of restoring native — when the rate-limit window resets, reconcile
// re-injects the same account and the running session continues.
func TestHandleNoAvailable_HoldsKeychainAfterGrace(t *testing.T) {
	c, be, st, _ := newCoord(t)
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	c.reconcile(context.Background())
	if c.CurrentAccountID() != id || !be.hasOAuth {
		t.Fatalf("setup: expected initial inject (currentID=%d hasOAuth=%v)",
			c.CurrentAccountID(), be.hasOAuth)
	}
	initialBlob := append([]byte(nil), be.oauthBlob...)
	initialDeletes := be.deletes

	// Pause the only account so choose() returns ErrNoAvailable on the next
	// reconcile — the rate-limit case picks the same path.
	if err := st.SetStatus(context.Background(), id, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	// Force grace to be in the past so the existing grace-window guard does
	// not mask the new behaviour.
	c.startupGraceUntil = c.clock().Add(-time.Second)

	c.handleNoAvailable()

	if c.CurrentAccountID() != id {
		t.Errorf("CurrentAccountID changed: got %d want %d (account should still be marked as last-injected)",
			c.CurrentAccountID(), id)
	}
	if !be.hasOAuth {
		t.Error("keychain blob was deleted; expected hold")
	}
	if string(be.oauthBlob) != string(initialBlob) {
		t.Errorf("keychain blob mutated: got %q want %q", string(be.oauthBlob), string(initialBlob))
	}
	if be.deletes != initialDeletes {
		t.Errorf("DeleteOAuthBlob called: got %d want %d", be.deletes, initialDeletes)
	}
}

// TestHandleNoAvailable_DoesNotSpamWarn pins the deduplication: the reconcile
// loop fires every 5s, so without a flag a multi-minute rate-limit window
// would post a warn event per tick (12/min × N minutes) into the activity
// log. The contract is one warn per "transition into all-unavailable", not
// per tick.
func TestHandleNoAvailable_DoesNotSpamWarn(t *testing.T) {
	c, _, st, _ := newCoord(t)

	bus, err := activity.NewBus(st.DB(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("activity bus: %v", err)
	}
	c.SetBus(bus)

	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())
	if c.CurrentAccountID() != id {
		t.Fatalf("setup: expected initial inject")
	}
	if err := st.SetStatus(context.Background(), id, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}
	c.startupGraceUntil = c.clock().Add(-time.Second)

	c.handleNoAvailable()
	c.handleNoAvailable()
	c.handleNoAvailable()

	var warns int
	for _, ev := range bus.List(activity.Filter{}) {
		if ev.Type == activity.TypeCredRestored {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("expected exactly 1 cred.restored warn across 3 handleNoAvailable calls, got %d; events: %+v",
			warns, bus.List(activity.Filter{}))
	}
}

// TestReverseSync_EmitsExternalRotationEvent pins the activity surface for the
// path where Claude Code rotated the keychain blob behind the vault. Without
// this emit, the agent silently uploads new tokens and the user has no way
// to correlate that with a downstream "credentials not found" symptom.
func TestReverseSync_EmitsExternalRotationEvent(t *testing.T) {
	c, be, st, _ := newCoord(t)

	bus, err := activity.NewBus(st.DB(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("activity bus: %v", err)
	}
	c.SetBus(bus)

	id := seedActive(t, st, "alpha", "sk-ant-oat01-V1")
	c.reconcile(context.Background())
	if c.CurrentAccountID() != id {
		t.Fatalf("inject: CurrentAccountID=%d", c.CurrentAccountID())
	}

	// Simulate Claude Code rotating the keychain entry. The new blob has a
	// different access token so reverseSync's hash check fires and the
	// store gets the rotated tuple via UpdateTokens.
	rotatedExpiry := time.Now().Add(8 * time.Hour).UnixMilli()
	rotatedBlob, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "sk-ant-oat01-NEW",
			"refreshToken": "sk-ant-ort01-NEW",
			"expiresAt":    rotatedExpiry,
		},
	})
	be.mu.Lock()
	be.oauthBlob = rotatedBlob
	be.mu.Unlock()

	c.reverseSync(context.Background())

	var found bool
	for _, ev := range bus.List(activity.Filter{}) {
		if ev.Type != activity.TypeTokenRefreshed || ev.AccountID != id {
			continue
		}
		if strings.Contains(strings.ToLower(ev.Message), "external") ||
			strings.Contains(strings.ToLower(ev.Message), "rotated") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected token.refreshed externally-rotated event for account %d after reverseSync; got events: %+v",
			id, bus.List(activity.Filter{}))
	}
}
