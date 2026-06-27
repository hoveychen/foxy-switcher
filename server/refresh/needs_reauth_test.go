package refresh

import (
	"context"
	"database/sql"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/store"

	_ "modernc.org/sqlite"
)

// invalidGrantServer returns a 400 invalid_grant on every token request and
// counts how many times it was hit.
func invalidGrantServer(hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token not found"}`))
	}))
}

// When the refresh_token is permanently rejected, the scheduler must mark the
// account needs_reauth (so it stops routing / retrying) rather than leaving it
// "active" with a dead token and re-failing every tick.
func TestSchedulerMarksNeedsReauthOnInvalidGrant(t *testing.T) {
	ctx := context.Background()

	srv := invalidGrantServer(nil)
	defer srv.Close()
	prevURL := authz.ClaudeTokenURL
	authz.ClaudeTokenURL = srv.URL
	defer func() { authz.ClaudeTokenURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:         "dead",
		Email:        "dead@example.com",
		AccessToken:  "old-access",
		RefreshToken: "dead-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
		Status:       store.StatusActive,
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	New(st, nil).tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.StatusNeedsReauth {
		t.Fatalf("status = %q, want %q", got.Status, store.StatusNeedsReauth)
	}
}

// A needs_reauth account must be skipped by tick entirely — retrying a dead
// refresh_token every 10 minutes is pointless and hammers the endpoint.
func TestSchedulerSkipsNeedsReauthAccount(t *testing.T) {
	ctx := context.Background()

	var hits int32
	srv := invalidGrantServer(&hits)
	defer srv.Close()
	prevURL := authz.ClaudeTokenURL
	authz.ClaudeTokenURL = srv.URL
	defer func() { authz.ClaudeTokenURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Name:         "already-dead",
		Email:        "ad@example.com",
		AccessToken:  "old-access",
		RefreshToken: "dead-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
		Status:       store.StatusNeedsReauth,
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	New(st, nil).tick(ctx)

	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Fatalf("token endpoint hit %d times for a needs_reauth account; want 0 (must be skipped)", got)
	}
}

// The invalid_grant path must emit a distinct account.needs_reauth event, not
// the generic error.refresh — so the UI can prompt for re-login.
func TestSchedulerEmitsNeedsReauthEvent(t *testing.T) {
	ctx := context.Background()

	srv := invalidGrantServer(nil)
	defer srv.Close()
	prevURL := authz.ClaudeTokenURL
	authz.ClaudeTokenURL = srv.URL
	defer func() { authz.ClaudeTokenURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	busDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "act.db"))
	if err != nil {
		t.Fatalf("open bus db: %v", err)
	}
	defer busDB.Close()
	bus, err := activity.NewBus(busDB, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("new bus: %v", err)
	}

	a := &store.Account{
		Name:         "dead",
		Email:        "dead-evt@example.com",
		AccessToken:  "old-access",
		RefreshToken: "dead-refresh",
		ExpiresAt:    time.Now().Add(-time.Minute).UnixMilli(),
		Status:       store.StatusActive,
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	s := New(st, nil)
	s.Bus = bus
	s.tick(ctx)

	reauthCount := 0
	genericErrCount := 0
	for _, ev := range bus.List(activity.Filter{}) {
		switch ev.Type {
		case activity.TypeAccountNeedsReauth:
			reauthCount++
			if ev.AccountID != a.ID {
				t.Errorf("needs_reauth event account_id = %d, want %d", ev.AccountID, a.ID)
			}
		case activity.TypeErrorRefresh:
			genericErrCount++
		}
	}
	if reauthCount != 1 {
		t.Errorf("account.needs_reauth events = %d, want 1", reauthCount)
	}
	if genericErrCount != 0 {
		t.Errorf("error.refresh events = %d, want 0 (invalid_grant must not look like a transient error)", genericErrCount)
	}
}
