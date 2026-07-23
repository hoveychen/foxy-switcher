// Package selector picks which account in the pool credinject should inject
// next. The strategy is intentionally simple — skip any account that's
// paused, has an expired token, or has reached a HARD per-window threshold
// (5h / 7d, which cap the whole account), then prefer the candidate with the
// most weekly runway (lowest 7-day utilization), breaking ties
// least-recently-used.
//
// The per-model weekly-scoped window (seven_day_sonnet slot — Fable/…) is a
// SOFT cap: reaching it does not make the account unavailable (only that one
// scoped model is throttled; every other model still works), so a
// scoped-exceeded account is kept as a low-priority fallback rather than
// excluded. Pick orders scoped-OK accounts ahead of scoped-exceeded ones and
// only falls back to a degraded account when no scoped-OK one is eligible.
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
// token, or has reached a HARD utilization threshold (5h / 7d). A
// scoped-model (Fable) cap alone does NOT trigger this — such accounts stay
// in the pool as degraded fallbacks. The credinject Coordinator treats this
// as the trigger to restore the user's native credentials.
var ErrNoAvailable = errors.New("no available account")

// Pick returns the best candidate account at time `now`. It does NOT mutate
// the store; the caller should call store.MarkUsed after the inject succeeds
// so the LRU clock advances.
func Pick(ctx context.Context, st *store.Store, now time.Time) (*store.Account, error) {
	return PickProviderWithFilter(ctx, st, store.ProviderClaude, now, "", nil)
}

// PickProvider selects from exactly one provider pool.
func PickProvider(ctx context.Context, st *store.Store, provider string, now time.Time) (*store.Account, error) {
	return PickProviderWithFilter(ctx, st, provider, now, "", nil)
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
	return PickProviderWithFilter(ctx, st, store.ProviderClaude, now, deviceID, extraSkip)
}

// PickProviderWithFilter is PickWithFilter scoped to one provider pool.
func PickProviderWithFilter(ctx context.Context, st *store.Store, provider string, now time.Time, deviceID string, extraSkip func(store.Account) bool) (*store.Account, error) {
	accs, err := st.ListProvider(ctx, provider)
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

	// Pinned-for-this-device first, then the account with the most long-window
	// runway (lowest 7-day utilization), then least-recently-used, tie-broken
	// by id for stability. The 7-day window is Claude's weekly cap; for Codex
	// the usage poller stores the weekly "secondary" window in the same
	// SevenDayUtil field, so this ranking prefers weekly headroom for both
	// providers rather than shortest-window (5h / primary) headroom.
	pinnedForMe := func(a store.Account) bool {
		return deviceID != "" && a.PinnedDeviceID == deviceID
	}
	sort.Slice(candidates, func(i, j int) bool {
		if pi, pj := pinnedForMe(candidates[i]), pinnedForMe(candidates[j]); pi != pj {
			return pi
		}
		// Scoped-OK accounts outrank scoped-exceeded (degraded) ones: a
		// degraded account still serves non-scoped models but is only a
		// fallback, so it sorts after every scoped-OK candidate regardless of
		// runway. Runway/LRU only break ties WITHIN a tier.
		if di, dj := scopedThreshold(candidates[i]), scopedThreshold(candidates[j]); di != dj {
			return !di // non-degraded (di == false) ranks first
		}
		if candidates[i].SevenDayUtil != candidates[j].SevenDayUtil {
			return candidates[i].SevenDayUtil < candidates[j].SevenDayUtil
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
	// Only HARD windows (5h / 7d) disqualify an account — they cap the whole
	// account, so hitting them means a 429 for any model. A scoped-model
	// (Fable) cap is soft: the account stays eligible and Pick merely
	// deprioritises it (see scopedThreshold + the tier sort).
	if hardThreshold(a) {
		return false
	}
	return true
}

// hardThreshold reports whether a HARD usage window (5h or 7d) has reached its
// per-account threshold. These caps apply account-wide — Anthropic 429s the
// account for every model once they're hit — so a hard-exceeded account is
// genuinely unavailable and IsEligible drops it. Windows with empty resets_at
// have not been measured yet (cold start, or the API didn't return that
// window) and are skipped so the selector doesn't lock everyone out before the
// first usage poll lands.
func hardThreshold(a store.Account) bool {
	if a.FiveHourResetsAt != "" && a.FiveHourUtil >= a.FiveHourThreshold {
		return true
	}
	if a.SevenDayResetsAt != "" && a.SevenDayUtil >= a.SevenDayThreshold {
		return true
	}
	return false
}

// scopedThreshold reports whether the per-model weekly-scoped window
// (seven_day_sonnet slot — Fable/…; see store.Account.SevenDaySonnetUtil) has
// reached its threshold. This is a SOFT cap: only that one scoped model is
// throttled, so the account remains usable for every other model. It does NOT
// make the account ineligible — Pick uses it purely to rank scoped-exceeded
// accounts below scoped-OK ones. Unmeasured windows (empty resets_at) count as
// not-exceeded.
func scopedThreshold(a store.Account) bool {
	return a.SevenDaySonnetResetsAt != "" && a.SevenDaySonnetUtil >= a.SevenDaySonnetThreshold
}
