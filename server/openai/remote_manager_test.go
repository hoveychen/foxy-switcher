package openai

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

func TestRemoteManagerLeaseReverseSyncAndRestore(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	storage := &fileCredentialStorage{authPath: authPath}
	native := authJSON(t, "native", "")
	if err := storage.Save(native); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	managedAuth, _ := ParseAuthFile(authJSON(t, "remote-managed", ""))
	managed, _ := managedAuth.Account()
	if err := st.Upsert(context.Background(), managed); err != nil {
		t.Fatal(err)
	}
	svc := vault.NewInProc(st)
	m := NewRemoteManager(svc, storage, "device-codex", nil)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if m.ManagedAccountID() != managed.ID || !st.IsAccountLeased(managed.ID) {
		t.Fatalf("managed=%d leased=%v", m.ManagedAccountID(), st.IsAccountLeased(managed.ID))
	}
	injected, _, _ := storage.Load()
	parsed, _ := ParseAuthFile(injected)
	if parsed.Tokens.AccountID != "remote-managed" {
		t.Fatalf("injected account = %q", parsed.Tokens.AccountID)
	}

	rotated := authJSON(t, "remote-managed", "-remote-rotation")
	if err := storage.Save(rotated); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reverse-sync reconcile: %v", err)
	}
	stored, _ := st.Get(context.Background(), managed.ID)
	rotatedAuth, _ := ParseAuthFile(rotated)
	if stored.AccessToken != rotatedAuth.Tokens.AccessToken || stored.CredentialJSON == managed.CredentialJSON {
		t.Fatal("remote token rotation was not written back to vault")
	}

	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, _, _ := storage.Load()
	parsed, _ = ParseAuthFile(restored)
	if parsed.Tokens.AccountID != "native" {
		t.Fatalf("restored account = %q", parsed.Tokens.AccountID)
	}
	if st.IsAccountLeased(managed.ID) || m.ManagedAccountID() != 0 {
		t.Fatal("Codex lease was not released on restore")
	}
	if _, err := os.Stat(storage.BackupPath()); !os.IsNotExist(err) {
		t.Fatalf("backup still present: %v", err)
	}
}
