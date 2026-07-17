package openai

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

type memoryKeyring struct {
	mu     sync.Mutex
	values map[string]string
	fail   bool
}

func (m *memoryKeyring) key(service, user string) string { return service + "\x00" + user }
func (m *memoryKeyring) Get(service, user string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return "", errors.New("keyring unavailable")
	}
	value, ok := m.values[m.key(service, user)]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}
func (m *memoryKeyring) Set(service, user, password string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("keyring unavailable")
	}
	if m.values == nil {
		m.values = map[string]string{}
	}
	m.values[m.key(service, user)] = password
	return nil
}
func (m *memoryKeyring) Delete(service, user string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("keyring unavailable")
	}
	key := m.key(service, user)
	if _, ok := m.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(m.values, key)
	return nil
}

func TestDirectKeyringUsesCodexServiceAndHomeHash(t *testing.T) {
	home := t.TempDir()
	kr := &memoryKeyring{}
	storage := &directKeyringStorage{
		codexHome: home, authPath: filepath.Join(home, "auth.json"), keyring: kr,
	}
	if err := os.WriteFile(storage.authPath, []byte("fallback"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := storage.Save([]byte(`{"auth_mode":"chatgpt"}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(storage.authPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fallback auth.json was not removed: %v", err)
	}
	wantKey := kr.key(directKeyringService, "cli|"+homeHash(home))
	if got := kr.values[wantKey]; got != `{"auth_mode":"chatgpt"}` {
		t.Fatalf("official keyring entry = %q", got)
	}
	got, found, err := storage.Load()
	if err != nil || !found || string(got) != `{"auth_mode":"chatgpt"}` {
		t.Fatalf("Load = %q, %v, %v", got, found, err)
	}
	if err := storage.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, err := storage.Load(); err != nil {
		t.Fatalf("Load after delete: %v", err)
	}
}

func TestSecretsKeyringRoundTripPreservesOtherSecrets(t *testing.T) {
	home := t.TempDir()
	kr := &memoryKeyring{}
	storage := &secretsKeyringStorage{
		codexHome: home, authPath: filepath.Join(home, "auth.json"),
		secretPath: filepath.Join(home, "secrets", "codex_auth.age"), keyring: kr,
	}
	if err := storage.Save([]byte(`{"tokens":{"account_id":"one"}}`)); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	file, err := storage.loadSecrets(false)
	if err != nil {
		t.Fatal(err)
	}
	file.Secrets["global/FUTURE_SECRET"] = "keep-me"
	if err := storage.saveSecrets(file); err != nil {
		t.Fatal(err)
	}
	if err := storage.Save([]byte(`{"tokens":{"account_id":"two"}}`)); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	got, found, err := storage.Load()
	if err != nil || !found || string(got) != `{"tokens":{"account_id":"two"}}` {
		t.Fatalf("Load = %q, %v, %v", got, found, err)
	}
	file, err = storage.loadSecrets(false)
	if err != nil || file.Secrets["global/FUTURE_SECRET"] != "keep-me" {
		t.Fatalf("unrelated secret lost: %+v, %v", file, err)
	}
	if _, ok := kr.values[kr.key(secretsKeyringService, "secrets|"+homeHash(home))]; !ok {
		t.Fatal("secrets passphrase was not stored under Codex's official key")
	}
}

func TestAutoStoragePrefersKeyringAndFallsBackToFile(t *testing.T) {
	home := t.TempDir()
	kr := &memoryKeyring{}
	file := &fileCredentialStorage{authPath: filepath.Join(home, "auth.json")}
	direct := &directKeyringStorage{codexHome: home, authPath: file.authPath, keyring: kr}
	auto := &autoCredentialStorage{keyring: direct, file: file}
	if err := file.Save([]byte("file-value")); err != nil {
		t.Fatal(err)
	}
	got, found, err := auto.Load()
	if err != nil || !found || string(got) != "file-value" {
		t.Fatalf("file fallback = %q, %v, %v", got, found, err)
	}
	if err := direct.Save([]byte("keyring-value")); err != nil {
		t.Fatal(err)
	}
	got, found, err = auto.Load()
	if err != nil || !found || string(got) != "keyring-value" {
		t.Fatalf("keyring preference = %q, %v, %v", got, found, err)
	}
}

func TestReadCredentialMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("# comment\ncli_auth_credentials_store = 'keyring' # selected\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readCredentialMode(path); got != "keyring" {
		t.Fatalf("mode = %q", got)
	}
}
