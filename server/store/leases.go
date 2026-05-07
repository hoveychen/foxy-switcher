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

// SweepLeases deletes expired rows so the leases_account_id_uniq index
// stays satisfiable for the next AcquireLease attempt. Run on a goroutine
// timer (typically every 30s).
func (s *Store) SweepLeases(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM leases WHERE expires_at <= ?`, time.Now().UnixMilli())
	return err
}
