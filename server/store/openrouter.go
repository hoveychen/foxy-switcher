package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// openrouter.go holds the two tables the OpenRouter provider needs on top of
// the shared `accounts` row. The split is deliberate and load-bearing:
//
//   - accounts (provider="openrouter") is the DERIVATION TEMPLATE. It says what
//     this OpenRouter account is allowed to mint — model allowlist, spend cap,
//     workspace — and carries no secret whatsoever. That matters because the
//     agent-facing GET /agent/v1/accounts serialises store.Account verbatim,
//     tokens included; anything we put on the row is visible to every paired
//     device.
//   - openrouter_management_keys holds the account's management key, the
//     credential that can mint and revoke runtime keys. It lives in its own
//     table precisely so it can never ride along on an Account serialisation.
//     Nothing outside the vault process ever reads it.
//   - device_openrouter_keys records which derived runtime key each authorised
//     device actually received. Revoking one device means deleting one row here
//     plus one DELETE /api/v1/keys/{hash} upstream — no other device is touched.
//
// There are no lease rows for OpenRouter. It bills per token and caps spend
// with guardrails, so parallel use of one account across devices is harmless
// and the LRU/lease machinery would only make devices fight each other. See
// ProviderOpenRouter.

const openrouterSchema = `
CREATE TABLE IF NOT EXISTS openrouter_management_keys (
  account_id     INTEGER PRIMARY KEY,
  management_key TEXT    NOT NULL,
  updated_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS device_openrouter_keys (
  device_id    TEXT    NOT NULL,
  account_id   INTEGER NOT NULL,
  -- key_hash is OpenRouter's own handle for the derived key. It is the ONLY
  -- way to revoke one (DELETE /api/v1/keys/{hash}), so losing it would leak a
  -- live key we can no longer kill.
  key_hash     TEXT    NOT NULL,
  -- key_secret is the sk-or-… runtime key. OpenRouter returns the plaintext
  -- exactly once, at creation, so the vault has to keep it: without it a daemon
  -- restart could only be served by rotating the device's key, which would
  -- break any in-flight codex session. This is the one OpenRouter secret that
  -- legitimately leaves the vault — but only ever to the one device it was
  -- minted for.
  key_secret   TEXT    NOT NULL DEFAULT '',
  -- guardrail_id is the guardrail carrying allowed_models + the spend cap.
  -- Empty means the derivation ran WITHOUT server-side model enforcement (the
  -- account's plan doesn't expose guardrails); the model allowlist then only
  -- constrains which profile files the device writes. See EnsureDeviceKey.
  guardrail_id TEXT    NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (device_id, account_id)
);
CREATE INDEX IF NOT EXISTS device_openrouter_keys_account
  ON device_openrouter_keys (account_id);
`

// OpenRouterAccountConfig is the JSON document stored in
// Account.CredentialJSON for provider="openrouter" rows. It is the single
// source of truth the admin edits once and that then drives BOTH sides of the
// contract: the guardrail's allowed_models (server-side enforcement) and which
// `or-<model>.config.toml` profile files the device writes (client-side
// visibility). Keeping it in one place is what prevents the "model shows up in
// Fleet's dropdown but OpenRouter rejects it" mismatch.
//
// It deliberately contains NO secret — see the openrouter_management_keys note
// above.
type OpenRouterAccountConfig struct {
	// AllowedModels are OpenRouter model slugs ("deepseek/deepseek-v4-flash").
	AllowedModels []string `json:"allowed_models"`
	// LimitUSD caps spend on each derived key. 0 = no cap.
	LimitUSD float64 `json:"limit_usd,omitempty"`
	// LimitReset is OpenRouter's reset_interval — "daily"/"weekly"/"monthly".
	// Empty means the limit never resets (a lifetime cap).
	LimitReset string `json:"limit_reset,omitempty"`
	// WorkspaceID scopes minted keys to an OpenRouter workspace. Empty = the
	// management key's default workspace.
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// ParseOpenRouterConfig decodes an OpenRouter account's credential_json.
// An empty document is a valid "configured nothing yet" state and yields a
// zero config rather than an error, so a freshly-added account renders in the
// UI before the admin has picked any models.
func ParseOpenRouterConfig(credentialJSON string) (OpenRouterAccountConfig, error) {
	var cfg OpenRouterAccountConfig
	if strings.TrimSpace(credentialJSON) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(credentialJSON), &cfg); err != nil {
		return cfg, fmt.Errorf("parse openrouter config: %w", err)
	}
	cfg.Normalise()
	return cfg, nil
}

