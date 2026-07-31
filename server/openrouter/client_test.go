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

// call records one request the fake OpenRouter saw.
type call struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

type fakeAPI struct {
	mu      sync.Mutex
	calls   []call
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

// happyHandler mirrors the shapes observed live: 201 on create with the
// plaintext key at the top level and the hash inside `data`.
func happyHandler(c call) (int, string) {
	switch {
	case c.Method == http.MethodPost && c.Path == "/keys":
		return http.StatusCreated, `{"key":"sk-or-v1-runtime","data":{"hash":"kh-1"}}`
	case c.Method == http.MethodGet && c.Path == "/keys":
		return http.StatusOK, `{"data":[],"total_count":0}`
	case c.Method == http.MethodDelete && strings.HasPrefix(c.Path, "/keys/"):
		return http.StatusOK, `{"deleted":true}`
	}
	return 0, ""
}

// Derivation is one call. It used to be three (create guardrail, create key,
// assign) — that shape is gone, and with it the partial-failure states that
// made it the riskiest part of the feature.
func TestDeriveDeviceKeyIsASingleCall(t *testing.T) {
	c, f := newFake(t, happyHandler)

	got, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", LimitUSD: 25, LimitReset: "monthly",
	})
	if err != nil {
		t.Fatalf("DeriveDeviceKey: %v", err)
	}
	if got.Secret != "sk-or-v1-runtime" || got.Hash != "kh-1" {
		t.Fatalf("derived = %+v", got)
	}
	wantPaths(t, f.paths(), []string{"POST /keys"})

	seen := f.seen()
	if seen[0].Auth != "Bearer sk-or-mgmt" {
		t.Fatalf("Authorization = %q, want the management key", seen[0].Auth)
	}
	// The spend cap is what actually bounds a device's cost, so it must ride on
	// the key itself.
	if seen[0].Body["name"] != "foxy-dev-1" || seen[0].Body["limit"] != 25.0 ||
		seen[0].Body["limit_reset"] != "monthly" {
		t.Fatalf("key body = %+v, want name + limit + limit_reset", seen[0].Body)
	}
	// A zero ExpiresAt must be omitted, not sent as year 0001 (which would
	// expire the key the instant it is created).
	if _, ok := seen[0].Body["expires_at"]; ok {
		t.Fatalf("key body carries expires_at %v for a zero time", seen[0].Body["expires_at"])
	}
}

// Verified live: /keys accepts a `limit` with no `limit_reset` — that's a
// lifetime cap. (The old guardrail path rejected this combination, which is why
// it's worth pinning now that the guardrail is gone.)
func TestLifetimeSpendCapIsAllowed(t *testing.T) {
	c, f := newFake(t, happyHandler)
	if _, err := c.DeriveDeviceKey(context.Background(), DeriveSpec{
		KeyName: "foxy-dev-1", LimitUSD: 25,
	}); err != nil {
		t.Fatalf("DeriveDeviceKey: %v", err)
	}
	body := f.seen()[0].Body
	if body["limit"] != 25.0 {
		t.Fatalf("key body = %+v, want the cap", body)
	}
	if _, ok := body["limit_reset"]; ok {
		t.Fatalf("empty limit_reset should be omitted, got %v", body["limit_reset"])
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
	if got := f.seen()[0].Body["expires_at"]; got != "2027-03-04T05:06:07Z" {
		t.Fatalf("expires_at = %v, want RFC3339", got)
	}
}

func TestCreateKeyRefusesAKeyItCannotRevoke(t *testing.T) {
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodPost && c.Path == "/keys" {
			// A live key with no hash: we could never DELETE /keys/{hash} it.
			return http.StatusCreated, `{"key":"sk-or-v1-runtime"}`
		}
		return happyHandler(c)
	})
	if _, err := c.CreateKey(context.Background(), KeySpec{Name: "n"}); err == nil ||
		!strings.Contains(err.Error(), "unrevocable") {
		t.Fatalf("CreateKey err = %v, want a refusal to accept a key with no hash", err)
	}
}

func TestCreateKeyAcceptsEitherResponseShape(t *testing.T) {
	for name, body := range map[string]string{
		"envelope":  `{"key":"sk-1","data":{"hash":"h-1"}}`, // the observed shape
		"flat":      `{"key":"sk-1","hash":"h-1"}`,
		"all-inner": `{"data":{"key":"sk-1","hash":"h-1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := newFake(t, func(c call) (int, string) {
				if c.Method == http.MethodPost && c.Path == "/keys" {
					return http.StatusCreated, body
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
	if _, err := c.CreateKey(context.Background(), KeySpec{Name: "n"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized so the UI can say 'wrong kind of key'", err)
	}
}

// --- revocation -----------------------------------------------------------

func TestRevokeDerivedKey(t *testing.T) {
	c, f := newFake(t, happyHandler)
	if err := c.RevokeDerivedKey(context.Background(), "kh-1"); err != nil {
		t.Fatalf("RevokeDerivedKey: %v", err)
	}
	wantPaths(t, f.paths(), []string{"DELETE /keys/kh-1"})
}

// Verified live: a repeat DELETE returns 404. Revocation must treat that as
// success or a device could never be fully cleaned up after a partial failure.
func TestRepeatedDeleteIsSuccess(t *testing.T) {
	var seen int
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodDelete {
			seen++
			if seen == 1 {
				return http.StatusOK, `{"deleted":true}`
			}
			return http.StatusNotFound, `{"error":{"message":"not found"}}`
		}
		return happyHandler(c)
	})
	if err := c.RevokeDerivedKey(context.Background(), "kh-1"); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := c.RevokeDerivedKey(context.Background(), "kh-1"); err != nil {
		t.Fatalf("repeat = %v, want nil", err)
	}
}

func TestRevokeSurfacesARealFailure(t *testing.T) {
	c, _ := newFake(t, func(c call) (int, string) {
		if c.Method == http.MethodDelete {
			return http.StatusInternalServerError, `{"error":{"message":"boom"}}`
		}
		return happyHandler(c)
	})
	if err := c.RevokeDerivedKey(context.Background(), "kh-1"); err == nil {
		t.Fatal("a key that could not be deleted must surface as an error — it still works")
	}
}

// --- management key probe --------------------------------------------------

// The probe is read-only: it lists keys rather than creating a throwaway
// object. The earlier guardrail-based probe mutated the operator's account and
// could fail for reasons unrelated to the credential.
func TestCheckManagementKeyIsReadOnly(t *testing.T) {
	c, f := newFake(t, happyHandler)
	caps, err := c.CheckManagementKey(context.Background())
	if err != nil {
		t.Fatalf("CheckManagementKey: %v", err)
	}
	if !caps.ManagementKeyValid {
		t.Fatalf("caps = %+v", caps)
	}
	wantPaths(t, f.paths(), []string{"GET /keys"})
	for _, c := range f.seen() {
		if c.Method != http.MethodGet {
			t.Fatalf("probe made a %s request; it must not mutate the account", c.Method)
		}
	}
}

func TestCheckManagementKeyRejectsAnInferenceKey(t *testing.T) {
	c, _ := newFake(t, func(call) (int, string) {
		return http.StatusUnauthorized, `{"error":{"message":"Invalid management key"}}`
	})
	caps, err := c.CheckManagementKey(context.Background())
	if err != nil {
		t.Fatalf("CheckManagementKey: %v", err)
	}
	if caps.ManagementKeyValid {
		t.Fatalf("caps = %+v, want invalid", caps)
	}
	if !strings.Contains(caps.Detail, "provisioning key") {
		t.Fatalf("Detail = %q, should tell the admin which key to paste", caps.Detail)
	}
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
