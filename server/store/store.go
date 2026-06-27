// Package store wraps a SQLite database holding the credential pool used by
// the foxy-switcher account selector. The schema is intentionally tiny: one
// row per Claude subscription account, plus a couple of denormalised counters
// the selector reads on every credinject reconcile.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
	Status           string // "active" | "paused" | "needs_reauth" | "org_disabled"
	LastUsedAt       int64  // unix millis
	// PinnedDeviceID scopes a "Use now" pin to the device that asked for
	// it. Only that device's selector promotes the row to the front;
	// other devices follow plain LRU. Empty = not device-pinned (the
	// legacy global pin is last_used_at == 0). Cleared by MarkUsed once
	// any device actually injects the account.
	PinnedDeviceID string
	CreatedAt      int64
	UpdatedAt      int64

	// Profile fields populated once at login from /api/oauth/profile.
	// AccountUUID is the stable per-user id and the dedup key used by Upsert.
	// Email / FullName can change (alias swap, SSO rebind, primary-email
	// migration on Anthropic's side); uuid does not. Older rows that predate
	// this column carry "" until the next UsagePoller tick fills them in.
	AccountUUID      string
	Email            string
	FullName         string
	OrganizationName string
	Plan             string // "Claude Max" | "Claude Pro" | "Claude Team Premium" | "Claude Team" | "API / Free"
	// RateLimitTier is the raw `organization.rate_limit_tier` from
	// /api/oauth/profile — "default_claude_pro" | "default_claude_max_5x" |
	// "default_claude_max_20x" (and "" for legacy rows that haven't been
	// backfilled yet). The pool-aggregate dashboard weights off this so we
	// can distinguish personal Max 5x from Max 20x; subscription_type maps
	// both to "max".
	RateLimitTier string

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

	// Per-account utilization thresholds (0–100). The selector skips an
	// account when any window's util has reached the matching threshold.
	// Default 95 leaves a small buffer below the upstream 429 line; raise
	// to 100 to opt out of pre-emptive skipping for that window.
	FiveHourThreshold       float64
	SevenDayThreshold       float64
	SevenDaySonnetThreshold float64
}

// TokenExpired reports whether the access_token is past its lifetime at
// `now`. ExpiresAt==0 is treated as "unknown / not expired" so freshly-added
// rows that haven't received a server-assigned expiry don't get gated out
// before their first refresh tick.
func (a Account) TokenExpired(now time.Time) bool {
	if a.ExpiresAt <= 0 {
		return false
	}
	return a.ExpiresAt <= now.UnixMilli()
}

// tableSchema is the create-from-scratch table DDL. For existing databases,
// columnMigrations adds any columns that were introduced after the initial
// release and migrateLegacyOrgUnique drops the obsolete UNIQUE constraint.
//
// Uniqueness is enforced via a partial index on `email` (see indexSchema)
// rather than a column-level constraint: empty emails (the fallback when the
// OAuth profile API doesn't surface one) must be allowed to coexist as
// distinct rows.
const tableSchema = `
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
  last_used_at               INTEGER NOT NULL DEFAULT 0,
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
  five_hour_threshold        REAL    NOT NULL DEFAULT 95,
  seven_day_threshold        REAL    NOT NULL DEFAULT 95,
  seven_day_sonnet_threshold REAL    NOT NULL DEFAULT 95,
  account_uuid               TEXT    NOT NULL DEFAULT '',
  rate_limit_tier            TEXT    NOT NULL DEFAULT '',
  pinned_device_id           TEXT    NOT NULL DEFAULT ''
);
`

// indexSchema's account_uuid_uniq is the canonical dedup key — email can
// shift over time (alias swap, SSO rebind), but Anthropic's account.uuid is
// stable, so two re-logins of the same user collapse onto one row. The
// pre-existing email_uniq is kept as a soft guardrail for rows that don't
// yet have an account_uuid (older installs before the next UsagePoller tick
// fills it in).
const indexSchema = `
CREATE INDEX IF NOT EXISTS accounts_status_lru
  ON accounts (status, last_used_at);
CREATE UNIQUE INDEX IF NOT EXISTS accounts_email_uniq
  ON accounts (email) WHERE email != '';
CREATE UNIQUE INDEX IF NOT EXISTS accounts_uuid_uniq
  ON accounts (account_uuid) WHERE account_uuid != '';
`

