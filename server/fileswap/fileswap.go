// Package fileswap replaces and removes credential files the agent CLIs read,
// staying correct when the path is a single-file bind mount.
//
// Several containers on one host can share one leased credential by
// bind-mounting the same host file at ~/.claude/.credentials.json (or
// $CODEX_HOME/auth.json). Claude Code writes such a file in place once its
// rename is refused, so a token it refreshes reaches every container. foxy's
// tmp + rename cannot replace a mount point: the kernel answers EBUSY and the
// injection fails. Writing in place in that one case keeps the shared inode,
// which is the whole point of the mount.
package fileswap

import (
	"errors"
	"os"
	"syscall"
)

// rename is a seam so tests can simulate a mount point without mounting one.
var rename = os.Rename

// remove is the same seam for Remove.
var remove = os.Remove

// Replace writes data to path through tmp + rename. When path is a mount point
// (rename fails with EBUSY) it rewrites path in place instead and discards tmp.
// tmp must sit in path's directory.
func Replace(path, tmp string, data []byte, perm os.FileMode) error {
	err := rename(tmp, path)
	if err == nil || !errors.Is(err, syscall.EBUSY) {
		return err
	}
	_ = os.Remove(tmp)
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	return os.Chmod(path, perm)
}

// Remove deletes path; a missing file is not an error. A mount point cannot be
// unlinked (EBUSY), so it is truncated to empty instead, which both CLIs treat
// as "no credential".
func Remove(path string) error {
	err := remove(path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if !errors.Is(err, syscall.EBUSY) {
		return err
	}
	return os.Truncate(path, 0)
}
