package credinject

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// Default cadences for the Coordinator. The reconcile interval matches the
// old HookCoordinator's 5-second beat (mutation routes also call Trigger()
// for sub-second response). The reverse-sync interval is slower because the
// only thing it catches is Claude Code refreshing tokens behind our back —
// 30s is plenty of slack between the rotation happening and us reflecting
// it back into the store.
const (
	DefaultReconcileInterval   = 5 * time.Second
	DefaultReverseSyncInterval = 30 * time.Second
)

// Coordinator owns the credinjection state machine. One instance per daemon.
//
// Lifecycle:
//   - Construct via New.
//   - Wire mutation triggers: every account-state change should call
//     Trigger() on its way out (the existing Hook plumbing did this; we
//     reuse the same call sites).
//   - Run on its own goroutine until the parent ctx is done.
//   - On graceful shutdown, call RestoreOnShutdown to put the user's native
//     credentials back.
type Coordinator struct {
	store    *store.Store
	logger   *log.Logger
	backend  Backend
	dataDir  string
	clock    func() time.Time
	listFn   func(ctx context.Context, st *store.Store) ([]store.Account, error) // overridable in tests
	pickFn   func(ctx context.Context, st *store.Store, now time.Time) (*store.Account, error)
	updateFn func(ctx context.Context, st *store.Store, id int64, accessToken, refreshToken string, expiresAt int64) error

	reconcileInterval   time.Duration
	reverseSyncInterval time.Duration

	trigger chan struct{}

	mu               sync.Mutex
	currentAccountID int64
	lastAccessHash   string
}

// New constructs a Coordinator. The dataDir is where injected.json /
// native-cred-backup.json live (typically ~/.foxy-switcher).
func New(st *store.Store, backend Backend, dataDir string, logger *log.Logger) *Coordinator {
	if logger == nil {
		logger = log.Default()
	}
	return &Coordinator{
		store:               st,
		logger:              logger,
		backend:             backend,
		dataDir:             dataDir,
		clock:               time.Now,
		reconcileInterval:   DefaultReconcileInterval,
		reverseSyncInterval: DefaultReverseSyncInterval,
		trigger:             make(chan struct{}, 1),
		pickFn: func(ctx context.Context, st *store.Store, now time.Time) (*store.Account, error) {
			return selector.Pick(ctx, st, now)
		},
		listFn: func(ctx context.Context, st *store.Store) ([]store.Account, error) {
			return st.List(ctx)
		},
		updateFn: func(ctx context.Context, st *store.Store, id int64, at, rt string, exp int64) error {
			return st.UpdateTokens(ctx, id, at, rt, exp)
		},
	}
}

// Trigger schedules an immediate reconcile. Multiple triggers between two
// ticks collapse into one (non-blocking send into a 1-buffered channel).
// Safe on a nil receiver — that's the path tests take when constructing a
// bare Server without wiring credinject.
func (c *Coordinator) Trigger() {
	if c == nil {
		return
	}
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// CurrentAccountID returns the ID of the account currently injected, or 0 if
// nothing is. Used by refresh.Scheduler to skip the injected account (Claude
// Code owns its refresh while it's the active credential).
func (c *Coordinator) CurrentAccountID() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentAccountID
}

// Run executes the coordinator's main loop until ctx is cancelled. Restores
// state from disk on entry so a daemon restart resumes reverse-sync without
// having to figure out which account is currently in the keychain.
func (c *Coordinator) Run(ctx context.Context) {
	c.loadState()

	reconcileT := time.NewTicker(c.reconcileInterval)
	defer reconcileT.Stop()
	syncT := time.NewTicker(c.reverseSyncInterval)
	defer syncT.Stop()

	c.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcileT.C:
			c.reconcile(ctx)
		case <-c.trigger:
			c.reconcile(ctx)
		case <-syncT.C:
			c.reverseSync(ctx)
		}
	}
}

// loadState reads injected.json and warms the in-memory pointers. Missing
// state file is normal — fresh install or first run after the
// hook-coordinator era.
func (c *Coordinator) loadState() {
	st, ok, err := readState(c.dataDir)
	if err != nil {
		c.logger.Printf("[credinject] read state: %v", err)
		return
	}
	if !ok {
		return
	}
	c.mu.Lock()
	c.currentAccountID = st.AccountID
	c.lastAccessHash = st.AccessHash
	c.mu.Unlock()
}

