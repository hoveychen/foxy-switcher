package openai

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestAutoStorageDelete_NoSecretServiceIsNotFatal is the mcn001 regression.
//
// auto mode is "keyring if it works, file otherwise": Load and Save both
// shrug off a keyring error and use the file. Delete didn't — it returned
// the keyring's error even when the file copy was removed cleanly. On a
// headless Linux box there is no D-Bus session bus, so every Delete
// "failed", and restoreCredentialBackup only clears its sentinel after a
// successful Delete. The restore therefore never completed and the codex
// reconcile loop retried it every 5 seconds: 689 identical
// `dial unix /run/user/0/bus: connect: no such file or directory` lines in
// one hour.
func TestAutoStorageDelete_NoSecretServiceIsNotFatal(t *testing.T) {
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	kr := &memoryKeyring{fail: true, failErr: errNoSecretService()}
	storage := &autoCredentialStorage{
		keyring: &directKeyringStorage{codexHome: home, authPath: authPath, keyring: kr},
		file:    &fileCredentialStorage{authPath: authPath},
	}
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := storage.Delete(); err != nil {
		t.Fatalf("Delete on a box with no secret service: got %v, want nil", err)
	}
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("auth.json survived the delete: %v", err)
	}
}

// TestAutoStorageDelete_ReachableKeyringErrorStillFails is the other half:
// the exemption is scoped to "there is no secret service here", not to
// keyring errors generally. A keyring that answered and refused is a real
// failure — silently reporting success would leave a live credential in it.
func TestAutoStorageDelete_ReachableKeyringErrorStillFails(t *testing.T) {
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	kr := &memoryKeyring{fail: true} // plain error: answered, refused
	storage := &autoCredentialStorage{
		keyring: &directKeyringStorage{codexHome: home, authPath: authPath, keyring: kr},
		file:    &fileCredentialStorage{authPath: authPath},
	}
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := storage.Delete(); err == nil {
		t.Fatal("Delete swallowed an error from a reachable keyring; want it surfaced")
	}
}

// TestAutoStorageDelete_FileErrorWins guards the precedence: the file copy
// is the one auto mode actually falls back to, so its failure must surface
// even when the keyring was unreachable and thus exempt.
func TestAutoStorageDelete_FileErrorWins(t *testing.T) {
	home := t.TempDir()
	// A directory where auth.json should be: os.Remove fails with EISDIR/
	// ENOTEMPTY rather than the ErrNotExist that fileCredentialStorage
	// tolerates.
	authPath := filepath.Join(home, "auth.json")
	if err := os.Mkdir(authPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(authPath, "occupied"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	kr := &memoryKeyring{fail: true, failErr: errNoSecretService()}
	storage := &autoCredentialStorage{
		keyring: &directKeyringStorage{codexHome: home, authPath: authPath, keyring: kr},
		file:    &fileCredentialStorage{authPath: authPath},
	}

	if err := storage.Delete(); err == nil {
		t.Fatal("Delete reported success while the file copy could not be removed")
	}
}

// TestRestoreCredentialBackup_CompletesWithoutSecretService is the
// end-to-end shape of the mcn001 loop: the sentinel says "the user had no
// codex login before foxy touched this box" ({"existed":false}), so restore
// deletes the injected credential and then clears the sentinel. Before the
// fix the delete failed, os.Remove never ran, and the next 5s tick found the
// sentinel still there and did it all again — forever.
func TestRestoreCredentialBackup_CompletesWithoutSecretService(t *testing.T) {
	home := t.TempDir()
	authPath := filepath.Join(home, "auth.json")
	kr := &memoryKeyring{fail: true, failErr: errNoSecretService()}
	storage := &autoCredentialStorage{
		keyring: &directKeyringStorage{codexHome: home, authPath: authPath, keyring: kr},
		file:    &fileCredentialStorage{authPath: authPath},
	}
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureCredentialBackup(storage.BackupPath(), nil, false); err != nil {
		t.Fatalf("ensureCredentialBackup: %v", err)
	}

	if err := restoreCredentialBackup(storage); err != nil {
		t.Fatalf("restoreCredentialBackup: %v", err)
	}

	if _, err := os.Stat(storage.BackupPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup sentinel survived — the restore loop would retry forever: %v", err)
	}
	if _, err := os.Stat(authPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("injected auth.json survived the restore: %v", err)
	}
}

func TestKeyringUnavailable(t *testing.T) {
	if !keyringUnavailable(errNoSecretService()) {
		t.Error("a missing session bus should count as unavailable")
	}
	if keyringUnavailable(errors.New("boom")) {
		t.Error("an ordinary error should not count as unavailable")
	}
	if keyringUnavailable(nil) {
		t.Error("nil should not count as unavailable")
	}
}
