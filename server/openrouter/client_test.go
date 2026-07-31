package openrouter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// call records one request the fake OpenRouter saw, so tests can assert on the
// exact three-step sequence and on the unwind.
type call struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

type fakeAPI struct {
	mu    sync.Mutex
	calls []call
	// handler decides the response per (method, path). Returning status 0 means
	// "not handled" and yields 404.
	handler func(c call) (status int, body string)
}

func newFake(t *testing.T, handler func(c call) (int, string)) (*Client, *fakeAPI) {
	t.Helper()
	f := &fakeAPI{handler: handler}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := call{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization")}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &c.Body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()

		status, body := f.handler(c)
		if status == 0 {
			status, body = http.StatusNotFound, `{"error":{"message":"no route"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, ManagementKey: "sk-or-mgmt", HTTP: srv.Client()}, f
}

func (f *fakeAPI) seen() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]call, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeAPI) paths() []string {
	var out []string
	for _, c := range f.seen() {
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func wantPaths(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("call sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call sequence = %v, want %v", got, want)
		}
	}
}

// happyHandler is the fake's normal behaviour: guardrails and keys both work.
func happyHandler(c call) (int, string) {
	switch {
	case c.Method == http.MethodPost && c.Path == "/guardrails":
		return http.StatusOK, `{"data":{"id":"gr-1"}}`
	case c.Method == http.MethodPost && c.Path == "/keys":
		return http.StatusOK, `{"key":"sk-or-v1-runtime","data":{"hash":"kh-1"}}`
	case c.Method == http.MethodPost && strings.HasPrefix(c.Path, "/guardrails/") &&
		strings.HasSuffix(c.Path, "/assignments/keys"):
		return http.StatusOK, `{}`
	case c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/keys/"):
		return http.StatusOK, `{}`
	case c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/guardrails/"):
		return http.StatusOK, `{}`
	}
	return 0, ""
}

func TestDeriveDeviceKeyRunsThreeStepsAndAuthenticates(t *testing.T) {
	c, f := newFake(t, happyHandler)

	got, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName:       "foxy-dev-1",
		GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"deepseek/deepseek-v4-flash"},
		LimitUSD:      25,
		LimitReset:    "monthly",
	})
	if err != nil {
		t.Fatalf("DeriveDeviceKey: %v", err)
	}
	if got.Secret != "sk-or-v1-runtime" || got.Hash != "kh-1" {
		t.Fatalf("derived = %+v, want secret/hash from the create-key response", got)
	}
	if got.GuardrailID != "gr-1" || !got.GuardrailEnforced {
		t.Fatalf("derived = %+v, want guardrail gr-1 marked enforced", got)
	}

	wantPaths(t, f.paths(), []string{
		"POST /guardrails",
		"POST /keys",
		"POST /guardrails/gr-1/assignments/keys",
	})

	seen := f.seen()
	for _, c := range seen {
		if c.Auth != "Bearer sk-or-mgmt" {
			t.Fatalf("call %s %s sent Authorization %q, want the management key",
				c.Method, c.Path, c.Auth)
		}
	}
	// The allowlist rides on the guardrail, not the key — that asymmetry is the
	// whole reason derivation is three calls.
	models, _ := seen[0].Body["allowed_models"].([]any)
	if len(models) != 1 || models[0] != "deepseek/deepseek-v4-flash" {
		t.Fatalf("guardrail body = %+v, want allowed_models", seen[0].Body)
	}
	if _, ok := seen[1].Body["allowed_models"]; ok {
		t.Fatalf("key body carries allowed_models (%+v) — keys have no model field", seen[1].Body)
	}
	if seen[1].Body["name"] != "foxy-dev-1" || seen[1].Body["limit"] != 25.0 {
		t.Fatalf("key body = %+v, want name + limit", seen[1].Body)
	}
	// A zero ExpiresAt must be omitted, not sent as year 0001 (which would
	// expire the key the instant it is created).
	if _, ok := seen[1].Body["expires_at"]; ok {
		t.Fatalf("key body carries expires_at %v for a zero time", seen[1].Body["expires_at"])
	}
	// key_hashes, plural and an array — see live_contract_test.go.
	hashes, _ := seen[2].Body["key_hashes"].([]any)
	if len(hashes) != 1 || hashes[0] != "kh-1" {
		t.Fatalf("assignment body = %+v, want key_hashes:[kh-1]", seen[2].Body)
	}
}

func TestDeriveDeviceKeySendsExpiryWhenSet(t *testing.T) {
	c, f := newFake(t, happyHandler)
	exp := time.Date(2027, 3, 4, 5, 6, 7, 0, time.UTC)
	if _, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", ExpiresAt: exp,
	}); err != nil {
		t.Fatalf("DeriveDeviceKey: %v", err)
	}
	body := f.seen()[0].Body
	if body["expires_at"] != "2027-03-04T05:06:07Z" {
		t.Fatalf("key body expires_at = %v, want RFC3339", body["expires_at"])
	}
}

func TestDeriveDeviceKeySkipsGuardrailWhenNoModelsRequested(t *testing.T) {
	c, f := newFake(t, happyHandler)
	// No allowlist means no guardrail, and /keys accepts a lifetime cap (a
	// `limit` with no `limit_reset`) — so this must NOT trip the guardrail-only
	// reset-interval rule.
	got, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", LimitUSD: 5,
	})
	if err != nil {
		t.Fatalf("DeriveDeviceKey: %v", err)
	}
	if got.GuardrailID != "" || got.GuardrailEnforced {
		t.Fatalf("derived = %+v, want no guardrail when no allowlist was asked for", got)
	}
	wantPaths(t, f.paths(), []string{"POST /keys"})
}

// The unwind tests are the reason this client isn't three inline HTTP calls at
// the call site: every partial failure leaves live upstream state.

func TestDeriveDeviceKeyDeletesGuardrailWhenKeyCreationFails(t *testing.T) {
	c, f := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodPost && c.Path == "/keys" {
			return http.StatusInternalServerError, `{"error":{"message":"boom"}}`
		}
		return happyHandler(c)
	})

	if _, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"a/b"}, LimitUSD: 5, LimitReset: "monthly",
	}); err == nil {
		t.Fatal("DeriveDeviceKey must fail when key creation fails")
	}
	wantPaths(t, f.paths(), []string{
		"POST /guardrails",
		"POST /keys",
		"DELETE /guardrails/gr-1", // orphan cleaned up
	})
}

// TestDeriveDeviceKeyUnwindsWhenAssignmentFails is the important one: at that
// point a LIVE, UNCONSTRAINED key exists upstream. Returning it (or even
// leaving it behind) would hand out a key that can call any model while the
// vault records it as restricted.
func TestDeriveDeviceKeyUnwindsWhenAssignmentFails(t *testing.T) {
	c, f := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodPost && strings.HasSuffix(c.Path, "/assignments/keys") {
			return http.StatusInternalServerError, `{"error":{"message":"nope"}}`
		}
		return happyHandler(c)
	})

	if _, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"a/b"}, LimitUSD: 5, LimitReset: "monthly",
	}); err == nil {
		t.Fatal("DeriveDeviceKey must fail when the guardrail can't be attached")
	}
	wantPaths(t, f.paths(), []string{
		"POST /guardrails",
		"POST /keys",
		"POST /guardrails/gr-1/assignments/keys",
		"DELETE /keys/kh-1",       // the unconstrained key is killed
		"DELETE /guardrails/gr-1", // and its orphan guardrail
	})
}

func TestCreateKeyRefusesAKeyItCannotRevoke(t *testing.T) {
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodPost && c.Path == "/keys" {
			// A live key with no hash: we could never DELETE /keys/{hash} it.
			return http.StatusOK, `{"key":"sk-or-v1-runtime"}`
		}
		return happyHandler(c)
	})
	if _, err := c.CreateKey(context.Background(), KeySpec{Name: "n"}); err == nil ||
		!strings.Contains(err.Error(), "unrevocable") {
		t.Fatalf("CreateKey err = %v, want a refusal to accept a key with no hash", err)
	}
}

func TestCreateKeyAcceptsEitherResponseShape(t *testing.T) {
	// The wire shape is unverified, so decoding tolerates the plaintext key and
	// hash appearing at the top level or inside a data envelope.
	for name, body := range map[string]string{
		"envelope":  `{"key":"sk-1","data":{"hash":"h-1"}}`,
		"flat":      `{"key":"sk-1","hash":"h-1"}`,
		"all-inner": `{"data":{"key":"sk-1","hash":"h-1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newFake(t, func(c call) (int, string) {
				if c.Method == http.MethodPost && c.Path == "/keys" {
					return http.StatusOK, body
				}
				return happyHandler(c)
			})
			got, err := c.CreateKey(context.Background(), KeySpec{Name: "n"})
			if err != nil {
				t.Fatalf("CreateKey(%s): %v", name, err)
			}
			if got.Secret != "sk-1" || got.Hash != "h-1" {
				t.Fatalf("CreateKey(%s) = %+v", name, got)
			}
		})
	}
}