func (c *Coordinator) reconcile(ctx context.Context) {
	a, err := c.pickFn(ctx, c.store, c.clock())
	if err != nil {
		if errors.Is(err, selector.ErrNoAvailable) {
			c.handleNoAvailable()
			return
		}
		c.logger.Printf("[credinject] selector.Pick: %v", err)
		return
	}

	hash := hashToken(a.AccessToken)
	c.mu.Lock()
	if c.currentAccountID == a.ID && c.lastAccessHash == hash {
		c.mu.Unlock()
		return
	}
	prev := c.currentAccountID
	c.mu.Unlock()

	c.maybeSnapshotNative()

	blob, err := buildOAuthBlob(a)
	if err != nil {
		c.logger.Printf("[credinject] build blob for account %d: %v", a.ID, err)
		return
	}
	if err := c.backend.WriteOAuthBlob(blob); err != nil {
		c.logger.Printf("[credinject] write OAuth blob (account %d): %v", a.ID, err)
		return
	}
	// Best-effort: flush the managed-API-key item so Claude Code can't pick
	// it ahead of our OAuth blob. Failure here doesn't undo the OAuth write.
	if err := c.backend.DeleteManagedAPIKey(); err != nil {
		c.logger.Printf("[credinject] delete managed api key: %v", err)
	}

	c.mu.Lock()
	c.currentAccountID = a.ID
	c.lastAccessHash = hash
	c.mu.Unlock()
	// Bump LRU only on a real switch — re-injecting the same account because
	// its token rotated isn't a fresh "use" from the pool's perspective.
	if prev != a.ID {
		if err := c.store.MarkUsed(ctx, a.ID); err != nil {
			c.logger.Printf("[credinject] mark used (account %d): %v", a.ID, err)
		}
	}
	if err := writeState(c.dataDir, stateFile{
		AccountID:  a.ID,
		AccessHash: hash,
		InjectedAt: c.clock().UnixMilli(),
	}); err != nil {
		c.logger.Printf("[credinject] persist state: %v", err)
	}

	if prev == a.ID {
		c.logger.Printf("[credinject] re-injected account %d (%s) — token rotated", a.ID, a.Name)
	} else if prev == 0 {
		c.logger.Printf("[credinject] injected account %d (%s)", a.ID, a.Name)
	} else {
		c.logger.Printf("[credinject] switched: account %d → %d (%s)", prev, a.ID, a.Name)
	}
}

