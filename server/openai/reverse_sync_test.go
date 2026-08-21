package openai

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// A device that was offline while the pool kept rotating holds a credential
// whose refresh_token has already been spent. Reverse-syncing it up replaces a
// live refresh_token with a dead one, so the pool's next refresh gets HTTP 401
// and the account is marked needs_reauth — and because the stale file is still
// on disk, it bricks the user's re-login too. Both managers must therefore
// refuse to push a credential that is older than the pool's copy.
func TestRemoteManagerRefusesStaleReverseSync(t *testing.T) {
	dir := t.TempDir()
	storage := &fileCredentialStorage{authPath: filepath.Join(dir, "auth.json")}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The pool holds a freshly re-authenticated credential.
	freshAuth, _ := ParseAuthFile(authJSON(t, "managed", "-fresh"))
	fresh, _ := freshAuth.Account()
	if err := st.Upsert(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	// The device still holds the copy it was injected with a week ago.
	stale := authJSONExpiring(t, "managed", "-stale", time.Now().Add(-174*time.Hour))
	if err := storage.Save(stale); err != nil {
		t.Fatal(err)
	}

	m := NewRemoteManager(vault.NewInProc(st), storage, "device-codex", nil)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	stored, err := st.Get(context.Background(), fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != freshAuth.Tokens.AccessToken {
		t.Fatalf("stale device credential clobbered the pool's fresh one: stored access token = %q",
			stored.AccessToken)
	}
	// The healthy direction must still run: the device gets the fresh copy.
	injected, _, _ := storage.Load()
	got, err := ParseAuthFile(injected)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tokens.AccessToken != freshAuth.Tokens.AccessToken {
		t.Fatal("device was not healed with the pool's fresh credential")
	}
}

func TestManagerRefusesStaleReverseSync(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	freshAuth, _ := ParseAuthFile(authJSON(t, "managed", "-fresh"))
	fresh, _ := freshAuth.Account()
	if err := st.Upsert(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	stale := authJSONExpiring(t, "managed", "-stale", time.Now().Add(-174*time.Hour))
	storage := &fileCredentialStorage{authPath: authPath}
	if err := storage.Save(stale); err != nil {
		t.Fatal(err)
	}

	m := NewManager(st, authPath, nil)
	if err := m.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	stored, err := st.Get(context.Background(), fresh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.AccessToken != freshAuth.Tokens.AccessToken {
		t.Fatalf("stale device credential clobbered the pool's fresh one: stored access token = %q",
			stored.AccessToken)
	}
}
