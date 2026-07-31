package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// authSchema holds the four tables vault Steps 3+4 introduce. The admin
// password is a single bcrypt blob stored in the existing kv table under
// `auth.password_hash`, since "set the password" is a one-row operation
// and doesn't justify its own table.
//
//   - devices       — paired agents' opaque tokens.
//   - pairings      — in-flight device-flow handshake rows.
//   - web_sessions  — HTTP cookie sessions for the admin Web UI.
//   - leases        — credinject's claim on accounts (one per account at
//     a time, enforced by leases_account_id_uniq below).
//   - lease_events   — append-only audit of who held what, when. A row is
//     opened on first acquire and closed (ended_at set) on
//     release/expiry. Unlike `leases` (current-state only),
//     this keeps history so per-device quota attribution can
//     replay which device held an account across a window.
const authSchema = `
CREATE TABLE IF NOT EXISTS devices (
  id            TEXT    PRIMARY KEY,
  name          TEXT    NOT NULL,
  token_hash    TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  last_seen_at  INTEGER NOT NULL DEFAULT 0,
  hostname      TEXT    NOT NULL DEFAULT '',
  os            TEXT    NOT NULL DEFAULT '',
  os_version    TEXT    NOT NULL DEFAULT '',
  arch          TEXT    NOT NULL DEFAULT '',
  model         TEXT    NOT NULL DEFAULT '',
  app_version   TEXT    NOT NULL DEFAULT '',
  client_type   TEXT    NOT NULL DEFAULT '',
  disabled_at   INTEGER NOT NULL DEFAULT 0,
  -- Per-device provider allowlist: which credential pools this device may
  -- lease/inject. Default claude-only (1/0/0) so a paired device never picks up
  -- Codex or OpenRouter unless the admin opts it in at approval or in the
  -- devices page.
  allow_claude     INTEGER NOT NULL DEFAULT 1,
  allow_codex      INTEGER NOT NULL DEFAULT 0,
  allow_openrouter INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS devices_token_hash ON devices (token_hash);

CREATE TABLE IF NOT EXISTS pairings (
  client_nonce  TEXT    PRIMARY KEY,
  user_code     TEXT    NOT NULL UNIQUE,
  device_name   TEXT    NOT NULL,
  status        TEXT    NOT NULL,
  device_id     TEXT    NOT NULL DEFAULT '',
  device_token  TEXT    NOT NULL DEFAULT '',
  expires_at    INTEGER NOT NULL,
  created_at    INTEGER NOT NULL,
  hostname      TEXT    NOT NULL DEFAULT '',
  os            TEXT    NOT NULL DEFAULT '',
  os_version    TEXT    NOT NULL DEFAULT '',
  arch          TEXT    NOT NULL DEFAULT '',
  model         TEXT    NOT NULL DEFAULT '',
  app_version   TEXT    NOT NULL DEFAULT '',
  client_type   TEXT    NOT NULL DEFAULT '',
  -- Provider allowlist chosen by the admin at approval; copied onto the
  -- device row when the pairing is promoted. Default claude-only (1/0/0).
  allow_claude     INTEGER NOT NULL DEFAULT 1,
  allow_codex      INTEGER NOT NULL DEFAULT 0,
  allow_openrouter INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS pairings_user_code ON pairings (user_code);
CREATE INDEX IF NOT EXISTS pairings_expires_at ON pairings (expires_at);

CREATE TABLE IF NOT EXISTS web_sessions (
  id            TEXT    PRIMARY KEY,
  expires_at    INTEGER NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS web_sessions_expires_at ON web_sessions (expires_at);

CREATE TABLE IF NOT EXISTS leases (
  id            TEXT    PRIMARY KEY,
  account_id    INTEGER NOT NULL,
  device_id     TEXT    NOT NULL,
  acquired_at   INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  -- last_active_at is the wall-clock (ms) of the holding agent's last REAL
  -- local Claude Code activity, as reported on each RenewLease (now - idleFor).
  -- Distinct from expires_at (which the 5s renew keeps fresh regardless of use):
  -- a lease can be live yet idle. Idle-beyond-threshold leases become
  -- reclaimable so an active device that finds the pool exhausted can preempt
  -- one. Defaults to 0 (legacy rows / treated as "very old" → reclaimable);
  -- AcquireLease stamps it to acquire-time since a fresh acquire is by
  -- definition active.
  last_active_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS leases_account_id_uniq ON leases (account_id);
CREATE INDEX IF NOT EXISTS leases_device_id ON leases (device_id);
CREATE INDEX IF NOT EXISTS leases_expires_at ON leases (expires_at);

CREATE TABLE IF NOT EXISTS lease_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  lease_id    TEXT    NOT NULL,
  account_id  INTEGER NOT NULL,
  device_id   TEXT    NOT NULL,
  started_at  INTEGER NOT NULL,
  ended_at    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS lease_events_account ON lease_events (account_id, started_at);
CREATE INDEX IF NOT EXISTS lease_events_open ON lease_events (lease_id, ended_at);
`

