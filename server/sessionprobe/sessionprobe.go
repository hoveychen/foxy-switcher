// Package sessionprobe answers one question for the lease state machine: how
// long has it been since the user last really used a coding agent on this
// machine?
//
// The vault can't see it — real provider traffic never crosses the vault (foxy
// injects a credential and the CLI talks to the provider directly). Only the
// agent, co-located with the CLI, can observe local activity. The most robust,
// cross-platform signal is the CLI's own session transcripts: both Claude Code
// (~/.claude/projects/<slug>/*.jsonl) and Codex (~/.codex/sessions/YYYY/MM/DD/
// rollout-*.jsonl) append to a .jsonl on every turn of a conversation. The
// newest such file's mtime is therefore the last-real-activity clock — and it
// captures both "the CLI isn't open" and "open but sitting idle", unlike
// process presence.
package sessionprobe

import (
	"io/fs"
	"path/filepath"
	"time"
)

// LatestMTime walks `dir` and returns the most recent modification time across
// all files with extension `ext`, plus whether any were found. Walk errors are
// swallowed and treated as "none found" (false) so callers fall back to their
// fail-safe default rather than mis-parking a lease.
func LatestMTime(dir, ext string) (time.Time, bool) {
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
		if d.IsDir() || filepath.Ext(path) != ext {
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

// IdleFor reports how long since the newest `ext` file under `dir` was
// touched. Uncertainty — an empty dir argument, an unreadable tree, or no
// transcripts at all — is deliberately reported as active (0): the fail-safe
// direction is to keep holding a lease, never to wrongly release one out from
// under a user we simply failed to observe. Clock skew (a file dated in the
// future) is clamped to 0 for the same reason.
func IdleFor(dir, ext string, now time.Time) time.Duration {
	if dir == "" {
		return 0
	}
	last, ok := LatestMTime(dir, ext)
	if !ok {
		return 0
	}
	d := now.Sub(last)
	if d < 0 {
		d = 0
	}
	return d
}
