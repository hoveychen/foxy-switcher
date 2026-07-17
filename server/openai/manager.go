package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

const reconcileInterval = 5 * time.Second

type backupFile struct {
	Existed bool   `json:"existed"`
	Data    []byte `json:"data,omitempty"`
}

// Manager mirrors the selected Codex account into auth.json and restores the
// user's pre-Foxy credentials when the pool is unavailable or Foxy stops.
type Manager struct {
	st         *store.Store
	storage    CredentialStorage
	backupPath string
	logger     *log.Logger
	mu         sync.Mutex
	stop       chan struct{}
	done       chan struct{}
}

func NewManager(st *store.Store, authPath string, logger *log.Logger) *Manager {
	return NewManagerWithStorage(st, &fileCredentialStorage{authPath: authPath}, logger)
}

func NewManagerWithStorage(st *store.Store, storage CredentialStorage, logger *log.Logger) *Manager {
	if logger == nil {
		logger = log.Default()
	}
	return &Manager{
		st: st, storage: storage, backupPath: storage.BackupPath(),
		logger: logger, stop: make(chan struct{}), done: make(chan struct{}),
	}
}

func (m *Manager) Start(ctx context.Context) {
	go func() {
		defer close(m.done)
		m.reconcileLogged(ctx)
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = m.Restore()
				return
			case <-m.stop:
				_ = m.Restore()
				return
			case <-ticker.C:
				m.reconcileLogged(ctx)
			}
		}
	}()
}

func (m *Manager) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	<-m.done
}

func (m *Manager) reconcileLogged(ctx context.Context) {
	if err := m.Reconcile(ctx); err != nil {
		m.logger.Printf("[codex] reconcile: %v", err)
	}
}

func (m *Manager) Reconcile(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	accounts, err := m.st.ListProvider(ctx, store.ProviderCodex)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return m.restoreLocked()
	}
	if err := m.syncCurrent(ctx, accounts); err != nil {
		m.logger.Printf("[codex] reverse sync skipped: %v", err)
	}
	selected, err := selector.PickProvider(ctx, m.st, store.ProviderCodex, time.Now())
	if err != nil {
		if errors.Is(err, selector.ErrNoAvailable) {
			return m.restoreLocked()
		}
		return err
	}
	if selected.CredentialJSON == "" {
		return fmt.Errorf("Codex account %d has no auth.json credential", selected.ID)
	}
	want, err := ParseAuthFile([]byte(selected.CredentialJSON))
	if err != nil {
		return fmt.Errorf("Codex account %d credential: %w", selected.ID, err)
	}
	current, existed, err := m.storage.Load()
	if err != nil {
		return fmt.Errorf("load Codex credentials from %s: %w", m.storage.Kind(), err)
	}
	if have, parseErr := ParseAuthFile(current); parseErr == nil &&
		have.Tokens.AccountID == want.Tokens.AccountID &&
		have.Tokens.AccessToken == want.Tokens.AccessToken {
		return nil
	}
	if err := m.ensureBackup(current, existed); err != nil {
		return err
	}
	raw, err := want.Marshal()
	if err != nil {
		return err
	}
	if err := m.storage.Save(raw); err != nil {
		return err
	}
	return m.st.MarkUsed(ctx, selected.ID)
}

// ManagedAccountID reports which stored Codex account currently matches the
// live Codex credential storage. It returns zero for native/unknown logins.
func (m *Manager) ManagedAccountID(ctx context.Context) int64 {
	raw, found, err := m.storage.Load()
	if err != nil || !found {
		return 0
	}
	auth, err := ParseAuthFile(raw)
	if err != nil {
		return 0
	}
	accounts, err := m.st.ListProvider(ctx, store.ProviderCodex)
	if err != nil {
		return 0
	}
	for i := range accounts {
		if accounts[i].AccountUUID == auth.Tokens.AccountID {
			return accounts[i].ID
		}
	}
	return 0
}

func (m *Manager) syncCurrent(ctx context.Context, accounts []store.Account) error {
	raw, found, err := m.storage.Load()
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	auth, err := ParseAuthFile(raw)
	if err != nil {
		return err
	}
	for i := range accounts {
		a := accounts[i]
		if a.AccountUUID != auth.Tokens.AccountID {
			continue
		}
		normalized, err := auth.Marshal()
		if err != nil {
			return err
		}
		if string(normalized) == a.CredentialJSON {
			return nil
		}
		return m.st.UpdateProviderCredential(ctx, a.ID,
			auth.Tokens.AccessToken, auth.Tokens.RefreshToken,
			tokenExpiryMillis(auth.Tokens.AccessToken, auth.Tokens.IDToken), string(normalized))
	}
	return nil
}

func (m *Manager) ensureBackup(current []byte, existed bool) error {
	if _, err := os.Stat(m.backupPath); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	backup := backupFile{Existed: existed, Data: current}
	raw, err := json.Marshal(backup)
	if err != nil {
		return err
	}
	return atomicWrite(m.backupPath, raw, 0o600)
}

func (m *Manager) Restore() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.restoreLocked()
}

func (m *Manager) restoreLocked() error {
	raw, err := os.ReadFile(m.backupPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var backup backupFile
	if err := json.Unmarshal(raw, &backup); err != nil {
		return fmt.Errorf("parse Codex credential backup: %w", err)
	}
	if backup.Existed {
		if err := m.storage.Save(backup.Data); err != nil {
			return err
		}
	} else {
		if err := m.storage.Delete(); err != nil {
			return err
		}
	}
	return os.Remove(m.backupPath)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".foxy-auth-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
