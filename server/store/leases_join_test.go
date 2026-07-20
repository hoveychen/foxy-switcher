package store

import (
	"context"
	"testing"
	"time"
)

// TestListAccountsWithLeases_Basic exercises the four states the join
// query must distinguish: account with no lease, account with an active
// lease (full LeaseInfo populated), account with an expired lease
// (LeaseInfo nil, since expired rows are filtered), and lease held by a
// device whose .Name is empty (DeviceName falls back to hostname so the
// UI never renders a blank "in use by …" badge).
func TestListAccountsWithLeases_Basic(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	mkAcc := func(name string) *Account {
		a := &Account{Name: name, AccessToken: "at-" + name, RefreshToken: "rt-" + name}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return a
	}
	alpha := mkAcc("alpha")
	bravo := mkAcc("bravo")
	charlie := mkAcc("charlie")
	delta := mkAcc("delta")

	// dev-1: normal name → DeviceName == "Boss-MBP"
	if err := st.InsertDevice(ctx, Device{
		ID: "dev-1", Name: "Boss-MBP", TokenHash: "h1", Hostname: "boss-mbp.lan",
	}); err != nil {
		t.Fatalf("ins dev1: %v", err)
	}
	// dev-2: empty .Name → DeviceName falls back to hostname.
	// InsertDevice rejects empty name, so go via raw SQL.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at, last_seen_at, hostname)
		 VALUES (?, '', ?, ?, 0, ?)`,
		"dev-2", "h2", time.Now().UnixMilli(), "shy-laptop.local"); err != nil {
		t.Fatalf("raw ins dev2: %v", err)
	}

	// Active leases:
	if _, err := st.AcquireLease(ctx, "lease-b", bravo.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("acq b: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-c", charlie.ID, "dev-2", time.Minute); err != nil {
		t.Fatalf("acq c: %v", err)
	}
	// Expired lease on delta — must be filtered out (Lease == nil).
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO leases (id, account_id, device_id, acquired_at, expires_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"lease-d", delta.ID, "dev-1",
		time.Now().Add(-2*time.Minute).UnixMilli(),
		time.Now().Add(-time.Minute).UnixMilli()); err != nil {
		t.Fatalf("raw ins expired lease: %v", err)
	}

	rows, err := st.ListAccountsWithLeases(ctx)
	if err != nil {
		t.Fatalf("ListAccountsWithLeases: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("got %d rows, want 4", len(rows))
	}

	byName := make(map[string]AccountWithLease, len(rows))
	for _, r := range rows {
		byName[r.Name] = r
	}
	_ = alpha
	if got := byName["alpha"]; got.Lease != nil {
		t.Fatalf("alpha: expected nil lease, got %+v", got.Lease)
	}
	if got := byName["bravo"]; got.Lease == nil ||
		got.Lease.DeviceID != "dev-1" || got.Lease.DeviceName != "Boss-MBP" {
		t.Fatalf("bravo: expected dev-1/Boss-MBP, got %+v", got.Lease)
	}
	if got := byName["charlie"]; got.Lease == nil ||
		got.Lease.DeviceID != "dev-2" || got.Lease.DeviceName != "shy-laptop.local" {
		t.Fatalf("charlie: expected dev-2/shy-laptop.local hostname fallback, got %+v", got.Lease)
	}
	if got := byName["delta"]; got.Lease != nil {
		t.Fatalf("delta: expected nil (expired lease filtered), got %+v", got.Lease)
	}

	// Sanity: AcquiredAt and ExpiresAt are non-zero on populated leases.
	if got := byName["bravo"]; got.Lease.AcquiredAt == 0 || got.Lease.ExpiresAt == 0 {
		t.Fatalf("bravo: expected non-zero timestamps, got %+v", got.Lease)
	}
}

// TestListActiveLeases covers the lighter-weight query used by the
// dashboard in_use[] list: returns every live lease joined with the
// holding device's display name; expired rows are filtered; ordered
// deterministically by acquired_at so multi-device dashboards don't
// flap on refresh.
func TestListActiveLeases(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "alpha", AccessToken: "at-a", RefreshToken: "rt-a"}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatal(err)
	}
	b := &Account{Name: "bravo", AccessToken: "at-b", RefreshToken: "rt-b"}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatal(err)
	}

	if err := st.InsertDevice(ctx, Device{ID: "dev-1", Name: "MacA", TokenHash: "h1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertDevice(ctx, Device{ID: "dev-2", Name: "MacB", TokenHash: "h2"}); err != nil {
		t.Fatal(err)
	}

	// Empty store → empty slice (not nil-vs-empty fuss; len==0 either way).
	leases, err := st.ListActiveLeases(ctx)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if len(leases) != 0 {
		t.Fatalf("empty store: got %d leases", len(leases))
	}

	if _, err := st.AcquireLease(ctx, "lease-a", a.ID, "dev-1", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Sleep to make acquired_at strictly monotonic across the two leases
	// so the ASC ordering check below is unambiguous.
	time.Sleep(5 * time.Millisecond)
	if _, err := st.AcquireLease(ctx, "lease-b", b.ID, "dev-2", time.Minute); err != nil {
		t.Fatal(err)
	}

	leases, err = st.ListActiveLeases(ctx)
	if err != nil {
		t.Fatalf("two: %v", err)
	}
	if len(leases) != 2 {
		t.Fatalf("got %d want 2", len(leases))
	}
	if leases[0].AccountID != a.ID || leases[0].DeviceID != "dev-1" || leases[0].DeviceName != "MacA" {
		t.Fatalf("first lease (acquired earliest): %+v", leases[0])
	}
	if leases[1].AccountID != b.ID || leases[1].DeviceID != "dev-2" || leases[1].DeviceName != "MacB" {
		t.Fatalf("second lease: %+v", leases[1])
	}

	// Expire lease-a → only lease-b should remain.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE leases SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).UnixMilli(), "lease-a"); err != nil {
		t.Fatal(err)
	}
	leases, err = st.ListActiveLeases(ctx)
	if err != nil {
		t.Fatalf("after expire: %v", err)
	}
	if len(leases) != 1 || leases[0].DeviceID != "dev-2" {
		t.Fatalf("after expire: %+v", leases)
	}
}

// TestListAccountsWithLeasesPopulatesProviderAndCredential guards the
// GET /api/accounts display path. The join query built on
// qualifiedAccountColumns must select the same columns as selectColumns —
// crucially `provider` and `credential_json`. Omitting `provider` makes every
// account come back with Provider=="" to the web UI, which silently breaks the
// Claude/Codex filter (nothing matches) and makes Codex accounts render with
// Claude usage labels. This account is stored as Codex, so the round-trip must
// preserve that.
func TestListAccountsWithLeasesPopulatesProviderAndCredential(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	codex := &Account{
		Provider:       ProviderCodex,
		Name:           "Harry C",
		Email:          "harry@example.com",
		AccessToken:    "at",
		RefreshToken:   "rt",
		ExpiresAt:      1,
		CredentialJSON: `{"tokens":{"access_token":"x"}}`,
	}
	if err := st.Upsert(ctx, codex); err != nil {
		t.Fatalf("upsert codex: %v", err)
	}

	rows, err := st.ListAccountsWithLeases(ctx)
	if err != nil {
		t.Fatalf("ListAccountsWithLeases: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	got := rows[0].Account
	if got.Provider != ProviderCodex {
		t.Fatalf("Provider = %q, want %q — the join query must select the provider column", got.Provider, ProviderCodex)
	}
	if got.CredentialJSON == "" {
		t.Fatalf("CredentialJSON empty — the join query must select the credential_json column")
	}
}
