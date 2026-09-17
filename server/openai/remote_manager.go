package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

const remoteLeaseTTL = time.Minute

// errIdleParked is ensureLease's benign "I'm idle and hold no slot, so I did
// nothing" result — the steady state of an idle device waiting for the user to
// run codex again, not a failure.
var errIdleParked = errors.New("codex: idle, lease parked")

// RemoteManager is the agent-mode counterpart to Manager. Account selection
// and persistence live on the vault; only provider-native credential storage
// and the native backup live on the agent device.
type RemoteManager struct {
	svc      vault.Service
	storage  CredentialStorage
	deviceID string
	logger   *log.Logger

	// activityDir is <CODEX_HOME>/sessions — the tree the Codex CLI appends
	// session rollouts to. idleFor() derives last-real-activity from the newest
	// rollout mtime there. Empty disables the probe (idleFor reports active),
	// which turns idle-reclaim off for this device. Set via SetActivityDir
	// before Start; read without the mutex, so don't mutate it afterwards.
	activityDir string
	// idleThreshold is how long without local Codex activity before this device
	// stops competing for a slot. Kept equal to the vault's reclaim threshold so
	// both sides agree on "idle".
	idleThreshold time.Duration
	// activityProbe, when non-nil, replaces the filesystem probe — a test seam.
	activityProbe func() time.Duration

	mu               sync.Mutex
	currentAccountID int64
	currentLeaseID   string
	restoreOnQuit    bool
	autoSwitchSource func(context.Context) (vault.AutoSwitch, error)
	stop             chan struct{}
	done             chan struct{}
}

func NewRemoteManager(svc vault.Service, storage CredentialStorage, deviceID string, logger *log.Logger) *RemoteManager {
	if logger == nil {
		logger = log.Default()
	}
	return &RemoteManager{
		svc: svc, storage: storage, deviceID: deviceID, logger: logger,
		idleThreshold: vault.DefaultIdleReclaimThreshold,
		restoreOnQuit: true, stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// SetActivityDir wires <CODEX_HOME>/sessions so idleFor() can measure local
// Codex activity, enabling idle-lease-reclaim for this device. Call before
// Start; leaving it unset (the default, and what most tests do) disables the
// probe, so idleFor always reports active and this device never yields its
// Codex slot. Safe on a nil receiver.
func (m *RemoteManager) SetActivityDir(dir string) {
	if m == nil {
		return
	}
	m.activityDir = dir
}

func (m *RemoteManager) Start(ctx context.Context) {
	go func() {
		defer close(m.done)
		m.reconcileLogged(ctx)
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = m.shutdown()
				return
			case <-m.stop:
				_ = m.shutdown()
				return
			case <-ticker.C:
				m.reconcileLogged(ctx)
			}
		}
	}()
}

func (m *RemoteManager) SetRestoreOnQuit(value bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreOnQuit = value
}

func (m *RemoteManager) SetAutoSwitchSource(source func(context.Context) (vault.AutoSwitch, error)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.autoSwitchSource = source
}

func (m *RemoteManager) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

func (m *RemoteManager) reconcileLogged(ctx context.Context) {
	if err := m.Reconcile(ctx); err != nil {
		m.logger.Printf("[codex-agent] reconcile: %v", err)
	}
}

func (m *RemoteManager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.reverseSync(ctx); err != nil {
		m.logger.Printf("[codex-agent] reverse sync skipped: %v", err)
	}

	// Idle-and-not-holding: there's nothing to renew and we won't take a slot
	// we aren't using, so skip the pick entirely. Without this gate a machine
	// that never runs codex still grabs a pool account at startup and squats it
	// until the process exits. A device that IS holding a lease falls through —
	// it must keep renewing so the vault sees a fresh (and now large) idleFor
	// and can reclaim the slot under pressure. When activity resumes, a later
	// tick acquires again.
	idleFor := m.idleFor()
	if idleFor >= m.idleThreshold && m.currentLeaseID == "" {
		return nil
	}

	accounts, err := m.svc.ListAccounts(ctx)
	if err != nil {
		return err
	}
	auto := vault.AutoSwitch{Enabled: true, Policy: "lru"}
	if m.autoSwitchSource != nil {
		if value, autoErr := m.autoSwitchSource(ctx); autoErr == nil {
			auto = value
		}
	} else if value, autoErr := m.svc.GetAutoSwitch(ctx); autoErr == nil {
		auto = value
	}
	selected, err := chooseStickyCodex(accounts, m.currentAccountID, m.deviceID, auto.Enabled, time.Now())
	if err == nil && selected == nil {
		selected, err = m.svc.PickProviderForDevice(ctx, time.Now(), m.deviceID, store.ProviderCodex)
	}
	if err != nil {
		if errors.Is(err, selector.ErrNoAvailable) {
			return m.restoreLocked(ctx)
		}
		return err
	}
	if selected.CredentialJSON == "" {
		return fmt.Errorf("Codex account %d has no provider credential", selected.ID)
	}
	want, err := ParseAuthFile([]byte(selected.CredentialJSON))
	if err != nil {
		return err
	}
	lease, err := m.ensureLease(ctx, selected.ID, idleFor)
	if err != nil {
		// Idle and parked: we let a reclaimed slot go rather than re-grabbing an
		// account we aren't using. Nothing to report — it's the steady state of
		// an idle device waiting for the user to come back.
		if errors.Is(err, errIdleParked) {
			return nil
		}
		// The vault refuses a lease when this device's provider allowlist
		// no longer grants Codex (e.g. the admin revoked it mid-session).
		// Treat it the same as an empty pool: drop the injected creds.
		if errors.Is(err, selector.ErrNoAvailable) {
			return m.restoreLocked(ctx)
		}
		return err
	}
	current, existed, err := m.storage.Load()
	if err != nil {
		return err
	}
	if have, parseErr := ParseAuthFile(current); parseErr == nil &&
		have.Tokens.AccountID == want.Tokens.AccountID &&
		have.Tokens.AccessToken == want.Tokens.AccessToken {
		m.currentAccountID = selected.ID
		m.currentLeaseID = lease.ID
		return nil
	}
	if err := ensureCredentialBackup(m.storage.BackupPath(), current, existed); err != nil {
		return err
	}
	raw, err := want.Marshal()
	if err != nil {
		return err
	}
	if err := m.storage.Save(raw); err != nil {
		return err
	}
	if err := m.svc.MarkUsed(ctx, selected.ID); err != nil {
		return err
	}
	m.currentAccountID = selected.ID
	m.currentLeaseID = lease.ID
	return nil
}

