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
	"log"
	"sync"
	"time"

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

	// SkipAccountID, if non-nil, returns the account ID currently injected
	// into Claude Code's keychain. While an account is injected, Claude Code
	// owns its refresh path — running our own refresh in parallel would race
	// the one-time-use refresh_token. The Coordinator wires this so the
	// scheduler simply skips the injected account each tick.
	SkipAccountID func() int64
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
	var skip int64
	if s.SkipAccountID != nil {
		skip = s.SkipAccountID()
	}
	for i := range accs {
		a := accs[i]
		if a.Status != "active" || a.RefreshToken == "" {
			continue
		}
		if skip != 0 && a.ID == skip {
			continue
		}
		remaining := time.Duration(a.ExpiresAt-now.UnixMilli()) * time.Millisecond
		if remaining > Threshold {
			continue
		}
		if err := s.RefreshOne(ctx, a.ID); err != nil {
			s.logger.Printf("[refresh] account %d (%s): %v", a.ID, a.Name, err)
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