// kvSchema holds simple daemon-wide key/value settings (auto-switch toggle,
// future preferences). Idempotent CREATE so existing databases pick it up on
// next start.
const kvSchema = `
CREATE TABLE IF NOT EXISTS kv (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// usageHistorySchema records successful usage snapshots so the Dashboard can
// render a 24h trend chart without re-fetching from Anthropic. UsagePoller
// appends one row per account per successful tick; rows older than 7 days
// are pruned (well past the 24h chart window, plenty of headroom).
const usageHistorySchema = `
CREATE TABLE IF NOT EXISTS usage_history (
  id                    INTEGER PRIMARY KEY AUTOINCREMENT,
  account_id            INTEGER NOT NULL,
  ts                    INTEGER NOT NULL,
  five_hour_util        REAL NOT NULL DEFAULT 0,
  seven_day_util        REAL NOT NULL DEFAULT 0,
  seven_day_sonnet_util REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_history_ts ON usage_history (ts DESC);
CREATE INDEX IF NOT EXISTS usage_history_account_ts ON usage_history (account_id, ts DESC);
`

// UsageHistoryRetention is how far back we keep snapshots. 7 days is well
// past the 24h chart window with enough slack to support a future "1 week"
// view without a schema change.
const UsageHistoryRetention = 7 * 24 * time.Hour

// columnMigrations adds columns to existing databases. Each entry is run in
// order and is idempotent (failures with "duplicate column" are silently
// ignored, so re-running on an already-migrated DB is fine).
var columnMigrations = []string{
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
	`ALTER TABLE accounts ADD COLUMN five_hour_threshold REAL NOT NULL DEFAULT 95`,
	`ALTER TABLE accounts ADD COLUMN seven_day_threshold REAL NOT NULL DEFAULT 95`,
	`ALTER TABLE accounts ADD COLUMN seven_day_sonnet_threshold REAL NOT NULL DEFAULT 95`,
	// account_uuid is the stable per-user id from /api/oauth/profile. Older
	// rows backfill this on the next UsagePoller tick (see refresh.UsagePoller).
	`ALTER TABLE accounts ADD COLUMN account_uuid TEXT NOT NULL DEFAULT ''`,
	// rate_limit_tier is the authoritative quota label from
	// /api/oauth/profile.organization.rate_limit_tier — values like
	// "default_claude_pro", "default_claude_max_5x", "default_claude_max_20x".
	// We use it (not subscription_type) to weight pool-aggregate dashboards,
	// because subscription_type can't tell personal Max 5x from Max 20x.
	// Older rows backfill on the next UsagePoller tick.
	`ALTER TABLE accounts ADD COLUMN rate_limit_tier TEXT NOT NULL DEFAULT ''`,
	// pinned_device_id scopes a "Use now" pin to the requesting device so
	// other devices' reconcile ticks don't race to grab the account.
	`ALTER TABLE accounts ADD COLUMN pinned_device_id TEXT NOT NULL DEFAULT ''`,
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
	if _, err := db.Exec(tableSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	for _, stmt := range columnMigrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			db.Close()
			return nil, fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	if err := migrateLegacyOrgUnique(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate legacy org-unique: %w", err)
	}
	if err := migrateDropCooldownColumns(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate drop cooldown columns: %w", err)
	}
	// Rename legacy 'disabled' status to 'paused'. Idempotent: matches no rows
	// once migration has run. The off-state was renamed when pause/resume
	// replaced enable/disable in the desktop UI.
	if _, err := db.Exec(`UPDATE accounts SET status='paused' WHERE status='disabled'`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate disabled->paused: %w", err)
	}
	// Drop the legacy cooldown index if it survived (e.g. from an older
	// install). The replacement accounts_status_lru is created via indexSchema
	// below.
	if _, err := db.Exec(`DROP INDEX IF EXISTS accounts_status_cooldown`); err != nil {
		db.Close()
		return nil, fmt.Errorf("drop legacy cooldown index: %w", err)
	}
	if _, err := db.Exec(indexSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply indexes: %w", err)
	}
	if _, err := db.Exec(kvSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply kv schema: %w", err)
	}
	if _, err := db.Exec(usageHistorySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply usage_history schema: %w", err)
	}
	if _, err := db.Exec(authSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply auth schema: %w", err)
	}
	for _, stmt := range authColumnMigrations {
		if _, err := db.Exec(stmt); err != nil && !isDuplicateColumn(err) {
			db.Close()
			return nil, fmt.Errorf("migrate %q: %w", stmt, err)
		}
	}
	return &Store{db: db}, nil
}

// migrateLegacyOrgUnique rebuilds the accounts table without the original
// `UNIQUE (organization_uuid)` column constraint. The constraint was a
// dead-letter from a planned dedup-by-org feature: the OAuth profile API
// never populated organization_uuid, so every login landed with org="" and
// silently overwrote the previous row. We can't DROP a column-level UNIQUE
// in SQLite, hence the table-rebuild dance.
//
// Idempotent: returns immediately when the legacy constraint isn't present
// (fresh installs, or already-migrated databases).
func migrateLegacyOrgUnique(db *sql.DB) error {
	var sqlText string
	row := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='accounts'`)
	if err := row.Scan(&sqlText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !strings.Contains(sqlText, "UNIQUE (organization_uuid)") {
		return nil
	}
	const rebuild = `
BEGIN TRANSACTION;
CREATE TABLE accounts_new (
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
  five_hour_threshold        REAL    NOT NULL DEFAULT 95,
  seven_day_threshold        REAL    NOT NULL DEFAULT 95,
  seven_day_sonnet_threshold REAL    NOT NULL DEFAULT 95,
  account_uuid               TEXT    NOT NULL DEFAULT '',
  rate_limit_tier            TEXT    NOT NULL DEFAULT '',
  pinned_device_id           TEXT    NOT NULL DEFAULT ''
);
INSERT INTO accounts_new SELECT
  id, name, access_token, refresh_token, expires_at, scopes,
  subscription_type, organization_uuid, status, cooldown_until,
  last_used_at, last_429_at, created_at, updated_at,
  email, full_name, organization_name, plan,
  five_hour_util, five_hour_resets_at,
  seven_day_util, seven_day_resets_at,
  seven_day_sonnet_util, seven_day_sonnet_resets_at,
  usage_fetched_at,
  five_hour_threshold, seven_day_threshold, seven_day_sonnet_threshold,
  account_uuid, rate_limit_tier, pinned_device_id FROM accounts;
DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
COMMIT;
`
	_, err := db.Exec(rebuild)
	return err
}

