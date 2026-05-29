package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestAttributionEndpoint exercises GET /api/accounts/{id}/attribution end to
// end: a device holds the account across a rising usage delta, so the response
// must attribute points to that device (with its display name) and report the
// sample count. The deep attribution math is covered in the store package; this
// asserts the HTTP envelope, id plumbing, and device-name resolution.
func TestAttributionEndpoint(t *testing.T) {
	st, _ := newTestStore(t)
	ctx := context.Background()

	acc := &store.Account{Name: "alpha", AccessToken: "at", RefreshToken: "rt", Status: "active",
		ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, acc); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.InsertDevice(ctx, store.Device{ID: "dev-1", Name: "Boss-MBP", TokenHash: "h1"}); err != nil {
		t.Fatalf("insert device: %v", err)
	}

	// Open the lease segment, then lay two snapshots bracketing it so the
	// rising delta overlaps the held span. A short sleep makes the held
	// portion (and thus the device's attributed points) unambiguously > 0.
	acq := time.Now().UnixMilli()
	if _, err := st.AcquireLease(ctx, "lease-1", acc.ID, "dev-1", time.Minute); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := st.AppendUsageHistory(ctx, acc.ID, acq-200, 0, 0, 0); err != nil {
		t.Fatalf("snap1: %v", err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := st.AppendUsageHistory(ctx, acc.ID, time.Now().UnixMilli(), 25, 0, 0); err != nil {
		t.Fatalf("snap2: %v", err)
	}

	srv := New(st, nil, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/accounts/"+strconv.FormatInt(acc.ID, 10)+"/attribution", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body=%q", w.Code, w.Body.String())
	}

	var out attributionView
	if err := json.NewDecoder(w.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.AccountID != acc.ID {
		t.Fatalf("account_id=%d want %d", out.AccountID, acc.ID)
	}
	if out.SampleCount != 2 {
		t.Fatalf("sample_count=%d want 2", out.SampleCount)
	}
	if len(out.Devices) != 1 {
		t.Fatalf("devices=%d want 1: %+v", len(out.Devices), out.Devices)
	}
	d := out.Devices[0]
	if d.DeviceID != "dev-1" || d.DeviceName != "Boss-MBP" {
		t.Fatalf("device identity: %+v", d)
	}
	if d.FiveHour <= 0 {
		t.Fatalf("device five_hour=%.3f want > 0", d.FiveHour)
	}
	// Device + unattributed points reconstruct the observed delta (25).
	total := d.FiveHour
	if out.Unattributed != nil {
		total += out.Unattributed.FiveHour
	}
	if total < 24.99 || total > 25.01 {
		t.Fatalf("device+unattributed five_hour=%.3f want ~25", total)
	}
}

// TestAttributionEndpoint_BadID rejects a non-numeric id with 400.
func TestAttributionEndpoint_BadID(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	req := httptest.NewRequest(http.MethodGet, "/api/accounts/not-a-number/attribution", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d want 400", w.Code)
	}
}
