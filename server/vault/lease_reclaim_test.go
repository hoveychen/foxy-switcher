package vault

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// TestPickForDevice_ReclaimsIdleLeaseUnderPressure is the end-to-end of
// idle-reclaim at the vault boundary: with a single-account pool held by an
// idle device, an active pool-starved seeker's Pick reclaims the idle lease and
// returns the account. The control half proves reclaim is strictly under
// pressure AND only for idle holders: while the holder is active, the same Pick
// still returns ErrNoAvailable.
func TestPickForDevice_ReclaimsIdleLeaseUnderPressure(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()

	a := seedAccount(t, st, "alpha") // the only account in the pool

	lease, err := svc.AcquireLease(ctx, a.ID, "device-other", time.Minute)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	// Holder is active (fresh acquire) → a starved seeker gets nothing.
	if _, err := svc.PickProviderForDevice(ctx, time.Now(), "device-self", store.ProviderClaude); !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("active holder must NOT be reclaimed: want ErrNoAvailable, got %v", err)
	}

	// Holder reports idle beyond the reclaim threshold on its next renew.
	if _, err := svc.RenewLease(ctx, lease.ID, time.Minute, DefaultIdleReclaimThreshold+time.Minute); err != nil {
		t.Fatalf("holder idle renew: %v", err)
	}

	// Now the seeker's Pick reclaims the idle lease and hands over the account.
	got, err := svc.PickProviderForDevice(ctx, time.Now(), "device-self", store.ProviderClaude)
	if err != nil {
		t.Fatalf("reclaim pick: %v", err)
	}
	if got == nil || got.ID != a.ID {
		t.Fatalf("reclaim pick: got %+v want id=%d", got, a.ID)
	}
	if st.IsAccountLeasedByOther(a.ID, "device-self") {
		t.Fatal("idle lease should have been reclaimed (account left free for the seeker)")
	}
	// The seeker can now actually take the freed account.
	if _, err := svc.AcquireLease(ctx, a.ID, "device-self", time.Minute); err != nil {
		t.Fatalf("seeker acquire after reclaim: %v", err)
	}
}

// TestPickForDevice_NoReclaimWhenFreeAccountExists proves reclaim never fires
// when the first pass already succeeds: a free account is always preferred over
// preempting an idle holder, so no lease churn happens outside real pressure.
func TestPickForDevice_NoReclaimWhenFreeAccountExists(t *testing.T) {
	st := openTestStore(t)
	svc := NewInProc(st)
	ctx := context.Background()

	a := seedAccount(t, st, "alpha")
	b := seedAccount(t, st, "beta")

	// device-other holds alpha and is idle — but beta is free.
	lease, err := svc.AcquireLease(ctx, a.ID, "device-other", time.Minute)
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	if _, err := svc.RenewLease(ctx, lease.ID, time.Minute, DefaultIdleReclaimThreshold+time.Minute); err != nil {
		t.Fatalf("holder idle renew: %v", err)
	}

	got, err := svc.PickForDevice(ctx, time.Now(), "device-self")
	if err != nil {
		t.Fatalf("PickForDevice: %v", err)
	}
	if got == nil || got.ID != b.ID {
		t.Fatalf("must pick the free account beta, not reclaim alpha: got %+v", got)
	}
	// alpha's idle lease is untouched — no gratuitous reclaim.
	if !st.IsAccountLeasedByOther(a.ID, "device-self") {
		t.Fatal("idle holder's lease must survive when a free account was available")
	}
}
