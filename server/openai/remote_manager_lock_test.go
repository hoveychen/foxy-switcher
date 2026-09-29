package openai

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// blockingListService stalls ListAccounts until release is closed, standing
// in for a vault that has stopped answering mid-Reconcile.
type blockingListService struct {
	vault.Service
	entered chan struct{}
	release chan struct{}
}

func (s *blockingListService) ListAccounts(ctx context.Context) ([]store.Account, error) {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return s.Service.ListAccounts(ctx)
}

// ManagedAccountID backs the local /api/cred/status route, so it must answer
// while Reconcile is stuck on a slow vault call instead of queueing behind it.
func TestManagedAccountIDDoesNotWaitForReconcile(t *testing.T) {
	dir := t.TempDir()
	storage := &fileCredentialStorage{authPath: filepath.Join(dir, "auth.json")}
	if err := storage.Save(authJSON(t, "native", "")); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	svc := &blockingListService{
		Service: vault.NewInProc(st),
		entered: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	m := NewRemoteManager(svc, storage, "device-codex", nil)
	m.currentAccountID.Store(42)

	done := make(chan struct{})
	go func() {
		_ = m.Reconcile(context.Background())
		close(done)
	}()
	<-svc.entered

	got := make(chan int64, 1)
	go func() { got <- m.ManagedAccountID() }()
	select {
	case id := <-got:
		if id != 42 {
			t.Fatalf("ManagedAccountID = %d, want 42", id)
		}
	case <-time.After(time.Second):
		t.Fatal("ManagedAccountID blocked behind an in-flight Reconcile")
	}

	close(svc.release)
	<-done
}