// migrateDropCooldownColumns drops the legacy `cooldown_until` and `last_429_at`
// columns. They backed a manual / planned-429-driven cooldown feature that was
// removed in favour of the per-window utilization thresholds (see
// selector.exceedsThreshold) and the existing pause/resume controls.
//
// SQLite < 3.35 doesn't support DROP COLUMN, so we go through the same
// table-rebuild dance as migrateLegacyOrgUnique. Idempotent: returns
// immediately when the columns are already gone (fresh installs, or already-
// migrated databases).
func migrateDropCooldownColumns(db *sql.DB) error {
	var sqlText string
	row := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='accounts'`)
	if err := row.Scan(&sqlText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if !strings.Contains(sqlText, "cooldown_until") && !strings.Contains(sqlText, "last_429_at") {
		return nil
	}
	const rebuild = `
BEGIN TRANSACTION;
CREATE TABLE accounts_new (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT,
  name                       TEXT    NOT NULL,
  access_token               TEXT    NOT NULL,
  refresh_token              TEXT    NOT NULL,
  expires_at                 INTEGER NOT NULL,
  scopes                     TEXT    NOT NULL DEFAULT '',
  subscription_type          TEXT    NOT NULL DEFAULT '',
  organization_uuid          TEXT    NOT NULL DEFAULT '',
  status                     TEXT    NOT NULL DEFAULT 'active',
  last_used_at               INTEGER NOT NULL DEFAULT 0,
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
  five_hour_threshold        REAL    NOT NULL DEFAULT 95,
  seven_day_threshold        REAL    NOT NULL DEFAULT 95,
  seven_day_sonnet_threshold REAL    NOT NULL DEFAULT 95,
  account_uuid               TEXT    NOT NULL DEFAULT '',
  rate_limit_tier            TEXT    NOT NULL DEFAULT '',
  pinned_device_id           TEXT    NOT NULL DEFAULT ''
);
INSERT INTO accounts_new SELECT
  id, name, access_token, refresh_token, expires_at, scopes,
  subscription_type, organization_uuid, status,
  last_used_at, created_at, updated_at,
  email, full_name, organization_name, plan,
  five_hour_util, five_hour_resets_at,
  seven_day_util, seven_day_resets_at,
  seven_day_sonnet_util, seven_day_sonnet_resets_at,
  usage_fetched_at,
  five_hour_threshold, seven_day_threshold, seven_day_sonnet_threshold,
  account_uuid, rate_limit_tier, pinned_device_id FROM accounts;
DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;
COMMIT;
`
	_, err := db.Exec(rebuild)
	return err
}

func isDuplicateColumn(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func (s *Store) Close() error { return s.db.Close() }

// DB exposes the underlying *sql.DB for callers that need transactions
// (currently only the OAuth refresh path uses this, to take a row-level lock
// before issuing the network exchange).
func (s *Store) DB() *sql.DB { return s.db }

// Upsert inserts a new account or, when one already represents the same
// underlying Anthropic user, replaces its tokens / metadata in place. The id
// of the resulting row is set on a.ID.
//
// Dedup precedence:
//  1. account_uuid (the stable id from /api/oauth/profile). This is the
//     canonical key — Anthropic's primary email can shift over time (alias
//     swap, SSO rebind, account migration), but uuid does not.
//  2. email — only when the existing row has no account_uuid yet (legacy
//     rows from before this column existed). This is the transition path:
//     once UsagePoller's profile-backfill writes account_uuid into older
//     rows, future Upserts hit case 1 cleanly.
//
// Both keys are skipped when their value is empty, so accounts that haven't
// surfaced either field coexist as distinct rows.
func (s *Store) Upsert(ctx context.Context, a *Account) error {
	now := time.Now().UnixMilli()
	if a.CreatedAt == 0 {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	if a.LastUsedAt == 0 {
		a.LastUsedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var existingID int64
	err = tx.QueryRowContext(ctx, `
SELECT id FROM accounts
 WHERE (? != '' AND account_uuid = ?)
    OR (? != '' AND account_uuid = '' AND email = ?)
 ORDER BY id
 LIMIT 1`,
		a.AccountUUID, a.AccountUUID,
		a.Email, a.Email,
	).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if existingID > 0 {
		a.ID = existingID
		if _, err := tx.ExecContext(ctx, `
UPDATE accounts SET
  name = ?,
  access_token = ?, refresh_token = ?, expires_at = ?,
  scopes = ?, subscription_type = ?, organization_uuid = ?,
  email = ?, full_name = ?, organization_name = ?, plan = ?,
  account_uuid = ?, rate_limit_tier = ?,
  status = 'active',
  updated_at = ?
WHERE id = ?`,
			a.Name,
			a.AccessToken, a.RefreshToken, a.ExpiresAt,
			a.Scopes, a.SubscriptionType, a.OrganizationUUID,
			a.Email, a.FullName, a.OrganizationName, a.Plan,
			a.AccountUUID, a.RateLimitTier,
			a.UpdatedAt, existingID,
		); err != nil {
			return err
		}
		return tx.Commit()
	}

	// Seed per-window thresholds from the pool-wide defaults for brand-new
	// accounts (existing rows above keep their own values). The caller rarely
	// populates these on the Account; when a field is left at its zero value
	// we fall back to the configured default so the "default threshold" knob
	// actually governs new accounts. GetSettings is read here, before the
	// INSERT acquires the write lock, so it doesn't deadlock the open tx.
	defaults, derr := s.GetSettings(ctx)
	if derr != nil {
		defaults = DefaultSettings
	}
	if a.FiveHourThreshold <= 0 {
		a.FiveHourThreshold = defaults.DefaultFiveHourThreshold
	}
	if a.SevenDayThreshold <= 0 {
		a.SevenDayThreshold = defaults.DefaultSevenDayThreshold
	}
	if a.SevenDaySonnetThreshold <= 0 {
		a.SevenDaySonnetThreshold = defaults.DefaultSevenDaySonnetThreshold
	}

	res, err := tx.ExecContext(ctx, `
INSERT INTO accounts
  (name, access_token, refresh_token, expires_at, scopes, subscription_type,
   organization_uuid, status, last_used_at,
   created_at, updated_at,
   email, full_name, organization_name, plan, account_uuid, rate_limit_tier,
   five_hour_threshold, seven_day_threshold, seven_day_sonnet_threshold)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Name, a.AccessToken, a.RefreshToken, a.ExpiresAt,
		a.Scopes, a.SubscriptionType, a.OrganizationUUID,
		ifEmpty(a.Status, "active"), a.LastUsedAt,
		a.CreatedAt, a.UpdatedAt,
		a.Email, a.FullName, a.OrganizationName, a.Plan,
		a.AccountUUID, a.RateLimitTier,
		clampPercent(a.FiveHourThreshold), clampPercent(a.SevenDayThreshold), clampPercent(a.SevenDaySonnetThreshold),
	)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	a.ID = id
	return tx.Commit()
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
//
// account_uuid is only written when (a) the existing row has none, (b) the
// caller actually has a uuid, and (c) no other row has already claimed
// that uuid. Condition (c) is the legacy-duplicate guard: when two rows
// historically represent the same Anthropic user (e.g. an email-shifted
// re-login created a second row before this column existed), only the
// first poll wins; the loser keeps account_uuid=” so the partial unique
// index doesn't trip. The user resolves the duplicate by deleting one row
// in the UI.
func (s *Store) SetProfile(ctx context.Context, id int64,
	accountUUID, email, fullName, organizationName, plan, subscriptionType, rateLimitTier string,
) error {
	const q = `
UPDATE accounts
   SET email = ?, full_name = ?, organization_name = ?, plan = ?,
       subscription_type = ?, rate_limit_tier = ?,
       account_uuid = CASE
         WHEN account_uuid = ''
              AND ? != ''
              AND NOT EXISTS (
                SELECT 1 FROM accounts WHERE account_uuid = ? AND id != ?
              )
           THEN ?
         ELSE account_uuid
       END,
       updated_at = ?
 WHERE id = ?`
	_, err := s.db.ExecContext(ctx, q,
		email, fullName, organizationName, plan, subscriptionType, rateLimitTier,
		accountUUID, accountUUID, id, accountUUID,
		time.Now().UnixMilli(), id)
	return err
}

