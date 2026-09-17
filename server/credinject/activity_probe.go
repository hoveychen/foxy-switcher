package credinject

import (
	"os"
	"path/filepath"
	"time"

	"github.com/hoveychen/foxy-switcher/server/sessionprobe"
)

// activity_probe.go answers one question for the lease state machine: how long
// has it been since the user last really used Claude Code on this machine?
// The walk itself lives in server/sessionprobe, which Codex's remote manager
// shares — see that package's doc comment for why transcript mtime is the
// signal.

// sessionTranscriptExt is the extension Claude Code writes conversation
// transcripts with under ~/.claude/projects/<project>/.
const sessionTranscriptExt = ".jsonl"

// idleFor reports how long since the last real local Claude Code activity.
// A zero (or effectively-zero) result means "active right now". Uncertainty —
// no activity directory configured, an unreadable tree, or no transcripts at
// all — is deliberately reported as active (0): the fail-safe direction is to
// keep holding a lease, never to wrongly release one out from under a user we
// simply failed to observe. A machine that HAS used Claude Code before but is
// idle now surfaces correctly as its (old) transcript mtime age.
func (c *Coordinator) idleFor() time.Duration {
	c.mu.Lock()
	dir := c.activityDir
	probe := c.activityProbe
	clock := c.clock
	c.mu.Unlock()

	if probe != nil { // test seam
		return probe()
	}
	return sessionprobe.IdleFor(dir, sessionTranscriptExt, clock())
}

// DefaultClaudeProjectsDir returns ~/.claude/projects for the current user, the
// directory Claude Code writes session transcripts into. Resolve failure yields
// "" so the caller leaves the activity probe unset (idleFor then reports active,
// disabling idle-reclaim rather than guessing a path).
func DefaultClaudeProjectsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "projects"), nil
}