// handleNoAvailable runs when selector.Pick returns ErrNoAvailable. Restores
// the user's native credentials so Claude Code keeps working with their own
// login while the foxy pool is empty / rate-limited.
func (c *Coordinator) handleNoAvailable() {
	c.mu.Lock()
	if c.currentAccountID == 0 {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	if err := c.restoreLocked(); err != nil {
		c.logger.Printf("[credinject] restore native creds: %v", err)
		return
	}
	c.mu.Lock()
	c.currentAccountID = 0
	c.lastAccessHash = ""
	c.mu.Unlock()
	if err := clearState(c.dataDir); err != nil {
		c.logger.Printf("[credinject] clear state: %v", err)
	}
	c.logger.Print("[credinject] no available account — restored native credentials")
}

// reverseSync pulls externally-rotated tokens back into the store. When
// Claude Code refreshes its access token it overwrites the keychain blob;
// without this loop the foxy store would still hold the old tokens, and
// `selector.Pick` would hand out a dead access_token on the next switch.
func (c *Coordinator) reverseSync(ctx context.Context) {
	c.mu.Lock()
	id := c.currentAccountID
	prevHash := c.lastAccessHash
	c.mu.Unlock()
	if id == 0 {
		return
	}
	blob, ok, err := c.backend.ReadOAuthBlob()
	if err != nil {
		c.logger.Printf("[credinject] reverse-sync read: %v", err)
		return
	}
	if !ok {
		// The blob disappeared. Treat it as "Claude Code logged out" — let
		// the next reconcile re-inject so things converge.
		return
	}
	at, rt, exp, ok := extractRotation(blob)
	if !ok || at == "" {
		return
	}
	hash := hashToken(at)
	if hash == prevHash {
		return
	}
	if err := c.updateFn(ctx, c.store, id, at, rt, exp); err != nil {
		c.logger.Printf("[credinject] reverse-sync update store (account %d): %v", id, err)
		return
	}
	c.mu.Lock()
	c.lastAccessHash = hash
	c.mu.Unlock()
	if err := writeState(c.dataDir, stateFile{
		AccountID:  id,
		AccessHash: hash,
		InjectedAt: c.clock().UnixMilli(),
	}); err != nil {
		c.logger.Printf("[credinject] reverse-sync persist state: %v", err)
	}
	c.logger.Printf("[credinject] reverse-sync: account %d rotated externally; new expiry in %s",
		id, time.Until(time.UnixMilli(exp)).Round(time.Second))
}

// maybeSnapshotNative captures the user's pre-foxy keychain content the
// first time the daemon needs to overwrite it. The check has to skip our
// own blobs — without that, restarting the daemon after a foxy injection
// would treat our last-injected token as the "native" backup.
func (c *Coordinator) maybeSnapshotNative() {
	if _, ok, _ := readBackup(c.dataDir); ok {
		return
	}
	blob, exists, err := c.backend.ReadOAuthBlob()
	if err != nil {
		c.logger.Printf("[credinject] snapshot: read keychain: %v", err)
		return
	}
	if exists && c.isFoxyOwned(blob) {
		// Keychain holds our prior injection — we can't infer what was here
		// before that; defer the decision rather than record a misleading
		// "empty" backup that would later overwrite a real native login.
		return
	}
	bf := backupFile{SnapshotAt: c.clock().UnixMilli()}
	if exists {
		bf.OAuthBlob = blob
		if k, ok, _ := c.backend.ReadManagedAPIKey(); ok {
			bf.ManagedAPIKey = k
		}
	}
	if err := writeBackup(c.dataDir, bf); err != nil {
		c.logger.Printf("[credinject] snapshot: write backup: %v", err)
		return
	}
	if len(bf.OAuthBlob) > 0 {
		c.logger.Print("[credinject] snapshot: native Claude Code login captured to native-cred-backup.json")
	} else {
		c.logger.Print("[credinject] snapshot: no native login present — recorded empty backup")
	}
}

// isFoxyOwned answers "is this keychain blob's access_token currently in our
// store?" — used to avoid mistaking a foxy-injected blob for a user's native
// login when capturing the backup.
func (c *Coordinator) isFoxyOwned(blob []byte) bool {
	at := extractAccessToken(blob)
	if at == "" {
		return false
	}
	accs, err := c.listFn(context.Background(), c.store)
	if err != nil {
		return false
	}
	for _, a := range accs {
		if a.AccessToken == at {
			return true
		}
	}
	return false
}

// RestoreOnShutdown is the explicit hook for graceful daemon exit. It writes
// the native backup back into the keychain (or clears it if no backup
// exists). Safe on a nil receiver.
func (c *Coordinator) RestoreOnShutdown() error {
	if c == nil {
		return nil
	}
	err := c.restoreLocked()
	if err != nil {
		return err
	}
	if err := clearState(c.dataDir); err != nil {
		c.logger.Printf("[credinject] clear state on shutdown: %v", err)
	}
	c.logger.Print("[credinject] shutdown: native credentials restored")
	return nil
}

// restoreLocked writes the snapshotted native blob/managed-key back into the
// backend. Missing snapshot → blank both items (the user can re-login through
// Claude Code's normal flow). Always idempotent.
func (c *Coordinator) restoreLocked() error {
	bf, ok, err := readBackup(c.dataDir)
	if err != nil {
		return err
	}
	if !ok {
		// No backup means we never overwrote a real native login (or it was
		// already empty). Clear our injection.
		_ = c.backend.DeleteOAuthBlob()
		_ = c.backend.DeleteManagedAPIKey()
		return nil
	}
	if len(bf.OAuthBlob) > 0 {
		if err := c.backend.WriteOAuthBlob(bf.OAuthBlob); err != nil {
			return err
		}
	} else {
		if err := c.backend.DeleteOAuthBlob(); err != nil {
			return err
		}
	}
	if bf.ManagedAPIKey != "" {
		if err := c.backend.WriteManagedAPIKey(bf.ManagedAPIKey); err != nil {
			return err
		}
	} else {
		if err := c.backend.DeleteManagedAPIKey(); err != nil {
			return err
		}
	}
	return nil
}

// Status describes the coordinator's current state for the /api/cred/status
// route. Reads are mutex-protected; the JSON shape is part of the public API.
type Status struct {
	ManagedAccountID    int64 `json:"managed_account_id"`
	NativeBackupPresent bool  `json:"native_backup_present"`
	InjectedAt          int64 `json:"injected_at"`
}

// Status snapshots the coordinator state for the HTTP surface.
func (c *Coordinator) Status() Status {
	if c == nil {
		return Status{}
	}
	c.mu.Lock()
	id := c.currentAccountID
	c.mu.Unlock()

	var injectedAt int64
	if st, ok, _ := readState(c.dataDir); ok {
		injectedAt = st.InjectedAt
	}
	_, hasBackup, _ := readBackup(c.dataDir)
	return Status{
		ManagedAccountID:    id,
		NativeBackupPresent: hasBackup,
		InjectedAt:          injectedAt,
	}
}

// hashToken returns a short fingerprint suitable for "did this change?"
// comparisons without storing the full access_token in the state file. Full
// SHA256 hex is plenty.
func hashToken(s string) string {
	if s == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
