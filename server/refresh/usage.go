package refresh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/anthropic"
	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// UsageInterval is how often the usage poller wakes up. Five minutes matches
// claude-fleet's cadence and is plenty for windows that reset on 5h / 7d
// boundaries — the bars never look stale to a human.
const UsageInterval = 5 * time.Minute

// UsagePoller fetches /api/oauth/usage for every account on its tick
// and writes the snapshot to the store. It runs alongside the token-refresh
// Scheduler but is intentionally a separate goroutine: token rotation has a
// strict serialisation guarantee per account (Anthropic invalidates the
// loser of a refresh race), whereas usage reads are idempotent and can run
// concurrently across accounts.
type UsagePoller struct {
	st     *store.Store
	stop   chan struct{}
	done   chan struct{}
	logger *log.Logger

	// Interval is the ticker period. Defaults to UsageInterval when zero.
	// Wired from store.Settings.UsagePollIntervalSec at daemon start so the
	// user's preference takes effect on the next launch.
	Interval time.Duration

	// OnChange fires once per tick if any account's usage row was updated.
	// The credinject Coordinator uses it to reconcile the keychain
	// immediately when, e.g., a 5h window resets and an account becomes
	// usable again. Must be set before Start; safe to leave nil.
	OnChange func()

	// Bus is the optional activity hub. We emit one usage.polled summary
	// per tick (not per account, to avoid log spam) and per-account
	// error.usage events on failure.
	Bus *activity.Bus

	// mu guards nextAllowedAt. Held only for the duration of map ops; the
	// HTTP fetch happens outside the lock so concurrent tick goroutines
	// (there shouldn't be any today, but the field is per-account anyway)
	// would not serialise on each other.
	mu            sync.Mutex
	nextAllowedAt map[int64]time.Time
}

// canPoll reports whether `id` is currently outside any 429 backoff window.
// Expired entries are GC'd here so the map can't grow without bound.
func (p *UsagePoller) canPoll(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.nextAllowedAt[id]
	if !ok {
		return true
	}
	if time.Now().Before(until) {
		return false
	}
	delete(p.nextAllowedAt, id)
	return true
}

// markBackoff records a per-account "do not poll until" timestamp. Called
// from tick when /api/oauth/usage returns 429; d comes from
// (*anthropic.RateLimitError).RetryAfter, which is already clamped.
func (p *UsagePoller) markBackoff(id int64, d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nextAllowedAt == nil {
		p.nextAllowedAt = make(map[int64]time.Time)
	}
	p.nextAllowedAt[id] = time.Now().Add(d)
}

// clearBackoff drops any pending backoff for `id`. Called on a successful
// poll (window naturally rolled over) and exposed for tests.
func (p *UsagePoller) clearBackoff(id int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.nextAllowedAt, id)
}

func NewUsagePoller(st *store.Store, logger *log.Logger) *UsagePoller {
	if logger == nil {
		logger = log.Default()
	}
	return &UsagePoller{
		st:     st,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logger: logger,
	}
}

// Start launches the poll goroutine. An immediate sweep runs before the
// first ticker fire so the UI doesn't have to wait 5 minutes for any usage
// data after a sidecar restart.
func (p *UsagePoller) Start(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = UsageInterval
	}
	go func() {
		defer close(p.done)
		p.tick(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case <-t.C:
				p.tick(ctx)
			}
		}
	}()
}

func (p *UsagePoller) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

