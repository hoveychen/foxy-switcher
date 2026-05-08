package credinject

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
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

	// DefaultLeaseTTL is comfortable headroom over reconcileInterval — the
	// reconcile loop renews on every tick, so even if the daemon misses one
	// (suspended laptop, blocked goroutine), the next tick still lands well
	// before the lease lapses.
	DefaultLeaseTTL = time.Minute

	// StartupGracePeriod is how long bootstrap suppresses the keychain
	// restore-on-no-available path. Sized to lease TTL + sweep cadence so a
	// stale lease left by another device (or our own previous process) has
	// time to expire and become Pickable before we conclude the pool is
	// empty and blank the user's keychain.
	StartupGracePeriod = 90 * time.Second
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
	svc     vault.Service
	logger  *log.Logger
	backend Backend
	dataDir string
	clock   func() time.Time

	// Bus is the optional activity hub the coordinator emits cred.injected /
	// cred.restored / cred.failed events to. Nil-safe — set via SetBus once
	// the bus is constructed in main; tests leave it nil.
	bus *activity.Bus

	reconcileInterval   time.Duration
	reverseSyncInterval time.Duration

	trigger chan struct{}

	// deviceID identifies this agent to the vault. Random per process; Step 4
	// will persist it so the same machine reuses an ID across restarts (so
	// the vault can recognise the same physical agent reattaching).
	deviceID string
	// leaseTTL is the lifetime requested on AcquireLease/RenewLease. The
	// reconcile loop renews on every tick.
	leaseTTL time.Duration

	mu               sync.Mutex
	currentAccountID int64
	currentLeaseID   string
	lastAccessHash   string
	// restoreOnQuit gates the shutdown restore path. True (default) puts the
	// user's native blob back when the daemon exits cleanly; false leaves the
	// last-injected credential in the keychain — useful when a user wants
	// Claude Code to keep using the foxy account between sessions.
	restoreOnQuit bool

	// startupGraceUntil suppresses the "restore native credentials" path of
	// handleNoAvailable while it is in the future. bootstrap sets it to
	// clock+StartupGracePeriod so the very first reconcile can't blank a
	// healthy keychain just because the vault briefly has no eligible
	// account (typical cause: another device on the same vault is still
	// holding a lease). Once the grace window passes the normal restore
	// behaviour resumes.
	startupGraceUntil time.Time

	// autoSwitchSource, when non-nil, replaces svc.GetAutoSwitch as the
	// authority for "is auto-switch enabled?". Agent mode wires this to a
	// local store-backed lookup so the desktop's per-agent toggle (which
	// writes to agent-activity.db via /api/auto-switch) actually drives
	// credinject's choose() decision instead of being silently ignored.
	// Daemon/combined mode leaves it nil — the in-process svc already reads
	// the same store the frontend writes to.
	autoSwitchSource func(context.Context) (vault.AutoSwitch, error)
}

// SetAutoSwitchSource overrides where Coordinator reads auto-switch from.
// Agent mode passes a closure backed by its local store.GetAutoSwitch so
// the per-agent kv row drives behaviour. Pass nil to revert to svc.
func (c *Coordinator) SetAutoSwitchSource(fn func(context.Context) (vault.AutoSwitch, error)) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Stub: store the function but choose() still asks svc until P2.
	c.autoSwitchSource = fn
}

// SetBus wires the activity bus after construction. Optional — when nil,
// emit calls below silently no-op (Bus methods are nil-safe). Tests that
// don't care about activity simply don't call SetBus.
func (c *Coordinator) SetBus(b *activity.Bus) {
	if c == nil {
		return
	}
	c.bus = b
}

// New constructs a Coordinator. The dataDir is where injected.json /
// native-cred-backup.json live (typically ~/.foxy-switcher).
//
// deviceID identifies this agent to the vault for lease bookkeeping. Agent
// mode passes the pair-assigned cfg.DeviceID so the vault's devices/leases
// tables stay coherent across restarts. Pass "" to fall back to a per-dataDir
// persisted ID — daemon/combined mode and tests use that path.
//
// Why the persistence matters: the vault's leases table is keyed by
// (account_id, device_id). If a previous process exited without releasing
// (kill -9, OS reboot, Tauri crash) and the new process gets a fresh random
// device_id, AcquireLease returns ErrLeaseLocked until the stale row's TTL
// expires (≤60s). During that window the coordinator falls back to "no
// available account" and restores the user's native credentials, blanking
// the keychain for ~minutes — exactly the "agent doesn't auto-inject on
// startup" symptom we hit in production.
func New(svc vault.Service, backend Backend, dataDir string, logger *log.Logger, deviceID string) *Coordinator {
	if logger == nil {
		logger = log.Default()
	}
	if deviceID == "" {
		deviceID = loadOrGenDeviceID(dataDir, logger)
	}
	return &Coordinator{
		svc:                 svc,
		logger:              logger,
		backend:             backend,
		dataDir:             dataDir,
		clock:               time.Now,
		reconcileInterval:   DefaultReconcileInterval,
		reverseSyncInterval: DefaultReverseSyncInterval,
		trigger:             make(chan struct{}, 1),
		deviceID:            deviceID,
		leaseTTL:            DefaultLeaseTTL,
		restoreOnQuit:       true,
	}
}

