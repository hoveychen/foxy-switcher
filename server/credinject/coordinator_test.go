package credinject

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	c := New(vault.NewInProc(st), be, dir, log.New(io.Discard, "", 0))
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

func TestReconcile_NoAvailable_RestoresBackup(t *testing.T) {
	c, be, st, dir := newCoord(t)

	// Pre-populate a "native" backup the way maybeSnapshotNative would have.
	nativeBlob := []byte(`{"native":"yes"}`)
	if err := writeBackup(dir, backupFile{
		OAuthBlob:     nativeBlob,
		ManagedAPIKey: "sk-ant-api03-native",
		SnapshotAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatalf("writeBackup: %v", err)
	}

	// Inject one account, then pause it so the next reconcile sees no
	// available accounts and triggers restore.
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())
	if c.CurrentAccountID() != id {
		t.Fatalf("inject: CurrentAccountID=%d", c.CurrentAccountID())
	}
	if err := st.SetStatus(context.Background(), id, "paused"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	c.reconcile(context.Background())

	if c.CurrentAccountID() != 0 {
		t.Errorf("after no-available: CurrentAccountID=%d, want 0", c.CurrentAccountID())
	}
	if !be.hasOAuth {
		t.Fatal("expected restore to put native blob back")
	}
	if string(be.oauthBlob) != string(nativeBlob) {
		t.Errorf("restored blob mismatch: got %q", string(be.oauthBlob))
	}
	if !be.hasAPIKey || be.apiKey != "sk-ant-api03-native" {
		t.Errorf("restored api key mismatch: hasAPIKey=%v apiKey=%q", be.hasAPIKey, be.apiKey)
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

// TestReconcile_ManualMode_RestoresWhenIneligible covers the safety net: if
// the account currently injected becomes ineligible (paused, cooldown,
// over-threshold), manual mode must NOT silently rotate to a different one —
// it must restore the user's native creds, same as auto mode with empty pool.
func TestReconcile_ManualMode_RestoresWhenIneligible(t *testing.T) {
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

	if err := st.SetAutoSwitch(context.Background(), store.AutoSwitch{Enabled: false, Policy: "lru"}); err != nil {
		t.Fatalf("SetAutoSwitch: %v", err)
	}
	if err := st.SetStatus(context.Background(), idA, "paused"); err != nil {
		t.Fatalf("pause idA: %v", err)
	}

	c.reconcile(context.Background())

	if c.CurrentAccountID() != 0 {
		t.Errorf("expected restore to clear current account, got %d", c.CurrentAccountID())
	}
	if got := extractAccessToken(be.oauthBlob); got == "sk-ant-oat01-beta" {
		t.Errorf("manual mode auto-rotated to beta; expected native restore instead")
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