func TestInferenceKeyMisconfigurationIsDistinguishable(t *testing.T) {
	c, _ := newFake(t, func(call) (int, string) {
		return http.StatusUnauthorized, `{"error":{"message":"Invalid management key"}}`
	})
	_, err := c.CreateKey(context.Background(), KeySpec{Name: "n"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized so the UI can say 'wrong kind of key'", err)
	}
}

// --- the degraded path (design §3's open question) -------------------------

func guardrailsUnavailable(status int) func(call) (int, string) {
	return func(c call) (int, string) {
		if strings.HasPrefix(c.Path, "/guardrails") {
			return status, `{"error":{"message":"guardrails require a team plan"}}`
		}
		return happyHandler(c)
	}
}

func TestGuardrailsUnavailableIsFatalUnlessExplicitlyAllowed(t *testing.T) {
	// 402 / 403 / 404 all mean "this plan doesn't do guardrails" and must map
	// to the same sentinel, since the caller's fallback is identical.
	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound} {
		c, f := newFake(t, guardrailsUnavailable(status))
		_, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
			KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
			AllowedModels: []string{"a/b"}, LimitUSD: 5, LimitReset: "monthly",
		})
		if !errors.Is(err, ErrGuardrailsUnavailable) {
			t.Fatalf("status %d: err = %v, want ErrGuardrailsUnavailable", status, err)
		}
		// Critically: no key was minted. Falling back silently would hand the
		// device an unrestricted key.
		wantPaths(t, f.paths(), []string{"POST /guardrails"})
	}
}