func (p *UsagePoller) tick(ctx context.Context) {
	accs, err := p.st.List(ctx)
	if err != nil {
		p.logger.Printf("[usage] list accounts: %v", err)
		return
	}
	changed := false
	polled := 0
	for i := range accs {
		a := accs[i]
		// Paused accounts (Status="paused") are still polled: pause means
		// "do not select for routing", not "stop maintaining the card". The
		// usage bars and profile must stay current so the resume action shows
		// fresh data.
		if a.AccessToken == "" {
			continue
		}
		// OpenRouter exposes no subscription usage windows (it's pay-as-you-go;
		// spend is capped by guardrails), and it has no access_token on the row at
		// all — so the AccessToken=="" check above already skips it. Explicit
		// guard so a future field on the row can't accidentally route an
		// OpenRouter account into Anthropic's /api/oauth/usage.
		if a.Provider == store.ProviderOpenRouter {
			continue
		}
		// Skip accounts whose token expired and hasn't been rotated yet —
		// the usage call would 401, and the token-refresh Scheduler will
		// pick it up on its own cadence.
		if a.ExpiresAt > 0 && a.ExpiresAt < time.Now().UnixMilli() {
			continue
		}
		// Honor any active 429 backoff window. We log nothing here so the
		// activity bus stays quiet on subsequent ticks — the original 429
		// already produced an error.usage event the user can see.
		if !p.canPoll(a.ID) {
			continue
		}
		if a.Provider == store.ProviderCodex {
			if p.pollCodex(ctx, a) {
				polled++
				changed = true
			}
			continue
		}
		// Backfill profile for accounts that predate the profile-fetching
		// feature, or that predate rate_limit_tier. Plan / AccountUUID /
		// RateLimitTier all get backfilled together since they come from
		// the same /api/oauth/profile call. We only run this when one of
		// them is missing so the per-tick cost stays one HTTP call per
		// account in steady state.
		//
		// Also re-fetch when the stored Plan label is inconsistent with
		// what DerivePlan would produce from the stored
		// SubscriptionType + RateLimitTier — that's a sign the row was
		// written by an older FetchProfile algorithm (e.g. before the
		// 5x / 20x suffix was introduced) and needs a one-shot migration.
		// Once the labels reconcile, the poll cost drops back to one call.
		stalePlan := a.SubscriptionType != "" && a.RateLimitTier != "" &&
			a.Plan != anthropic.DerivePlan(a.SubscriptionType, a.RateLimitTier)
		if a.Plan == "" || a.AccountUUID == "" || a.RateLimitTier == "" || stalePlan {
			if prof, err := anthropic.FetchProfile(ctx, a.AccessToken); err != nil {
				p.logger.Printf("[usage] account %d profile backfill: %v", a.ID, err)
			} else if err := p.st.SetProfile(ctx, a.ID,
				prof.AccountUUID,
				prof.Email, prof.FullName, prof.OrganizationName,
				prof.Plan, prof.SubscriptionType, prof.RateLimitTier,
			); err != nil {
				p.logger.Printf("[usage] account %d profile store: %v", a.ID, err)
			}
		}
		u, err := anthropic.FetchUsage(ctx, a.AccessToken)
		if err != nil {
			var rl *anthropic.RateLimitError
			if errors.As(err, &rl) {
				// 429 is a known degraded state, not a failure: the
				// canPoll guard above will skip this account on every
				// subsequent tick until the window passes, so this info
				// event fires exactly once per backoff period.
				p.markBackoff(a.ID, rl.RetryAfter)
				p.logger.Printf("[usage] account %d (%s): rate limited, paused for %s",
					a.ID, a.Name, rl.RetryAfter)
				p.Bus.EmitInfo(activity.TypeUsageBackoff, a.ID,
					fmt.Sprintf("Usage poll for %s paused for %s (rate limited)",
						a.Name, rl.RetryAfter))
				continue
			}
			var od *anthropic.OrgDisabledError
			if errors.As(err, &od) {
				// Org-level OAuth block (403 permission_error): every API call
				// for this account is rejected, so routing to it just breaks the
				// user's session. Flag it so the selector excludes it; re-login
				// can't fix an org policy. Only act on the active→disabled edge
				// so we emit one event per transition, not every tick — and a
				// later successful poll (org re-enabled) auto-heals it below.
				if a.Status != store.StatusOrgDisabled {
					if mErr := p.st.MarkOrgDisabled(ctx, a.ID); mErr != nil {
						p.logger.Printf("[usage] account %d (%s): mark org_disabled: %v", a.ID, a.Name, mErr)
					}
					p.Bus.EmitError(activity.TypeAccountOrgDisabled, a.ID,
						fmt.Sprintf("%s disabled — its organization has OAuth access turned off (re-login won't fix this)", a.Name))
				}
				continue
			}
			p.logger.Printf("[usage] account %d (%s): %v", a.ID, a.Name, err)
			p.Bus.EmitError(activity.TypeErrorUsage, a.ID,
				fmt.Sprintf("Usage poll for %s failed: %v", a.Name, err))
			continue
		}
		// Successful poll — drop any stale backoff so a recovered account
		// resumes its normal cadence immediately.
		p.clearBackoff(a.ID)
		// Auto-heal: a poll succeeding on a previously org-disabled account means
		// the org re-enabled OAuth, so restore it to active and let routing resume.
		if a.Status == store.StatusOrgDisabled {
			if sErr := p.st.SetStatus(ctx, a.ID, store.StatusActive); sErr != nil {
				p.logger.Printf("[usage] account %d (%s): clear org_disabled: %v", a.ID, a.Name, sErr)
			} else {
				p.Bus.EmitInfo(activity.TypeAccountResumed, a.ID,
					fmt.Sprintf("%s re-enabled — organization OAuth access restored", a.Name))
			}
		}
		fhU, sdU, ssU := flattenUsage(u)
		if err := writeUsage(ctx, p.st, a.ID, u); err != nil {
			p.logger.Printf("[usage] account %d store: %v", a.ID, err)
			continue
		}
		// Append a history row so the Dashboard 24h trend can render. Failure
		// is non-fatal — the live row in `accounts` is the source of truth;
		// history is decorative.
		if err := p.st.AppendUsageHistory(ctx, a.ID, time.Now().UnixMilli(), fhU, sdU, ssU); err != nil {
			p.logger.Printf("[usage] account %d history: %v", a.ID, err)
		}
		polled++
		changed = true
	}
	if polled > 0 {
		p.Bus.EmitInfo(activity.TypeUsagePolled, 0,
			fmt.Sprintf("Refreshed usage for %d account%s", polled, pluralS(polled)))
	}
	if changed && p.OnChange != nil {
		p.OnChange()
	}
}

