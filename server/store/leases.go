package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrLeaseLocked is returned when AcquireLease finds a live lease held by
// a different device. The agent's coordinator treats this as "another
// device beat me to this account; pick a different one on the next
// reconcile" rather than retrying immediately.
var ErrLeaseLocked = errors.New("account already leased by another device")

// Lease is a row in the leases table. The `id` is what callers pass to
// RenewLease / ReleaseLease — opaque random hex generated in vault/auth.
type Lease struct {
	ID         string
	AccountID  int64
	DeviceID   string
	AcquiredAt int64
	ExpiresAt  int64
}

// AcquireLease records (or replaces) the lease for accountID. Semantics:
//
//   - If no live lease exists for the account, insert with the supplied id.
//   - If the existing lease belongs to deviceID, update it in place
//     (renew TTL, return the same row's id). The supplied newID is ignored
//     in that case.
//   - If a different device holds it, return ErrLeaseLocked.
//
// The whole thing runs inside a transaction so two agents racing to
// acquire the same account can't both succeed.
func (s *Store) AcquireLease(ctx context.Context, newID string, accountID int64, deviceID string, ttl time.Duration) (Lease, error) {
	if newID == "" || accountID == 0 || deviceID == "" || ttl <= 0 {
		return Lease{}, fmt.Errorf("AcquireLease: id, account_id, device_id, ttl required")
	}
	now := time.Now()
	expiresAt := now.Add(ttl).UnixMilli()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Lease{}, err
	}
	defer tx.Rollback()

	// Sweep stale rows so the unique index doesn't reject our insert
	// because of a long-expired row that nobody released.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM leases WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return Lease{}, err
	}

	var existingID, existingDeviceID string
	err = tx.QueryRowContext(ctx,
		`SELECT id, device_id FROM leases WHERE account_id = ?`, accountID).
		Scan(&existingID, &existingDeviceID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Free — insert.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at)
			 VALUES (?, ?, ?, ?, ?)`,
			newID, accountID, deviceID, now.UnixMilli(), expiresAt); err != nil {
			return Lease{}, err
		}
		if err := tx.Commit(); err != nil {
			return Lease{}, err
		}
		return Lease{ID: newID, AccountID: accountID, DeviceID: deviceID,
			AcquiredAt: now.UnixMilli(), ExpiresAt: expiresAt}, nil
	case err != nil:
		return Lease{}, err
	}
	if existingDeviceID != deviceID {
		return Lease{}, ErrLeaseLocked
	}
	// Same device — renew in place.
	if _, err := tx.ExecContext(ctx,
		`UPDATE leases SET expires_at = ? WHERE id = ?`,
		expiresAt, existingID); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return Lease{ID: existingID, AccountID: accountID, DeviceID: deviceID,
		AcquiredAt: now.UnixMilli(), ExpiresAt: expiresAt}, nil
}

// RenewLease bumps a known lease's TTL. Returns ErrNotFound when the
// lease has expired or been released — caller is expected to re-acquire.
func (s *Store) RenewLease(ctx context.Context, leaseID string, ttl time.Duration) (Lease, error) {
	if leaseID == "" || ttl <= 0 {
		return Lease{}, fmt.Errorf("RenewLease: id and ttl required")
	}
	now := time.Now()
	expiresAt := now.Add(ttl).UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE leases SET expires_at = ?
		   WHERE id = ? AND expires_at > ?`,
		expiresAt, leaseID, now.UnixMilli())
	if err != nil {
		return Lease{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Lease{}, ErrNotFound
	}
	var l Lease
	err = s.db.QueryRowContext(ctx,
		`SELECT id, account_id, device_id, acquired_at, expires_at
		   FROM leases WHERE id = ?`, leaseID).
		Scan(&l.ID, &l.AccountID, &l.DeviceID, &l.AcquiredAt, &l.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	return l, err
}

// ReleaseLease removes the lease early. Idempotent — releasing an unknown
// lease is not an error.
func (s *Store) ReleaseLease(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM leases WHERE id = ?`, leaseID)
	return err
}

// FirstActiveLease returns the account_id of an arbitrary live lease, or
// (0, false) when no live lease exists. Used by the vault Web UI's
// dashboard handler to surface "which account is currently being used by
// some agent" — vault mode has no local credinject Coordinator, so this
// is the only way to derive in_use_account_id without round-tripping to
// the agent. Picks the longest-held lease deterministically (lowest
// acquired_at) so a multi-device deployment shows the same account on
// every dashboard reload instead of flapping between concurrent leases.
func (s *Store) FirstActiveLease(ctx context.Context) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id FROM leases
		   WHERE expires_at > ?
		   ORDER BY acquired_at ASC, id ASC
		   LIMIT 1`,
		time.Now().UnixMilli()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// LeaseInfo is the minimal lease shape returned by joined queries that
// also carry the holding device's display name. The dashboard's
// in_use[] surface and the per-account "leased by which device" badge
// both consume this; see ListAccountsWithLeases / ListActiveLeases.
type LeaseInfo struct {
	DeviceID   string
	DeviceName string
	AcquiredAt int64
	ExpiresAt  int64
}

// AccountWithLease pairs an Account with its current live lease (nil
// when no live lease exists). Returned by ListAccountsWithLeases so the
// vault accounts API can render per-account "in use by Device X"
// without an N+1 query per row.
type AccountWithLease struct {
	Account
	Lease *LeaseInfo
}

// LeaseWithDevice is a live lease row joined with the holding device's
// display name. Returned by ListActiveLeases so the dashboard can
// enumerate every in-use account in one round-trip.
type LeaseWithDevice struct {
	Lease
	DeviceName string
}

// LeaseWithAccount is a live lease row joined with the leased account's
// display name. Returned by ListActiveLeasesWithAccounts for the admin
// devices page (each device shows the name of the account it currently
// holds).
type LeaseWithAccount struct {
	Lease
	AccountName string
}

// ListAccountsWithLeases returns every account joined LEFT against the
// leases table (filtering expired rows) and devices (for the holding
// device's display name). Accounts without a live lease have Lease == nil.
// One query, no N+1 — callers like the vault accounts API render the
// "in use by Device X" badge straight from the result.
//
// DeviceName falls back to devices.hostname when devices.name is empty
// (defensive against legacy rows; current InsertDevice rejects empty
// names) so the UI never shows a blank badge.
func (s *Store) ListAccountsWithLeases(ctx context.Context) ([]AccountWithLease, error) {
	now := time.Now().UnixMilli()
	q := `SELECT ` + qualifiedAccountColumns + `,
	             leases.device_id, leases.acquired_at, leases.expires_at,
	             COALESCE(NULLIF(devices.name, ''), devices.hostname, '') AS device_name
	        FROM accounts
	        LEFT JOIN leases  ON leases.account_id = accounts.id AND leases.expires_at > ?
	        LEFT JOIN devices ON devices.id        = leases.device_id
	       ORDER BY accounts.id`
	rows, err := s.db.QueryContext(ctx, q, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AccountWithLease
	for rows.Next() {
		var a Account
		var deviceID, deviceName sql.NullString
		var acquiredAt, expiresAt sql.NullInt64
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
			&a.AccountUUID, &a.RateLimitTier,
			&deviceID, &acquiredAt, &expiresAt, &deviceName,
		); err != nil {
			return nil, err
		}
		var lease *LeaseInfo
		if deviceID.Valid {
			lease = &LeaseInfo{
				DeviceID:   deviceID.String,
				DeviceName: deviceName.String,
				AcquiredAt: acquiredAt.Int64,
				ExpiresAt:  expiresAt.Int64,
			}
		}
		out = append(out, AccountWithLease{Account: a, Lease: lease})
	}
	return out, rows.Err()
}

// ListActiveLeases returns every live lease joined with the holding
// device's display name. Ordered by acquired_at ASC, id ASC for
// deterministic dashboard rendering across multi-device deployments
// (matches FirstActiveLease's tiebreak so single-vs-list views agree).
func (s *Store) ListActiveLeases(ctx context.Context) ([]LeaseWithDevice, error) {
	now := time.Now().UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT leases.id, leases.account_id, leases.device_id,
		        leases.acquired_at, leases.expires_at,
		        COALESCE(NULLIF(devices.name, ''), devices.hostname, '') AS device_name
		   FROM leases
		   LEFT JOIN devices ON devices.id = leases.device_id
		  WHERE leases.expires_at > ?
		  ORDER BY leases.acquired_at ASC, leases.id ASC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LeaseWithDevice
	for rows.Next() {
		var l LeaseWithDevice
		var deviceName sql.NullString
		if err := rows.Scan(&l.ID, &l.AccountID, &l.DeviceID,
			&l.AcquiredAt, &l.ExpiresAt, &deviceName); err != nil {
			return nil, err
		}
		l.DeviceName = deviceName.String
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListActiveLeasesWithAccounts returns every live lease joined with
// the leased account's display name. Used by the vault admin's
// /admin/api/devices to render "currently using X" per device. Joining
// here (vs N+1 fetches in the handler) keeps the device list cheap
// even with many concurrent leases.
func (s *Store) ListActiveLeasesWithAccounts(ctx context.Context) ([]LeaseWithAccount, error) {
	now := time.Now().UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT leases.id, leases.account_id, leases.device_id,
		        leases.acquired_at, leases.expires_at,
		        COALESCE(accounts.name, '') AS account_name
		   FROM leases
		   LEFT JOIN accounts ON accounts.id = leases.account_id
		  WHERE leases.expires_at > ?
		  ORDER BY leases.acquired_at ASC, leases.id ASC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaseWithAccount
	for rows.Next() {
		var l LeaseWithAccount
		var accountName sql.NullString
		if err := rows.Scan(&l.ID, &l.AccountID, &l.DeviceID,
			&l.AcquiredAt, &l.ExpiresAt, &accountName); err != nil {
			return nil, err
		}
		l.AccountName = accountName.String
		out = append(out, l)
	}
	return out, rows.Err()
}

// qualifiedAccountColumns mirrors store.selectColumns but with each
// column prefixed by `accounts.` so it can be used inside JOINs without
// triggering ambiguous-column errors against leases.id / devices.id.
// Kept in lockstep with selectColumns; if you add a column to one,
// add it to the other.
const qualifiedAccountColumns = `
accounts.id, accounts.name, accounts.access_token, accounts.refresh_token, accounts.expires_at, accounts.scopes,
accounts.subscription_type, accounts.organization_uuid, accounts.status,
accounts.last_used_at, accounts.created_at, accounts.updated_at,
accounts.email, accounts.full_name, accounts.organization_name, accounts.plan,
accounts.five_hour_util, accounts.five_hour_resets_at,
accounts.seven_day_util, accounts.seven_day_resets_at,
accounts.seven_day_sonnet_util, accounts.seven_day_sonnet_resets_at,
accounts.usage_fetched_at,
accounts.five_hour_threshold, accounts.seven_day_threshold, accounts.seven_day_sonnet_threshold,
accounts.account_uuid, accounts.rate_limit_tier`

// IsAccountLeased reports whether accountID has a live lease. Used by
// refresh.Scheduler.IsAccountInUse and by selector.Pick to skip in-use
// accounts.
func (s *Store) IsAccountLeased(accountID int64) bool {
	var n int
	if err := s.db.QueryRow(
		`SELECT 1 FROM leases WHERE account_id = ? AND expires_at > ? LIMIT 1`,
		accountID, time.Now().UnixMilli()).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// IsAccountLeasedByOther reports whether accountID has a live lease
// held by some device OTHER than deviceID. Used by
// vault.InProc.PickForDevice so the caller's own lease isn't a
// disqualifier on rotation — a single-account pool can keep re-picking
// the held account on every reconcile tick instead of bouncing into
// ErrNoAvailable.
func (s *Store) IsAccountLeasedByOther(accountID int64, deviceID string) bool {
	var n int
	if err := s.db.QueryRow(
		`SELECT 1 FROM leases
		   WHERE account_id = ? AND device_id != ? AND expires_at > ?
		   LIMIT 1`,
		accountID, deviceID, time.Now().UnixMilli()).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// SweepLeases deletes expired rows so the leases_account_id_uniq index
// stays satisfiable for the next AcquireLease attempt. Run on a goroutine
// timer (typically every 30s).
func (s *Store) SweepLeases(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM leases WHERE expires_at <= ?`, time.Now().UnixMilli())
	return err
}
