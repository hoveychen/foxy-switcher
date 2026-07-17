package openai

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"filippo.io/age"
	keyring "github.com/zalando/go-keyring"
)

const (
	directKeyringService  = "Codex Auth"
	secretsKeyringService = "codex"
	codexAuthSecretKey    = "global/CODEX_AUTH"
)

// CredentialStorage is the provider-native location Codex reads and writes.
// bool reports whether credentials exist; a missing login is not an error.
type CredentialStorage interface {
	Load() ([]byte, bool, error)
	Save([]byte) error
	Delete() error
	BackupPath() string
	Kind() string
}

type osKeyring interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type systemKeyring struct{}

func (systemKeyring) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (systemKeyring) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}
func (systemKeyring) Delete(service, user string) error { return keyring.Delete(service, user) }

type fileCredentialStorage struct{ authPath string }

func (s *fileCredentialStorage) Load() ([]byte, bool, error) {
	raw, err := os.ReadFile(s.authPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return raw, err == nil, err
}
func (s *fileCredentialStorage) Save(raw []byte) error { return atomicWrite(s.authPath, raw, 0o600) }
func (s *fileCredentialStorage) Delete() error {
	if err := os.Remove(s.authPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func (s *fileCredentialStorage) BackupPath() string { return s.authPath + ".foxy-backup" }
func (s *fileCredentialStorage) Kind() string       { return "file" }

type directKeyringStorage struct {
	codexHome string
	authPath  string
	keyring   osKeyring
}

func (s *directKeyringStorage) account() string { return "cli|" + homeHash(s.codexHome) }
func (s *directKeyringStorage) Load() ([]byte, bool, error) {
	value, err := s.keyring.Get(directKeyringService, s.account())
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(value), true, nil
}
func (s *directKeyringStorage) Save(raw []byte) error {
	if err := s.keyring.Set(directKeyringService, s.account(), string(raw)); err != nil {
		return err
	}
	return removeIfExists(s.authPath)
}
func (s *directKeyringStorage) Delete() error {
	if err := s.keyring.Delete(directKeyringService, s.account()); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return removeIfExists(s.authPath)
}
func (s *directKeyringStorage) BackupPath() string { return s.authPath + ".foxy-backup" }
func (s *directKeyringStorage) Kind() string       { return "keyring-direct" }

type secretsKeyringStorage struct {
	codexHome  string
	authPath   string
	secretPath string
	keyring    osKeyring
}

type secretsFile struct {
	Version uint8             `json:"version"`
	Secrets map[string]string `json:"secrets"`
}

func (s *secretsKeyringStorage) account() string { return "secrets|" + homeHash(s.codexHome) }
func (s *secretsKeyringStorage) Load() ([]byte, bool, error) {
	file, err := s.loadSecrets(false)
	if err != nil {
		return nil, false, err
	}
	if file == nil {
		return nil, false, nil
	}
	value, ok := file.Secrets[codexAuthSecretKey]
	return []byte(value), ok, nil
}
func (s *secretsKeyringStorage) Save(raw []byte) error {
	file, err := s.loadSecrets(true)
	if err != nil {
		return err
	}
	if file == nil {
		file = &secretsFile{Version: 1, Secrets: map[string]string{}}
	}
	if file.Secrets == nil {
		file.Secrets = map[string]string{}
	}
	file.Version = 1
	file.Secrets[codexAuthSecretKey] = string(raw)
	if err := s.saveSecrets(file); err != nil {
		return err
	}
	return removeIfExists(s.authPath)
}
func (s *secretsKeyringStorage) Delete() error {
	file, err := s.loadSecrets(false)
	if err != nil {
		return err
	}
	if file != nil {
		delete(file.Secrets, codexAuthSecretKey)
		if err := s.saveSecrets(file); err != nil {
			return err
		}
	}
	return removeIfExists(s.authPath)
}
func (s *secretsKeyringStorage) BackupPath() string { return s.authPath + ".foxy-backup" }
func (s *secretsKeyringStorage) Kind() string       { return "keyring-secrets" }

func (s *secretsKeyringStorage) loadSecrets(createKey bool) (*secretsFile, error) {
	ciphertext, err := os.ReadFile(s.secretPath)
	if errors.Is(err, os.ErrNotExist) {
		if !createKey {
			return nil, nil
		}
		return &secretsFile{Version: 1, Secrets: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	passphrase, err := s.loadPassphrase(false)
	if err != nil {
		return nil, err
	}
	identity, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return nil, err
	}
	reader, err := age.Decrypt(bytes.NewReader(ciphertext), identity)
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	var file secretsFile
	if err := json.Unmarshal(plain, &file); err != nil {
		return nil, err
	}
	if file.Version > 1 {
		return nil, fmt.Errorf("Codex secrets version %d is newer than supported version 1", file.Version)
	}
	return &file, nil
}

func (s *secretsKeyringStorage) saveSecrets(file *secretsFile) error {
	passphrase, err := s.loadPassphrase(true)
	if err != nil {
		return err
	}
	recipient, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return err
	}
	plain, err := json.Marshal(file)
	if err != nil {
		return err
	}
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		return err
	}
	if _, err := writer.Write(plain); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return atomicWrite(s.secretPath, encrypted.Bytes(), 0o600)
}

func (s *secretsKeyringStorage) loadPassphrase(create bool) (string, error) {
	value, err := s.keyring.Get(secretsKeyringService, s.account())
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, keyring.ErrNotFound) || !create {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	value = base64.StdEncoding.EncodeToString(buf)
	if err := s.keyring.Set(secretsKeyringService, s.account(), value); err != nil {
		return "", err
	}
	return value, nil
}

type autoCredentialStorage struct {
	keyring CredentialStorage
	file    CredentialStorage
}

func (s *autoCredentialStorage) Load() ([]byte, bool, error) {
	raw, found, err := s.keyring.Load()
	if err == nil && found {
		return raw, true, nil
	}
	return s.file.Load()
}
func (s *autoCredentialStorage) Save(raw []byte) error {
	if err := s.keyring.Save(raw); err == nil {
		return nil
	}
	return s.file.Save(raw)
}
func (s *autoCredentialStorage) Delete() error {
	keyringErr := s.keyring.Delete()
	fileErr := s.file.Delete()
	if keyringErr != nil {
		return keyringErr
	}
	return fileErr
}
func (s *autoCredentialStorage) BackupPath() string { return s.file.BackupPath() }
func (s *autoCredentialStorage) Kind() string       { return "auto" }

func DefaultCredentialStorage() (CredentialStorage, error) {
	codexHome, err := defaultCodexHome()
	if err != nil {
		return nil, err
	}
	return credentialStorageForHome(codexHome, systemKeyring{}), nil
}

func credentialStorageForHome(codexHome string, kr osKeyring) CredentialStorage {
	authPath := filepath.Join(codexHome, "auth.json")
	file := &fileCredentialStorage{authPath: authPath}
	direct := CredentialStorage(&directKeyringStorage{codexHome: codexHome, authPath: authPath, keyring: kr})
	secrets := CredentialStorage(&secretsKeyringStorage{
		codexHome: codexHome, authPath: authPath,
		secretPath: filepath.Join(codexHome, "secrets", "codex_auth.age"), keyring: kr,
	})
	keyringStore := direct
	if runtime.GOOS == "windows" {
		keyringStore = secrets
	}
	switch readCredentialMode(filepath.Join(codexHome, "config.toml")) {
	case "file":
		return file
	case "keyring":
		return keyringStore
	default:
		return &autoCredentialStorage{keyring: keyringStore, file: file}
	}
}

func readCredentialMode(configPath string) string {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return "auto"
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == "cli_auth_credentials_store" {
			return strings.Trim(strings.TrimSpace(parts[1]), "\"'")
		}
	}
	return "auto"
}

func defaultCodexHome() (string, error) {
	if home := strings.TrimSpace(os.Getenv("CODEX_HOME")); home != "" {
		return filepath.Clean(home), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func homeHash(home string) string {
	canonical, err := filepath.EvalSymlinks(home)
	if err != nil {
		canonical = filepath.Clean(home)
	}
	sum := sha256.Sum256([]byte(canonical))
	return fmt.Sprintf("%x", sum)[:16]
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
