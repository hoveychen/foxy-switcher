package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// stubGrants lets a test flip what the vault says between syncs, which is the
// only interesting axis here: the writer's whole job is reacting to that.
type stubGrants struct {
	grant *vault.OpenRouterGrant
	err   error
	calls int
}

func (s *stubGrants) OpenRouterConfig(context.Context) (*vault.OpenRouterGrant, error) {
	s.calls++
	return s.grant, s.err
}

func newWriter(t *testing.T, src openRouterGrantSource) (*openRouterWriter, string) {
	t.Helper()
	home := t.TempDir()
	return newOpenRouterWriter(src, home, "/opt/foxy/foxy-switcher", log.New(io.Discard, "", 0)), home
}

func profileCount(t *testing.T, home string) int {
	t.Helper()
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".config.toml") && strings.HasPrefix(e.Name(), "or-") {
			n++
		}
	}
	return n
}

func grant(models ...string) *vault.OpenRouterGrant {
	return &vault.OpenRouterGrant{
		AccountID: 1, AccountName: "pool", APIKey: "sk-or-runtime",
		BaseURL: vault.DefaultOpenRouterBaseURL, AllowedModels: models,
		GuardrailEnforced: true,
	}
}

func TestWriterAppliesGrantAndHoldsKeyInMemoryOnly(t *testing.T) {
	src := &stubGrants{grant: grant("a/b", "c/d")}
	w, home := newWriter(t, src)

	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if n := profileCount(t, home); n != 2 {
		t.Fatalf("profiles = %d, want 2", n)
	}
	cfg, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	// The key must reach codex only through the token command.
	if strings.Contains(string(cfg), "sk-or-runtime") {
		t.Fatalf("the runtime key was written to config.toml:\n%s", cfg)
	}
	if !strings.Contains(string(cfg), `command = "/opt/foxy/foxy-switcher"`) ||
		!strings.Contains(string(cfg), `args = ["cred", "openrouter-token"]`) {
		t.Fatalf("token command not wired:\n%s", cfg)
	}

	got, err := w.Token()
	if err != nil || got != "sk-or-runtime" {
		t.Fatalf("Token() = %q, %v", got, err)
	}
}

// Losing the grant (revoked / suspended / provider withdrawn) must tear the
// config down, or codex keeps advertising models this device can no longer use
// and every session fails with an opaque auth error.
func TestWriterTearsDownWhenTheGrantIsWithdrawn(t *testing.T) {
	src := &stubGrants{grant: grant("a/b")}
	w, home := newWriter(t, src)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	src.grant, src.err = nil, selector.ErrNoAvailable
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if n := profileCount(t, home); n != 0 {
		t.Fatalf("profiles = %d after withdrawal, want 0", n)
	}
	cfg, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if strings.Contains(string(cfg), "model_providers.openrouter") {
		t.Fatalf("provider block survived withdrawal:\n%s", cfg)
	}
	if _, err := w.Token(); err == nil {
		t.Fatal("Token() still returns a key after the grant was withdrawn")
	}
}

// A transient vault outage is NOT a withdrawal. Tearing down on a network blip
// would drop codex's provider mid-session for no reason.
func TestWriterKeepsConfigThroughATransientVaultError(t *testing.T) {
	src := &stubGrants{grant: grant("a/b")}
	w, home := newWriter(t, src)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}

	src.grant, src.err = nil, errors.New("vault unreachable")
	if err := w.Sync(context.Background()); err == nil {
		t.Fatal("Sync must surface the outage")
	}
	if n := profileCount(t, home); n != 1 {
		t.Fatalf("profiles = %d after a blip, want the config left in place", n)
	}
	if _, err := w.Token(); err != nil {
		t.Fatalf("Token() = %v; the last known-good key should still serve", err)
	}
}

func TestWriterFollowsAllowlistChanges(t *testing.T) {
	src := &stubGrants{grant: grant("a/b", "c/d", "e/f")}
	w, home := newWriter(t, src)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n := profileCount(t, home); n != 3 {
		t.Fatalf("profiles = %d, want 3", n)
	}

	// Admin shrinks the allowlist and the vault re-derives; the device must stop
	// offering the dropped models.
	src.grant = grant("a/b")
	src.grant.APIKey = "sk-or-rederived"
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if n := profileCount(t, home); n != 1 {
		t.Fatalf("profiles = %d after shrink, want 1", n)
	}
	if got, _ := w.Token(); got != "sk-or-rederived" {
		t.Fatalf("Token() = %q, want the re-derived key", got)
	}
}

func TestTeardownIsIdempotent(t *testing.T) {
	src := &stubGrants{grant: grant("a/b")}
	w, _ := newWriter(t, src)
	for i := 0; i < 3; i++ {
		if err := w.Teardown(); err != nil {
			t.Fatalf("teardown %d: %v", i, err)
		}
	}
}

