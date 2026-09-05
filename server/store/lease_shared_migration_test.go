package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestSharedLeaseMigrationDropsLegacyUniqueIndex covers the upgrade path: a
// database created before shared leases carries the FULL unique index
// leases_account_id_uniq on (account_id), which would reject a second device's
// Codex lease no matter what AcquireLease decides. Reopening the store must
// drop it and install the partial index instead.
func TestSharedLeaseMigrationDropsLegacyUniqueIndex(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Rewind to the pre-migration index layout.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS leases_exclusive_account_uniq`,
		`DROP INDEX IF EXISTS leases_account_device_uniq`,
		`CREATE UNIQUE INDEX leases_account_id_uniq ON leases (account_id)`,
	} {
		if _, err := st.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	acc := &Account{Provider: ProviderCodex, Name: "codex", AccessToken: "at"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()

	if _, err := st2.AcquireLease(ctx, "l1", acc.ID, "dev1", time.Minute); err != nil {
		t.Fatalf("acquire dev1 after migration: %v", err)
	}
	if _, err := st2.AcquireLease(ctx, "l2", acc.ID, "dev2", time.Minute); err != nil {
		t.Fatalf("acquire dev2 after migration: %v (legacy unique index still present?)", err)
	}
	if n := countLeases(t, st2, acc.ID); n != 2 {
		t.Fatalf("live leases = %d, want 2", n)
	}
}
