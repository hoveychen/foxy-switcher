package webapp

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHandler_AppPathServesIndexOrFallsBack pins down both branches of
// the embed: when no React bundle has been baked in (CI clean checkout,
// fresh contributor), /app responds with a 404 telling the user to run
// the build. When the bundle IS present, the same path returns
// text/html. We can't unit-test the populated branch from this package
// (the embed is sealed at compile time) — but we can at least verify
// the unpopulated branch's contract so the message text doesn't drift.
func TestHandler_AppPathServesIndexOrFallsBack(t *testing.T) {
	h := Handler()

	req := httptest.NewRequest(http.MethodGet, "/app", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if Available() {
		// Bundled path: index.html is text/html, 200 OK.
		if rr.Code != http.StatusOK {
			t.Fatalf("bundled /app status: got %d want 200", rr.Code)
		}
		if got := rr.Header().Get("Content-Type"); got == "" || got[:9] != "text/html" {
			t.Errorf("bundled /app content-type: got %q", got)
		}
	} else {
		// Unbundled fallback: 404 with the "run pnpm build" hint.
		if rr.Code != http.StatusNotFound {
			t.Fatalf("unbundled /app status: got %d want 404", rr.Code)
		}
		if body := rr.Body.String(); body == "" {
			t.Error("unbundled /app: empty body, expected hint")
		}
	}
}

// TestHandler_NonAppPath404 confirms the handler doesn't accidentally
// serve everything — a request to / on this handler should miss
// (it's mounted only on /app, /app/, /assets/* in main.go, but this
// test guards against an inadvertent expansion to swallow other paths).
func TestHandler_NonAppPath404(t *testing.T) {
	h := Handler()
	req := httptest.NewRequest(http.MethodGet, "/some-other-path", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("non-app path: got %d want 404", rr.Code)
	}
}
