package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestVaultProxy_InjectsBearerAndForwardsBody asserts the two contracts
// the agent's reverse proxy is built around: every forwarded request
// carries the agent's bearer token, and request bodies / methods / paths
// reach the upstream verbatim. Without these, /api/accounts/{id}/pause
// (POST with empty body) and /api/accounts/login (POST with JSON body)
// would silently break.
func TestVaultProxy_InjectsBearerAndForwardsBody(t *testing.T) {
	var (
		gotAuth string
		gotPath string
		gotBody string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		buf, _ := io.ReadAll(r.Body)
		gotBody = string(buf)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	proxy := newVaultProxy(target, "secret-token")

	req := httptest.NewRequest(http.MethodPost,
		"http://agent.local/api/accounts/login",
		strings.NewReader(`{"pasted":"abc#xyz","state":"xyz"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200, body=%s", rr.Code, rr.Body.String())
	}
	if gotAuth != "Bearer secret-token" {
		t.Errorf("Authorization header: got %q want %q", gotAuth, "Bearer secret-token")
	}
	if gotPath != "/api/accounts/login" {
		t.Errorf("path: got %q want /api/accounts/login", gotPath)
	}
	if gotBody != `{"pasted":"abc#xyz","state":"xyz"}` {
		t.Errorf("body: got %q", gotBody)
	}
}

// TestVaultProxy_UpstreamDownReturns502 confirms the ErrorHandler hook
// surfaces a JSON 502 when the vault is unreachable. Without it, the
// frontend would see a Go-default plain-text 502 and fail to render the
// "vault unreachable" toast cleanly.
func TestVaultProxy_UpstreamDownReturns502(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:1") // port 1 → connection refused
	proxy := newVaultProxy(target, "tok")
	req := httptest.NewRequest(http.MethodGet, "http://agent.local/api/accounts", nil)
	rr := httptest.NewRecorder()
	proxy.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d want 502", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "vault unreachable") {
		t.Errorf("body missing vault-unreachable signal: %s", rr.Body.String())
	}
}
