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
	// LastActiveAt is the ms wall-clock of the holder's last real Claude Code
	// activity (now - idleFor, reported on renew). Drives idle-reclaim.
	LastActiveAt int64
}

// AcquireLease records (or replaces) the lease for accountID. Semantics:
//
//   - If this device already holds a live lease on the account, update it in
//     place (renew TTL, return the same row's id). The supplied newID is
//     ignored in that case.
//   - Otherwise insert with the supplied id, EXCEPT on an exclusive-provider
//     account another device already holds, which returns ErrLeaseLocked.
//
// Exclusivity is per provider (see ProviderSharesAccounts): Claude accounts
// are single-holder, Codex accounts may be held by any number of devices at
// once. The whole thing runs inside a transaction so two agents racing to
// acquire the same exclusive account can't both succeed.
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

	// Close the lease_events segments of any rows we're about to sweep,
	// bounding each at its own expires_at (the device couldn't have used
	// the account past that). Must run before the DELETE, while expires_at
	// is still readable.
	if err := closeExpiredLeaseEventsTx(ctx, tx, now.UnixMilli()); err != nil {
		return Lease{}, err
	}
	// Sweep stale rows so the unique index doesn't reject our insert
	// because of a long-expired row that nobody released.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM leases WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return Lease{}, err
	}

	// Shared providers (Codex) allow several devices on one account, so the
	// existing-lease lookup is scoped to this device: another device's row is
	// not a conflict, it's a co-tenant.
	shared, err := accountSharesTx(ctx, tx, accountID)
	if err != nil {
		return Lease{}, err
	}
	lookup := `SELECT id, device_id, acquired_at FROM leases WHERE account_id = ?`
	args := []any{accountID}
	if shared {
		lookup += ` AND device_id = ?`
		args = append(args, deviceID)
	}
	var existingID, existingDeviceID string
	var existingAcquiredAt int64
	err = tx.QueryRowContext(ctx, lookup, args...).
		Scan(&existingID, &existingDeviceID, &existingAcquiredAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Free — insert the lease and open its attribution segment.
		// last_active_at = now: a fresh acquire is by definition active, so the
		// account can't be immediately reclaimed from the new holder.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at, last_active_at, shared)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			newID, accountID, deviceID, now.UnixMilli(), expiresAt, now.UnixMilli(), boolToInt(shared)); err != nil {
			return Lease{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO lease_events (lease_id, account_id, device_id, started_at, ended_at)
			 VALUES (?, ?, ?, ?, 0)`,
			newID, accountID, deviceID, now.UnixMilli()); err != nil {
			return Lease{}, err
		}
		if err := releaseOtherDeviceProviderLeasesTx(ctx, tx, deviceID, accountID, now.UnixMilli()); err != nil {
			return Lease{}, err
		}
		if err := tx.Commit(); err != nil {
			return Lease{}, err
		}
		return Lease{ID: newID, AccountID: accountID, DeviceID: deviceID,
			AcquiredAt: now.UnixMilli(), ExpiresAt: expiresAt, LastActiveAt: now.UnixMilli()}, nil
	case err != nil:
		return Lease{}, err
	}
	if existingDeviceID != deviceID {
		return Lease{}, ErrLeaseLocked
	}
	// Same device — renew in place. Re-stamp last_active_at: a device that
	// re-acquires (rather than renews) its own account is going through the
	// active-selection path, so treat it as freshly active.
	if _, err := tx.ExecContext(ctx,
		`UPDATE leases SET expires_at = ?, last_active_at = ?, shared = ? WHERE id = ?`,
		expiresAt, now.UnixMilli(), boolToInt(shared), existingID); err != nil {
		return Lease{}, err
	}
	// Backfill an attribution segment for a lease that has none — e.g. one
	// first acquired before lease_events existed, then kept alive purely by
	// renewals (the renew path otherwise never opens a segment, so the held
	// account would show as unattributed forever). Anchored at the lease's
	// original acquired_at so the full held span becomes attributable.
	if err := ensureOpenLeaseEventTx(ctx, tx, existingID, accountID, deviceID, existingAcquiredAt); err != nil {
		return Lease{}, err
	}
	if err := releaseOtherDeviceProviderLeasesTx(ctx, tx, deviceID, accountID, now.UnixMilli()); err != nil {
		return Lease{}, err
	}
	if err := tx.Commit(); err != nil {
		return Lease{}, err
	}
	return Lease{ID: existingID, AccountID: accountID, DeviceID: deviceID,
		AcquiredAt: now.UnixMilli(), ExpiresAt: expiresAt, LastActiveAt: now.UnixMilli()}, nil
}

// ProviderSharesAccounts reports whether one account of this provider may be
// held by several devices at the same time. Codex (ChatGPT) plans are not
// single-session, so pinning one account to one device only shrinks the usable
// pool; Claude stays exclusive because two agents injecting the same OAuth
// credential fight over the same rate-limit window and session state.
func ProviderSharesAccounts(provider string) bool {
	return provider == ProviderCodex
}

// accountSharesTx resolves ProviderSharesAccounts for accountID from inside a
// transaction. An unknown account (no row) is treated as exclusive — the
// conservative default, matching the pre-sharing behaviour.
func accountSharesTx(ctx context.Context, tx *sql.Tx, accountID int64) (bool, error) {
	var provider string
	err := tx.QueryRowContext(ctx, `SELECT provider FROM accounts WHERE id = ?`, accountID).Scan(&provider)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ProviderSharesAccounts(provider), nil
}

// RenewLease bumps a known lease's TTL. Returns ErrNotFound when the
// lease has expired or been released — caller is expected to re-acquire.
//
// idleFor is how long the holding agent has gone without real local Claude
// Code activity (0 == active right now). It's recorded as
// last_active_at = now - idleFor so the vault can tell a live-but-idle lease
// apart from a live-and-in-use one for idle-reclaim. A negative idleFor
// (clock skew) is clamped to 0.
func (s *Store) RenewLease(ctx context.Context, leaseID string, ttl, idleFor time.Duration) (Lease, error) {
	if leaseID == "" || ttl <= 0 {
		return Lease{}, fmt.Errorf("RenewLease: id and ttl required")
	}
	if idleFor < 0 {
		idleFor = 0
	}
	now := time.Now()
	expiresAt := now.Add(ttl).UnixMilli()
	lastActiveAt := now.Add(-idleFor).UnixMilli()
	res, err := s.db.ExecContext(ctx,
		`UPDATE leases SET expires_at = ?, last_active_at = ?
		   WHERE id = ? AND expires_at > ?`,
		expiresAt, lastActiveAt, leaseID, now.UnixMilli())
	if err != nil {
		return Lease{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Lease{}, ErrNotFound
	}
	var l Lease
	err = s.db.QueryRowContext(ctx,
		`SELECT id, account_id, device_id, acquired_at, expires_at, last_active_at
		   FROM leases WHERE id = ?`, leaseID).
		Scan(&l.ID, &l.AccountID, &l.DeviceID, &l.AcquiredAt, &l.ExpiresAt, &l.LastActiveAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, ErrNotFound
	}
	if err != nil {
		return Lease{}, err
	}
	// Backfill a missing attribution segment (same rationale as the
	// AcquireLease renew-in-place path): a lease kept alive purely via
	// RenewLease never opens a segment otherwise. Anchored at acquired_at.
	if err := ensureOpenLeaseEventTx(ctx, s.db, l.ID, l.AccountID, l.DeviceID, l.AcquiredAt); err != nil {
		return Lease{}, err
	}
	return l, nil
}

// ReleaseLease removes the lease early. Idempotent — releasing an unknown
// lease is not an error. Closes the lease's open attribution segment at the
// release instant so per-device usage replay stops counting it.
func (s *Store) ReleaseLease(ctx context.Context, leaseID string) error {
	if leaseID == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE lease_events SET ended_at = ? WHERE lease_id = ? AND ended_at = 0`,
		time.Now().UnixMilli(), leaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE id = ?`, leaseID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReleaseDeviceLeases releases every lease currently held by deviceID,
// closing each lease's open attribution segment at the release instant.
// Used when an admin suspends a device: the accounts it was holding return
// to the pool immediately instead of waiting for TTL expiry. Idempotent —
// a device with no live lease is a no-op. Returns the number of leases
// released so the caller can log/report.
func (s *Store) ReleaseDeviceLeases(ctx context.Context, deviceID string) (int, error) {
	if deviceID == "" {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`UPDATE lease_events SET ended_at = ?
		   WHERE ended_at = 0
		     AND lease_id IN (SELECT id FROM leases WHERE device_id = ?)`,
		now, deviceID); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE device_id = ?`, deviceID)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
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
	// Lease is the representative holder — the longest-held one — kept for the
	// single-holder surfaces that predate shared leases. Nil when unheld.
	Lease *LeaseInfo
	// Leases is every live holder, oldest first. Length > 1 only for shared
	// providers (Codex), where several devices co-hold one account.
	Leases []LeaseInfo
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
	// A shared (Codex) account can have several live leases, so this join fans
	// out into one row per holder. The scan below collapses them back into a
	// single AccountWithLease carrying every holder — ordering by acquired_at
	// within an account both groups the duplicates and makes the first holder
	// (the longest-held one) the representative Lease.
	q := `SELECT ` + qualifiedAccountColumns + `,
	             leases.device_id, leases.acquired_at, leases.expires_at,
	             COALESCE(NULLIF(devices.name, ''), devices.hostname, '') AS device_name
	        FROM accounts
	        LEFT JOIN leases  ON leases.account_id = accounts.id AND leases.expires_at > ?
	        LEFT JOIN devices ON devices.id        = leases.device_id
	       ORDER BY accounts.id, leases.acquired_at ASC, leases.id ASC`
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
			&a.ID, &a.Provider, &a.Name, &a.AccessToken, &a.RefreshToken, &a.ExpiresAt,
			&a.Scopes, &a.SubscriptionType,
			&a.OrganizationUUID, &a.Status, &a.LastUsedAt,
			&a.CreatedAt, &a.UpdatedAt,
			&a.Email, &a.FullName, &a.OrganizationName, &a.Plan,
			&a.FiveHourUtil, &a.FiveHourResetsAt,
			&a.SevenDayUtil, &a.SevenDayResetsAt,
			&a.SevenDaySonnetUtil, &a.SevenDaySonnetResetsAt,
			&a.SevenDayScopedLabel,
			&a.UsageFetchedAt,
			&a.FiveHourThreshold, &a.SevenDayThreshold, &a.SevenDaySonnetThreshold,
			&a.AccountUUID, &a.RateLimitTier, &a.CredentialJSON, &a.PinnedDeviceID,
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
		// Same account as the previous row → another holder of a shared
		// account, not a new account.
		if n := len(out); n > 0 && out[n-1].ID == a.ID {
			if lease != nil {
				out[n-1].Leases = append(out[n-1].Leases, *lease)
			}
			continue
		}
		row := AccountWithLease{Account: a, Lease: lease}
		if lease != nil {
			row.Leases = []LeaseInfo{*lease}
		}
		out = append(out, row)
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
accounts.id, accounts.provider, accounts.name, accounts.access_token, accounts.refresh_token, accounts.expires_at, accounts.scopes,
accounts.subscription_type, accounts.organization_uuid, accounts.status,
accounts.last_used_at, accounts.created_at, accounts.updated_at,
accounts.email, accounts.full_name, accounts.organization_name, accounts.plan,
accounts.five_hour_util, accounts.five_hour_resets_at,
accounts.seven_day_util, accounts.seven_day_resets_at,
accounts.seven_day_sonnet_util, accounts.seven_day_sonnet_resets_at,
accounts.seven_day_scoped_label,
accounts.usage_fetched_at,
accounts.five_hour_threshold, accounts.seven_day_threshold, accounts.seven_day_sonnet_threshold,
accounts.account_uuid, accounts.rate_limit_tier, accounts.credential_json, accounts.pinned_device_id`

// ActiveLeaseCounts returns how many devices currently hold a live lease on
// each account, keyed by account id. Accounts with no live lease are absent
// (a nil/missing key reads as 0). Feeds the selector's load balancing for
// shared providers, where several devices may hold the same account and the
// picker should prefer the least-crowded one.
//
// excludeDeviceID drops that device's own leases from the tally — the picking
// device must not count as load against the account it is already on, or every
// reconcile tick would bounce it onto a different account. "" counts everyone.
func (s *Store) ActiveLeaseCounts(ctx context.Context, excludeDeviceID string) (map[int64]int, error) {
	q := `SELECT account_id, COUNT(*) FROM leases WHERE expires_at > ?`
	args := []any{time.Now().UnixMilli()}
	if excludeDeviceID != "" {
		q += ` AND device_id != ?`
		args = append(args, excludeDeviceID)
	}
	q += ` GROUP BY account_id`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]int)
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

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

// IsAccountLeasedByOtherActive reports whether accountID has a live lease held
// by a device OTHER than deviceID that is NOT idle-reclaimable — i.e. the
// holder reported real activity within the last idleThresholdMs. Used as the
// second-pass Pick filter for idle-reclaim: an account whose only foreign lease
// is idle-beyond-threshold returns false here (not excluded), so the selector
// may hand it to an active, pool-starved device. A missing threshold
// (idleThresholdMs <= 0) collapses to IsAccountLeasedByOther (never reclaim).
func (s *Store) IsAccountLeasedByOtherActive(accountID int64, deviceID string, idleThresholdMs int64) bool {
	now := time.Now().UnixMilli()
	if idleThresholdMs <= 0 {
		return s.IsAccountLeasedByOther(accountID, deviceID)
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT 1 FROM leases
		   WHERE account_id = ? AND device_id != ? AND expires_at > ?
		     AND last_active_at > ?
		   LIMIT 1`,
		accountID, deviceID, now, now-idleThresholdMs).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// ReclaimIdleForeignLease releases (deletes + closes the attribution segment of)