// newDeviceID returns a random opaque identifier.
func newDeviceID() string {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Crypto/rand failure is fatal-grade — falling back to a degenerate
		// fixed ID would silently collapse multiple agents into one slot.
		panic("credinject: rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}

// deviceIDFile is where loadOrGenDeviceID stashes the persisted ID for
// daemon/combined mode. Agent mode bypasses this entirely by passing the
// pair-assigned cfg.DeviceID into New.
const deviceIDFile = "device-id"

// loadOrGenDeviceID returns a deviceID that survives across process restarts
// for the same dataDir. On first call it generates a fresh ID and writes it;
// on subsequent calls it reads the file. Read errors and partial writes both
// fall back to a fresh in-memory ID so a corrupted file never wedges startup
// (the cost is one extra "stale lease 60s" cycle, not a crash).
func loadOrGenDeviceID(dataDir string, logger *log.Logger) string {
	path := filepath.Join(dataDir, deviceIDFile)
	if buf, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(buf))
		if id != "" {
			return id
		}
	} else if !os.IsNotExist(err) {
		logger.Printf("[credinject] read %s: %v (regenerating)", path, err)
	}
	id := newDeviceID()
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		logger.Printf("[credinject] persist device-id: %v (will regenerate next start)", err)
	}
	return id
}

