package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Tauri webview loads the React UI from http://127.0.0.1:1420 (vite) in
// dev and from tauri://localhost (custom protocol) in release. Either way, the
// fetch to http://127.0.0.1:<sidecar-port>/api/* is cross-origin and the
// browser blocks it without proper CORS headers.

func TestCORS_PreflightAllowsLocalhost(t *testing.T) {
	srv := New(nil, nil, nil)
	req := httptest.NewRequest(http.MethodOptions, "/api/accounts/login", nil)
	req.Header.Set("Origin", "http://127.0.0.1:1420")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent && w.Code != http.StatusOK {
		t.Fatalf("preflight: want 204/200, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Errorf("preflight: missing Access-Control-Allow-Origin (got headers: %v)", w.Header())
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); got == "" {
		t.Errorf("preflight: missing Access-Control-Allow-Methods (got headers: %v)", w.Header())
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got == "" {
		t.Errorf("preflight: missing Access-Control-Allow-Headers (got headers: %v)", w.Header())
	}
}

func TestCORS_SimpleResponseHasOriginHeader(t *testing.T) {
	srv := New(nil, nil, nil)
	// /healthz is a simple GET — won't trigger a preflight, but the response
	// still needs Access-Control-Allow-Origin or the browser swallows it.
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.Header.Set("Origin", "http://127.0.0.1:1420")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/healthz: want 200, got %d body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got == "" {
		t.Errorf("/healthz: missing Access-Control-Allow-Origin (got headers: %v)", w.Header())
	}
}
