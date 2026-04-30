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
	c := New(st, be, dir, log.New(io.Discard, "", 0))
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

	// Disable account A → next reconcile must switch to beta.
	if err := st.SetStatus(context.Background(), idA, "disabled"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	c.reconcile(context.Background())
	got := extractAccessToken(be.oauthBlob)
	if got != "sk-ant-oat01-beta" {
		t.Errorf("after disable A: injected token %q (expected beta)", got)
	}
	if c.CurrentAccountID() == idA {
		t.Errorf("CurrentAccountID still A after disable")
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

	// Inject one account, then disable it so the next reconcile sees no
	// available accounts and triggers restore.
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")
	c.reconcile(context.Background())
	if c.CurrentAccountID() != id {
		t.Fatalf("inject: CurrentAccountID=%d", c.CurrentAccountID())
	}
	if err := st.SetStatus(context.Background(), id, "disabled"); err != nil {
		t.Fatalf("disable: %v", err)
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
