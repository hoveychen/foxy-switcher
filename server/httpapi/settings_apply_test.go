package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestApplyThresholdDefaults_OverwritesAllAccounts verifies the
// POST /api/settings/apply-thresholds wiring: it reads the persisted
// pool-wide defaults and writes them across every account (including a
// manually-tuned one), returning the affected-row count.
func TestApplyThresholdDefaults_OverwritesAllAccounts(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")
	ctx := context.Background()

	a1 := &store.Account{Name: "a", Email: "a@x", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	a2 := &store.Account{Name: "b", Email: "b@x", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a1); err != nil {
		t.Fatalf("upsert a1: %v", err)
	}
	if err := st.Upsert(ctx, a2); err != nil {
		t.Fatalf("upsert a2: %v", err)
	}
	if err := st.SetThresholds(ctx, a2.ID, 30, 40, 50); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}

	if _, err := st.SetSettings(ctx, store.Settings{
		UsagePollIntervalSec:           60,
		DefaultFiveHourThreshold:       88,
		DefaultSevenDayThreshold:       77,
		DefaultSevenDaySonnetThreshold: 66,
		RestoreNativeOnQuit:            true,
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/settings/apply-thresholds", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST: want 200, got %d body=%q", w.Code, w.Body.String())
	}
	var resp struct {
		Updated int64 `json:"updated"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Updated != 2 {
		t.Fatalf("updated: got %d want 2", resp.Updated)
	}

	for _, id := range []int64{a1.ID, a2.ID} {
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %d: %v", id, err)
		}
		if got.FiveHourThreshold != 88 || got.SevenDayThreshold != 77 || got.SevenDaySonnetThreshold != 66 {
			t.Fatalf("account %d not overwritten with defaults: got %v/%v/%v want 88/77/66",
				id, got.FiveHourThreshold, got.SevenDayThreshold, got.SevenDaySonnetThreshold)
		}
	}
}