// UsageHistoryRow is one snapshot from usage_history. Always sorted ascending
// by ts when returned from UsageHistorySince so chart code can plot directly.
type UsageHistoryRow struct {
	AccountID          int64
	Timestamp          int64
	FiveHourUtil       float64
	SevenDayUtil       float64
	SevenDaySonnetUtil float64
}

// AppendUsageHistory inserts one snapshot row, then prunes rows older than
// UsageHistoryRetention so the table never grows unboundedly. UsagePoller
// calls this from inside its tick after writeUsage succeeds.
func (s *Store) AppendUsageHistory(ctx context.Context, accountID int64,
	ts int64, fhU, sdU, ssU float64,
) error {
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO usage_history (account_id, ts, five_hour_util, seven_day_util, seven_day_sonnet_util)
VALUES (?, ?, ?, ?, ?)`, accountID, ts, fhU, sdU, ssU); err != nil {
		return err
	}
	cutoff := time.Now().Add(-UsageHistoryRetention).UnixMilli()
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM usage_history WHERE ts < ?`, cutoff); err != nil {
		return err
	}
	return nil
}

// UsageHistorySince returns every snapshot row with ts >= since, ordered
// ascending by ts so the caller can iterate in time order. The Dashboard
// trend handler buckets these into hours; tests use the raw rows.
func (s *Store) UsageHistorySince(ctx context.Context, since int64) ([]UsageHistoryRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT account_id, ts, five_hour_util, seven_day_util, seven_day_sonnet_util
  FROM usage_history
 WHERE ts >= ?
 ORDER BY ts ASC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageHistoryRow
	for rows.Next() {
		var r UsageHistoryRow
		if err := rows.Scan(&r.AccountID, &r.Timestamp, &r.FiveHourUtil, &r.SevenDayUtil, &r.SevenDaySonnetUtil); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageHistoryForAccountSince returns one account's snapshots with ts >= since,
// ordered ascending by ts. Per-device quota attribution walks these deltas;
// the (account_id, ts DESC) index keeps the scan cheap.
func (s *Store) UsageHistoryForAccountSince(ctx context.Context, accountID, since int64) ([]UsageHistoryRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT account_id, ts, five_hour_util, seven_day_util, seven_day_sonnet_util
  FROM usage_history
 WHERE account_id = ? AND ts >= ?
 ORDER BY ts ASC`, accountID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageHistoryRow
	for rows.Next() {
		var r UsageHistoryRow
		if err := rows.Scan(&r.AccountID, &r.Timestamp, &r.FiveHourUtil, &r.SevenDayUtil, &r.SevenDaySonnetUtil); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
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

// MarkUsed bumps last_used_at to now. Called by credinject after a successful
// inject so the selector's LRU tiebreaker reflects real pool usage. Also
// consumes any device pin on the row — the pin is a one-shot "switch to this
// next" request, satisfied the moment some device actually injects it.
func (s *Store) MarkUsed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET last_used_at = ?, pinned_device_id = '' WHERE id = ?`,
		time.Now().UnixMilli(), id)
	return err
}

// MarkForNextPick requests a one-shot manual switch to this account ("Use
// now" in the UI). With a non-empty deviceID the pin is scoped to that
// device: only its selector promotes the row, so other devices' reconcile
// ticks can't race to grab the account first. Any previous pin held by the
// same device is replaced. deviceID == "" falls back to the legacy global
// pin (last_used_at = 0) used by callers with no device identity (web-admin
// sessions); every auto-mode device races for those, by design. Either form
// is consumed by the next MarkUsed.
func (s *Store) MarkForNextPick(ctx context.Context, id int64, deviceID string) error {
	now := time.Now().UnixMilli()
	if deviceID == "" {
		_, err := s.db.ExecContext(ctx,
			`UPDATE accounts SET last_used_at = 0, updated_at = ? WHERE id = ?`,
			now, id)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET pinned_device_id = '', updated_at = ? WHERE pinned_device_id = ?`,
		now, deviceID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET pinned_device_id = ?, updated_at = ? WHERE id = ?`,
		deviceID, now, id); err != nil {
		return err
	}
	return tx.Commit()
}

// SetThresholds rewrites the per-window utilization thresholds for one
// account. Inputs are clamped to [0, 100]. The selector skips accounts whose
// util on any window has reached the matching threshold; the schema default
// is 95 (small pre-emptive buffer); 100 disables skipping for that window.
func (s *Store) SetThresholds(ctx context.Context, id int64, fiveHour, sevenDay, sevenDaySonnet float64) error {
	const q = `
UPDATE accounts
   SET five_hour_threshold = ?, seven_day_threshold = ?, seven_day_sonnet_threshold = ?,
       updated_at = ?
 WHERE id = ?`
	_, err := s.db.ExecContext(ctx, q,
		clampPercent(fiveHour), clampPercent(sevenDay), clampPercent(sevenDaySonnet),
		time.Now().UnixMilli(), id)
	return err
}

// ApplyThresholdsToAll overwrites every account's per-window thresholds with
// the supplied values (clamped to [0, 100]), returning the number of rows
// updated. This is an intentional, indiscriminate overwrite — accounts that
// were manually tuned are reset too, which is the documented behaviour of the
// operator's "apply default thresholds to all accounts" action.
func (s *Store) ApplyThresholdsToAll(ctx context.Context, fiveHour, sevenDay, sevenDaySonnet float64) (int64, error) {
	const q = `
UPDATE accounts
   SET five_hour_threshold = ?, seven_day_threshold = ?, seven_day_sonnet_threshold = ?,
       updated_at = ?`
	res, err := s.db.ExecContext(ctx, q,
		clampPercent(fiveHour), clampPercent(sevenDay), clampPercent(sevenDaySonnet),
		time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// Account status values. Stored verbatim in the `status` TEXT column. The
// selector treats any value other than StatusActive as "do not route here".
const (
	StatusActive = "active"
	StatusPaused = "paused"
	// StatusNeedsReauth marks an account whose refresh_token was rejected by
	// the token endpoint with invalid_grant — the token is dead and only the
	// user re-authenticating can revive it. Being non-active it's already
	// excluded from routing; unlike StatusPaused it's set by the system (the
	// refresh scheduler), and the scheduler stops retrying it. A successful
	// re-login flows through Upsert, which resets status back to active.
	StatusNeedsReauth = "needs_reauth"
	// StatusOrgDisabled marks an account whose organization has OAuth/API
	// access disabled (Anthropic answers the OAuth API with 403
	// permission_error). The token still refreshes fine, but every API call —
	// usage poll AND inference — is rejected, so routing to it just breaks the
	// user's session. Unlike needs_reauth, re-login can't fix it (it's an org
	// policy); the fix is at the org level or removing the account. Set by the
	// usage poller and auto-cleared back to active on the next successful poll
	// (so a transient block or a re-enabled org self-heals).
	StatusOrgDisabled = "org_disabled"
)

// SetStatus toggles between "active" and "paused". "paused" only suppresses
// routing (selector skips it, credinject won't inject it) — refresh and usage
// pollers continue to maintain the row. needs_reauth is intentionally NOT a
// valid target here: it's a system verdict set via MarkNeedsReauth, not a
// user toggle.
func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	if status != StatusActive && status != StatusPaused {
		return fmt.Errorf("invalid status %q", status)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`,
		status, time.Now().UnixMilli(), id)
	return err
}

// MarkNeedsReauth flags an account as requiring re-authentication after its
// refresh_token was permanently rejected (see StatusNeedsReauth). Idempotent.
func (s *Store) MarkNeedsReauth(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`,
		StatusNeedsReauth, time.Now().UnixMilli(), id)
	return err
}

// MarkOrgDisabled flags an account whose organization has OAuth disabled (see
// StatusOrgDisabled). Idempotent. Cleared by the usage poller via SetStatus on
// the next successful poll.
func (s *Store) MarkOrgDisabled(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET status = ?, updated_at = ? WHERE id = ?`,
		StatusOrgDisabled, time.Now().UnixMilli(), id)
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
last_used_at, created_at, updated_at,
email, full_name, organization_name, plan,
five_hour_util, five_hour_resets_at,
seven_day_util, seven_day_resets_at,
seven_day_sonnet_util, seven_day_sonnet_resets_at,
usage_fetched_at,
five_hour_threshold, seven_day_threshold, seven_day_sonnet_threshold,
account_uuid, rate_limit_tier, pinned_device_id`

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
			&a.OrganizationUUID, &a.Status, &a.LastUsedAt,
			&a.CreatedAt, &a.UpdatedAt,
			&a.Email, &a.FullName, &a.OrganizationName, &a.Plan,
			&a.FiveHourUtil, &a.FiveHourResetsAt,
			&a.SevenDayUtil, &a.SevenDayResetsAt,
			&a.SevenDaySonnetUtil, &a.SevenDaySonnetResetsAt,
			&a.UsageFetchedAt,
			&a.FiveHourThreshold, &a.SevenDayThreshold, &a.SevenDaySonnetThreshold,
			&a.AccountUUID, &a.RateLimitTier, &a.PinnedDeviceID,
		); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AutoSwitch holds the daemon-wide policy that the credinject coordinator
// consults on every reconcile tick. Persisted to the kv table under key
// "auto_switch" as JSON; defaults are returned for missing rows so a fresh
// install behaves like the old "always rotate" behaviour.
type AutoSwitch struct {
	Enabled bool   `json:"enabled"`
	Policy  string `json:"policy"`
}

const autoSwitchKey = "auto_switch"

// DefaultAutoSwitch is the fallback returned by GetAutoSwitch when no row
// has ever been written. Matches the pre-feature behaviour: rotate on LRU.
var DefaultAutoSwitch = AutoSwitch{Enabled: true, Policy: "lru"}

// GetAutoSwitch reads the auto-switch setting. Missing row → defaults; a
// corrupt JSON value is treated as missing (defaults returned, no error)
// so a malformed manual edit can't brick the daemon.
func (s *Store) GetAutoSwitch(ctx context.Context) (AutoSwitch, error) {
	var raw string
	row := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, autoSwitchKey)
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultAutoSwitch, nil
		}
		return AutoSwitch{}, err
	}
	var v AutoSwitch
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return DefaultAutoSwitch, nil
	}
	if v.Policy == "" {
		v.Policy = DefaultAutoSwitch.Policy
	}
	return v, nil
}

