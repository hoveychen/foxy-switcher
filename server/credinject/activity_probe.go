package credinject

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// activity_probe.go answers one question for the lease state machine: how long
// has it been since the user last really used Claude Code on this machine?
//
// The vault can't see it — real Claude API traffic never crosses the vault
// (foxy injects a token into the keychain and Claude Code talks to Anthropic
// directly). Only the agent, co-located with Claude Code, can observe local
// activity. The most robust, cross-platform signal is Claude Code's own session
// transcripts: it appends to a `.jsonl` under ~/.claude/projects/<slug>/ on
// every turn of a conversation. The newest such file's mtime is therefore the
// last-real-activity clock — and it captures both "Claude Code isn't open" and
// "open but sitting idle", unlike process presence.

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
	if dir == "" {
		return 0
	}
	last, ok := latestSessionMTime(dir)
	if !ok {
		return 0
	}
	d := clock().Sub(last)
	if d < 0 {
		d = 0
	}
	return d
}

// latestSessionMTime walks `dir` (~/.claude/projects) and returns the most
// recent modification time across all session transcript files, plus whether
// any were found. Walk errors are swallowed and treated as "none found" (false)
// so the caller falls back to the active default rather than mis-parking.
func latestSessionMTime(dir string) (time.Time, bool) {
	var newest time.Time
	found := false
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable subtree — skip it, keep walking the rest.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || filepath.Ext(path) != sessionTranscriptExt {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if mt := info.ModTime(); mt.After(newest) {
			newest = mt
			found = true
		}
		return nil
	})
	return newest, found
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