func (p *UsagePoller) pollCodex(ctx context.Context, a store.Account) bool {
	u, err := openai.FetchUsage(ctx, a.AccessToken, a.AccountUUID)
	if err != nil {
		p.logger.Printf("[usage] Codex account %d (%s): %v", a.ID, a.Name, err)
		p.Bus.EmitError(activity.TypeErrorUsage, a.ID,
			fmt.Sprintf("Usage poll for %s failed: %v", a.Name, err))
		return false
	}
	var primaryUtil, secondaryUtil float64
	var primaryReset, secondaryReset string
	if u.Primary != nil {
		primaryUtil = u.Primary.UsedPercent
		primaryReset = u.Primary.ResetAt.Format(time.RFC3339)
	}
	if u.Secondary != nil {
		secondaryUtil = u.Secondary.UsedPercent
		secondaryReset = u.Secondary.ResetAt.Format(time.RFC3339)
	}
	if err := p.st.SetUsage(ctx, a.ID,
		primaryUtil, primaryReset, secondaryUtil, secondaryReset, 0, "", ""); err != nil {
		p.logger.Printf("[usage] Codex account %d store: %v", a.ID, err)
		return false
	}
	if u.PlanType != "" && u.PlanType != a.SubscriptionType {
		_ = p.st.SetProfile(ctx, a.ID, a.AccountUUID, a.Email, a.FullName,
			a.OrganizationName, "Codex "+strings.ToUpper(u.PlanType[:1])+u.PlanType[1:], u.PlanType, "")
	}
	if err := p.st.AppendUsageHistory(ctx, a.ID, time.Now().UnixMilli(), primaryUtil, secondaryUtil, 0); err != nil {
		p.logger.Printf("[usage] Codex account %d history: %v", a.ID, err)
	}
	return true
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// writeUsage flattens an anthropic.Usage value into store columns. Mirrors
// the helper in package httpapi (kept duplicated to avoid a circular import:
// httpapi already depends on refresh).
func writeUsage(ctx context.Context, st *store.Store, id int64, u *anthropic.Usage) error {
	fhU, sdU, ssU := flattenUsage(u)
	var fhR, sdR, ssR string
	if u.FiveHour != nil {
		fhR = u.FiveHour.ResetsAt
	}
	if u.SevenDay != nil {
		sdR = u.SevenDay.ResetsAt
	}
	if u.SevenDaySonnet != nil {
		ssR = u.SevenDaySonnet.ResetsAt
	}
	return st.SetUsage(ctx, id, fhU, fhR, sdU, sdR, ssU, ssR, u.ScopedLabel)
}

func flattenUsage(u *anthropic.Usage) (fhU, sdU, ssU float64) {
	if u.FiveHour != nil {
		fhU = u.FiveHour.Utilization
	}
	if u.SevenDay != nil {
		sdU = u.SevenDay.Utilization
	}
	if u.SevenDaySonnet != nil {
		ssU = u.SevenDaySonnet.Utilization
	}
	return
}