func TestGuardrailsUnavailableDegradesWhenOptedInWithACap(t *testing.T) {
	c, f := newFake(t, guardrailsUnavailable(http.StatusForbidden))
	got, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"a/b"}, LimitUSD: 5, LimitReset: "monthly",
		AllowUnenforcedModels: true,
	})
	if err != nil {
		t.Fatalf("DeriveDeviceKey: %v", err)
	}
	if got.Secret == "" {
		t.Fatal("degraded derivation should still mint a usable key")
	}
	// The caller must be able to see that the allowlist is not enforced, so it
	// can say so instead of implying protection that isn't there.
	if got.GuardrailEnforced || got.GuardrailID != "" {
		t.Fatalf("derived = %+v, want GuardrailEnforced=false on the degraded path", got)
	}
	wantPaths(t, f.paths(), []string{"POST /guardrails", "POST /keys"})
}

func TestGuardrailsUnavailableWithoutSpendCapIsRefused(t *testing.T) {
	// Opting into an unenforced allowlist with no spend cap would produce a key
	// that can spend anything on any model — the one combination with no limit
	// of any kind.
	c, f := newFake(t, guardrailsUnavailable(http.StatusForbidden))
	_, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", GuardrailName: "foxy-dev-1",
		AllowedModels: []string{"a/b"}, AllowUnenforcedModels: true,
	})
	if err == nil || !strings.Contains(err.Error(), "spend cap") {
		t.Fatalf("err = %v, want a refusal citing the missing spend cap", err)
	}
	wantPaths(t, f.paths(), []string{"POST /guardrails"})
}

