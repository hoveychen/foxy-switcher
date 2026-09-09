//go:build darwin

package openai

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

func TestKeychainAddCommandEncodesAndQuotes(t *testing.T) {
	cmd, err := keychainAddCommand("Codex Auth", "cli|abc", "hello\nworld")
	if err != nil {
		t.Fatalf("keychainAddCommand: %v", err)
	}
	if !strings.HasPrefix(cmd, "add-generic-password -U -s ") {
		t.Fatalf("unexpected command shape: %q", cmd)
	}
	if !strings.HasSuffix(cmd, "\n") {
		t.Fatalf("command must end with a newline so `security -i` executes it: %q", cmd)
	}
	if !strings.Contains(cmd, keychainBase64Prefix) {
		t.Fatalf("password should be base64-tagged: %q", cmd)
	}
	// The raw secret must never reach the command line unencoded.
	if strings.Contains(cmd, "hello") {
		t.Fatalf("plaintext secret leaked into command: %q", cmd)
	}
	// Space-bearing service names must survive as one argument.
	if !strings.Contains(cmd, "'Codex Auth'") {
		t.Fatalf("service name not quoted: %q", cmd)
	}
}

func TestKeychainAddCommandRejectsOversized(t *testing.T) {
	// A Codex auth.json is ~4KB, which base64-expands past the limit.
	_, err := keychainAddCommand("Codex Auth", "cli|abc", strings.Repeat("x", 4000))
	if !errors.Is(err, keyring.ErrSetDataTooBig) {
		t.Fatalf("want ErrSetDataTooBig, got %v", err)
	}
}

// The bug this package works around: go-keyring started `security -i`
// before checking the length, then returned without Wait()ing, leaving a
// zombie per credential write. Our oversized path must not exec at all.
func TestKeyringSetOversizedSpawnsNothing(t *testing.T) {
	before := childCount(t)
	if err := keyringSet("Codex Auth", "cli|test", strings.Repeat("x", 4000)); !errors.Is(err, keyring.ErrSetDataTooBig) {
		t.Fatalf("want ErrSetDataTooBig, got %v", err)
	}
	if after := childCount(t); after != before {
		t.Fatalf("oversized Set changed child count: before=%d after=%d", before, after)
	}
}

// childCount reports how many processes currently name this test binary
// as their parent — zombies included, since `ps` still lists them.
func childCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-eo", "ppid=").Output()
	if err != nil {
		t.Skipf("ps unavailable: %v", err)
	}
	self := os.Getpid()
	n := 0
	for _, line := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(line); err == nil && pid == self {
			n++
		}
	}
	return n
}

// TestKeyringSetRoundTrip talks to the real login keychain, so it only
// runs when explicitly opted in (FOXY_KEYCHAIN_TEST=1). It is the check
// that our hand-rolled `security -i` invocation stays compatible with
// go-keyring's Get, and that a successful write reaps its child too.
func TestKeyringSetRoundTrip(t *testing.T) {
	if os.Getenv("FOXY_KEYCHAIN_TEST") != "1" {
		t.Skip("set FOXY_KEYCHAIN_TEST=1 to exercise the real keychain")
	}
	const service, account = "foxy-switcher-test", "roundtrip"
	secret := "line one\nline two \xe2\x9c\x93"
	t.Cleanup(func() { _ = keyring.Delete(service, account) })

	before := childCount(t)
	if err := keyringSet(service, account, secret); err != nil {
		t.Fatalf("keyringSet: %v", err)
	}
	if after := childCount(t); after != before {
		t.Fatalf("successful Set left a child behind: before=%d after=%d", before, after)
	}
	got, err := keyring.Get(service, account)
	if err != nil {
		t.Fatalf("keyring.Get: %v", err)
	}
	if got != secret {
		t.Fatalf("round trip mismatch: got %q want %q", got, secret)
	}
}
