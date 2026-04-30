package tui

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/httpapi"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// startServer wires the same HTTP surface the daemon exposes against an
// in-process httptest.Server so we can exercise the TUI client end-to-end
// without dealing with port files / sidecar lifecycle.
func startServer(t *testing.T) (*Client, *store.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	pkce := authz.NewPKCEStore()
	rf := refresh.New(st, log.New(io.Discard, "", 0))
	srv := httpapi.New(st, pkce, rf, dir)
	ts := httptest.NewServer(srv.Handler())

	cleanup := func() {
		ts.Close()
		st.Close()
	}
	return newClientForBase(ts.URL), st, cleanup
}

// seedAccount inserts a usable account row directly through the store. We
// bypass the OAuth /login → /callback flow because that requires Anthropic's
// real authorize endpoint; for client wiring tests the row only needs to
// exist with a stable id.
func seedAccount(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	a := store.Account{
		Name:             name,
		AccessToken:      "dummy-access-token",
		RefreshToken:     "dummy-refresh-token",
		ExpiresAt:        time.Now().Add(time.Hour).UnixMilli(),
		Scopes:           "user:profile",
		SubscriptionType: "max",
		OrganizationUUID: name + "-uuid",
		Email:            name + "@example.com",
		Plan:             "Claude Max",
		Status:           "active",
	}
	if err := st.Upsert(context.Background(), &a); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return a.ID
}

func TestClient_ListAndHook_EmptyPool(t *testing.T) {
	c, _, cleanup := startServer(t)
	defer cleanup()

	ctx := context.Background()
	accs, err := c.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accs) != 0 {
		t.Fatalf("expected empty pool, got %d accounts", len(accs))
	}
	hook, err := c.HookStatus(ctx)
	if err != nil {
		t.Fatalf("hook: %v", err)
	}
	if hook.Installed {
		t.Fatalf("hook should not be installed in fresh data dir")
	}
	if hook.HelperPath == "" {
		t.Fatalf("hook helper path should be populated even when not installed")
	}
}

func TestClient_StatusToggle(t *testing.T) {
	c, st, cleanup := startServer(t)
	defer cleanup()
	id := seedAccount(t, st, "alpha")

	ctx := context.Background()
	if err := c.Disable(ctx, id); err != nil {
		t.Fatalf("disable: %v", err)
	}
	accs, err := c.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accs) != 1 || accs[0].Status != "disabled" {
		t.Fatalf("expected status=disabled, got %+v", accs)
	}

	if err := c.Enable(ctx, id); err != nil {
		t.Fatalf("enable: %v", err)
	}
	accs, err = c.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accs) != 1 || accs[0].Status != "active" {
		t.Fatalf("expected status=active, got %+v", accs)
	}
}

func TestClient_CooldownSetAndClear(t *testing.T) {
	c, st, cleanup := startServer(t)
	defer cleanup()
	id := seedAccount(t, st, "beta")

	ctx := context.Background()
	if err := c.SetCooldown(ctx, id, 30*time.Minute); err != nil {
		t.Fatalf("set cooldown: %v", err)
	}
	accs, err := c.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accs) != 1 {
		t.Fatalf("want 1 account, got %d", len(accs))
	}
	cd := accs[0].CooldownUntil
	now := time.Now().UnixMilli()
	// Allow a generous window — handler stamps now+30m, listing happens
	// milliseconds later, so the lower bound is "near 30m" not "exactly 30m".
	if cd < now+25*time.Minute.Milliseconds() || cd > now+35*time.Minute.Milliseconds() {
		t.Fatalf("cooldown_until=%d not within ~30m of now=%d", cd, now)
	}

	if err := c.SetCooldown(ctx, id, 0); err != nil {
		t.Fatalf("clear cooldown: %v", err)
	}
	accs, err = c.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if accs[0].CooldownUntil != 0 {
		t.Fatalf("cooldown_until should be 0 after clear, got %d", accs[0].CooldownUntil)
	}
}

func TestClient_Delete(t *testing.T) {
	c, st, cleanup := startServer(t)
	defer cleanup()
	id := seedAccount(t, st, "gamma")

	ctx := context.Background()
	if err := c.Delete(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	accs, err := c.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(accs) != 0 {
		t.Fatalf("expected empty pool after delete, got %+v", accs)
	}
}

func TestClient_DeleteUnknownIDIsHandled(t *testing.T) {
	// Sanity: deleting a non-existent id should not error (the handler runs
	// the SQL DELETE which is a no-op on missing rows). Mostly a guard so
	// future "404 if not found" changes get caught here.
	c, _, cleanup := startServer(t)
	defer cleanup()
	if err := c.Delete(context.Background(), 9999); err != nil {
		t.Fatalf("delete missing id: %v", err)
	}
}
