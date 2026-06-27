package anthropic

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fetchUsageAgainst points BaseURL at a stub returning the given status + body
// for /api/oauth/usage, runs FetchUsage, and restores BaseURL afterwards.
func fetchUsageAgainst(t *testing.T, status int, body string) error {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	orig := BaseURL
	BaseURL = srv.URL
	t.Cleanup(func() { BaseURL = orig })

	_, err := FetchUsage(context.Background(), "tok")
	return err
}

// A 403 permission_error is the org-level OAuth block: it must surface as
// *OrgDisabledError so the poller can flag the account distinctly instead of
// treating it like a transient error.
func TestFetchUsage_OrgDisabledIsTyped(t *testing.T) {
	body := `{"type":"error","error":{"type":"permission_error","message":"OAuth authentication is currently not allowed for this organization.","details":{"error_visibility":"user_facing"}}}`
	err := fetchUsageAgainst(t, http.StatusForbidden, body)
	if err == nil {
		t.Fatal("expected an error for a 403 permission_error response, got nil")
	}
	var od *OrgDisabledError
	if !errors.As(err, &od) {
		t.Fatalf("expected errors.As(err, *OrgDisabledError) == true, got err = %v", err)
	}
}

// A 403 that is NOT a permission_error (e.g. some other forbidden reason) must
// NOT be classified as OrgDisabled — only the org-level OAuth block qualifies.
func TestFetchUsage_OtherForbiddenIsNotOrgDisabled(t *testing.T) {
	err := fetchUsageAgainst(t, http.StatusForbidden, `{"type":"error","error":{"type":"not_found_error","message":"nope"}}`)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var od *OrgDisabledError
	if errors.As(err, &od) {
		t.Fatalf("a non-permission_error 403 must NOT be OrgDisabledError, got err = %v", err)
	}
}
