package credinject

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// diskBackend is a Backend that keeps the OAuth blob in a real file and
// reports its path, so it satisfies pathReporter the way the Linux/Windows
// fileBackend does. fakeBackend deliberately doesn't — it's in-memory, so
// VerifyMarker can't see it and every marker-aware code path short-circuits
// to SidecarMissing. These tests need the marker machinery live, and the
// real fileBackend is //go:build !darwin, which would make them invisible on
// the machines this code is most often developed on.
type diskBackend struct {
	mu     sync.Mutex
	path   string
	apiKey string
	writes int
}

func (b *diskBackend) CredentialsPath() string { return b.path }

func (b *diskBackend) ReadOAuthBlob() ([]byte, bool, error) {
	data, err := os.ReadFile(b.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return data, true, nil
}

func (b *diskBackend) WriteOAuthBlob(blob []byte) error {
	b.mu.Lock()
	b.writes++
	b.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(b.path, blob, 0o600)
}

func (b *diskBackend) DeleteOAuthBlob() error {
	if err := os.Remove(b.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (b *diskBackend) ReadManagedAPIKey() (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.apiKey, b.apiKey != "", nil
}

func (b *diskBackend) WriteManagedAPIKey(k string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.apiKey = k
	return nil
}

func (b *diskBackend) DeleteManagedAPIKey() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.apiKey = ""
	return nil
}

func (b *diskBackend) writeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.writes
}

// newDiskCoord mirrors newCoord but wires a file-backed backend so the
// credentials file, the marker inside it, and the sidecar are all real.
func newDiskCoord(t *testing.T) (*Coordinator, *diskBackend, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	be := &diskBackend{path: filepath.Join(dir, "claude", ".credentials.json")}
	c := New(vault.NewInProc(st), be, dir, log.New(io.Discard, "", 0), "")
	return c, be, st, dir
}

// TestReconcile_ReinjectsWhenCredentialsFileVanished is the mcn001 regression.
//
// The box sat at Claude Code's /login prompt for an hour and a half with a
// healthy agent running beside it: ~/.claude/.credentials.json had been
// deleted out from under us, but reconcile's no-op early return compared only
// in-memory state (account id + token hash), which still said "account 83 is
// injected". Nothing rotated and nothing switched, so every 5s tick for 90
// minutes returned early and the file never came back. Recovery took a manual
// `rm injected.json` + service restart.
//
// After the fix the very next tick notices the file is gone and writes it
// back — same account, same token, no switch.
func TestReconcile_ReinjectsWhenCredentialsFileVanished(t *testing.T) {
	ctx := context.Background()
	c, be, st, _ := newDiskCoord(t)
	id := seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	c.reconcile(ctx)
	if c.CurrentAccountID() != id {
		t.Fatalf("first reconcile: CurrentAccountID = %d, want %d", c.CurrentAccountID(), id)
	}
	if _, err := os.Stat(be.path); err != nil {
		t.Fatalf("first reconcile did not write the credentials file: %v", err)
	}
	if got := be.writeCount(); got != 1 {
		t.Fatalf("first reconcile: writes = %d, want 1", got)
	}

	// Someone wipes ~/.claude behind our back. The agent is none the wiser:
	// its in-memory state and injected.json still name this account.
	if err := os.Remove(be.path); err != nil {
		t.Fatalf("remove credentials: %v", err)
	}

	c.reconcile(ctx)

	if _, err := os.Stat(be.path); err != nil {
		t.Fatalf("reconcile left the credentials file missing — Claude Code stays at /login: %v", err)
	}
	if got := be.writeCount(); got != 2 {
		t.Fatalf("writes = %d, want 2 (the restore)", got)
	}
	blob, _, err := be.ReadOAuthBlob()
	if err != nil {
		t.Fatalf("ReadOAuthBlob: %v", err)
	}
	if got := extractAccessToken(blob); got != "sk-ant-oat01-alpha" {
		t.Errorf("restored blob carries the wrong token: got %q", got)
	}
	if c.CurrentAccountID() != id {
		t.Errorf("restore switched accounts: CurrentAccountID = %d, want %d", c.CurrentAccountID(), id)
	}
	// The sidecar must track the restore, or a later VerifyMarker compares
	// the new file against the pre-delete marker and reports Overwritten
	// forever.
	if state, err := c.VerifyMarker(); err != nil {
		t.Errorf("VerifyMarker after restore: %v", err)
	} else if state != MarkerStateIntact {
		t.Errorf("VerifyMarker after restore = %s, want %s", state, MarkerStateIntact)
	}
}

// TestReconcile_IntactCredentialsStayNoOp guards the other side: the vanish
// check must not turn the steady state into a write on every 5s tick. A
// credentials file that's still there, still ours, still holding the same
// token gets left alone.
func TestReconcile_IntactCredentialsStayNoOp(t *testing.T) {
	ctx := context.Background()
	c, be, st, _ := newDiskCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	c.reconcile(ctx)
	if got := be.writeCount(); got != 1 {
		t.Fatalf("first reconcile: writes = %d, want 1", got)
	}

	for i := 0; i < 3; i++ {
		c.reconcile(ctx)
	}
	if got := be.writeCount(); got != 1 {
		t.Fatalf("steady-state reconciles wrote again: writes = %d, want 1", got)
	}
}

// TestReconcile_ExternallyRotatedBlobIsNotRestored pins the deliberate
// narrowness of the check: only a *missing* file counts as vanished.
//
// When Claude Code refreshes its own access token it rewrites the blob
// without our marker, so VerifyMarker reports Overwritten. Treating that as
// "restore it" would race reverseSync and clobber CC's newer token with the
// store's older one — the very failure the post-inject tripwire in reconcile
// exists to warn about. Overwritten must therefore stay a no-op and leave the
// rotated blob for reverseSync to pull into the store.
func TestReconcile_ExternallyRotatedBlobIsNotRestored(t *testing.T) {
	ctx := context.Background()
	c, be, st, _ := newDiskCoord(t)
	seedActive(t, st, "alpha", "sk-ant-oat01-alpha")

	c.reconcile(ctx)
	writesAfterInject := be.writeCount()

	// Claude Code rotates in place: new access token, no foxy marker.
	rotated, _ := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "sk-ant-oat01-alpha-CC-ROTATED",
			"refreshToken": "sk-ant-ort01-alpha-CC-ROTATED",
			"expiresAt":    time.Now().Add(8 * time.Hour).UnixMilli(),
		},
	})
	if err := os.WriteFile(be.path, rotated, 0o600); err != nil {
		t.Fatalf("simulate CC rotation: %v", err)
	}
	if state, err := c.VerifyMarker(); err != nil {
		t.Fatalf("VerifyMarker: %v", err)
	} else if state != MarkerStateOverwritten {
		t.Fatalf("precondition: VerifyMarker = %s, want %s", state, MarkerStateOverwritten)
	}

	c.reconcile(ctx)

	if got := be.writeCount(); got != writesAfterInject {
		t.Fatalf("reconcile overwrote a CC-rotated blob: writes = %d, want %d", got, writesAfterInject)
	}
	blob, _, err := be.ReadOAuthBlob()
	if err != nil {
		t.Fatalf("ReadOAuthBlob: %v", err)
	}
	if got := extractAccessToken(blob); got != "sk-ant-oat01-alpha-CC-ROTATED" {
		t.Errorf("CC's rotated token was clobbered: got %q", got)
	}
}
