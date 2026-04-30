// Package store wraps a SQLite database holding the credential pool used by
// the foxy-switcher account selector. The schema is intentionally tiny: one
// row per Claude subscription account, plus a couple of denormalised counters
// the selector reads on every /api/token call.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Account is the in-memory representation of a row in the accounts table.
// Token fields are stored unencrypted; the SQLite file lives at 0600 inside
// ~/.foxy-switcher.
type Account struct {
	ID               int64
	Name             string
	AccessToken      string
	RefreshToken     string
	ExpiresAt        int64 // unix millis
	Scopes           string
	SubscriptionType string // "max" | "pro" | "team_premium" | "team" | "free"
	OrganizationUUID string
	Status           string // "active" | "disabled"
	CooldownUntil    int64  // unix millis; 0 = no cooldown
	LastUsedAt       int64  // unix millis
	Last429At        int64  // unix millis
	CreatedAt        int64
	UpdatedAt        int64

	// Profile fields populated once at login from /api/oauth/profile.
	Email            string
	FullName         string
	OrganizationName string
	Plan             string // "Claude Max" | "Claude Pro" | "Claude Team Premium" | "Claude Team" | "API / Free"

	// Usage fields refreshed periodically from /api/oauth/usage.
	// Utilization is 0–100 (percent). ResetsAt is RFC3339; empty when the
	// API didn't return that window.
	FiveHourUtil           float64
	FiveHourResetsAt       string
	SevenDayUtil           float64
	SevenDayResetsAt       string
	SevenDaySonnetUtil     float64
	SevenDaySonnetResetsAt string
	UsageFetchedAt         int64 // unix millis; 0 = never
}

// schema is the create-from-scratch DDL. For existing databases, migrate()
// adds any columns that were introduced after the initial release.
const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT,
  name                       TEXT    NOT NULL,
  access_token               TEXT    NOT NULL,
  refresh_token              TEXT    NOT NULL,
  expires_at                 INTEGER NOT NULL,
  scopes                     TEXT    NOT NULL DEFAULT '',
  subscription_type          TEXT    NOT NULL DEFAULT '',
  organization_uuid          TEXT    NOT NULL DEFAULT '',
  status                     TEXT    NOT NULL DEFAULT 'active',
  cooldown_until             INTEGER NOT NULL DEFAULT 0,
  last_used_at               INTEGER NOT NULL DEFAULT 0,
  last_429_at                INTEGER NOT NULL DEFAULT 0,
  created_at                 INTEGER NOT NULL,
  updated_at                 INTEGER NOT NULL,
  email                      TEXT    NOT NULL DEFAULT '',
  full_name                  TEXT    NOT NULL DEFAULT '',
  organization_name          TEXT    NOT NULL DEFAULT '',
  plan                       TEXT    NOT NULL DEFAULT '',
  five_hour_util             REAL    NOT NULL DEFAULT 0,
  five_hour_resets_at        TEXT    NOT NULL DEFAULT '',
  seven_day_util             REAL    NOT NULL DEFAULT 0,
  seven_day_resets_at        TEXT    NOT NULL DEFAULT '',
  seven_day_sonnet_util      REAL    NOT NULL DEFAULT 0,
  seven_day_sonnet_resets_at TEXT    NOT NULL DEFAULT '',
  usage_fetched_at           INTEGER NOT NULL DEFAULT 0,
  UNIQUE (organization_uuid)
);
CREATE INDEX IF NOT EXISTS accounts_status_cooldown
  ON accounts (status, cooldown_until, last_used_at);
`

// migrations adds columns to existing databases. Each entry is run in order
// and is idempotent (failures with "duplicate column" are silently ignored,
// so re-running on an already-migrated DB is fine).
var migrations = []string{
	`ALTER TABLE accounts ADD COLUMN email TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN full_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN organization_name TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN plan TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN five_hour_util REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN five_hour_resets_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN seven_day_util REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN seven_day_resets_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN seven_day_sonnet_util REAL NOT NULL DEFAULT 0`,
	`ALTER TABLE accounts ADD COLUMN seven_day_sonnet_resets_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE accounts ADD COLUMN usage_fetched_at INTEGER NOT NULL DEFAULT 0`,
}

type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite file at path. Sets WAL + 0600 perms.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			db.Close()
			return nil, fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	return &Store{db: db}, nil
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying *sql.DB for callers that need transactions
// (currently only the OAuth refresh path uses this, to take a row-level lock
// before issuing the network exchange).
func (s *Store) DB() *sql.DB { return s.db }

// Upsert inserts a new account or, when an account with the same
// organization_uuid already exists, replaces its tokens / metadata. The id of
// the resulting row is set on a.ID.
func (s *Store) Upsert(ctx context.Context, a *Account) error {
	now := time.Now().UnixMilli()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now

	const q = `
INSERT INTO accounts
  (name, access_token, refresh_token, expires_at, scopes, subscription_type,
   organization_uuid, status, cooldown_until, last_used_at,
   last_429_at, created_at, updated_at,
   email, full_name, organization_name, plan)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(organization_uuid) DO UPDATE SET
  name = excluded.name,
  access_token = excluded.access_token,
  refresh_token = excluded.refresh_token,
  expires_at = excluded.expires_at,
  scopes = excluded.scopes,
  subscription_type = excluded.subscription_type,
  email = excluded.email,
  full_name = excluded.full_name,
  organization_name = excluded.organization_name,
  plan = excluded.plan,
  status = 'active',
  updated_at = excluded.updated_at
