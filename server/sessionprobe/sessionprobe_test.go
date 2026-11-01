package sessionprobe

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A symlinked root (Fleet Cloud links ~/.claude/projects onto its state
// volume) must be walked like the real directory, not reported as empty.
func TestIdleForFollowsSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(filepath.Join(real, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(real, "proj", "s.jsonl")
	if err := os.WriteFile(transcript, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(transcript, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if _, ok := LatestMTime(link, ".jsonl"); !ok {
		t.Fatal("LatestMTime found no transcript under a symlinked root")
	}
	if got := IdleFor(link, ".jsonl", time.Now()); got < 59*time.Minute {
		t.Fatalf("IdleFor = %v, want ~1h (symlinked root read as active)", got)
	}
}