func (m *RemoteManager) ensureLease(ctx context.Context, accountID int64, idleFor time.Duration) (vault.Lease, error) {
	active := idleFor < m.idleThreshold
	if m.currentLeaseID != "" && m.currentAccountID == accountID {
		// Renew the held lease, reporting idleFor so the vault can tell a
		// live-but-idle lease apart from one in active use. An idle holder still
		// keeps its slot (zero churn) until the vault actually reclaims it.
		lease, err := m.svc.RenewLease(ctx, m.currentLeaseID, remoteLeaseTTL, idleFor)
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, vault.ErrLeaseNotFound) {
			return vault.Lease{}, err
		}
		// The lease is gone: expired (vault GC / restart) or reclaimed under
		// pool pressure because we reported ourselves idle.
		if !active {
			// Idle → park: drop the local lease and DON'T re-grab an account we
			// aren't using. A later tick re-acquires once codex runs again.
			m.currentLeaseID = ""
			return vault.Lease{}, errIdleParked
		}
		// Active → fall through and re-acquire our slot below.
	}
	oldLease := m.currentLeaseID
	lease, err := m.svc.AcquireLease(ctx, accountID, m.deviceID, remoteLeaseTTL)
	if err != nil {
		if errors.Is(err, vault.ErrLeaseLocked) {
			// Another device holds this account now — we lost it (reclaimed
			// while we were idle, or a plain acquire race). Drop stickiness so
			// the next tick re-picks a free account via PickProviderForDevice
			// instead of looping forever on this now-foreign account.
			m.currentLeaseID = ""
			m.currentAccountID = 0
		}
		return vault.Lease{}, err
	}
	if oldLease != "" && oldLease != lease.ID {
		if err := m.svc.ReleaseLease(ctx, oldLease); err != nil {
			m.logger.Printf("[codex-agent] release previous lease %s: %v", oldLease, err)
		}
	}
	return lease, nil
}

func (m *RemoteManager) reverseSync(ctx context.Context) error {
	raw, found, err := m.storage.Load()
	if err != nil || !found {
		return err
	}
	auth, err := ParseAuthFile(raw)
	if err != nil {
		return err
	}
	accounts, err := m.svc.ListAccounts(ctx)
	if err != nil {
		return err
	}
	for i := range accounts {
		a := accounts[i]
		if !auth.Matches(&a) {
			continue
		}
		normalized, err := auth.Marshal()
		if err != nil {
			return err
		}
		if string(normalized) == a.CredentialJSON {
			return nil
		}
		if !reverseSyncAllowed(auth, &a, time.Now()) {
			m.logger.Printf("[codex-agent] account %d (%s): refusing reverse sync — local credential is stale (expiry %s); the vault copy wins",
				a.ID, a.Name,
				time.UnixMilli(tokenExpiryMillis(auth.Tokens.AccessToken, auth.Tokens.IDToken)).UTC().Format(time.RFC3339))
			return nil
		}
		return m.svc.UpdateProviderCredential(ctx, a.ID,
			auth.Tokens.AccessToken, auth.Tokens.RefreshToken,
			tokenExpiryMillis(auth.Tokens.AccessToken, auth.Tokens.IDToken), string(normalized))
	}
	return nil
}

func (m *RemoteManager) ManagedAccountID() int64 {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.currentAccountID
}

func (m *RemoteManager) Restore() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restoreLocked(context.Background())
}

func (m *RemoteManager) shutdown() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.restoreOnQuit {
		return m.restoreLocked(context.Background())
	}
	return m.releaseLeaseLocked(context.Background())
}

func (m *RemoteManager) restoreLocked(ctx context.Context) error {
	if err := restoreCredentialBackup(m.storage); err != nil {
		return err
	}
	return m.releaseLeaseLocked(ctx)
}

func (m *RemoteManager) releaseLeaseLocked(ctx context.Context) error {
	if m.currentLeaseID != "" {
		if err := m.svc.ReleaseLease(ctx, m.currentLeaseID); err != nil {
			return err
		}
	}
	m.currentAccountID = 0
	m.currentLeaseID = ""
	return nil
}

func ensureCredentialBackup(path string, current []byte, existed bool) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	raw, err := json.Marshal(backupFile{Existed: existed, Data: current})
	if err != nil {
		return err
	}
	return atomicWrite(path, raw, 0o600)
}

func restoreCredentialBackup(storage CredentialStorage) error {
	raw, err := os.ReadFile(storage.BackupPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var backup backupFile
	if err := json.Unmarshal(raw, &backup); err != nil {
		return err
	}
	if backup.Existed {
		if err := storage.Save(backup.Data); err != nil {
			return err
		}
	} else if err := storage.Delete(); err != nil {
		return err
	}
	return os.Remove(storage.BackupPath())
}