// SetAutoSwitch persists the auto-switch setting. Caller is expected to have
// already validated `policy` against the allowed set.
func (s *Store) SetAutoSwitch(ctx context.Context, v AutoSwitch) error {
	buf, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal auto-switch: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		autoSwitchKey, string(buf), time.Now().UnixMilli())
	return err
}

// Settings is the user-preference blob persisted under one kv row. Frontend
// edits go through PUT /api/settings; backend components read at startup
// (refresh interval) or on every consult (theme is frontend-only, restore
// flag is read in the shutdown defer). Keep new fields backward-compatible
// — missing fields fall back to DefaultSettings via a zero-aware merge.
type Settings struct {
	// Theme: "system" | "light" | "dark". Frontend-only — server stores
	// verbatim and the UI applies via `data-theme` on the document root.
	Theme string `json:"theme"`
	// SidebarMode: "expanded" | "auto". Frontend-only.
	SidebarMode string `json:"sidebar_mode"`
	// UsagePollIntervalSec controls UsagePoller's ticker. Range 30–300; the
	// server clamps. Applies on next daemon start (the existing ticker is
	// fixed-period; rebuilding it on every PUT would race with in-flight ticks).
	UsagePollIntervalSec int `json:"usage_poll_interval_sec"`
	// Default{FiveHour,SevenDay,SevenDaySonnet}Threshold are the pool-wide
	// defaults for new accounts' per-window thresholds, and the values the
	// "apply to all accounts" action writes across every existing account.
	// Existing accounts otherwise keep their own per-window values.
	//
	// The JSON form was historically a single `default_threshold_percent`
	// (and, even earlier, `cooldown_threshold_percent`). GetSettings lifts
	// either legacy single value into all three windows so old persisted
	// blobs survive the split into per-window defaults.
	DefaultFiveHourThreshold       float64 `json:"default_five_hour"`
	DefaultSevenDayThreshold       float64 `json:"default_seven_day"`
	DefaultSevenDaySonnetThreshold float64 `json:"default_seven_day_sonnet"`
	// RestoreNativeOnQuit gates the credinject Restore call at shutdown. Off
	// is "leave whatever was last injected in place" — useful for users who
	// don't want their native login auto-replaced on quit.
	RestoreNativeOnQuit bool `json:"restore_native_on_quit"`
}

