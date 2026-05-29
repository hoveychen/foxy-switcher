package credinject

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// config.go holds the cross-platform `~/.claude.json` operations. Claude Code
// reads the same config file on every OS ($HOME/.claude.json, %USERPROFILE%\
// .claude.json on Windows — os.UserHomeDir resolves both), so the read/write
// helpers and the account-profile sync live here without a build tag rather
// than in the platform-split darwin.go / other.go. The Linux/Windows
// fileBackend reuses readConfig/writeConfig for its primaryApiKey handling;
// macOS picks them up for the account-profile sync even though its token blob
// lives in the keychain.

// onboardingKey is the ~/.claude.json field Claude Code's CLI checks to decide
// whether to show the "Select login method" onboarding screen. A terminal-
// launched `claude` with this unset stops at onboarding even when a valid
// token is present; foxy sets it true on every inject so a switched account
// drops the user straight into a session. (The VS Code extension uses its own
// onboarding state and ignores this field.)
const onboardingKey = "hasCompletedOnboarding"

// oauthAccountKey is the ~/.claude.json field carrying the signed-in account's
// profile (email, org, uuid). Claude Code surfaces it in /status and the UI;
// keeping it in sync with the injected token avoids the "token is account A
// but the UI says account B" mismatch.
const oauthAccountKey = "oauthAccount"

// DefaultClaudeConfigPath returns the absolute path to ~/.claude.json for the
// current user ($HOME/.claude.json; %USERPROFILE%\.claude.json on Windows, via
// os.UserHomeDir). main.go / agent.go pass the result to
// Coordinator.SetClaudeConfigPath; on resolve failure they leave the path
// unset, which disables profile sync rather than guessing a wrong location.
func DefaultClaudeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("UserHomeDir: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// readConfig loads ~/.claude.json into a generic map. A missing file returns
// (nil, nil) — callers treat that as "no config yet" and create one. Parse
// errors are surfaced so we never silently clobber a file we couldn't read.
func readConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// writeConfig serialises cfg back to ~/.claude.json via tmp + rename (atomic,
// mode 0600). Mirrors Claude Code's own 2-space-indent layout; the file is
// read by humans during debugging so we keep it pretty-printed.
func writeConfig(path string, cfg map[string]any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// buildOAuthAccount maps a store.Account to the `oauthAccount` object Claude
// Code expects in ~/.claude.json. Only non-empty fields are written: legacy
// store rows that predate the profile columns (Email / AccountUUID / … still
// "") would otherwise stamp empty strings over whatever Claude Code had, and
// Claude Code tolerates missing keys (it renders "no badge") but not a blank
// email where it expects an address. Field names match what `claude /login`
// writes — verified against a real ~/.claude.json (accountUuid, emailAddress,
// displayName, organizationName, organizationUuid, organizationRateLimitTier).
func buildOAuthAccount(a *store.Account) map[string]any {
	acct := map[string]any{}
	setIf := func(key, val string) {
		if val != "" {
			acct[key] = val
		}
	}
	setIf("accountUuid", a.AccountUUID)
	setIf("emailAddress", a.Email)
	setIf("displayName", a.FullName)
	setIf("organizationName", a.OrganizationName)
	setIf("organizationUuid", a.OrganizationUUID)
	setIf("organizationRateLimitTier", a.RateLimitTier)
	return acct
}

// applyAccountProfile writes the switched account's oauthAccount block and
// hasCompletedOnboarding=true into ~/.claude.json, creating the file (and the
// ~/.claude dir) if it doesn't exist and preserving every other field. Called
// from reconcile right after the token blob is written so the CLI sees a
// coherent (token, profile, onboarding) triple. configPath == "" is a no-op,
// which is how unit tests that don't exercise the profile path opt out.
func applyAccountProfile(configPath string, a *store.Account) error {
	if configPath == "" {
		return nil
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg[oauthAccountKey] = buildOAuthAccount(a)
	cfg[onboardingKey] = true
	return writeConfig(configPath, cfg)
}

// configProfileSnapshot records the pre-foxy state of the two ~/.claude.json
// fields applyAccountProfile overwrites. We track presence separately from
// value so restore is field-level symmetric: a key foxy added (absent at
// snapshot) is deleted on restore; a key foxy overwrote (present at snapshot)
// is set back to its original value. This is safer than deleting the whole
// file, which could discard unrelated state Claude Code wrote into the config
// between inject and shutdown.
type configProfileSnapshot struct {
	OAuthAccount    any  `json:"oauth_account,omitempty"`
	HadOAuthAccount bool `json:"had_oauth_account"`
	Onboarding      any  `json:"onboarding,omitempty"`
	HadOnboarding   bool `json:"had_onboarding"`
}

// snapshotConfigProfile reads the current oauthAccount + hasCompletedOnboarding
// out of ~/.claude.json so they can be restored later. A missing file yields a
// snapshot with both Had* flags false (restore will then delete whatever foxy
// added). configPath == "" returns (nil, nil) — the no-op path tests use.
func snapshotConfigProfile(configPath string) (*configProfileSnapshot, error) {
	if configPath == "" {
		return nil, nil
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		return nil, err
	}
	snap := &configProfileSnapshot{}
	if cfg != nil {
		if v, ok := cfg[oauthAccountKey]; ok {
			snap.OAuthAccount = v
			snap.HadOAuthAccount = true
		}
		if v, ok := cfg[onboardingKey]; ok {
			snap.Onboarding = v
			snap.HadOnboarding = true
		}
	}
	return snap, nil
}

// restoreConfigProfile puts the snapshotted oauthAccount + onboarding state
// back into ~/.claude.json, preserving every other field. Keys that were
// absent at snapshot time are deleted (foxy added them); keys that were
// present are set back to their original value. Nil snapshot or empty path is
// a no-op so legacy backups (no ConfigProfile) and tests both fall through
// cleanly.
func restoreConfigProfile(configPath string, snap *configProfileSnapshot) error {
	if configPath == "" || snap == nil {
		return nil
	}
	cfg, err := readConfig(configPath)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	restoreKey(cfg, oauthAccountKey, snap.HadOAuthAccount, snap.OAuthAccount)
	restoreKey(cfg, onboardingKey, snap.HadOnboarding, snap.Onboarding)
	return writeConfig(configPath, cfg)
}

// restoreKey sets cfg[key]=val when the key was present at snapshot time, or
// deletes it when it was absent (foxy-added).
func restoreKey(cfg map[string]any, key string, had bool, val any) {
	if had {
		cfg[key] = val
		return
	}
	delete(cfg, key)
}
