package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquireLease_ExclusiveForEveryProvider pins the exclusivity rule: one
// device per account, Codex included. Codex was briefly shareable across
// devices; that is gone, so a second device must be rejected exactly the way
// it is on a Claude account.
func TestAcquireLease_ExclusiveForEveryProvider(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	mkAcc := func(name, provider string) *Account {
		a := &Account{Provider: provider, Name: name, AccessToken: "at-" + name, RefreshToken: "rt-" + name}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return a
	}

	for _, provider := range []string{ProviderCodex, ProviderClaude} {
		acc := mkAcc(provider+"-acct", provider)
		if _, err := st.AcquireLease(ctx, provider+"-dev1", acc.ID, "dev1", time.Minute); err != nil {
			t.Fatalf("acquire %s on dev1: %v", provider, err)
		}
		if _, err := st.AcquireLease(ctx, provider+"-dev2", acc.ID, "dev2", time.Minute); !errors.Is(err, ErrLeaseLocked) {
			t.Fatalf("acquire %s on dev2 = %v, want ErrLeaseLocked", provider, err)
		}
		if n := countLeases(t, st, acc.ID); n != 1 {
			t.Fatalf("%s live leases = %d, want 1", provider, n)
		}
		// The holder re-acquiring renews its own row in place.
		l, err := st.AcquireLease(ctx, provider+"-dev1-again", acc.ID, "dev1", time.Minute)
		if err != nil {
			t.Fatalf("re-acquire %s on dev1: %v", provider, err)
		}
		if l.ID != provider+"-dev1" {
			t.Fatalf("re-acquire returned lease id %q, want the device's existing lease", l.ID)
		}
		if n := countOpenSegments(t, st, acc.ID); n != 1 {
			t.Fatalf("open lease_events for %s = %d, want 1", provider, n)
		}
	}
}

// TestExclusiveLeaseMigrationCollapsesSharedRows covers the downgrade path: a
// database written by the shared-Codex build can hold several live leases on
// one account and carries the partial unique index that allowed them.
// Reopening must keep the oldest holder, drop the rest, close their dangling
// attribution segments, and reinstate the full unique index so a second device
// is refused again.
func TestExclusiveLeaseMigrationCollapsesSharedRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	acc := &Account{Provider: ProviderCodex, Name: "codex", AccessToken: "at"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Rewind to the shared-era schema (the `shared` column plus the partial
	// unique index that let several devices onto one account) and plant two
	// live holders the way that build would have.
	for _, stmt := range []string{
		`ALTER TABLE leases ADD COLUMN shared INTEGER NOT NULL DEFAULT 0`,
		`DROP INDEX IF EXISTS leases_account_id_uniq`,
		`CREATE UNIQUE INDEX leases_exclusive_account_uniq ON leases (account_id) WHERE shared = 0`,
		`CREATE UNIQUE INDEX leases_account_device_uniq ON leases (account_id, device_id)`,
	} {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	now := time.Now().UnixMilli()
	expires := now + time.Minute.Milliseconds()
	for i, d := range []struct {
		lease, device string
		acquiredAt    int64
	}{{"l-old", "dev1", now - 10_000}, {"l-new", "dev2", now}} {
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at, last_active_at, shared)
			 VALUES (?, ?, ?, ?, ?, ?, 1)`,
			d.lease, acc.ID, d.device, d.acquiredAt, expires, d.acquiredAt); err != nil {
			t.Fatalf("plant lease %d: %v", i, err)
		}
		if _, err := st.db.ExecContext(ctx,
			`INSERT INTO lease_events (lease_id, account_id, device_id, started_at, ended_at)
			 VALUES (?, ?, ?, ?, 0)`,
			d.lease, acc.ID, d.device, d.acquiredAt); err != nil {
			t.Fatalf("plant lease_event %d: %v", i, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	if n := countLeases(t, st2, acc.ID); n != 1 {
		t.Fatalf("live leases after migration = %d, want 1", n)
	}
	var survivor string
	if err := st2.db.QueryRowContext(ctx,
		`SELECT device_id FROM leases WHERE account_id = ?`, acc.ID).Scan(&survivor); err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	if survivor != "dev1" {
		t.Fatalf("survivor = %q, want dev1 (the oldest holder)", survivor)
	}
	// dev2's attribution segment must be closed, not left open forever.
	if n := countOpenSegments(t, st2, acc.ID); n != 1 {
		t.Fatalf("open lease_events after migration = %d, want 1 (only the survivor's)", n)
	}
	// And the account is exclusive again — in the code path and, underneath it,
	// in the index that backstops a racing writer.
	if _, err := st2.AcquireLease(ctx, "l3", acc.ID, "dev3", time.Minute); !errors.Is(err, ErrLeaseLocked) {
		t.Fatalf("acquire on dev3 = %v, want ErrLeaseLocked", err)
	}
	var idx string
	if err := st2.db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'index' AND name = 'leases_account_id_uniq'`).
		Scan(&idx); err != nil {
		t.Fatalf("leases_account_id_uniq not restored: %v", err)
	}
}

func countLeases(t *testing.T, st *Store, accountID int64) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM leases WHERE account_id = ? AND expires_at > ?`,
		accountID, time.Now().UnixMilli()).Scan(&n); err != nil {
		t.Fatalf("count leases: %v", err)
	}
	return n
}

func countOpenSegments(t *testing.T, st *Store, accountID int64) int {
	t.Helper()
	var n int
	if err := st.db.QueryRow(
		`SELECT COUNT(*) FROM lease_events WHERE account_id = ? AND ended_at = 0`,
		accountID).Scan(&n); err != nil {
		t.Fatalf("count open segments: %v", err)
	}
	return n
}