// --- the loopback token endpoint -------------------------------------------

func TestOpenRouterTokenHandlerServesPlainToken(t *testing.T) {
	src := &stubGrants{grant: grant("a/b")}
	w, _ := newWriter(t, src)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}

	rec := httptest.NewRecorder()
	openRouterTokenHandler(w)(rec, httptest.NewRequest(http.MethodGet, "/api/cred/openrouter-token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// codex reads the entire body as the bearer token, so it must be the token
	// and nothing else — no JSON wrapper, no trailing newline noise.
	if rec.Body.String() != "sk-or-runtime" {
		t.Fatalf("body = %q, want the bare token", rec.Body.String())
	}
}

// 503 rather than an empty 200: codex must treat "no key" as a failed command,
// not authenticate with an empty bearer and get an opaque 401 from OpenRouter.
func TestOpenRouterTokenHandlerFailsClosed(t *testing.T) {
	for name, w := range map[string]*openRouterWriter{
		"provider disabled": nil,
		"not yet authorised": newOpenRouterWriter(
			&stubGrants{err: selector.ErrNoAvailable}, "", "", log.New(io.Discard, "", 0)),
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			openRouterTokenHandler(w)(rec, httptest.NewRequest(http.MethodGet, "/x", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
			if strings.TrimSpace(rec.Body.String()) == "" {
				t.Fatal("a failure with no message gives codex nothing to report")
			}
		})
	}
}

// The endpoint hands out a live third-party credential and carries no auth of
// its own, so loopback IS the authorisation.
func TestOpenRouterTokenHandlerIsLoopbackGated(t *testing.T) {
	src := &stubGrants{grant: grant("a/b")}
	w, _ := newWriter(t, src)
	if err := w.Sync(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	gated := loopbackOnly(openRouterTokenHandler(w))

	req := httptest.NewRequest(http.MethodGet, "/api/cred/openrouter-token", nil)
	req.RemoteAddr = "203.0.113.7:5555"
	rec := httptest.NewRecorder()
	gated(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote caller status = %d, want 403", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sk-or-runtime") {
		t.Fatal("the key leaked to a non-loopback caller")
	}
}

// --- the CLI ---------------------------------------------------------------

func TestCredOpenRouterTokenPrintsOnlyTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/cred/openrouter-token" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, "sk-or-from-daemon\n")
	}))
	defer srv.Close()

	dir := writePortFileFor(t, srv.URL)
	var out bytes.Buffer
	if err := runCredOpenRouterToken(dir, &out); err != nil {
		t.Fatalf("runCredOpenRouterToken: %v", err)
	}
	// Trailing whitespace would end up inside the Authorization header value.
	if out.String() != "sk-or-from-daemon" {
		t.Fatalf("stdout = %q, want the trimmed token and nothing else", out.String())
	}
}

func TestCredOpenRouterTokenFailsWhenDaemonIsNotRunning(t *testing.T) {
	var out bytes.Buffer
	err := runCredOpenRouterToken(t.TempDir(), &out)
	if err == nil {
		t.Fatal("must fail when there is no daemon")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Fatalf("err = %v, want a message naming the missing daemon", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q to stdout on failure; codex would use it as a token", out.String())
	}
}

func TestCredOpenRouterTokenSurfacesDaemonRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "this device is not authorised for OpenRouter", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	dir := writePortFileFor(t, srv.URL)
	var out bytes.Buffer
	err := runCredOpenRouterToken(dir, &out)
	if err == nil {
		t.Fatal("a 503 must be an error, not an empty token")
	}
	if !strings.Contains(err.Error(), "not authorised") {
		t.Fatalf("err = %v, want the daemon's reason passed through", err)
	}
	if out.Len() != 0 {
		t.Fatalf("wrote %q to stdout on refusal", out.String())
	}
}

func TestCredOpenRouterTokenRejectsAnEmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK) // 200 with nothing in it
	}))
	defer srv.Close()

	dir := writePortFileFor(t, srv.URL)
	var out bytes.Buffer
	if err := runCredOpenRouterToken(dir, &out); err == nil {
		t.Fatal("an empty 200 must fail — otherwise codex authenticates with an empty bearer")
	}
}

// writePortFileFor points a data dir at an httptest server by writing its port
// where the daemon would have.
func writePortFileFor(t *testing.T, serverURL string) string {
	t.Helper()
	port := serverURL[strings.LastIndex(serverURL, ":")+1:]
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte(port), 0o600); err != nil {
		t.Fatalf("write port file: %v", err)
	}
	return dir
}
