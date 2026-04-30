//go:build darwin

package credinject

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"os/user"
)

// macOS Keychain service names. See docs/keychain-credentials-pool.md §1.
// We hit the prod, default-config-dir variants — staging / CLAUDE_CONFIG_DIR
// users will need explicit override support later.
const (
	macOAuthService      = "Claude Code-credentials"
	macManagedAPIService = "Claude Code"
)

// errSecItemNotFound — `security` returns 44 for "item not found in keychain".
// Treated as "doesn't exist" rather than a real error.
const errSecItemNotFound = 44

// NewBackend returns the platform-default credential backend. On darwin it
// drives the `security` CLI; the helper functions below are unexported
// because the Backend interface is the only sanctioned entry point.
func NewBackend() (Backend, error) {
	u, err := user.Current()
	if err != nil {
		return nil, fmt.Errorf("user.Current: %w", err)
	}
	return &darwinBackend{account: u.Username}, nil
}

type darwinBackend struct {
	account string
}

func (b *darwinBackend) ReadOAuthBlob() ([]byte, bool, error) {
	return b.read(macOAuthService)
}

func (b *darwinBackend) WriteOAuthBlob(blob []byte) error {
	return b.write(macOAuthService, string(blob))
}

func (b *darwinBackend) DeleteOAuthBlob() error {
	return b.delete(macOAuthService)
}

func (b *darwinBackend) ReadManagedAPIKey() (string, bool, error) {
	blob, ok, err := b.read(macManagedAPIService)
	if err != nil || !ok {
		return "", ok, err
	}
	return string(blob), true, nil
}

func (b *darwinBackend) WriteManagedAPIKey(key string) error {
	return b.write(macManagedAPIService, key)
}

func (b *darwinBackend) DeleteManagedAPIKey() error {
	return b.delete(macManagedAPIService)
}

// read invokes `security find-generic-password -s SVC -a ACCT -w`. Returns
// (blob, true, nil) on success and (nil, false, nil) when the item is not
// present (exit status 44).
func (b *darwinBackend) read(service string) ([]byte, bool, error) {
	cmd := exec.Command("security", "find-generic-password", "-s", service, "-a", b.account, "-w")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		// security trails the password with "\n"; trim it back to bytes.
		out := stdout.Bytes()
		if n := len(out); n > 0 && out[n-1] == '\n' {
			out = out[:n-1]
		}
		return out, true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == errSecItemNotFound {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("security find %q: %w (stderr: %s)", service, err, stderr.String())
}

// write invokes `security add-generic-password -U -s SVC -a ACCT -w PASS`.
// The -w form passes the password as an argv element; on a single-user
// developer machine this is acceptable. A future revision should switch to
// `-i` interactive + `-X` hex encoding to keep the value off `ps` listings
// (see docs/keychain-credentials-pool.md §3).
func (b *darwinBackend) write(service, value string) error {
	cmd := exec.Command("security", "add-generic-password", "-U", "-s", service, "-a", b.account, "-w", value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add %q: %w (stderr: %s)", service, err, stderr.String())
	}
	return nil
}

// delete invokes `security delete-generic-password -s SVC -a ACCT`. Missing
// items are treated as success.
func (b *darwinBackend) delete(service string) error {
	cmd := exec.Command("security", "delete-generic-password", "-s", service, "-a", b.account)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == errSecItemNotFound {
		return nil
	}
	return fmt.Errorf("security delete %q: %w (stderr: %s)", service, err, stderr.String())
}
