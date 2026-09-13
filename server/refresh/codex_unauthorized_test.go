package refresh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"

	_ "modernc.org/sqlite"
)

// codexUsageServer serves the Codex usage endpoint with the given status and
// counts hits.
func codexUsageServer(status int, body string, hits *int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func codexAccount(t *testing.T, st *store.Store, status string) *store.Account {
	t.Helper()
	a := &store.Account{
		Provider:     store.ProviderCodex,
		Name:         "Harry C",
		Email:        "harry@example.com",
		AccessToken:  "live-access",
		RefreshToken: "live-refresh",
		// Deliberately far in the future: a revoked Codex grant keeps a
		// perfectly valid-looking `exp` for days, which is exactly why the
		// refresh Scheduler never gets around to noticing it.
		ExpiresAt:      time.Now().Add(10 * 24 * time.Hour).UnixMilli(),
		Status:         status,
		AccountUUID:    "acct-1",
		ProviderUserID: "user-1",
		Plan:           "Codex Business Premium",
	}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	return a
}

func withCodexUsageURL(t *testing.T, url string) {
	t.Helper()
	prev := openai.UsageURL
	openai.UsageURL = url
	t.Cleanup(func() { openai.UsageURL = prev })
}

// `codex login` / `codex logout` revoke the grant upstream, so a pooled Codex
// account can start 401ing while its stored token is nowhere near `exp`. The
// refresh Scheduler won't look at such a row for days, so the usage poller has
// to be the one that flags it — otherwise the selector keeps handing a dead
// credential to devices and every Codex CLI it lands on reports "your refresh
// token was revoked". One 401 is not enough; three consecutive ones are.
func TestUsagePollerFlagsCodexAfterRepeated401(t *testing.T) {
	ctx := context.Background()
	srv := codexUsageServer(http.StatusUnauthorized, `{"detail":"Unauthorized"}`, nil)
	defer srv.Close()
	withCodexUsageURL(t, srv.URL)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := codexAccount(t, st, store.StatusActive)

	p := NewUsagePoller(st, nil)
	for i := 1; i < codexUnauthorizedStrikes; i++ {
		p.tick(ctx)
		got, err := st.Get(ctx, a.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Status != store.StatusActive {
			t.Fatalf("after %d 401s status = %q, want %q — a single blip must not kill the account",
				i, got.Status, store.StatusActive)
		}
	}

	p.tick(ctx)
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.StatusNeedsReauth {
		t.Fatalf("after %d 401s status = %q, want %q",
			codexUnauthorizedStrikes, got.Status, store.StatusNeedsReauth)
	}
}

// A 401 that recovers must not accumulate towards the strike count.
func TestUsagePollerResetsCodex401StreakOnSuccess(t *testing.T) {
	ctx := context.Background()
	var fail atomic.Bool
	fail.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"self_serve_business_prolite","rate_limit":{"primary_window":{"used_percent":1.5,"reset_at":0}}}`))
	}))
	defer srv.Close()
	withCodexUsageURL(t, srv.URL)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	a := codexAccount(t, st, store.StatusActive)

	p := NewUsagePoller(st, nil)
	p.tick(ctx)
	p.tick(ctx)
	fail.Store(false)
	p.tick(ctx)
	fail.Store(true)
	p.tick(ctx)
	p.tick(ctx)

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != store.StatusActive {
		t.Fatalf("status = %q, want %q — the successful poll should have reset the streak",
			got.Status, store.StatusActive)
	}
}

// Once flagged, a Codex account must stop being polled: a dead token has no
// usage to report, and re-polling it just reprints the same 401 every tick.
func TestUsagePollerSkipsNeedsReauthCodex(t *testing.T) {
	ctx := context.Background()
	var hits int32
	srv := codexUsageServer(http.StatusUnauthorized, `{}`, &hits)
	defer srv.Close()
	withCodexUsageURL(t, srv.URL)

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	codexAccount(t, st, store.StatusNeedsReauth)

	NewUsagePoller(st, nil).tick(ctx)

	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("usage endpoint hit %d times, want 0", n)
	}
}