// authColumnMigrations adds device-meta columns to existing databases. The
// devices table started with five columns (id/name/token_hash/created_at/
// last_seen_at); these ALTERs bring older installs in line with the
// expanded authSchema above. Idempotent — duplicate-column errors are
// swallowed in store.Open, so running on a fresh DB is a no-op.
var authColumnMigrations = []string{
	`ALTER TABLE devices ADD COLUMN hostname TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE devices ADD COLUMN os TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE devices ADD COLUMN os_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE devices ADD COLUMN arch TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE devices ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE devices ADD COLUMN app_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE devices ADD COLUMN client_type TEXT NOT NULL DEFAULT ''`,
	// disabled_at != 0 marks a device as suspended: BearerAuth rejects its
	// token (temporary kick without deleting the row, so resume needs no
	// re-pair). 0 = active.
	`ALTER TABLE devices ADD COLUMN disabled_at INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE pairings ADD COLUMN hostname TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pairings ADD COLUMN os TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pairings ADD COLUMN os_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pairings ADD COLUMN arch TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pairings ADD COLUMN model TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pairings ADD COLUMN app_version TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE pairings ADD COLUMN client_type TEXT NOT NULL DEFAULT ''`,
	// Per-device provider allowlist. Existing devices/pairings migrate to
	// claude-only (1/0) — a device paired before this feature never starts
	// picking up Codex on its own; the admin opts in explicitly.
	`ALTER TABLE devices ADD COLUMN allow_claude INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE devices ADD COLUMN allow_codex INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE pairings ADD COLUMN allow_claude INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE pairings ADD COLUMN allow_codex INTEGER NOT NULL DEFAULT 0`,
	// OpenRouter joins the allowlist off-by-default for the same reason Codex
	// did: a device paired before the provider existed must not silently start
	// getting a derived third-party key minted for it.
	`ALTER TABLE devices ADD COLUMN allow_openrouter INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE pairings ADD COLUMN allow_openrouter INTEGER NOT NULL DEFAULT 0`,
	// Idle-reclaim bookkeeping: the holding agent reports its last real Claude
	// Code activity on each renew. Legacy rows migrate to 0 ("very old"); since
	// leases expire within a TTL (~60s) any pre-migration row is gone almost
	// immediately, so no backfill from acquired_at is needed.
	`ALTER TABLE leases ADD COLUMN last_active_at INTEGER NOT NULL DEFAULT 0`,
}

const passwordHashKey = "auth.password_hash"

// HasPassword reports whether an admin password has been set. The vault's
// /setup route uses this to gate first-run onboarding: when false, every
// other route redirects to /setup.
func (s *Store) HasPassword(ctx context.Context) (bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, passwordHashKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v != "", nil
}

// SetPasswordHash writes the password hash. Caller is responsible for
// computing it (bcrypt lives in vault/auth so the store layer doesn't pull
// in golang.org/x/crypto). The hash blob is stored verbatim.
func (s *Store) SetPasswordHash(ctx context.Context, hash string) error {
	if hash == "" {
		return fmt.Errorf("password hash is empty")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		passwordHashKey, hash, time.Now().UnixMilli())
	return err
}

// PasswordHash returns the stored bcrypt blob. Empty when unset.
func (s *Store) PasswordHash(ctx context.Context) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, passwordHashKey).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// Device is a paired agent. The token is never persisted in plaintext;
// only its sha256 lives in the row. The Hostname/OS/Model/etc. fields are
// captured at pair time from the agent's machine and copied here when the
// pairing row is promoted; they're empty for devices paired before the
// device-meta migration.
type Device struct {
	ID         string
	Name       string
	TokenHash  string
	CreatedAt  int64
	LastSeenAt int64
	Hostname   string
	OS         string
	OSVersion  string
	Arch       string
	Model      string
	AppVersion string
	ClientType string
	// DisabledAt is the suspend timestamp (UnixMilli). 0 = active; non-zero
	// means an admin suspended the device and BearerAuth 401s its token.
	DisabledAt int64
	// AllowClaude / AllowCodex / AllowOpenRouter are the per-device provider
	// allowlist: which credential pools this device may lease/inject. Chosen at
	// approval and editable in the devices page. Existing devices migrate to
	// claude-only. AllowOpenRouter gates key derivation rather than leasing —
	// see ProviderOpenRouter.
	AllowClaude     bool
	AllowCodex      bool
	AllowOpenRouter bool
}

