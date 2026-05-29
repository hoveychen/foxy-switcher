//go:build !darwin

package credinject

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// On non-macOS platforms Claude Code falls back to plaintext storage:
//
//   - OAuth blob → ~/.claude/.credentials.json (mode 0600)
//   - Managed API key → primaryApiKey field inside ~/.claude.json
//
// (See claude-code-fork/src/utils/secureStorage/index.ts and config.ts:224.)
// We keep file permissions tight even though the daemon itself runs as the
// user; defense in depth.

func NewBackend() (Backend, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("UserHomeDir: %w", err)
	}
	return &fileBackend{
		credentialsPath: filepath.Join(home, ".claude", ".credentials.json"),
		configPath:      filepath.Join(home, ".claude.json"),
	}, nil
}

type fileBackend struct {
	credentialsPath string
	configPath      string
}

// CredentialsPath exposes the on-disk file the OAuth blob lives in. Satisfies
// the credinject.pathReporter probe so coordinator's marker bookkeeping can
// Stat the file right after a write.
func (b *fileBackend) CredentialsPath() string {
	return b.credentialsPath
}

func (b *fileBackend) ReadOAuthBlob() ([]byte, bool, error) {
	data, err := os.ReadFile(b.credentialsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (b *fileBackend) WriteOAuthBlob(blob []byte) error {
	if err := os.MkdirAll(filepath.Dir(b.credentialsPath), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(b.credentialsPath), err)
	}
	tmp := b.credentialsPath + ".tmp"
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	return os.Rename(tmp, b.credentialsPath)
}

func (b *fileBackend) DeleteOAuthBlob() error {
	if err := os.Remove(b.credentialsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (b *fileBackend) ReadManagedAPIKey() (string, bool, error) {
	cfg, err := readConfig(b.configPath)
	if err != nil {
		return "", false, err
	}
	if cfg == nil {
		return "", false, nil
	}
	v, ok := cfg["primaryApiKey"].(string)
	if !ok || v == "" {
		return "", false, nil
	}
	return v, true, nil
}

func (b *fileBackend) WriteManagedAPIKey(key string) error {
	cfg, err := readConfig(b.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["primaryApiKey"] = key
	return writeConfig(b.configPath, cfg)
}

func (b *fileBackend) DeleteManagedAPIKey() error {
	cfg, err := readConfig(b.configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	if _, ok := cfg["primaryApiKey"]; !ok {
		return nil
	}
	delete(cfg, "primaryApiKey")
	return writeConfig(b.configPath, cfg)
}
