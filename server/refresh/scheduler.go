// Package refresh keeps the access tokens in the account pool alive by
// trading their refresh_tokens for new ones before the upstream expires.
//
// Anthropic's refresh endpoint rotates the refresh_token on every successful
// call: two concurrent refreshes for the same account race, the loser sees
// "invalid_grant", and we lose the account. We therefore guard each account
// with its own mutex and serialise all refresh attempts through it.
package refresh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// Threshold is the remaining-lifetime below which the scheduler proactively
// refreshes a token. One hour is conservative — Anthropic's access tokens
// typically last 8 hours, so we always have plenty of slack on retry.
const Threshold = time.Hour

// Interval is how often the scheduler wakes up to scan for nearly-expired
// tokens. Short enough to catch any token that just dipped below the
// threshold, long enough that an idle pool doesn't thrash.
const Interval = 10 * time.Minute

// InUseFallbackThreshold is the safety net for the currently-injected
// account. While it's injected we'd normally defer to Claude Code (the
// refresh_token is one-time-use, parallel rotations would race), but CC only
// rotates the token while it's actively making requests. If the user is idle
// or the machine slept through the normal refresh window, nobody else is
// keeping the token alive — once `remaining` drops below this cutoff the
// scheduler takes over despite the injection. Set well under Threshold so
// CC has the full normal window to do its job first.
const InUseFallbackThreshold = 15 * time.Minute

// narrateBelow controls per-account decision logging in tick(). Above this
// remaining lifetime an account is healthy and uninteresting; below it we
// log the decision (refresh / skip-injected / skip-not-due) so a user
// reporting "my token wasn't refreshed" can be diagnosed by reading the
// timestamps — was the tick even running? was the account skipped, and why?
const narrateBelow = 2 * time.Hour

// Scheduler scans the store on Interval and refreshes any account whose
// access_token expires within Threshold.
type Scheduler struct {
	st     *store.Store
	locks  sync.Map // accountID(int64) → *sync.Mutex
	stop   chan struct{}
	done   chan struct{}
	logger *log.Logger

	// OnChange fires after a successful token rotation. The credinject
	// Coordinator uses it to reconcile the keychain immediately instead of
	// waiting up to 5s for its next ticker. Must be set before Start; safe
	// to leave nil (no-op).
	OnChange func()

	// IsAccountInUse, if non-nil, reports whether the given account is
	// currently being injected by some agent. While an account is injected,
	// Claude Code owns its refresh path — running our own refresh in
	// parallel would race the one-time-use refresh_token. The vault wires
	// this to its LeaseStore.IsLeased so any device with a live lease
	// suppresses scheduler-driven rotation (modulo the InUseFallbackThreshold
	// safety net for stale leases / idle agents).
	IsAccountInUse func(accountID int64) bool

	// Bus is the optional activity hub the scheduler emits token.refreshed /
	// error.refresh events to. Nil-safe; tests leave it unset.
	Bus *activity.Bus
}

func New(st *store.Store, logger *log.Logger) *Scheduler {
	if logger == nil {
		logger = log.Default()
	}
	return &Scheduler{
		st:     st,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		logger: logger,
	}
}

// Start launches the scheduler goroutine. It performs an immediate sweep
// before entering its periodic loop.
func (s *Scheduler) Start(ctx context.Context) {
	go func() {
		defer close(s.done)
		s.tick(ctx)
		t := time.NewTicker(Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			case <-t.C:
				s.tick(ctx)
			}
		}
	}()
}

func (s *Scheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	<-s.done
}