// boolToInt maps a Go bool to the 0/1 SQLite INTEGER we store provider
// allowlist flags as. database/sql won't scan an INTEGER back into a *bool,
// so reads go through an intermediate int and compare != 0.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InsertDevice records a paired device. The caller has already produced
// the token and its hash in vault/auth — the store treats both as opaque.
func (s *Store) InsertDevice(ctx context.Context, d Device) error {
	if d.ID == "" || d.Name == "" || d.TokenHash == "" {
		return fmt.Errorf("device requires id, name, token_hash")
	}
	if d.CreatedAt == 0 {
		d.CreatedAt = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO devices
		   (id, name, token_hash, created_at, last_seen_at,
		    hostname, os, os_version, arch, model, app_version, client_type,
		    allow_claude, allow_codex, allow_openrouter)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.Name, d.TokenHash, d.CreatedAt, d.LastSeenAt,
		d.Hostname, d.OS, d.OSVersion, d.Arch, d.Model, d.AppVersion, d.ClientType,
		boolToInt(d.AllowClaude), boolToInt(d.AllowCodex), boolToInt(d.AllowOpenRouter))
	return err
}

// deviceColumns is the shared SELECT list for single-device lookups so a new
// column can't be added to one reader and forgotten in the other.
const deviceColumns = `id, name, token_hash, created_at, last_seen_at,
	        hostname, os, os_version, arch, model, app_version, client_type, disabled_at,
	        allow_claude, allow_codex, allow_openrouter`

// FindDeviceByTokenHash is the per-request auth lookup. Returns
// ErrNotFound when no match — the Bearer middleware translates that to 401.
func (s *Store) FindDeviceByTokenHash(ctx context.Context, hash string) (*Device, error) {
	return s.findDevice(ctx, `token_hash = ?`, hash)
}

// FindDevice looks a device up by its id. Used by admin handlers that need the
// row's current state before applying a partial update (e.g. a provider-toggle
// payload that omits a field must preserve it, not zero it).
func (s *Store) FindDevice(ctx context.Context, id string) (*Device, error) {
	if id == "" {
		return nil, fmt.Errorf("id required")
	}
	return s.findDevice(ctx, `id = ?`, id)
}