const settingsKey = "settings"

// DefaultSettings mirrors PRD §5.4 defaults: System theme, 60s poll interval,
// 95% default per-window threshold, restore native on quit.
var DefaultSettings = Settings{
	Theme:                          "system",
	SidebarMode:                    "auto",
	UsagePollIntervalSec:           60,
	DefaultFiveHourThreshold:       95,
	DefaultSevenDayThreshold:       95,
	DefaultSevenDaySonnetThreshold: 95,
	RestoreNativeOnQuit:            true,
}

// GetSettings reads the settings blob. Missing row → defaults. Corrupt JSON
// is treated like missing so a hand-edited kv row can't brick startup.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var raw string
	row := s.db.QueryRowContext(ctx, `SELECT value FROM kv WHERE key = ?`, settingsKey)
	if err := row.Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DefaultSettings, nil
		}
		return Settings{}, err
	}
	v := DefaultSettings
	// Unmarshal into a copy of defaults so missing fields stay populated.
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return DefaultSettings, nil
	}
	// Legacy fallback: blobs written before the per-window split stored a
	// single threshold under `default_threshold_percent` (and, even earlier,
	// `cooldown_threshold_percent`). Lift that single value into all three
	// windows, but only when none of the new per-window keys are present so a
	// co-resident new key always wins. Prefer the newer legacy key when both
	// single-value keys somehow coexist.
	var probe struct {
		FiveHour       *float64 `json:"default_five_hour"`
		SevenDay       *float64 `json:"default_seven_day"`
		SevenDaySonnet *float64 `json:"default_seven_day_sonnet"`
		LegacyDefault  *float64 `json:"default_threshold_percent"`
		LegacyCooldown *float64 `json:"cooldown_threshold_percent"`
	}
	if json.Unmarshal([]byte(raw), &probe) == nil &&
		probe.FiveHour == nil && probe.SevenDay == nil && probe.SevenDaySonnet == nil {
		legacy := probe.LegacyDefault
		if legacy == nil {
			legacy = probe.LegacyCooldown
		}
		if legacy != nil {
			v.DefaultFiveHourThreshold = *legacy
			v.DefaultSevenDayThreshold = *legacy
			v.DefaultSevenDaySonnetThreshold = *legacy
		}
	}
	return mergeSettingsDefaults(v), nil
}

