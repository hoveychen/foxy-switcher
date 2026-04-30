package credinject

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// stateFileName is the on-disk record of which account is currently injected.
// We persist it so that a daemon restart resumes reverse-sync without having
// to reverse-engineer the keychain contents.
const stateFileName = "injected.json"

// backupFileName captures the user's pre-foxy native Claude Code login (one
// keychain blob plus the optional managed API key). Used to restore on
// graceful shutdown / when the pool empties.
const backupFileName = "native-cred-backup.json"

type stateFile struct {
	AccountID  int64  `json:"account_id"`
	AccessHash string `json:"access_hash"` // sha256 of the access token currently in keychain
	InjectedAt int64  `json:"injected_at"` // unix millis
}

type backupFile struct {
	OAuthBlob     []byte `json:"oauth_blob"`      // raw JSON blob; nil = no native login was present
	ManagedAPIKey string `json:"managed_api_key"` // empty = no managed key was present
	SnapshotAt    int64  `json:"snapshot_at"`
}

func readState(dataDir string) (stateFile, bool, error) {
	path := filepath.Join(dataDir, stateFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateFile{}, false, nil
		}
		return stateFile{}, false, err
	}
	var s stateFile
	if err := json.Unmarshal(b, &s); err != nil {
		return stateFile{}, false, err
	}
	return s, true, nil
}

func writeState(dataDir string, s stateFile) error {
	path := filepath.Join(dataDir, stateFileName)
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'), 0o600)
}

func clearState(dataDir string) error {
	path := filepath.Join(dataDir, stateFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func readBackup(dataDir string) (backupFile, bool, error) {
	path := filepath.Join(dataDir, backupFileName)
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return backupFile{}, false, nil
		}
		return backupFile{}, false, err
	}
	var bf backupFile
	if err := json.Unmarshal(b, &bf); err != nil {
		return backupFile{}, false, err
	}
	return bf, true, nil
}

func writeBackup(dataDir string, b backupFile) error {
	path := filepath.Join(dataDir, backupFileName)
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(raw, '\n'), 0o600)
}

// atomicWrite writes file contents via tmp + rename so a crash mid-write
// can't leave a half-baked state file (which would mis-cue reverse-sync on
// the next daemon start).
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
