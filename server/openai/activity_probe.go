package openai

import (
	"path/filepath"
	"time"

	"github.com/hoveychen/foxy-switcher/server/sessionprobe"
)

// sessionRolloutExt is the extension the Codex CLI writes conversation
// rollouts with under <CODEX_HOME>/sessions/YYYY/MM/DD/rollout-*.jsonl.
const sessionRolloutExt = ".jsonl"

// idleFor reports how long since the last real local Codex activity, mirroring
// credinject's Claude-side probe. Zero means "active right now", and every
// uncertain case (no sessions dir wired, unreadable tree, no rollouts yet)
// reports zero on purpose: holding a lease we might not need is recoverable,
// releasing one out from under a working user is not.
//
// An explicit "use now" counts as activity too (see NoteExplicitSelect): the
// user asking for a Codex account is the strongest signal they're about to run
// codex, and without it an idle device skipped the pick and never injected.
// Caller holds m.mu.
func (m *RemoteManager) idleFor() time.Duration {
	if m == nil {
		return 0
	}
	var idle time.Duration
	if m.activityProbe != nil { // test seam
		idle = m.activityProbe()
	} else {
		idle = sessionprobe.IdleFor(m.activityDir, sessionRolloutExt, time.Now())
	}
	if !m.selectedAt.IsZero() {
		if since := time.Since(m.selectedAt); since < idle {
			idle = since
		}
	}
	return idle
}

// DefaultCodexSessionsDir returns <CODEX_HOME>/sessions, the tree the Codex CLI
// appends session rollouts to. Resolve failure yields "" so the caller leaves
// the probe unset — idleFor then reports active, which disables idle-reclaim
// for this device rather than guessing a path.
func DefaultCodexSessionsDir() (string, error) {
	home, err := DefaultCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "sessions"), nil
}