// Normalise trims, de-dupes and sorts AllowedModels. Sorting makes the stored
// document stable so an unchanged allowlist doesn't look like an edit (which
// would otherwise re-derive every device's key), and de-duping keeps a
// double-added slug from producing two profile files with the same model.
func (c *OpenRouterAccountConfig) Normalise() {
	seen := make(map[string]bool, len(c.AllowedModels))
	out := make([]string, 0, len(c.AllowedModels))
	for _, m := range c.AllowedModels {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	if len(out) == 0 {
		out = nil
	}
	c.AllowedModels = out
}

// Marshal serialises the config for storage in credential_json. Normalise runs
// first so callers can't persist an unsorted / duplicated allowlist.
func (c OpenRouterAccountConfig) Marshal() (string, error) {
	c.Normalise()
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// SetOpenRouterConfig writes the derivation template onto an OpenRouter
// account row. Rejects non-OpenRouter accounts rather than silently stamping a
// Claude/Codex row's credential_json (which for Codex holds its whole
// auth.json — overwriting it would destroy a working credential).
func (s *Store) SetOpenRouterConfig(ctx context.Context, accountID int64, cfg OpenRouterAccountConfig) error {
	acc, err := s.Get(ctx, accountID)
	if err != nil {
		return err
	}
	if acc.Provider != ProviderOpenRouter {
		return fmt.Errorf("account %d is provider %q, not %q", accountID, acc.Provider, ProviderOpenRouter)
	}
	raw, err := cfg.Marshal()
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET credential_json = ?, updated_at = ? WHERE id = ?`,
		raw, time.Now().UnixMilli(), accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// OpenRouterConfig reads back the derivation template for one account.
func (s *Store) OpenRouterConfig(ctx context.Context, accountID int64) (OpenRouterAccountConfig, error) {
	acc, err := s.Get(ctx, accountID)
	if err != nil {
		return OpenRouterAccountConfig{}, err
	}
	return ParseOpenRouterConfig(acc.CredentialJSON)
}

// --- management key -------------------------------------------------------

// SetOpenRouterManagementKey stores (or replaces) the management key used to
// mint and revoke this account's derived runtime keys. Passing an empty key
// deletes the row — that is how an admin un-configures an account without
// deleting it.
func (s *Store) SetOpenRouterManagementKey(ctx context.Context, accountID int64, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return s.DeleteOpenRouterManagementKey(ctx, accountID)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO openrouter_management_keys (account_id, management_key, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(account_id) DO UPDATE
		   SET management_key = excluded.management_key, updated_at = excluded.updated_at`,
		accountID, key, time.Now().UnixMilli())
	return err
}

// OpenRouterManagementKey returns the account's management key. ErrNotFound
// when the admin hasn't entered one yet — callers surface that as "this
// account can't derive keys" rather than attempting an unauthenticated call.
func (s *Store) OpenRouterManagementKey(ctx context.Context, accountID int64) (string, error) {
	var key string
	err := s.db.QueryRowContext(ctx,
		`SELECT management_key FROM openrouter_management_keys WHERE account_id = ?`,
		accountID).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return key, err
}

// HasOpenRouterManagementKey reports whether a key is on file. The admin UI
// renders "configured / not configured" off this without ever fetching the
// secret itself.
func (s *Store) HasOpenRouterManagementKey(ctx context.Context, accountID int64) (bool, error) {
	_, err := s.OpenRouterManagementKey(ctx, accountID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteOpenRouterManagementKey drops the stored key. Idempotent.
func (s *Store) DeleteOpenRouterManagementKey(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM openrouter_management_keys WHERE account_id = ?`, accountID)
	return err
}

// --- per-device derived keys ---------------------------------------------

// DeviceOpenRouterKey is one device's derived runtime key for one OpenRouter
// account. KeyHash is the revocation handle; KeySecret is the bearer token the
// device's codex sessions actually use.
type DeviceOpenRouterKey struct {
	DeviceID    string
	AccountID   int64
	KeyHash     string
	KeySecret   string
	GuardrailID string
	CreatedAt   int64
	// ExpiresAt is the key's upstream expiry (unix millis). 0 = never expires.
	ExpiresAt int64
}

// Expired reports whether the derived key is past its upstream expiry.
// ExpiresAt==0 means "no expiry was requested", so never expired.
func (k DeviceOpenRouterKey) Expired(now time.Time) bool {
	if k.ExpiresAt <= 0 {
		return false
	}
	return k.ExpiresAt <= now.UnixMilli()
}

// PutDeviceOpenRouterKey records a freshly derived key, replacing any previous
// row for the same (device, account). The caller is responsible for revoking
// the old key upstream BEFORE overwriting — this function only touches the
// local mapping, so overwriting without revoking would orphan a live key.
func (s *Store) PutDeviceOpenRouterKey(ctx context.Context, k DeviceOpenRouterKey) error {
	if k.DeviceID == "" || k.AccountID == 0 || k.KeyHash == "" {
		return fmt.Errorf("device_id, account_id, key_hash required")
	}
	if k.CreatedAt == 0 {
		k.CreatedAt = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO device_openrouter_keys
		   (device_id, account_id, key_hash, key_secret, guardrail_id, created_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id, account_id) DO UPDATE SET
		   key_hash     = excluded.key_hash,
		   key_secret   = excluded.key_secret,
		   guardrail_id = excluded.guardrail_id,
		   created_at   = excluded.created_at,
		   expires_at   = excluded.expires_at`,
		k.DeviceID, k.AccountID, k.KeyHash, k.KeySecret, k.GuardrailID, k.CreatedAt, k.ExpiresAt)
	return err
}

// DeviceOpenRouterKeyFor returns the key minted for one (device, account)
// pair. ErrNotFound when this device has never been given a key for it.
func (s *Store) DeviceOpenRouterKeyFor(ctx context.Context, deviceID string, accountID int64) (*DeviceOpenRouterKey, error) {
	k := DeviceOpenRouterKey{DeviceID: deviceID, AccountID: accountID}
	err := s.db.QueryRowContext(ctx,
		`SELECT key_hash, key_secret, guardrail_id, created_at, expires_at
		   FROM device_openrouter_keys WHERE device_id = ? AND account_id = ?`,
		deviceID, accountID).
		Scan(&k.KeyHash, &k.KeySecret, &k.GuardrailID, &k.CreatedAt, &k.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// ListDeviceOpenRouterKeys returns every key minted for one device — the set a
// device-revoke has to walk and kill upstream.
func (s *Store) ListDeviceOpenRouterKeys(ctx context.Context, deviceID string) ([]DeviceOpenRouterKey, error) {
	return s.queryOpenRouterKeys(ctx, `device_id = ?`, deviceID)
}

// ListAccountOpenRouterKeys returns every key minted from one account — the
// set that has to be re-derived when the account's allowed_models change, and
// killed when the account is deleted.
func (s *Store) ListAccountOpenRouterKeys(ctx context.Context, accountID int64) ([]DeviceOpenRouterKey, error) {
	return s.queryOpenRouterKeys(ctx, `account_id = ?`, accountID)
}

func (s *Store) queryOpenRouterKeys(ctx context.Context, where string, arg any) ([]DeviceOpenRouterKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT device_id, account_id, key_hash, key_secret, guardrail_id, created_at, expires_at
		   FROM device_openrouter_keys WHERE `+where+` ORDER BY created_at DESC`, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceOpenRouterKey
	for rows.Next() {
		var k DeviceOpenRouterKey
		if err := rows.Scan(&k.DeviceID, &k.AccountID, &k.KeyHash, &k.KeySecret,
			&k.GuardrailID, &k.CreatedAt, &k.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteDeviceOpenRouterKey removes one mapping row. Idempotent. Upstream
// revocation is the caller's job — see openrouter.Revoker.
func (s *Store) DeleteDeviceOpenRouterKey(ctx context.Context, deviceID string, accountID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM device_openrouter_keys WHERE device_id = ? AND account_id = ?`,
		deviceID, accountID)
	return err
}

// DeleteDeviceOpenRouterKeys removes every mapping row for a device. Called
// after the upstream keys have been revoked, when a device is revoked or
// suspended.
func (s *Store) DeleteDeviceOpenRouterKeys(ctx context.Context, deviceID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM device_openrouter_keys WHERE device_id = ?`, deviceID)
	return err
}

// DeleteAccountOpenRouterKeys removes every mapping row for an account.
func (s *Store) DeleteAccountOpenRouterKeys(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM device_openrouter_keys WHERE account_id = ?`, accountID)
	return err
}