// SetRestoreOnQuit toggles whether RestoreOnShutdown actually restores the
// native credential blob. Called from PUT /api/settings so the user's
// preference takes effect without a daemon restart. Safe on a nil receiver.
func (c *Coordinator) SetRestoreOnQuit(v bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restoreOnQuit = v
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

// CurrentAccountID returns the ID of the account currently injected, or 0
// if nothing is. The vault uses LeaseStore.IsLeased to skip refresh on
// in-use accounts; this method exists for tests and for the /api/cred/
// status surface.
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
	c.bootstrap(ctx)

	reconcileT := time.NewTicker(c.reconcileInterval)
	defer reconcileT.Stop()
	syncT := time.NewTicker(c.reverseSyncInterval)
	defer syncT.Stop()

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

// bootstrap is the cold-start sequence: load persistent state, sync the
// store with whatever the keychain currently holds, then reconcile.
//
// Why reverseSync runs first: choose() decides eligibility from
// store.expires_at. If the daemon was off while CC (or another tool, or
// another machine) rotated the keychain blob, the store still has the old
// expires_at — long past. IsEligible would then mark the previously-injected
// account "expired", choose() would fall back to LRU and pick a different
// account, and reconcile would write that account's blob into the keychain,
// clobbering the externally-rotated tokens. Pulling the rotation back into
// the store first keeps the previously-injected account eligible.
func (c *Coordinator) bootstrap(ctx context.Context) {
	c.mu.Lock()
	c.startupGraceUntil = c.clock().Add(StartupGracePeriod)
	c.mu.Unlock()
	c.loadState()
	c.reverseSync(ctx)
	c.reconcile(ctx)
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

// choose decides which account reconcile should target this tick. The policy
// is sticky-with-explicit-pin: keep the currently-injected account if it's
// still eligible (active, token live, under threshold) AND no other account
// has had its last_used_at zeroed (the MarkForNextPick sentinel used by
// /select and fresh-add). Otherwise fall back to the LRU selector.
//
// Why: without stickiness, every 5s reconcile re-runs LRU; MarkUsed bumps the
// just-injected account to "now", which makes the *other* eligible account
// look least-recently-used, so the next tick flips to it — and so on
// forever. The "In use" badge in the UI ping-pongs every refresh.
//
// Manual mode (auto_switch.enabled=false): never spontaneously rotate. Only
// switch when the user explicitly pinned (MarkForNextPick → LastUsedAt==0) or
// when the current account becomes ineligible. Without an eligible target,
// return ErrNoAvailable so the caller restores the user's native creds.
func (c *Coordinator) choose(ctx context.Context) (*vault.Account, error) {
	c.mu.Lock()
	src := c.autoSwitchSource
	c.mu.Unlock()
	getAuto := func(ctx context.Context) (vault.AutoSwitch, error) {
		if src != nil {
			return src(ctx)
		}
		return c.svc.GetAutoSwitch(ctx)
	}
	auto, err := getAuto(ctx)
	if err != nil {
		c.logger.Printf("[credinject] read auto-switch: %v", err)
		auto = vault.AutoSwitch{Enabled: true, Policy: "lru"}
	}

	c.mu.Lock()
	currentID := c.currentAccountID
	c.mu.Unlock()

	if !auto.Enabled {
		return c.chooseManual(ctx, currentID)
	}

	if currentID == 0 {
		return c.svc.PickForDevice(ctx, c.clock(), c.deviceID)
	}
	accs, err := c.svc.ListAccounts(ctx)
	if err != nil {
		return c.svc.PickForDevice(ctx, c.clock(), c.deviceID)
	}
	now := c.clock()
	var cur *vault.Account
	var pinnedOther bool
	for i := range accs {
		a := accs[i]
		// Reuse the selector's eligibility predicate so the sticky path
		// honours every disqualifier (paused, expired token, usage
		// threshold) — otherwise we'd happily re-inject a dead account
		// just because it was the previous "in use" one.
		if !selector.IsEligible(a, now) {
			continue
		}
		if a.ID == currentID {
			ac := a
			cur = &ac
			continue
		}
		if a.LastUsedAt == 0 {
			pinnedOther = true
		}
	}
	if cur != nil && !pinnedOther {
		return cur, nil
	}
	return c.svc.PickForDevice(ctx, c.clock(), c.deviceID)
}

// chooseManual implements the auto-switch=off path. Order:
//  1. Honour an explicit pin (LastUsedAt==0) — that's how the UI's "Use now"
//     button reaches us, and a manual user expects clicks to take effect.
//  2. Stick to the current account if it's still eligible.
//  3. Otherwise no rotation — return ErrNoAvailable so credinject restores
//     native creds rather than silently picking some other pool member.
func (c *Coordinator) chooseManual(ctx context.Context, currentID int64) (*vault.Account, error) {
	accs, err := c.svc.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	now := c.clock()
	var pinned, cur *vault.Account
	for i := range accs {
		a := accs[i]
		if !selector.IsEligible(a, now) {
			continue
		}
		if a.LastUsedAt == 0 && (pinned == nil || a.ID < pinned.ID) {
			pp := a
			pinned = &pp
		}
		if a.ID == currentID {
			cc := a
			cur = &cc
		}
	}
	if pinned != nil {
		return pinned, nil
	}
	if cur != nil {
		return cur, nil
	}
	return nil, selector.ErrNoAvailable
}

func (c *Coordinator) reconcile(ctx context.Context) {
	a, err := c.choose(ctx)
	if err != nil {
		if errors.Is(err, selector.ErrNoAvailable) {
			c.handleNoAvailable()
			return
		}
		c.logger.Printf("[credinject] selector.Pick: %v", err)
		return
	}

	// Lease bookkeeping. Run before the no-op early return so a non-switch
	// tick still renews the lease — otherwise the vault's refresh scheduler
	// would think this account is free and start rotating it parallel to CC.
	// If the vault rejects the lease (another device is already injecting
	// this account), bail out without writing the keychain. The next
	// reconcile will pick a different account because vault.Pick excludes
	// foreign-leased accounts.
	if err := c.refreshLease(ctx, a.ID); err != nil {
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
		c.bus.EmitError(activity.TypeCredFailed, a.ID,
			fmt.Sprintf("Build OAuth blob failed: %v", err))
		return
	}
	if err := c.backend.WriteOAuthBlob(blob); err != nil {
		c.logger.Printf("[credinject] write OAuth blob (account %d): %v", a.ID, err)
		c.bus.EmitError(activity.TypeCredFailed, a.ID,
			fmt.Sprintf("Write OAuth blob failed: %v", err))
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
		if err := c.svc.MarkUsed(ctx, a.ID); err != nil {
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
		// Emit so agent-mode debugging has a timeline to read against
		// reverseSync events: a fast string of "Re-injected" without an
		// intervening "rotated externally" is the signature of the
		// reconcile-vs-reverseSync race overwriting CC-rotated tokens.
		c.bus.EmitInfo(activity.TypeCredInjected, a.ID,
			fmt.Sprintf("Re-injected %s — token rotated", a.Name))
	} else if prev == 0 {
		c.logger.Printf("[credinject] injected account %d (%s)", a.ID, a.Name)
		c.bus.EmitInfo(activity.TypeCredInjected, a.ID,
			fmt.Sprintf("Injected %s", a.Name))
	} else {
		c.logger.Printf("[credinject] switched: account %d → %d (%s)", prev, a.ID, a.Name)
		c.bus.EmitInfo(activity.TypeCredInjected, a.ID,
			fmt.Sprintf("Switched to %s", a.Name))
	}
}

// refreshLease keeps the agent's claim on accountID alive at the vault. On
// account changes (or first-time pick) it acquires a fresh lease; on the
// stickiness path it just renews. A renew that fails because the lease
// has been reaped (vault GC, vault restart) falls through to acquire so
// the steady state always has a live lease.
//
// Returns vault.ErrLeaseLocked when another device is already injecting
// this account. The reconcile loop bails on that — injecting in parallel
// with another agent races the one-time-use refresh_token and is exactly
// what the lease was added to prevent.
func (c *Coordinator) refreshLease(ctx context.Context, accountID int64) error {
	c.mu.Lock()
	prevAccountID := c.currentAccountID
	leaseID := c.currentLeaseID
	c.mu.Unlock()

	if leaseID != "" && prevAccountID == accountID {
		if _, err := c.svc.RenewLease(ctx, leaseID, c.leaseTTL); err == nil {
			return nil
		}
		// Lease vanished server-side; re-acquire below.
	}

	lease, err := c.svc.AcquireLease(ctx, accountID, c.deviceID, c.leaseTTL)
	if err != nil {
		c.logger.Printf("[credinject] acquire lease for account %d: %v", accountID, err)
		return err
	}
	c.mu.Lock()
	c.currentLeaseID = lease.ID
	c.mu.Unlock()
	return nil
}

// releaseLease drops the agent's claim. Idempotent on the wire; clears local
// bookkeeping unconditionally so retries after a transient release error
// don't leave dangling state.
func (c *Coordinator) releaseLease(ctx context.Context) {
	c.mu.Lock()
	leaseID := c.currentLeaseID
	c.currentLeaseID = ""
	c.mu.Unlock()
	if leaseID == "" {
		return
	}
	if err := c.svc.ReleaseLease(ctx, leaseID); err != nil {
		c.logger.Printf("[credinject] release lease %s: %v", leaseID, err)
	}
}

// handleNoAvailable runs when selector.Pick returns ErrNoAvailable. Restores
// the user's native credentials so Claude Code keeps working with their own
// login while the foxy pool is empty / rate-limited.
//
// During the bootstrap grace window we suppress the restore: a transient
// ErrNoAvailable right after launch is usually a stale lease (own or
// another device's) that will clear within a minute. Reconcile retries
// every 5s, so once the lease lapses we converge without ever touching
// the user's keychain.
func (c *Coordinator) handleNoAvailable() {
	c.mu.Lock()
	if c.currentAccountID == 0 {
		c.mu.Unlock()
		return
	}
	if c.clock().Before(c.startupGraceUntil) {
		c.mu.Unlock()
		c.logger.Print("[credinject] no available account during startup grace; keeping current keychain")
		return
	}
	c.mu.Unlock()

	if err := c.restoreLocked(); err != nil {
		c.logger.Printf("[credinject] restore native creds: %v", err)
		return
	}
	c.releaseLease(context.Background())
	c.mu.Lock()
	c.currentAccountID = 0
	c.lastAccessHash = ""
	c.mu.Unlock()
	if err := clearState(c.dataDir); err != nil {
		c.logger.Printf("[credinject] clear state: %v", err)
	}
	c.logger.Print("[credinject] no available account — restored native credentials")
	c.bus.EmitWarn(activity.TypeCredRestored, 0,
		"All accounts unavailable — restored native credentials")
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
	if err := c.svc.UpdateTokens(ctx, id, at, rt, exp); err != nil {
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
	// Emit so agent-mode debugging can correlate keychain-side rotations
	// (CC writing new tokens behind us) with reconcile re-inject events
	// — see the comment in reconcile's prev==a.ID branch.
	c.bus.EmitInfo(activity.TypeTokenRefreshed, id,
		fmt.Sprintf("Account #%d rotated externally — synced to vault", id))
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
	accs, err := c.svc.ListAccounts(context.Background())
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
	c.mu.Lock()
	restore := c.restoreOnQuit
	c.mu.Unlock()
	if !restore {
		c.logger.Print("[credinject] shutdown: restore_native_on_quit=false, leaving injected credentials in place")
		return nil
	}
	err := c.restoreLocked()
	if err != nil {
		return err
	}
	c.releaseLease(context.Background())
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

// DeviceID returns this Coordinator's stable identity used in lease
// bookkeeping. Nil-safe — returns "" when called on a nil receiver, so
// callers like the agent's dashboard patch can probe without first
// checking whether a Coordinator was wired.
func (c *Coordinator) DeviceID() string {
	if c == nil {
		return ""
	}
	return c.deviceID
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
