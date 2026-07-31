package refresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// OpenRouter accounts are pay-as-you-go: there is no OAuth token to rotate and
// no subscription usage window to poll. Both loops must leave them completely
// alone — routing one into Anthropic's token or usage endpoint would produce a
// permanent 401 error card for an account that is working fine.
//
// Both rows below are given access/refresh tokens on purpose. In production an
// OpenRouter row carries neither, so the loops' pre-existing
// `RefreshToken == ""` / `AccessToken == ""` guards would already skip them —
// which would make this test pass without exercising anything. Populating the
// tokens removes that accidental protection so the assertions actually pin the
// provider check.
func TestSchedulerSkipsOpenRouterAccounts(t *testing.T) {
	ctx := context.Background()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "the refresh scheduler must never call out for an OpenRouter account",
			http.StatusInternalServerError)
	}))
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
		Provider:     store.ProviderOpenRouter,
		Name:         "openrouter-pool",
		AccountUUID:  "or-1",
		Email:        "or@openrouter.local",
		AccessToken:  "should-be-ignored",
		RefreshToken: "should-be-ignored",
		// Long expired, so the "due for refresh" gate would otherwise fire.
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
		Status:    "active",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	New(st, nil).tick(ctx)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("scheduler made %d token call(s) for an OpenRouter account; want 0", n)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status == store.StatusNeedsReauth {
		t.Fatal("OpenRouter account was flagged needs_reauth — it has no OAuth grant to lose")
	}
	if got.AccessToken != "should-be-ignored" {
		t.Fatalf("OpenRouter row was rewritten by the scheduler: access_token = %q", got.AccessToken)
	}
}

func TestUsagePollerSkipsOpenRouterAccounts(t *testing.T) {
	ctx := context.Background()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "the usage poller must never call out for an OpenRouter account",
			http.StatusInternalServerError)
	}))
	defer srv.Close()
	prevURL := anthropic.BaseURL
	anthropic.BaseURL = srv.URL
	defer func() { anthropic.BaseURL = prevURL }()

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Provider:     store.ProviderOpenRouter,
		Name:         "openrouter-pool",
		AccountUUID:  "or-1",
		Email:        "or@openrouter.local",
		AccessToken:  "should-be-ignored",
		RefreshToken: "should-be-ignored",
		ExpiresAt:    time.Now().Add(time.Hour).UnixMilli(),
		Status:       "active",
		Plan:         "OpenRouter",
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	NewUsagePoller(st, nil).tick(ctx)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("usage poller made %d call(s) for an OpenRouter account; want 0", n)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UsageFetchedAt != 0 {
		t.Fatalf("OpenRouter account has usage_fetched_at = %d; it has no usage windows", got.UsageFetchedAt)
	}
}
