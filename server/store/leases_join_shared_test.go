package store

import (
	"context"
	"testing"
	"time"
)

// TestListAccountsWithLeases_SharedAccountCollapses guards the accounts list
// against the JOIN fan-out that shared leases introduce: a Codex account held
// by two devices must still be ONE row (otherwise the UI renders the account
// twice), carrying both holders.
func TestListAccountsWithLeases_SharedAccountCollapses(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	acc := &Account{Provider: ProviderCodex, Name: "codex", AccessToken: "at"}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	for _, d := range []struct{ id, name string }{{"dev-1", "Alpha"}, {"dev-2", "Beta"}} {
		if err := st.InsertDevice(ctx, Device{ID: d.id, Name: d.name, TokenHash: "h-" + d.id}); err != nil {
			t.Fatalf("insert device %s: %v", d.id, err)
		}
	}
	if _, err := st.AcquireLease(ctx, "l1", acc.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("acquire dev-1: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "l2", acc.ID, "dev-2", time.Minute); err != nil {
		t.Fatalf("acquire dev-2: %v", err)
	}

	rows, err := st.ListAccountsWithLeases(ctx)
	if err != nil {
		t.Fatalf("ListAccountsWithLeases: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (a shared account must not fan out)", len(rows))
	}
	got := rows[0]
	if len(got.Leases) != 2 {
		t.Fatalf("got %d holders, want 2", len(got.Leases))
	}
	if got.Lease == nil {
		t.Fatalf("representative Lease is nil despite two holders")
	}
	if got.Lease.DeviceID != got.Leases[0].DeviceID {
		t.Fatalf("representative lease %q is not the first holder %q", got.Lease.DeviceID, got.Leases[0].DeviceID)
	}
	names := map[string]bool{got.Leases[0].DeviceName: true, got.Leases[1].DeviceName: true}
	if !names["Alpha"] || !names["Beta"] {
		t.Fatalf("holder names = %v, want Alpha and Beta", names)
	}
}
