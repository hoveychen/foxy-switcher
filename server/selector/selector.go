// Package selector picks which account in the pool credinject should inject
// next. The strategy is intentionally simple — avoid any account that's still
// inside its 429 cooldown window, then prefer the least-recently-used
// candidate. Swap in a smarter scorer (rate-limit-tier weighting, usage-API
// integration) by replacing Pick.
package selector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// ErrNoAvailable is returned when every account is either paused or still
// in cooldown. The credinject Coordinator treats this as the trigger to
// restore the user's native credentials.
var ErrNoAvailable = errors.New("no available account")

// Pick returns the best candidate account at time `now`. It does NOT mutate
// the store; the caller should call store.MarkUsed after the inject succeeds
// so the LRU clock advances.
func Pick(ctx context.Context, st *store.Store, now time.Time) (*store.Account, error) {
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
		candidates = append(candidates, a)
	}
	if len(candidates) == 0 {
		return nil, ErrNoAvailable
	}

	// Least-recently-used wins. Tie-break by id for stability.
	sort.Slice(candidates, func(i, j int) bool {
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
	if a.CooldownUntil > now.UnixMilli() {
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