func (s *Store) findDevice(ctx context.Context, where string, arg any) (*Device, error) {
	var d Device
	var allowClaude, allowCodex, allowOpenRouter int
	err := s.db.QueryRowContext(ctx,
		`SELECT `+deviceColumns+` FROM devices WHERE `+where, arg).
		Scan(&d.ID, &d.Name, &d.TokenHash, &d.CreatedAt, &d.LastSeenAt,
			&d.Hostname, &d.OS, &d.OSVersion, &d.Arch, &d.Model, &d.AppVersion, &d.ClientType, &d.DisabledAt,
			&allowClaude, &allowCodex, &allowOpenRouter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.AllowClaude = allowClaude != 0
	d.AllowCodex = allowCodex != 0
	d.AllowOpenRouter = allowOpenRouter != 0
	return &d, nil
}

// TouchDevice bumps last_seen_at; called from the Bearer middleware so the
// Devices page can show a live "last active" column.
func (s *Store) TouchDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen_at = ? WHERE id = ?`,
		time.Now().UnixMilli(), id)
	return err
}

// UpdateDeviceName changes the display name an admin sees in the Devices
// page. Returns ErrNotFound when no row matches so the handler can 404.
func (s *Store) UpdateDeviceName(ctx context.Context, id, name string) error {
	if id == "" || name == "" {
		return fmt.Errorf("id and name required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceDisabled suspends (disabled=true) or resumes (disabled=false) a
// device. Suspending stamps disabled_at with now so BearerAuth rejects the
// token; resuming clears it back to 0 and the same token works again with
// no re-pair. Returns ErrNotFound when no row matches so the handler can
// 404. Idempotent-ish: re-suspending refreshes the timestamp, which is
// harmless (only zero/non-zero is load-bearing).
func (s *Store) SetDeviceDisabled(ctx context.Context, id string, disabled bool) error {
	if id == "" {
		return fmt.Errorf("id required")
	}
	var disabledAt int64
	if disabled {
		disabledAt = time.Now().UnixMilli()
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET disabled_at = ? WHERE id = ?`, disabledAt, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceProviders updates a device's provider allowlist (which credential
// pools it may lease/inject). Used by the devices admin page to change the
// choice made at approval. Returns ErrNotFound when no row matches.
func (s *Store) SetDeviceProviders(ctx context.Context, id string, allowClaude, allowCodex, allowOpenRouter bool) error {
	if id == "" {
		return fmt.Errorf("id required")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE devices SET allow_claude = ?, allow_codex = ?, allow_openrouter = ? WHERE id = ?`,
		boolToInt(allowClaude), boolToInt(allowCodex), boolToInt(allowOpenRouter), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeviceAllowsProvider reports whether the given device may use the given
// provider pool ("claude"/"codex"/"openrouter"). Enforced by the vault at lease
// time (Claude/Codex) or at key-derivation time (OpenRouter, which has no
// leases) so a paired device never picks up a provider the admin didn't grant
// it. Devices paired before this feature migrate to claude-only.
//
// A device id with no row is NOT a paired device — it's combined/local mode,
// which generates a local device id but never inserts a devices row and is
// deliberately un-gated (Fork D) — so a missing row returns true (allowed).
// An unknown provider string is denied.
func (s *Store) DeviceAllowsProvider(ctx context.Context, deviceID, provider string) (bool, error) {
	if deviceID == "" {
		return false, fmt.Errorf("deviceID required")
	}
	var col string
	switch provider {
	case ProviderClaude:
		col = "allow_claude"
	case ProviderCodex:
		col = "allow_codex"
	case ProviderOpenRouter:
		col = "allow_openrouter"
	default:
		return false, nil
	}
	var allowed int
	err := s.db.QueryRowContext(ctx,
		`SELECT `+col+` FROM devices WHERE id = ?`, deviceID).Scan(&allowed)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // not a paired device (combined/local) — un-gated
	}
	if err != nil {
		return false, err
	}
	return allowed != 0, nil
}

// ListDevices returns every paired device, newest first.
func (s *Store) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, token_hash, created_at, last_seen_at,
		        hostname, os, os_version, arch, model, app_version, client_type, disabled_at,
		        allow_claude, allow_codex, allow_openrouter
		   FROM devices ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var d Device
		var allowClaude, allowCodex, allowOpenRouter int
		if err := rows.Scan(&d.ID, &d.Name, &d.TokenHash, &d.CreatedAt, &d.LastSeenAt,
			&d.Hostname, &d.OS, &d.OSVersion, &d.Arch, &d.Model, &d.AppVersion, &d.ClientType, &d.DisabledAt,
			&allowClaude, &allowCodex, &allowOpenRouter); err != nil {
			return nil, err
		}
		d.AllowClaude = allowClaude != 0
		d.AllowCodex = allowCodex != 0
		d.AllowOpenRouter = allowOpenRouter != 0
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeleteDevice revokes a device. Subsequent requests with that token 401.
func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id = ?`, id)
	return err
}

// PairingStatus is the pairing-row state machine. Values are wire-format
// strings so callers (and the agent's poll loop) can compare directly.
type PairingStatus = string

const (
	PairingPending  PairingStatus = "pending"
	PairingApproved PairingStatus = "approved"
	PairingDenied   PairingStatus = "denied"
)

// Pairing is a row in the pairings table — used by the device-flow handshake.
// The Hostname/OS/Model/etc. fields are sent up by the agent in pair-init
// and copied to devices when ApprovePairing succeeds.
type Pairing struct {
	ClientNonce string
	UserCode    string
	DeviceName  string
	Status      PairingStatus
	DeviceID    string
	DeviceToken string
	ExpiresAt   int64
	CreatedAt   int64
	Hostname    string
	OS          string
	OSVersion   string
	Arch        string
	Model       string
	AppVersion  string
	ClientType  string
	// AllowClaude / AllowCodex / AllowOpenRouter is the provider allowlist the
	// admin chose at approval; copied onto the device row when the pairing is
	// promoted. Defaults to claude-only until an approve call sets them.
	AllowClaude     bool
	AllowCodex      bool
	AllowOpenRouter bool
}

// InsertPairing records a pair-init request. The caller has already
// generated the user_code in vault/auth — store treats it as opaque.
func (s *Store) InsertPairing(ctx context.Context, p Pairing) error {
	if p.ClientNonce == "" || p.UserCode == "" || p.DeviceName == "" {
		return fmt.Errorf("pairing requires client_nonce, user_code, device_name")
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().UnixMilli()
	}
	if p.Status == "" {
		p.Status = PairingPending
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pairings
		 (client_nonce, user_code, device_name, status, device_id, device_token, expires_at, created_at,
		  hostname, os, os_version, arch, model, app_version, client_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ClientNonce, p.UserCode, p.DeviceName, p.Status,
		p.DeviceID, p.DeviceToken, p.ExpiresAt, p.CreatedAt,
		p.Hostname, p.OS, p.OSVersion, p.Arch, p.Model, p.AppVersion, p.ClientType)
	return err
}

// FindPairingByNonce returns the row for an agent's pair-poll. Filters out
// expired rows so the agent sees them as "expired" rather than stuck pending.
func (s *Store) FindPairingByNonce(ctx context.Context, nonce string) (*Pairing, error) {
	return s.findPairing(ctx, `client_nonce = ?`, nonce)
}

// FindPairingByCode powers the Web UI's /pair?code=… landing — finds the
// pending row the user is approving.
func (s *Store) FindPairingByCode(ctx context.Context, code string) (*Pairing, error) {
	return s.findPairing(ctx, `user_code = ?`, code)
}

func (s *Store) findPairing(ctx context.Context, where, val string) (*Pairing, error) {
	var p Pairing
	var allowClaude, allowCodex, allowOpenRouter int
	err := s.db.QueryRowContext(ctx,
		`SELECT client_nonce, user_code, device_name, status, device_id, device_token, expires_at, created_at,
		        hostname, os, os_version, arch, model, app_version, client_type,
		        allow_claude, allow_codex, allow_openrouter
		   FROM pairings WHERE `+where+` AND expires_at > ?`,
		val, time.Now().UnixMilli()).
		Scan(&p.ClientNonce, &p.UserCode, &p.DeviceName, &p.Status,
			&p.DeviceID, &p.DeviceToken, &p.ExpiresAt, &p.CreatedAt,
			&p.Hostname, &p.OS, &p.OSVersion, &p.Arch, &p.Model, &p.AppVersion, &p.ClientType,
			&allowClaude, &allowCodex, &allowOpenRouter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.AllowClaude = allowClaude != 0
	p.AllowCodex = allowCodex != 0
	p.AllowOpenRouter = allowOpenRouter != 0
	return &p, nil
}

// ApprovePairing flips pending → approved and stamps the device token /
// device id on the row, plus the admin's provider allowlist (copied onto the
// device when the pairing is promoted). The agent's next pair-poll picks them
// up. Returns ErrNotFound when the pairing row has expired or doesn't exist.
func (s *Store) ApprovePairing(ctx context.Context, code, deviceID, deviceToken string, allowClaude, allowCodex, allowOpenRouter bool) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pairings
		    SET status = ?, device_id = ?, device_token = ?,
		        allow_claude = ?, allow_codex = ?, allow_openrouter = ?
		  WHERE user_code = ? AND status = ? AND expires_at > ?`,
		PairingApproved, deviceID, deviceToken,
		boolToInt(allowClaude), boolToInt(allowCodex), boolToInt(allowOpenRouter),
		code, PairingPending, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DenyPairing flips pending → denied without producing a device token.
func (s *Store) DenyPairing(ctx context.Context, code string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pairings SET status = ?
		   WHERE user_code = ? AND status = ? AND expires_at > ?`,
		PairingDenied, code, PairingPending, time.Now().UnixMilli())
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SweepPairings removes expired rows. Run on a goroutine timer or before
// each pair-init to keep the table from growing.
func (s *Store) SweepPairings(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pairings WHERE expires_at <= ?`, time.Now().UnixMilli())
	return err
}

// WebSession is a Web UI cookie session.
type WebSession struct {
	ID        string
	ExpiresAt int64
	CreatedAt int64
}

// CreateWebSession persists a session row. ID is random hex generated by
// the caller; expiresAt is unix millis.
func (s *Store) CreateWebSession(ctx context.Context, id string, expiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO web_sessions (id, expires_at, created_at) VALUES (?, ?, ?)`,
		id, expiresAt, time.Now().UnixMilli())
	return err
}

// LookupWebSession returns the session row when it exists and hasn't
// expired. Returns ErrNotFound otherwise so the middleware can 302 to
// /login.
func (s *Store) LookupWebSession(ctx context.Context, id string) (*WebSession, error) {
	var w WebSession
	err := s.db.QueryRowContext(ctx,
		`SELECT id, expires_at, created_at FROM web_sessions
		  WHERE id = ? AND expires_at > ?`,
		id, time.Now().UnixMilli()).
		Scan(&w.ID, &w.ExpiresAt, &w.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// DeleteWebSession invalidates the cookie — used by /logout and the
// session sweeper.
func (s *Store) DeleteWebSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM web_sessions WHERE id = ?`, id)
	return err
}

// SweepWebSessions drops expired rows.
func (s *Store) SweepWebSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM web_sessions WHERE expires_at <= ?`, time.Now().UnixMilli())
	return err
}
