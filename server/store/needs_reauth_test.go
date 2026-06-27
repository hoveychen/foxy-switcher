package store

import (
	"context"
	"testing"
)

// MarkNeedsReauth must persist StatusNeedsReauth so the selector stops routing
// to the account and the UI can surface "needs re-authentication".
func TestMarkNeedsReauth(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "alice", Email: "alice@example.com", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if a.Status != "" && a.Status != StatusActive {
		// freshly-inserted rows default to active in the DB
	}

	if err := st.MarkNeedsReauth(ctx, a.ID); err != nil {
		t.Fatalf("MarkNeedsReauth: %v", err)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusNeedsReauth {
		t.Fatalf("status = %q, want %q", got.Status, StatusNeedsReauth)
	}
}

// SetStatus must reject needs_reauth: it's a system verdict, not a user toggle.
func TestSetStatusRejectsNeedsReauth(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "bob", Email: "bob@example.com", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := st.SetStatus(ctx, a.ID, StatusNeedsReauth); err == nil {
		t.Fatal("expected SetStatus(needs_reauth) to be rejected, got nil error")
	}
}