func (s *Scheduler) tick(ctx context.Context) {
	accs, err := s.st.List(ctx)
	if err != nil {
		s.logger.Printf("[refresh] list accounts: %v", err)
		return
	}
	now := time.Now()
	inUse := s.IsAccountInUse
	if inUse == nil {
		inUse = func(int64) bool { return false }
	}
	// Heartbeat: tick fired, so a missed-refresh report can be cross-
	// referenced against the wall clock to detect tick gaps (e.g. macOS
	// suspended the process longer than Interval).
	s.logger.Printf("[refresh] tick: scanning %d accounts", len(accs))
	for i := range accs {
		a := accs[i]
		// Paused accounts (Status="paused") are still maintained: pause is
		// a "do not select for routing" signal, not "stop refreshing". Letting
		// a paused token expire means the next resume lands on a dead token.
		if a.RefreshToken == "" {
			continue
		}
		// needs_reauth is terminal: the refresh_token was rejected with
		// invalid_grant and only the user re-authenticating can revive it.
		// Retrying every tick is pointless and hammers the token endpoint.
		if a.Status == store.StatusNeedsReauth {
			s.logger.Printf("[refresh] account %d (%s): skip — needs re-auth (dead refresh_token)",
				a.ID, a.Name)
			continue
		}
		remaining := time.Duration(a.ExpiresAt-now.UnixMilli()) * time.Millisecond
		narrate := remaining < narrateBelow
		leased := inUse(a.ID)
		// Skip leased accounts while they have comfortable runway — the agent
		// (Claude Code) owns the refresh path under normal use. Once
		// `remaining` drops below InUseFallbackThreshold the assumption "CC
		// will get to it" no longer holds (idle user / slept machine / CC
		// closed / agent crashed holding a stale lease), so the vault takes
		// over despite the lease.
		if leased && remaining > InUseFallbackThreshold {
			if narrate {
				s.logger.Printf("[refresh] account %d (%s): skip — leased, CC owns refresh (remaining=%s)",
					a.ID, a.Name, remaining.Round(time.Second))
			}
			continue
		}
		if remaining > Threshold {
			if narrate {
				s.logger.Printf("[refresh] account %d (%s): skip — not due (remaining=%s)",
					a.ID, a.Name, remaining.Round(time.Second))
			}
			continue
		}
		if leased {
			s.logger.Printf("[refresh] account %d (%s): leased but remaining=%s ≤ fallback; refreshing despite lease",
				a.ID, a.Name, remaining.Round(time.Second))
		} else {
			s.logger.Printf("[refresh] account %d (%s): refreshing (remaining=%s)",
				a.ID, a.Name, remaining.Round(time.Second))
		}
		if err := s.RefreshOne(ctx, a.ID); err != nil {
			s.logger.Printf("[refresh] account %d (%s): %v", a.ID, a.Name, err)
			// invalid_grant is terminal: RefreshOne already marked the account
			// needs_reauth and emitted account.needs_reauth. Emitting
			// error.refresh on top would double-report it as a transient blip.
			if !errors.Is(err, authz.ErrInvalidGrant) {
				s.Bus.EmitError(activity.TypeErrorRefresh, a.ID,
					fmt.Sprintf("Refresh %s failed: %v", a.Name, err))
			}
		}
	}
}

// RefreshOne forces a refresh for the given account, regardless of remaining
// lifetime. Safe to call from HTTP handlers ("Refresh now" button) — the
// per-account mutex serialises concurrent attempts.
func (s *Scheduler) RefreshOne(ctx context.Context, id int64) error {
	mu := s.lockFor(id)
	mu.Lock()
	defer mu.Unlock()

	// Re-read inside the critical section: another goroutine may have
	// already rotated the tokens while we were waiting on the mutex.
	a, err := s.st.Get(ctx, id)
	if err != nil {
		return err
	}
	if a.RefreshToken == "" {
		return nil
	}

	tr, err := authz.RefreshToken(ctx, a.RefreshToken)
	if err != nil {
		// Terminal failure: the refresh_token is dead (revoked, or already
		// rotated by a racing refresh). Flag the account so the selector stops
		// routing to it and the scheduler stops retrying; only a user re-login
		// (via Upsert) clears the flag. Distinct from transient 5xx/network
		// errors, which fall through and get retried on the next tick.
		if errors.Is(err, authz.ErrInvalidGrant) {
			if mErr := s.st.MarkNeedsReauth(ctx, id); mErr != nil {
				s.logger.Printf("[refresh] account %d (%s): mark needs_reauth: %v", a.ID, a.Name, mErr)
			}
			s.Bus.EmitError(activity.TypeAccountNeedsReauth, a.ID,
				fmt.Sprintf("%s needs re-authentication — its refresh token was rejected (invalid_grant)", a.Name))
			// The account just became non-active; let credinject reconcile it
			// out of the keychain the same way a successful rotation would.
			if s.OnChange != nil {
				s.OnChange()
			}
		}
		return err
	}

	newRefresh := tr.RefreshToken
	if newRefresh == "" {
		// OAuth 2.0 §6 allows the server to omit refresh_token on refresh,
		// in which case the previous one stays valid.
		newRefresh = a.RefreshToken
	}
	expiresAt := tr.ExpiresAtMillis()
	if expiresAt == 0 {
		// Defensive: assume 8h if the server didn't tell us.
		expiresAt = time.Now().Add(8 * time.Hour).UnixMilli()
	}
	if err := s.st.UpdateTokens(ctx, id, tr.AccessToken, newRefresh, expiresAt); err != nil {
		return err
	}
	s.logger.Printf("[refresh] account %d (%s) rotated; next expiry in %s",
		a.ID, a.Name, time.Until(time.UnixMilli(expiresAt)).Round(time.Second))
	s.Bus.EmitInfo(activity.TypeTokenRefreshed, a.ID,
		fmt.Sprintf("Rotated %s — next expiry in %s",
			a.Name, time.Until(time.UnixMilli(expiresAt)).Round(time.Second)))
	if s.OnChange != nil {
		s.OnChange()
	}
	return nil
}

func (s *Scheduler) lockFor(id int64) *sync.Mutex {
	if v, ok := s.locks.Load(id); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := s.locks.LoadOrStore(id, mu)
	return actual.(*sync.Mutex)
}