// a live lease on accountID held by a device OTHER than deviceID whose holder
// has been idle for at least idleThresholdMs, freeing the account so the caller
// can AcquireLease it. Returns true when a lease was reclaimed, false when there
// was nothing eligible to reclaim (no foreign lease, or it's still active).
// Idempotent and race-tolerant: if the row vanishes between select and delete,
// it simply reports false. The caller's subsequent AcquireLease is the
// authoritative take-over — a lost race there surfaces as ErrLeaseLocked.
func (s *Store) ReclaimIdleForeignLease(ctx context.Context, accountID int64, deviceID string, idleThresholdMs int64) (bool, error) {
	if idleThresholdMs <= 0 {
		return false, nil
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var leaseID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM leases
		   WHERE account_id = ? AND device_id != ? AND expires_at > ?
		     AND last_active_at <= ?
		   LIMIT 1`,
		accountID, deviceID, now, now-idleThresholdMs).Scan(&leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Close the attribution segment at now (mirrors ReleaseLease) so per-device
	// usage replay stops counting the reclaimed hold, then drop the row.
	if _, err := tx.ExecContext(ctx,
		`UPDATE lease_events SET ended_at = ? WHERE lease_id = ? AND ended_at = 0`,
		now, leaseID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM leases WHERE id = ?`, leaseID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// SweepLeases deletes expired rows so the leases_account_id_uniq index
// stays satisfiable for the next AcquireLease attempt. Run on a goroutine
// timer (typically every 30s). Closes each swept lease's open attribution
// segment at its expires_at before deleting, so the lease_events history
// outlives the leases row.
func (s *Store) SweepLeases(ctx context.Context) error {
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := closeExpiredLeaseEventsTx(ctx, tx, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM leases WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	return tx.Commit()
}

// closeExpiredLeaseEventsTx stamps ended_at on every still-open lease_events
// row whose lease has expired (expires_at <= cutoff), bounding the segment at
// the lease's own expires_at — a device cannot have used the account past the
// moment its lease lapsed. Shared by SweepLeases and AcquireLease so both
// expiry paths produce identical history. Leases already gone from the table
// (released early) are untouched: ReleaseLease closed their segment itself.
func closeExpiredLeaseEventsTx(ctx context.Context, tx *sql.Tx, cutoff int64) error {
	_, err := tx.ExecContext(ctx, `
UPDATE lease_events
   SET ended_at = (SELECT leases.expires_at FROM leases WHERE leases.id = lease_events.lease_id)
 WHERE ended_at = 0
   AND lease_id IN (SELECT id FROM leases WHERE expires_at <= ?)`, cutoff)
	return err
}

// releaseOtherDeviceProviderLeasesTx enforces the one-live-lease-per-(device,
// provider) invariant from inside AcquireLease's transaction: it releases every
// OTHER lease held by deviceID whose account shares keepAccountID's provider,
// closing each released lease's open attribution segment at `now` first (same
// contract as ReleaseLease). keepAccountID — the account this AcquireLease call
// is inserting/renewing — is excluded, so a renew of the held account is never a
// self-eviction.
//
// Why this exists: the coordinator's release-old-after-acquire dedup is
// per-process (it only tracks its own currentLeaseID). When one host runs two
// agent processes sharing a pair-assigned device_id, the processes can't see
// each other's leases, so the same device_id ends up holding two accounts of the
// same provider at once (observed 2026-07-20: device "loki" held cws_pool_02 and
// cws_pool_05, both Claude, both actively renewed). Enforcing it in SQL closes
// that gap regardless of how many processes share the id.
//
// Scoped to a single provider on purpose: a device legitimately managing Claude
// AND Codex simultaneously (PickProviderForDevice) must keep both leases, so
// acquiring a Claude account must not disturb that device's Codex lease.
func releaseOtherDeviceProviderLeasesTx(ctx context.Context, tx *sql.Tx, deviceID string, keepAccountID int64, now int64) error {
	// Close attribution segments before the DELETE, while the rows still exist.
	if _, err := tx.ExecContext(ctx, `
UPDATE lease_events SET ended_at = ?
 WHERE ended_at = 0
   AND lease_id IN (
     SELECT l.id FROM leases l
       JOIN accounts a  ON a.id = l.account_id
       JOIN accounts ka ON ka.id = ?
      WHERE l.device_id = ?
        AND l.account_id <> ?
        AND a.provider = ka.provider
   )`, now, keepAccountID, deviceID, keepAccountID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `
DELETE FROM leases
 WHERE device_id = ?
   AND account_id <> ?
   AND account_id IN (
     SELECT a.id FROM accounts a
       JOIN accounts ka ON ka.id = ?
      WHERE a.provider = ka.provider
   )`, deviceID, keepAccountID, keepAccountID)
	return err
}

// execer is the subset of *sql.DB / *sql.Tx that ensureOpenLeaseEventTx needs,
// so the helper works both inside AcquireLease's transaction and on the bare
// db from RenewLease.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureOpenLeaseEventTx opens an attribution segment for leaseID only if it
// has no currently-open one. Idempotent: the conditional INSERT no-ops when an
// open segment already exists, so it is safe to call on every renew. startedAt
// should be the lease's original acquired_at so a backfilled segment covers the
// full held span. This closes the gap where a lease kept alive purely by
// renewals (never re-inserted) would otherwise never get a segment — the cause
// of an actively-held account rendering as 100% unattributed.
func ensureOpenLeaseEventTx(ctx context.Context, ex execer, leaseID string, accountID int64, deviceID string, startedAt int64) error {
	_, err := ex.ExecContext(ctx, `
INSERT INTO lease_events (lease_id, account_id, device_id, started_at, ended_at)
SELECT ?, ?, ?, ?, 0
 WHERE NOT EXISTS (SELECT 1 FROM lease_events WHERE lease_id = ? AND ended_at = 0)`,
		leaseID, accountID, deviceID, startedAt, leaseID)
	return err
}

// LeaseEvent is one (device held account) segment from lease_events. An open
// segment (still-live lease) has EndedAt == 0; attribution code treats that as
// "ongoing, cap at now". Returned by LeaseEventsForAccountSince ordered by
// started_at ascending.
type LeaseEvent struct {
	LeaseID   string
	AccountID int64
	DeviceID  string
	StartedAt int64
	EndedAt   int64 // 0 == still open
}

// LeaseEventsForAccountSince returns every lease segment for accountID that
// overlaps [since, ∞): either still open (ended_at == 0) or ended at/after
// `since`. Ordered by started_at ascending so attribution can walk them in
// time order. Used by the per-device quota attribution computation.
func (s *Store) LeaseEventsForAccountSince(ctx context.Context, accountID, since int64) ([]LeaseEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT lease_id, account_id, device_id, started_at, ended_at
  FROM lease_events
 WHERE account_id = ?
   AND (ended_at = 0 OR ended_at >= ?)
 ORDER BY started_at ASC`, accountID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeaseEvent
	for rows.Next() {
		var e LeaseEvent
		if err := rows.Scan(&e.LeaseID, &e.AccountID, &e.DeviceID, &e.StartedAt, &e.EndedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
