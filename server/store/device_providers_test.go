package store

import (
	"context"
	"testing"
	"time"
)

// TestDeviceProviderAllowlistRoundTrip covers the per-device provider
// allowlist: InsertDevice persists the chosen flags, the read paths surface
// them, SetDeviceProviders edits them, and DeviceAllowsProvider gates on them.
func TestDeviceProviderAllowlistRoundTrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.InsertDevice(ctx, Device{
		ID: "dev-both", Name: "Both", TokenHash: "h-both",
		AllowClaude: true, AllowCodex: true,
	}); err != nil {
		t.Fatalf("insert both: %v", err)
	}
	if err := st.InsertDevice(ctx, Device{
		ID: "dev-claude", Name: "ClaudeOnly", TokenHash: "h-claude",
		AllowClaude: true, AllowCodex: false,
	}); err != nil {
		t.Fatalf("insert claude-only: %v", err)
	}

	// Read back via the auth lookup.
	both, err := st.FindDeviceByTokenHash(ctx, "h-both")
	if err != nil || !both.AllowClaude || !both.AllowCodex {
		t.Fatalf("both device = %+v err=%v", both, err)
	}
	claudeOnly, err := st.FindDeviceByTokenHash(ctx, "h-claude")
	if err != nil || !claudeOnly.AllowClaude || claudeOnly.AllowCodex {
		t.Fatalf("claude-only device = %+v err=%v", claudeOnly, err)
	}

	// ListDevices surfaces the same flags.
	list, err := st.ListDevices(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v err=%v", list, err)
	}

	// DeviceAllowsProvider gates correctly.
	assertAllows(t, st, "dev-claude", ProviderClaude, true)
	assertAllows(t, st, "dev-claude", ProviderCodex, false)
	assertAllows(t, st, "dev-both", ProviderCodex, true)
	// A device id with no row is combined/local mode (generates an id but
	// never inserts a row) — deliberately un-gated, so it reads as allowed.
	assertAllows(t, st, "ghost", ProviderCodex, true)
	// An unknown provider string is denied even for a real device.
	if ok, err := st.DeviceAllowsProvider(ctx, "dev-both", "gemini"); err != nil || ok {
		t.Fatalf("unknown provider should be denied: ok=%v err=%v", ok, err)
	}

	// SetDeviceProviders edits the choice later (enable Codex on the claude-only device).
	if err := st.SetDeviceProviders(ctx, "dev-claude", true, true, false); err != nil {
		t.Fatalf("SetDeviceProviders: %v", err)
	}
	assertAllows(t, st, "dev-claude", ProviderCodex, true)
	if err := st.SetDeviceProviders(ctx, "ghost", true, true, false); err != ErrNotFound {
		t.Fatalf("SetDeviceProviders unknown = %v, want ErrNotFound", err)
	}
}

// TestDeviceAllowlistMigrationDefaultsClaudeOnly proves that a device row
// written WITHOUT the allow_* columns (i.e. a device that existed before this
// feature) reads back as claude-only — the column defaults 1/0 are the
// migration behaviour we promised (existing devices never auto-pick Codex).
func TestDeviceAllowlistMigrationDefaultsClaudeOnly(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	// Insert bypassing the allow_* columns, as a pre-feature row would exist.
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO devices (id, name, token_hash, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, 0)`,
		"legacy", "Legacy", "h-legacy", time.Now().UnixMilli()); err != nil {
		t.Fatalf("raw insert legacy device: %v", err)
	}
	d, err := st.FindDeviceByTokenHash(ctx, "h-legacy")
	if err != nil {
		t.Fatalf("find legacy: %v", err)
	}
	if !d.AllowClaude || d.AllowCodex || d.AllowOpenRouter {
		t.Fatalf("legacy device = claude:%v codex:%v openrouter:%v, want claude-only",
			d.AllowClaude, d.AllowCodex, d.AllowOpenRouter)
	}
}

// TestApprovePairingCarriesProviderChoice verifies the admin's provider choice
// at approval survives on the pairing row (later copied onto the device).
func TestApprovePairingCarriesProviderChoice(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.InsertPairing(ctx, Pairing{
		ClientNonce: "nonce-1", UserCode: "CODE-1", DeviceName: "Laptop",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("insert pairing: %v", err)
	}
	// Admin approves with both providers enabled.
	if err := st.ApprovePairing(ctx, "CODE-1", "dev-x", "tok-x", true, true, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	p, err := st.FindPairingByCode(ctx, "CODE-1")
	if err != nil {
		t.Fatalf("find pairing: %v", err)
	}
	if p.Status != PairingApproved || !p.AllowClaude || !p.AllowCodex || !p.AllowOpenRouter {
		t.Fatalf("approved pairing = %+v, want approved + all three providers", p)
	}
}

func assertAllows(t *testing.T, st *Store, deviceID, provider string, want bool) {
	t.Helper()
	got, err := st.DeviceAllowsProvider(context.Background(), deviceID, provider)
	if err != nil {
		t.Fatalf("DeviceAllowsProvider(%s,%s): %v", deviceID, provider, err)
	}
	if got != want {
		t.Fatalf("DeviceAllowsProvider(%s,%s) = %v, want %v", deviceID, provider, got, want)
	}
}
