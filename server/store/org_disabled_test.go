package store

import (
	"context"
	"testing"
)

// MarkOrgDisabled must persist StatusOrgDisabled so the selector stops routing
// to the account and the UI can surface "organization OAuth disabled".
func TestMarkOrgDisabled(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "org", Email: "org@example.com", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := st.MarkOrgDisabled(ctx, a.ID); err != nil {
		t.Fatalf("MarkOrgDisabled: %v", err)
	}

	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusOrgDisabled {
		t.Fatalf("status = %q, want %q", got.Status, StatusOrgDisabled)
	}
}
