package httpapi

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/hoveychen/foxy-switcher/server/hook"
	"github.com/hoveychen/foxy-switcher/server/selector"
)

// HookCoordinator drives hook.Reconcile based on account-pool availability.
//
// Two reconcile paths feed into the same logic:
//   - a periodic ticker (catches state changes that don't go through HTTP:
//     cooldown windows expiring, the refresh.Scheduler / usage poller flipping
//     an account's status flag in the background)
//   - explicit Trigger() calls from mutation routes so the user-visible
//     "I just added an account" → "Claude Code starts working" loop is
//     instant rather than waiting for the next tick.
type HookCoordinator struct {
	server   *Server
	logger   *log.Logger
	interval time.Duration
	trigger  chan struct{}
}

// NewHookCoordinator builds a coordinator for the given server. Caller is
// responsible for running it via Run(ctx) on its own goroutine.
func (s *Server) NewHookCoordinator(logger *log.Logger, interval time.Duration) *HookCoordinator {
	return &HookCoordinator{
		server:   s,
		logger:   logger,
		interval: interval,
		trigger:  make(chan struct{}, 1),
	}
}

// Trigger schedules an immediate reconcile. Multiple triggers between two
// ticks collapse into one (non-blocking send into a 1-buffered channel).
// Safe to call on a nil receiver — that's the path taken before the
// coordinator is wired in (e.g. during tests that construct a bare Server).
func (c *HookCoordinator) Trigger() {
	if c == nil {
		return
	}
	select {
	case c.trigger <- struct{}{}:
	default:
	}
}

// Run executes one initial reconcile and then loops on the ticker / trigger
// channel until ctx is cancelled.
func (c *HookCoordinator) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	c.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick(ctx)
		case <-c.trigger:
			c.tick(ctx)
		}
	}
}

func (c *HookCoordinator) tick(ctx context.Context) {
	_, err := selector.Pick(ctx, c.server.Store, time.Now())
	hasAvailable := err == nil
	if err != nil && !errors.Is(err, selector.ErrNoAvailable) {
		// A real DB error — leave hook state alone. Logging only.
		c.logger.Printf("hook coordinator: probe selector: %v", err)
		return
	}

	changed, recErr := hook.Reconcile(c.server.DataDir, hasAvailable)
	if recErr != nil {
		c.logger.Printf("hook reconcile: %v", recErr)
		return
	}
	if changed {
		if hasAvailable {
			c.logger.Print("hook installed (account became available)")
		} else {
			c.logger.Print("hook uninstalled (no usable account)")
		}
	}
}
