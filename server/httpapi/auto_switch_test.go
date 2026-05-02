package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAutoSwitch_GetReturnsDefaults covers the cold-start path: a brand-new
// daemon (kv table empty) must answer with the documented defaults rather
// than 404 / 500, so the UI's hydrate-on-mount fetch never falls back to
// optimistic local state.
func TestAutoSwitch_GetReturnsDefaults(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")

	req := httptest.NewRequest(http.MethodGet, "/api/auto-switch", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET: want 200, got %d body=%q", w.Code, w.Body.String())
	}
	var got autoSwitchView
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Enabled || got.Policy != "lru" {
		t.Errorf("defaults: got %+v want {true, lru}", got)
	}
}

// TestAutoSwitch_PostPersists checks that a POST round-trips through the
// kv table and the next GET reflects the new state.
func TestAutoSwitch_PostPersists(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")

	body := strings.NewReader(`{"enabled":false,"policy":"lowest"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auto-switch", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST: want 200, got %d body=%q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/auto-switch", nil)
	w = httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	var got autoSwitchView
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Enabled || got.Policy != "lowest" {
		t.Errorf("after POST: got %+v want {false, lowest}", got)
	}
}

// TestAutoSwitch_PostRejectsUnknownPolicy guards the UI contract: only the
// three documented policies are accepted. An invalid string must come back
// as 400 so the client surfaces the error rather than persisting garbage
// the coordinator wouldn't know how to interpret.
func TestAutoSwitch_PostRejectsUnknownPolicy(t *testing.T) {
	st, _ := newTestStore(t)
	srv := New(st, nil, nil, "")

	body := strings.NewReader(`{"enabled":true,"policy":"random"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/auto-switch", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d body=%q", w.Code, w.Body.String())
	}
}
