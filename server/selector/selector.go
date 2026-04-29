// Package selector picks which account in the pool should service the next
// /api/token request. The strategy is intentionally simple — avoid any
// account that's still inside its 429 cooldown window, then prefer the
// least-recently-used candidate. Swap in a smarter scorer (rate-limit-tier
// weighting, usage-API integration) by replacing Pick.
package selector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

// ErrNoAvailable is returned when every account is either disabled or still
// in cooldown. The HTTP layer maps this to 503 Service Unavailable.
var ErrNoAvailable = errors.New("no available account")

// Pick returns the best candidate account at time `now`. It does NOT mutate
// the store; the caller should call store.MarkUsed after handing the token
// out, so the LRU clock advances.
func Pick(ctx context.Context, st *store.Store, now time.Time) (*store.Account, error) {
	accs, err := st.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list accounts: %w", err)
	}
	nowMs := now.UnixMilli()

	candidates := accs[:0]
	for i := range accs {
		a := accs[i]
		if a.Status != "active" {
			continue
		}
		if a.CooldownUntil > nowMs {
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