RETURNING id`

	row := s.db.QueryRowContext(ctx, q,
		a.Name, a.AccessToken, a.RefreshToken, a.ExpiresAt,
		a.Scopes, a.SubscriptionType, a.OrganizationUUID,
		ifEmpty(a.Status, "active"), a.CooldownUntil, a.LastUsedAt,
		a.Last429At, a.CreatedAt, a.UpdatedAt,
		a.Email, a.FullName, a.OrganizationName, a.Plan,
	)
	return row.Scan(&a.ID)
}

// UpdateTokens rewrites the access/refresh/expires columns for one account,
// typically after a refresh exchange.
func (s *Store) UpdateTokens(ctx context.Context, id int64, accessToken, refreshToken string, expiresAt int64) error {
	const q = `
UPDATE accounts
   SET access_token = ?, refresh_token = ?, expires_at = ?, updated_at = ?
 WHERE id = ?`
	_, err := s.db.ExecContext(ctx, q, accessToken, refreshToken, expiresAt, time.Now().UnixMilli(), id)
	return err
}

// SetProfile rewrites the columns populated by /api/oauth/profile. Called
// at login time and as a one-shot backfill for accounts that predate the
// profile-fetching feature.
func (s *Store) SetProfile(ctx context.Context, id int64,
	email, fullName, organizationName, plan, subscriptionType string,
) error {
	const q = `
UPDATE accounts
   SET email = ?, full_name = ?, organization_name = ?, plan = ?,
       subscription_type = ?, updated_at = ?
 WHERE id = ?`
	_, err := s.db.ExecContext(ctx, q,
		email, fullName, organizationName, plan, subscriptionType,
		time.Now().UnixMilli(), id)
	return err
}

// SetUsage replaces the usage snapshot for one account. Pointer fields may be
// nil, in which case the corresponding columns are zeroed (the API didn't
// return that window this poll).
func (s *Store) SetUsage(ctx context.Context, id int64,
	fiveHourUtil float64, fiveHourResetsAt string,
	sevenDayUtil float64, sevenDayResetsAt string,
	sevenDaySonnetUtil float64, sevenDaySonnetResetsAt string,
) error {
	const q = `
UPDATE accounts
   SET five_hour_util = ?, five_hour_resets_at = ?,
       seven_day_util = ?, seven_day_resets_at = ?,
       seven_day_sonnet_util = ?, seven_day_sonnet_resets_at = ?,
       usage_fetched_at = ?, updated_at = ?
 WHERE id = ?`
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx, q,
		fiveHourUtil, fiveHourResetsAt,
		sevenDayUtil, sevenDayResetsAt,
		sevenDaySonnetUtil, sevenDaySonnetResetsAt,
		now, now, id)
	return err
}

// MarkUsed bumps last_used_at to now. Cheap helper called from /api/token.
func (s *Store) MarkUsed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET last_used_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// SetCooldown stamps cooldown_until and last_429_at. Pass time.Time{} as
// cooldownUntil to clear.
func (s *Store) SetCooldown(ctx context.Context, id int64, cooldownUntil time.Time) error {
	now := time.Now().UnixMilli()
	until := int64(0)
	if !cooldownUntil.IsZero() {
		until = cooldownUntil.UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET cooldown_until = ?, last_429_at = ?, updated_at = ? WHERE id = ?`,
		until, now, now, id)
	return err
}

// SetStatus toggles between "active" and "disabled".
func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("invalid status %q", status)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UnixMilli(), id)
	return err
}

// Delete removes the row entirely. Called when the user clicks "Remove" in
// the UI. The Claude side keeps no state we need to clean up; the
// refresh_token simply expires.
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id = ?`, id)
	return err
}

const selectColumns = `
id, name, access_token, refresh_token, expires_at, scopes,
subscription_type, organization_uuid, status,
cooldown_until, last_used_at, last_429_at, created_at, updated_at,
email, full_name, organization_name, plan,
five_hour_util, five_hour_resets_at,
seven_day_util, seven_day_resets_at,
seven_day_sonnet_util, seven_day_sonnet_resets_at,
usage_fetched_at`

// List returns every row ordered by id (stable insertion order).
func (s *Store) List(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectColumns+` FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAccounts(rows)
}

// Get fetches one row by id; returns ErrNotFound when missing.
var ErrNotFound = errors.New("account not found")

func (s *Store) Get(ctx context.Context, id int64) (*Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectColumns+` FROM accounts WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accs, err := scanAccounts(rows)
	if err != nil {
		return nil, err
	}
	if len(accs) == 0 {
		return nil, ErrNotFound
	}
	return &accs[0], nil
}

func scanAccounts(rows *sql.Rows) ([]Account, error) {
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(
			&a.ID, &a.Name, &a.AccessToken, &a.RefreshToken, &a.ExpiresAt,
			&a.Scopes, &a.SubscriptionType,
			&a.OrganizationUUID, &a.Status, &a.CooldownUntil, &a.LastUsedAt,
			&a.Last429At, &a.CreatedAt, &a.UpdatedAt,
			&a.Email, &a.FullName, &a.OrganizationName, &a.Plan,
			&a.FiveHourUtil, &a.FiveHourResetsAt,
			&a.SevenDayUtil, &a.SevenDayResetsAt,
			&a.SevenDaySonnetUtil, &a.SevenDaySonnetResetsAt,
			&a.UsageFetchedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func ifEmpty(s, dflt string) string {
	if strings.TrimSpace(s) == "" {
		return dflt
	}
	return s
}
