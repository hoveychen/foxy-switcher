package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestSetDeviceDisabled_Roundtrip verifies suspend stamps disabled_at and
// resume clears it, and that the flag survives the Find/List read paths.
func TestSetDeviceDisabled_Roundtrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if err := st.InsertDevice(ctx, Device{ID: "dev-1", Name: "MacA", TokenHash: "h1"}); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	// Fresh device is active.
	d, err := st.FindDeviceByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if d.DisabledAt != 0 {
		t.Fatalf("fresh device DisabledAt = %d, want 0", d.DisabledAt)
	}

	// Suspend → disabled_at non-zero.
	if err := st.SetDeviceDisabled(ctx, "dev-1", true); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	d, err = st.FindDeviceByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("Find after suspend: %v", err)
	}
	if d.DisabledAt == 0 {
		t.Fatal("after suspend DisabledAt = 0, want non-zero")
	}

	// ListDevices reflects the same flag.
	list, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(list) != 1 || list[0].DisabledAt == 0 {
		t.Fatalf("ListDevices DisabledAt = %d, want non-zero", list[0].DisabledAt)
	}

	// Resume → back to 0.
	if err := st.SetDeviceDisabled(ctx, "dev-1", false); err != nil {
		t.Fatalf("resume: %v", err)
	}
	d, err = st.FindDeviceByTokenHash(ctx, "h1")
	if err != nil {
		t.Fatalf("Find after resume: %v", err)
	}
	if d.DisabledAt != 0 {
		t.Fatalf("after resume DisabledAt = %d, want 0", d.DisabledAt)
	}
}

// TestSetDeviceDisabled_NotFound surfaces ErrNotFound for unknown ids so
// the handler can 404.
func TestSetDeviceDisabled_NotFound(t *testing.T) {
	st := openTempStore(t)
	if err := st.SetDeviceDisabled(context.Background(), "ghost", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetDeviceDisabled(ghost) = %v, want ErrNotFound", err)
	}
}

// TestReleaseDeviceLeases frees every lease held by a device (so its
// accounts return to the pool on suspend) while leaving other devices'
// leases untouched.
func TestReleaseDeviceLeases(t *testing.T) {
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

	if _, err := st.AcquireLease(ctx, "lease-a", alpha.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-b", bravo.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	if _, err := st.AcquireLease(ctx, "lease-c", charlie.ID, "dev-2", time.Minute); err != nil {
		t.Fatalf("acquire c: %v", err)
	}

	n, err := st.ReleaseDeviceLeases(ctx, "dev-1")
	if err != nil {
		t.Fatalf("ReleaseDeviceLeases: %v", err)
	}
	if n != 2 {
		t.Fatalf("released %d leases, want 2", n)
	}

	// dev-1's leases gone; dev-2's lease survives.
	active, err := st.ListActiveLeases(ctx)
	if err != nil {
		t.Fatalf("ListActiveLeases: %v", err)
	}
	if len(active) != 1 || active[0].DeviceID != "dev-2" {
		t.Fatalf("remaining leases = %+v, want only dev-2", active)
	}

	// Idempotent: releasing again frees nothing.
	if n, err := st.ReleaseDeviceLeases(ctx, "dev-1"); err != nil || n != 0 {
		t.Fatalf("second release = (%d, %v), want (0, nil)", n, err)
	}
}
