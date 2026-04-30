package refresh

import (
	"context"
	"log"
	"time"

	"github.com/hoveychen/foxy-switcher/server/anthropic"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// UsageInterval is how often the usage poller wakes up. Five minutes matches
// claude-fleet's cadence and is plenty for windows that reset on 5h / 7d
// boundaries — the bars never look stale to a human.
const UsageInterval = 5 * time.Minute

// UsagePoller fetches /api/oauth/usage for every active account on its tick
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

	// OnChange fires once per tick if any account's usage row was updated.
	// The HookCoordinator uses it to reconcile the apiKeyHelper hook
	// immediately when, e.g., a 5h window resets and an account becomes
	// usable again. Must be set before Start; safe to leave nil.
	OnChange func()
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
	go func() {
		defer close(p.done)
		p.tick(ctx)
		t := time.NewTicker(UsageInterval)
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
	for i := range accs {
		a := accs[i]
		if a.Status != "active" || a.AccessToken == "" {
			continue
		}
		// Skip accounts whose token expired and hasn't been rotated yet —
		// the usage call would 401, and the token-refresh Scheduler will
		// pick it up on its own cadence.
		if a.ExpiresAt > 0 && a.ExpiresAt < time.Now().UnixMilli() {
			continue
		}
		// Backfill profile for accounts that predate the profile-fetching
		// feature. Plan is the canonical "is this row populated" tell — it
		// always gets a non-empty value at login. We only run this when
		// missing so the per-tick cost stays one HTTP call per account in
		// steady state.
		if a.Plan == "" {
			if prof, err := anthropic.FetchProfile(ctx, a.AccessToken); err != nil {
				p.logger.Printf("[usage] account %d profile backfill: %v", a.ID, err)
			} else if err := p.st.SetProfile(ctx, a.ID,
				prof.Email, prof.FullName, prof.OrganizationName,
				prof.Plan, prof.SubscriptionType,
			); err != nil {
				p.logger.Printf("[usage] account %d profile store: %v", a.ID, err)
			}
		}
		u, err := anthropic.FetchUsage(ctx, a.AccessToken)
		if err != nil {
			p.logger.Printf("[usage] account %d (%s): %v", a.ID, a.Name, err)
			continue
		}
		if err := writeUsage(ctx, p.st, a.ID, u); err != nil {
			p.logger.Printf("[usage] account %d store: %v", a.ID, err)
			continue
		}
		changed = true
	}
	if changed && p.OnChange != nil {
		p.OnChange()
	}
}

// writeUsage flattens an anthropic.Usage value into store columns. Mirrors
// the helper in package httpapi (kept duplicated to avoid a circular import:
// httpapi already depends on refresh).
func writeUsage(ctx context.Context, st *store.Store, id int64, u *anthropic.Usage) error {
	var fhU, sdU, ssU float64
	var fhR, sdR, ssR string
	if u.FiveHour != nil {
		fhU, fhR = u.FiveHour.Utilization, u.FiveHour.ResetsAt
	}
	if u.SevenDay != nil {
		sdU, sdR = u.SevenDay.Utilization, u.SevenDay.ResetsAt
	}
	if u.SevenDaySonnet != nil {
		ssU, ssR = u.SevenDaySonnet.Utilization, u.SevenDaySonnet.ResetsAt
	}
	return st.SetUsage(ctx, id, fhU, fhR, sdU, sdR, ssU, ssR)
}
