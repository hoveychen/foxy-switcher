package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// refreshAgainst points ClaudeTokenURL at a stub that returns the given status
// + body, runs RefreshToken, and restores the original URL afterwards.
func refreshAgainst(t *testing.T, status int, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	orig := ClaudeTokenURL
	ClaudeTokenURL = srv.URL
	t.Cleanup(func() { ClaudeTokenURL = orig })

	_, err := RefreshToken(context.Background(), "some-refresh-token")
	return err
}

// A 400 invalid_grant is the terminal case: the error must be classified as
// ErrInvalidGrant so the scheduler can mark the account needs-reauth instead
// of retrying a dead token forever.
func TestRefreshToken_InvalidGrantIsTerminal(t *testing.T) {
	err := refreshAgainst(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refresh token not found"}`)
	if err == nil {
		t.Fatal("expected an error for a 400 invalid_grant response, got nil")
	}
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("expected errors.Is(err, ErrInvalidGrant) == true, got err = %v", err)
	}
}

// A 5xx (or any non-invalid_grant failure) is transient: it must NOT be
// classified as ErrInvalidGrant, so the scheduler keeps retrying.
func TestRefreshToken_ServerErrorIsTransient(t *testing.T) {
	err := refreshAgainst(t, http.StatusInternalServerError, `{"error":"server_error"}`)
	if err == nil {
		t.Fatal("expected an error for a 500 response, got nil")
	}
	if errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("a 500 server_error must NOT be ErrInvalidGrant (it is retryable), got err = %v", err)
	}
}
