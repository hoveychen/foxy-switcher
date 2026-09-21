package httpserver

import (
	"net/http"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// getConditional issues a bearer GET carrying If-None-Match when tag is
// non-empty, returning the status and the response's own ETag.
func getConditional(t *testing.T, url, token, tag string) (int, string, int64) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if tag != "" {
		req.Header.Set("If-None-Match", tag)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("ETag"), resp.ContentLength
}

// TestAgentAccountsETagRevalidates is the bandwidth contract behind the
// 2026-09-21 egress incident: an agent re-reading the pool on its 5s tick must
// get a bodiless 304 while nothing has changed, and a fresh 200 the moment it
// has.
func TestAgentAccountsETagRevalidates(t *testing.T) {
	f := newAuthFixture(t)
	token := deviceWithAllowlist(t, f.st, "dev-etag", true, false, false)
	seedProviderAccount(t, f.st, store.ProviderClaude, "one")
	url := f.server.URL + "/agent/v1/accounts"

	status, tag, _ := getConditional(t, url, token, "")
	if status != http.StatusOK {
		t.Fatalf("first GET: status %d, want 200", status)
	}
	if tag == "" {
		t.Fatal("first GET: no ETag — the agent has nothing to revalidate with")
	}

	status, echoed, length := getConditional(t, url, token, tag)
	if status != http.StatusNotModified {
		t.Fatalf("revalidated GET: status %d, want 304", status)
	}
	if echoed != tag {
		t.Errorf("revalidated GET: ETag %q, want the same %q", echoed, tag)
	}
	if length > 0 {
		t.Errorf("304 carried %d bytes of body; the whole point is that it doesn't", length)
	}

	// A change to the pool must invalidate the tag, or agents would sit on a
	// stale account list indefinitely.
	seedProviderAccount(t, f.st, store.ProviderClaude, "two")
	status, fresh, _ := getConditional(t, url, token, tag)
	if status != http.StatusOK {
		t.Fatalf("GET after pool change: status %d, want 200", status)
	}
	if fresh == tag {
		t.Error("pool gained an account but the ETag did not change")
	}
}

// TestAgentAccountsETagIsPerDevice guards the claim that makes the 304 safe:
// the tag covers the bytes AFTER the provider allowlist filter, so one
// device's tag can never let another device skip a body it would have seen
// differently. Both devices see a different slice of the same pool here.
func TestAgentAccountsETagIsPerDevice(t *testing.T) {
	f := newAuthFixture(t)
	claudeOnly := deviceWithAllowlist(t, f.st, "dev-claude", true, false, false)
	bothProviders := deviceWithAllowlist(t, f.st, "dev-both", true, false, true)
	seedProviderAccount(t, f.st, store.ProviderClaude, "c1")
	seedProviderAccount(t, f.st, store.ProviderOpenRouter, "o1")
	url := f.server.URL + "/agent/v1/accounts"

	_, narrowTag, _ := getConditional(t, url, claudeOnly, "")
	_, wideTag, _ := getConditional(t, url, bothProviders, "")
	if narrowTag == "" || wideTag == "" {
		t.Fatal("both devices must get an ETag")
	}
	if narrowTag == wideTag {
		t.Fatal("devices with different allowlists share an ETag — a 304 could hide rows one of them should see")
	}

	// Cross-feeding the other device's tag must not short-circuit.
	if status, _, _ := getConditional(t, url, claudeOnly, wideTag); status != http.StatusOK {
		t.Errorf("claude-only device got %d for another device's tag, want 200", status)
	}
}