// --- revocation -----------------------------------------------------------

func TestRevokeDerivedKeyDeletesKeyThenGuardrail(t *testing.T) {
	c, f := newFake(t, happyHandler)
	if err := c.RevokeDerivedKey(context.Background(), "kh-1", "gr-1"); err != nil {
		t.Fatalf("RevokeDerivedKey: %v", err)
	}
	wantPaths(t, f.paths(), []string{"DELETE /keys/kh-1", "DELETE /guardrails/gr-1"})
}

func TestRevokeIsIdempotentOn404(t *testing.T) {
	// Re-revoking (retry, or a key an admin already deleted upstream) must
	// succeed, or a device could never be fully cleaned up.
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodDelete {
			return http.StatusNotFound, `{"error":{"message":"not found"}}`
		}
		return happyHandler(c)
	})
	if err := c.RevokeDerivedKey(context.Background(), "kh-1", "gr-1"); err != nil {
		t.Fatalf("RevokeDerivedKey on already-deleted = %v, want nil", err)
	}
}

func TestRevokeReportsASurvivingKey(t *testing.T) {
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/keys/") {
			return http.StatusInternalServerError, `{"error":{"message":"boom"}}`
		}
		return happyHandler(c)
	})
	if err := c.RevokeDerivedKey(context.Background(), "kh-1", "gr-1"); err == nil {
		t.Fatal("a key that could not be deleted must surface as an error — it still works")
	}
}

// --- capability probe ------------------------------------------------------

func TestCheckCapabilities(t *testing.T) {
	t.Run("guardrails available", func(t *testing.T) {
		c, f := newFake(t, happyHandler)
		caps, err := c.CheckCapabilities(context.Background())
		if err != nil {
			t.Fatalf("CheckCapabilities: %v", err)
		}
		if !caps.ManagementKeyValid || !caps.GuardrailsAvailable {
			t.Fatalf("caps = %+v, want valid + available", caps)
		}
		// The probe must clean up after itself.
		wantPaths(t, f.paths(), []string{"POST /guardrails", "DELETE /guardrails/gr-1"})
	})

	t.Run("guardrails unavailable", func(t *testing.T) {
		c, _ := newFake(t, guardrailsUnavailable(http.StatusForbidden))
		caps, err := c.CheckCapabilities(context.Background())
		if err != nil {
			t.Fatalf("CheckCapabilities: %v", err)
		}
		if !caps.ManagementKeyValid || caps.GuardrailsAvailable {
			t.Fatalf("caps = %+v, want valid key but no guardrails", caps)
		}
		if !strings.Contains(caps.Detail, "spend caps") {
			t.Fatalf("Detail = %q, should tell the admin what protection remains", caps.Detail)
		}
	})

	t.Run("wrong kind of key", func(t *testing.T) {
		c, _ := newFake(t, func(call) (int, string) {
			return http.StatusUnauthorized, `{"error":{"message":"Invalid management key"}}`
		})
		caps, err := c.CheckCapabilities(context.Background())
		if err != nil {
			t.Fatalf("CheckCapabilities: %v", err)
		}
		if caps.ManagementKeyValid || caps.GuardrailsAvailable {
			t.Fatalf("caps = %+v, want both false", caps)
		}
	})
}

func TestNoManagementKeyIsAClearError(t *testing.T) {
	c := &Client{BaseURL: "http://127.0.0.1:1"}
	if _, err := c.CreateKey(context.Background(), KeySpec{Name: "n"}); err == nil ||
		!strings.Contains(err.Error(), "no management key") {
		t.Fatalf("err = %v, want an explicit 'no management key configured'", err)
	}
}

func TestErrorMessageExtraction(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"error":{"message":"deep"}}`, "deep"},
		{`{"error":"flat"}`, "flat"},
		{`<html>gateway</html>`, "<html>gateway</html>"},
	} {
		if got := errorMessage([]byte(tc.body)); got != tc.want {
			t.Fatalf("errorMessage(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
