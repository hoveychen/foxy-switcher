package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAcquireLease_CodexSharedAcrossDevices pins the Codex sharing rule: a
// Codex account may be held by several devices at once (ChatGPT plans are not
// single-session, so the exclusivity that Claude needs only shrinks the usable
// pool), while Claude stays strictly one-device-per-account.
func TestAcquireLease_CodexSharedAcrossDevices(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	mkAcc := func(name, provider string) *Account {
		a := &Account{Provider: provider, Name: name, AccessToken: "at-" + name, RefreshToken: "rt-" + name}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return a
	}
	codex := mkAcc("codex", ProviderCodex)
	claude := mkAcc("claude", ProviderClaude)

	if _, err := st.AcquireLease(ctx, "codex-dev1", codex.ID, "dev1", time.Minute); err != nil {
		t.Fatalf("acquire codex on dev1: %v", err)
	}
	// Second device on the SAME Codex account must succeed and coexist.
	if _, err := st.AcquireLease(ctx, "codex-dev2", codex.ID, "dev2", time.Minute); err != nil {
		t.Fatalf("acquire codex on dev2: %v (Codex accounts must be shareable)", err)
	}
	if n := countLeases(t, st, codex.ID); n != 2 {
		t.Fatalf("codex live leases = %d, want 2 (both devices hold it)", n)
	}
	// Re-acquiring on dev1 renews in place rather than adding a third row.
	l1, err := st.AcquireLease(ctx, "codex-dev1-again", codex.ID, "dev1", time.Minute)
	if err != nil {
		t.Fatalf("re-acquire codex on dev1: %v", err)
	}
	if l1.ID != "codex-dev1" {
		t.Fatalf("re-acquire returned lease id %q, want the device's existing lease %q", l1.ID, "codex-dev1")
	}
	if n := countLeases(t, st, codex.ID); n != 2 {
		t.Fatalf("codex live leases = %d after dev1 renew, want 2", n)
	}
	// Each holder gets exactly one open attribution segment.
	if n := countOpenSegments(t, st, codex.ID); n != 2 {
		t.Fatalf("open lease_events for codex = %d, want 2 (one per holder)", n)
	}
	// Releasing one holder leaves the other running.
	if err := st.ReleaseLease(ctx, "codex-dev2"); err != nil {
		t.Fatalf("release dev2: %v", err)
	}
	if n := countLeases(t, st, codex.ID); n != 1 {
		t.Fatalf("codex live leases = %d after dev2 release, want 1", n)
	}
	if !st.IsAccountLeasedByOther(codex.ID, "dev2") {
		t.Fatalf("dev1 lease invisible to dev2 after release of dev2's own lease")
	}

	// Claude keeps its exclusive semantics.
	if _, err := st.AcquireLease(ctx, "claude-dev1", claude.ID, "dev1", time.Minute); err != nil {
		t.Fatalf("acquire claude on dev1: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "claude-dev2", claude.ID, "dev2", time.Minute); !errors.Is(err, ErrLeaseLocked) {
		t.Fatalf("acquire claude on dev2 = %v, want ErrLeaseLocked", err)
	}
}

// TestActiveLeaseCounts reports how many devices currently hold each account —
// the input the selector's load balancing sorts on.
func TestActiveLeaseCounts(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	mk := func(name string) *Account {
		a := &Account{Provider: ProviderCodex, Name: name, AccessToken: "at-" + name}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return a
	}
	busy, quiet, free := mk("busy"), mk("quiet"), mk("free")

	for _, dev := range []string{"d1", "d2", "d3"} {
		if _, err := st.AcquireLease(ctx, "busy-"+dev, busy.ID, dev, time.Minute); err != nil {
			t.Fatalf("acquire busy on %s: %v", dev, err)
		}
	}
	if _, err := st.AcquireLease(ctx, "quiet-d4", quiet.ID, "d4", time.Minute); err != nil {
		t.Fatalf("acquire quiet: %v", err)
	}

	counts, err := st.ActiveLeaseCounts(ctx)
	if err != nil {
		t.Fatalf("ActiveLeaseCounts: %v", err)
	}
	if counts[busy.ID] != 3 {
		t.Fatalf("busy count = %d, want 3", counts[busy.ID])
	}
	if counts[quiet.ID] != 1 {
		t.Fatalf("quiet count = %d, want 1", counts[quiet.ID])
	}
	if counts[free.ID] != 0 {
		t.Fatalf("free count = %d, want 0 (absent from the map)", counts[free.ID])
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
