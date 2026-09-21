package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// statusRecorder wraps a RoundTripper to remember the status of every
// response, so a test can prove the second poll was a 304 and not just
// another full download that happened to return the same data.
type statusRecorder struct {
	next     http.RoundTripper
	statuses []int
}

func (s *statusRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.next.RoundTrip(req)
	if resp != nil {
		s.statuses = append(s.statuses, resp.StatusCode)
	}
	return resp, err
}

// TestListAccountsRevalidates is the agent half of the 2026-09-21 egress fix:
// a repeated poll must come back as a 304 and still yield the full account
// list from cache.
func TestListAccountsRevalidates(t *testing.T) {
	f := newRoundtripFixture(t)
	seedAccount(t, f.st, "one")
	rec := &statusRecorder{next: http.DefaultTransport}
	f.client.hc.Transport = rec
	ctx := context.Background()

	first, err := f.client.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("first ListAccounts: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first ListAccounts: %d accounts, want 1", len(first))
	}

	second, err := f.client.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("second ListAccounts: %v", err)
	}
	if len(second) != 1 || second[0].Name != first[0].Name {
		t.Fatalf("second ListAccounts returned %#v, want the cached copy of %#v", second, first)
	}
	// The tokens are the reason the payload is 33KB; a cache that dropped
	// them would break credential injection silently.
	if second[0].AccessToken != first[0].AccessToken {
		t.Errorf("cached account lost its access token: %q vs %q", second[0].AccessToken, first[0].AccessToken)
	}
	if want := []int{http.StatusOK, http.StatusNotModified}; !equalInts(rec.statuses, want) {
		t.Errorf("statuses %v, want %v — the second poll re-downloaded the body", rec.statuses, want)
	}

	// A change upstream must break through the cache.
	seedAccount(t, f.st, "two")
	third, err := f.client.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("third ListAccounts: %v", err)
	}
	if len(third) != 2 {
		t.Errorf("third ListAccounts: %d accounts, want 2 — cache served a stale pool", len(third))
	}
}

// TestListAccountsRejects304WithoutCache covers the defensive branch: a vault
// that answers 304 for a body we never stored must produce an error and clear
// the tag, so the next tick asks unconditionally instead of looping.
func TestListAccountsRejects304WithoutCache(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	}))
	defer srv.Close()
	c := New(srv.URL)
	c.accounts.etag = `"tag-we-never-got-a-body-for"`

	if _, err := c.ListAccounts(context.Background()); err == nil {
		t.Fatal("ListAccounts succeeded on an unsatisfiable 304, want an error")
	}
	if c.accounts.etag != "" {
		t.Errorf("stale tag %q survived; the next poll would 304 again", c.accounts.etag)
	}
}

// TestListAccountsAgainstVaultWithoutETags is the other half of the rollout
// contract. An updated agent may well reach a vault that predates the ETag —
// the two halves deploy separately — and that vault ignores If-None-Match and
// answers 200 with no tag. The agent must simply keep working, caching
// nothing, rather than treating the missing header as an error or a hit.
func TestListAccountsAgainstVaultWithoutETags(t *testing.T) {
	var conditionalSeen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			conditionalSeen++
		}
		// No ETag header, and no notion of 304 — an older vault.
		w.Header().Set("Content-Type", "application/json")
		// Field names match store.Account's Go names — it carries no json tags.
		_, _ = w.Write([]byte(`{"accounts":[{"Name":"legacy","AccessToken":"at-legacy"}]}`))
	}))
	defer srv.Close()
	c := New(srv.URL)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		accs, err := c.ListAccounts(ctx)
		if err != nil {
			t.Fatalf("poll %d against an ETag-less vault: %v", i, err)
		}
		if len(accs) != 1 || accs[0].Name != "legacy" {
			t.Fatalf("poll %d: got %#v, want the one account", i, accs)
		}
		if accs[0].AccessToken != "at-legacy" {
			t.Errorf("poll %d: access token %q, want it decoded", i, accs[0].AccessToken)
		}
	}
	if conditionalSeen != 0 {
		t.Errorf("sent %d conditional requests to a vault that never gave us a tag", conditionalSeen)
	}
}

func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
