package deepseek

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newDsh(t *testing.T) DshConfig {
	t.Helper()
	return DshConfig{Home: t.TempDir()}
}

func read(t *testing.T, c DshConfig) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.Home, ".credentials.yaml"))
	if err != nil {
		t.Fatalf("read credentials: %v", err)
	}
	return string(raw)
}

func TestApplyCreatesVersionedDocument(t *testing.T) {
	c := newDsh(t)
	if err := c.Apply("sk-ds-abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := read(t, c)
	for _, want := range []string{"version: 1", "refs:", "DEEPSEEK_API_KEY: sk-ds-abc", beginSentinel, endSentinel} {
		if !strings.Contains(got, want) {
			t.Fatalf("document missing %q:\n%s", want, got)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(c.Home, ".credentials.yaml"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		// dsh refuses to load a credential file any other user can read.
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

// Re-applying the same key must not touch the file: dsh watches it, and churn
// means a reload per tick.
func TestApplyIsIdempotent(t *testing.T) {
	c := newDsh(t)
	if err := c.Apply("sk-ds-abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	first := read(t, c)
	info, _ := os.Stat(filepath.Join(c.Home, ".credentials.yaml"))
	before := info.ModTime()

	if err := c.Apply("sk-ds-abc"); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if got := read(t, c); got != first {
		t.Fatalf("re-apply changed the document:\n%s\n---\n%s", first, got)
	}
	info, _ = os.Stat(filepath.Join(c.Home, ".credentials.yaml"))
	if !info.ModTime().Equal(before) {
		t.Fatal("re-apply rewrote an unchanged file; dsh's watcher would see churn")
	}
}

// Everything the user owns has to survive: other refs, records, comments.
func TestApplyPreservesTheUsersDocument(t *testing.T) {
	c := newDsh(t)
	original := `version: 1

# my notes
refs:
  # the openai one
  OPENAI_API_KEY: sk-openai-mine
  ANTHROPIC_API_KEY: sk-ant-mine

records:
  llm-pi-ai/openai-codex:
    kind: grant
    payload:
      type: oauth
`
	writeFile(t, c, original)
	if err := c.Apply("sk-ds-foxy"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := read(t, c)
	for _, want := range []string{
		"# my notes", "# the openai one",
		"OPENAI_API_KEY: sk-openai-mine", "ANTHROPIC_API_KEY: sk-ant-mine",
		"records:", "llm-pi-ai/openai-codex:", "type: oauth",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("Apply destroyed %q:\n%s", want, got)
		}
	}
	// Our entry must land inside the refs mapping, above `records:`.
	if strings.Index(got, "DEEPSEEK_API_KEY") > strings.Index(got, "records:") {
		t.Fatalf("managed entry landed outside the refs mapping:\n%s", got)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := read(t, c); got != original {
		t.Fatalf("Remove did not restore the original document:\n%s\n---\n%s", original, got)
	}
}

// The user's own key is parked and restored, not destroyed — including after
// foxy rotates its own key several times.
func TestApplyParksAndRestoresTheUsersOwnKey(t *testing.T) {
	c := newDsh(t)
	original := `version: 1

refs:
  DEEPSEEK_API_KEY: sk-ds-mine
  OPENAI_API_KEY: sk-openai-mine
`
	writeFile(t, c, original)

	if err := c.Apply("sk-ds-foxy-1"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := read(t, c)
	if strings.Count(got, "DEEPSEEK_API_KEY") != 2 {
		t.Fatalf("expected the foxy entry plus one parked line:\n%s", got)
	}
	if !strings.Contains(got, savedPrefix+"DEEPSEEK_API_KEY: sk-ds-mine") {
		t.Fatalf("the user's key was not parked:\n%s", got)
	}
	if !strings.Contains(got, "DEEPSEEK_API_KEY: sk-ds-foxy-1") {
		t.Fatalf("foxy's key is not in effect:\n%s", got)
	}

	// Rotating foxy's key must not mistake the previous foxy key for the
	// user's, nor lose the parked line.
	if err := c.Apply("sk-ds-foxy-2"); err != nil {
		t.Fatalf("Apply rotate: %v", err)
	}
	got = read(t, c)
	if strings.Contains(got, "sk-ds-foxy-1") {
		t.Fatalf("the previous foxy key survived a rotation:\n%s", got)
	}
	if !strings.Contains(got, savedPrefix+"DEEPSEEK_API_KEY: sk-ds-mine") {
		t.Fatalf("rotation lost the parked user key:\n%s", got)
	}

	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := read(t, c); got != original {
		t.Fatalf("Remove did not restore the user's key:\n%s\n---\n%s", original, got)
	}
}

// Remove is idempotent and must not touch a file foxy never wrote to.
func TestRemoveIsIdempotentAndConservative(t *testing.T) {
	c := newDsh(t)
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove with no file: %v", err)
	}
	original := "version: 1\n\nrefs:\n  OPENAI_API_KEY: sk-openai-mine\n"
	writeFile(t, c, original)
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := read(t, c); got != original {
		t.Fatalf("Remove touched a document foxy never wrote:\n%s", got)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove twice: %v", err)
	}
}

// A refs section foxy created on its own must go away with it: dsh refuses a
// document with an empty value, so leaving a bare `refs:` behind would stop
// the harness from starting.
func TestRemoveDropsTheRefsSectionItCreated(t *testing.T) {
	c := newDsh(t)
	if err := c.Apply("sk-ds-abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got := read(t, c)
	if strings.Contains(got, "refs:") {
		t.Fatalf("an empty refs mapping was left behind:\n%s", got)
	}
	if !strings.Contains(got, "version: 1") {
		t.Fatalf("the document lost its version:\n%s", got)
	}
}

// The pre-release flat layout must be refused, not nested into. Corrupting a
// credential file is worse than asking the operator to start dsh once.
func TestApplyRefusesUnversionedDocument(t *testing.T) {
	c := newDsh(t)
	writeFile(t, c, "DEEPSEEK_API_KEY: sk-ds-mine\nOPENAI_API_KEY: sk-openai\n")
	err := c.Apply("sk-ds-foxy")
	if !errors.Is(err, ErrUnversionedDocument) {
		t.Fatalf("err = %v, want ErrUnversionedDocument", err)
	}
	if got := read(t, c); !strings.Contains(got, "sk-ds-mine") {
		t.Fatalf("the refused write must leave the file alone:\n%s", got)
	}
}

// A value YAML would misread has to be quoted, or the document silently means
// something else.
func TestApplyQuotesAwkwardValues(t *testing.T) {
	c := newDsh(t)
	if err := c.Apply("has space: and #hash"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, c); !strings.Contains(got, "DEEPSEEK_API_KEY: 'has space: and #hash'") {
		t.Fatalf("value was not quoted:\n%s", got)
	}
}

func TestApplyRejectsEmptyKey(t *testing.T) {
	c := newDsh(t)
	if err := c.Apply("   "); err == nil {
		t.Fatal("an empty key must be refused rather than written")
	}
	if _, err := os.Stat(filepath.Join(c.Home, ".credentials.yaml")); !os.IsNotExist(err) {
		t.Fatal("a refused Apply must not create the file")
	}
}

// A user who chmod'd the file open would otherwise stop dsh from loading at
// all, and we are about to depend on it loading.
func TestApplyReassertsFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX mode on Windows, and dsh skips the check there too")
	}
	c := newDsh(t)
	if err := c.Apply("sk-ds-abc"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	path := filepath.Join(c.Home, ".credentials.yaml")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	// Same key: this exercises the unchanged-content path specifically.
	if err := c.Apply("sk-ds-abc"); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestDefaultHomeHonoursDshHome(t *testing.T) {
	t.Setenv("DSH_HOME", "/tmp/custom-dsh")
	got, err := DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if got != "/tmp/custom-dsh" {
		t.Fatalf("DefaultHome = %q, want the DSH_HOME override", got)
	}
	t.Setenv("DSH_HOME", "")
	got, err = DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if !strings.HasSuffix(got, ".dsh") {
		t.Fatalf("DefaultHome = %q, want a ~/.dsh fallback", got)
	}
}

func TestEnvShadowed(t *testing.T) {
	t.Setenv(credentialRef, "")
	if EnvShadowed() {
		t.Fatal("an unset variable must not read as shadowing")
	}
	t.Setenv(credentialRef, "sk-from-the-shell")
	if !EnvShadowed() {
		t.Fatal("a variable in the launch environment beats anything on disk and must be reported")
	}
}

func writeFile(t *testing.T, c DshConfig, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.Home, ".credentials.yaml"), []byte(content), 0o600); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
}
