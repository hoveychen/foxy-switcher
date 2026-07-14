package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
)

// TestAPIDevicesSuspendResume drives the admin suspend/resume endpoints
// end-to-end: suspend flips disabled_at and frees the device's lease, the
// list reflects both, and resume clears disabled_at.
func TestAPIDevicesSuspendResume(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	if err := f.st.SetPasswordHash(ctx, hash); err != nil {
		t.Fatalf("set password: %v", err)
	}

	dev := store.Device{ID: vaultauth.NewID(), Name: "dev-a", TokenHash: vaultauth.HashToken(vaultauth.NewToken())}
	if err := f.st.InsertDevice(ctx, dev); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	acc := &store.Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt"}
	if err := f.st.Upsert(ctx, acc); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := f.st.AcquireLease(ctx, "lease-1", acc.ID, dev.ID, time.Minute); err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	jar := newCookieJar(t)
	postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "pw"}).Body.Close()

	// Suspend → 204.
	resp := postJSON(t, f.server.URL+"/admin/api/devices/suspend", jar, map[string]string{"id": dev.ID})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("suspend status: got %s want 204", resp.Status)
	}
	resp.Body.Close()

	// Lease freed immediately.
	active, err := f.st.ListActiveLeases(ctx)
	if err != nil {
		t.Fatalf("ListActiveLeases: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("after suspend %d live leases remain, want 0", len(active))
	}

	// List shows disabled_at != 0 and no current_lease.
	listResp := getWith(t, f.server.URL+"/admin/api/devices", jar)
	var out apiDevicesResp
	if err := json.NewDecoder(listResp.Body).Decode(&out); err != nil {
		t.Fatalf("decode devices: %v", err)
	}
	listResp.Body.Close()
	if len(out.Devices) != 1 {
		t.Fatalf("devices len = %d, want 1", len(out.Devices))
	}
	if out.Devices[0].DisabledAt == 0 {
		t.Error("suspended device DisabledAt = 0 in list, want non-zero")
	}
	if out.Devices[0].CurrentLease != nil {
		t.Error("suspended device still shows a current_lease")
	}

	// Resume → 204, disabled_at back to 0.
	resp = postJSON(t, f.server.URL+"/admin/api/devices/resume", jar, map[string]string{"id": dev.ID})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("resume status: got %s want 204", resp.Status)
	}
	resp.Body.Close()

	listResp = getWith(t, f.server.URL+"/admin/api/devices", jar)
	out = apiDevicesResp{}
	_ = json.NewDecoder(listResp.Body).Decode(&out)
	listResp.Body.Close()
	if out.Devices[0].DisabledAt != 0 {
		t.Errorf("resumed device DisabledAt = %d, want 0", out.Devices[0].DisabledAt)
	}
}

// TestAPIDevicesSuspend_NotFound → 404 for an unknown device id.
func TestAPIDevicesSuspend_NotFound(t *testing.T) {
	f := newAuthFixture(t)
	ctx := context.Background()
	hash, _ := vaultauth.HashPassword("pw")
	_ = f.st.SetPasswordHash(ctx, hash)

	jar := newCookieJar(t)
	postJSON(t, f.server.URL+"/admin/api/login", jar, map[string]string{"password": "pw"}).Body.Close()

	resp := postJSON(t, f.server.URL+"/admin/api/devices/suspend", jar, map[string]string{"id": "ghost"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("suspend ghost: got %s want 404", resp.Status)
	}
	resp.Body.Close()
}