// SetSettings writes the settings blob, after clamping numeric ranges and
// substituting defaults for empty strings — the frontend can submit raw
// values and trust the server to land on a valid state.
func (s *Store) SetSettings(ctx context.Context, v Settings) (Settings, error) {
	v = mergeSettingsDefaults(v)
	buf, err := json.Marshal(v)
	if err != nil {
		return Settings{}, fmt.Errorf("marshal settings: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingsKey, string(buf), time.Now().UnixMilli())
	if err != nil {
		return Settings{}, err
	}
	return v, nil
}

// mergeSettingsDefaults clamps numeric ranges and substitutes empty strings
// with their default, so a partial PUT body (or a hand-edited kv row) lands
// on a coherent value rather than a half-zeroed struct.
func mergeSettingsDefaults(v Settings) Settings {
	if v.Theme == "" {
		v.Theme = DefaultSettings.Theme
	}
	if v.SidebarMode == "" {
		v.SidebarMode = DefaultSettings.SidebarMode
	}
	if v.UsagePollIntervalSec <= 0 {
		v.UsagePollIntervalSec = DefaultSettings.UsagePollIntervalSec
	}
	if v.UsagePollIntervalSec < 30 {
		v.UsagePollIntervalSec = 30
	}
	if v.UsagePollIntervalSec > 300 {
		v.UsagePollIntervalSec = 300
	}
	if v.DefaultFiveHourThreshold <= 0 {
		v.DefaultFiveHourThreshold = DefaultSettings.DefaultFiveHourThreshold
	}
	if v.DefaultSevenDayThreshold <= 0 {
		v.DefaultSevenDayThreshold = DefaultSettings.DefaultSevenDayThreshold
	}
	if v.DefaultSevenDaySonnetThreshold <= 0 {
		v.DefaultSevenDaySonnetThreshold = DefaultSettings.DefaultSevenDaySonnetThreshold
	}
	v.DefaultFiveHourThreshold = clampPercent(v.DefaultFiveHourThreshold)
	v.DefaultSevenDayThreshold = clampPercent(v.DefaultSevenDayThreshold)
	v.DefaultSevenDaySonnetThreshold = clampPercent(v.DefaultSevenDaySonnetThreshold)
	return v
}

func ifEmpty(s, dflt string) string {
	if strings.TrimSpace(s) == "" {
		return dflt
	}
	return s
}
