// Package selector picks which account in the pool credinject should inject
// next. The strategy is intentionally simple — skip any account that's
// paused, has an expired token, or has reached one of its per-window
// utilization thresholds, then prefer the least-recently-used candidate.
// Swap in a smarter scorer (rate-limit-tier weighting, fancier per-window
// scoring) by replacing Pick.
package selector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// ErrNoAvailable is returned when every account is paused, has an expired
// token, or has reached its utilization threshold on at least one window.
// The credinject Coordinator treats this as the trigger to restore the
// user's native credentials.
var ErrNoAvailable = errors.New("no available account")

// Pick returns the best candidate account at time `now`. It does NOT mutate
// the store; the caller should call store.MarkUsed after the inject succeeds
// so the LRU clock advances.
func Pick(ctx context.Context, st *store.Store, now time.Time) (*store.Account, error) {
	return PickWithFilter(ctx, st, now, "", nil)
}

// PickWithFilter is Pick with an extra disqualifier the caller can apply
// on top of IsEligible. Step 4's vault.InProc.Pick uses this to skip
// accounts another device currently holds a live lease on. `extraSkip`
// returns true to drop the account from the candidate set; nil means
// "no extra filter" and PickWithFilter behaves identically to Pick.
//
// deviceID identifies the picking device so an account pinned for it
// (store.MarkForNextPick with that device) jumps to the front of the
// order. Accounts pinned for OTHER devices get no promotion here — they
// keep their plain LRU position, which is what stops every device's
// reconcile tick from stampeding onto a freshly-pinned account. "" means
// the caller has no device identity; only LRU order applies.
func PickWithFilter(ctx context.Context, st *store.Store, now time.Time, deviceID string, extraSkip func(store.Account) bool) (*store.Account, error) {
	accs, err := st.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	candidates := accs[:0]
	for i := range accs {
		a := accs[i]
		if !IsEligible(a, now) {
			continue
		}
		if extraSkip != nil && extraSkip(a) {
			continue
		}
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailable
	}

	// Pinned-for-this-device first, then least-recently-used. Tie-break by
	// id for stability.
	pinnedForMe := func(a store.Account) bool {
		return deviceID != "" && a.PinnedDeviceID == deviceID
	}
	sort.Slice(candidates, func(i, j int) bool {
		if pi, pj := pinnedForMe(candidates[i]), pinnedForMe(candidates[j]); pi != pj {
			return pi
		}
		if candidates[i].LastUsedAt != candidates[j].LastUsedAt {
			return candidates[i].LastUsedAt < candidates[j].LastUsedAt
		}
		return candidates[i].ID < candidates[j].ID
	})
	return &candidates[0], nil
}

// IsEligible reports whether `a` could be the injected account at time `now`.
// Used by both Pick (for the rotation candidate set) and the credinject
// coordinator's manual mode (which keeps the current account only while
// it's still eligible).
func IsEligible(a store.Account, now time.Time) bool {
	if a.Status != "active" {
		return false
	}
	if a.TokenExpired(now) {
		return false
	}
	if exceedsThreshold(a) {
		return false
	}
	return true
}

// exceedsThreshold reports whether any usage window has reached its
// per-account threshold. Windows with empty resets_at have not been measured
// yet (cold start, or the API didn't return that window) and are skipped so
// the selector doesn't lock everyone out before the first usage poll lands.
func exceedsThreshold(a store.Account) bool {
	if a.FiveHourResetsAt != "" && a.FiveHourUtil >= a.FiveHourThreshold {
		return true
	}
	if a.SevenDayResetsAt != "" && a.SevenDayUtil >= a.SevenDayThreshold {
		return true
	}
	if a.SevenDaySonnetResetsAt != "" && a.SevenDaySonnetUtil >= a.SevenDaySonnetThreshold {
		return true
	}
	return false
}
