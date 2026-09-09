//go:build darwin

package openai

import (
	"encoding/base64"
	"fmt"
	"io"
	"os/exec"

	"al.essio.dev/pkg/shellescape"
	keyring "github.com/zalando/go-keyring"
)

// go-keyring's macOS Set() spawns `security -i`, then returns
// ErrSetDataTooBig on an over-long command WITHOUT calling cmd.Wait() —
// the started process is never reaped and lingers as a zombie for the
// lifetime of the daemon. Codex auth blobs are ~4KB, which base64-expands
// past the limit on every single write, so every credential switch leaked
// one <defunct> `security` process. autoCredentialStorage then silently
// fell back to the file store, hiding the failure.
//
// We build and run the same command ourselves so every early-return path
// still reaps the child. The 4096 cap stays: `security -i` truncates its
// input line there, and a truncated line is not merely rejected — the
// tail is parsed as a second command and a partial password is written to
// the keychain. Guarding before we exec also means the oversized case
// spawns nothing at all.
const (
	// keychainCmdLimit is the longest command line `security -i` accepts
	// on one line before it truncates.
	keychainCmdLimit = 4096

	// keychainBase64Prefix marks a base64-encoded value. It must stay in
	// sync with go-keyring's base64EncodingPrefix — Get() (still
	// go-keyring's) keys the decode off this exact string.
	keychainBase64Prefix = "go-keyring-base64:"

	securityBin = "/usr/bin/security"
)

// keychainAddCommand renders the `add-generic-password` line fed to
// `security -i`. The password is always base64-encoded: macOS hex-encodes
// values with newlines or non-ASCII bytes on read, and encoding
// unconditionally keeps the round trip byte-exact.
func keychainAddCommand(service, username, password string) (string, error) {
	encoded := keychainBase64Prefix + base64.StdEncoding.EncodeToString([]byte(password))
	cmd := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s\n",
		shellescape.Quote(service), shellescape.Quote(username), shellescape.Quote(encoded))
	if len(cmd) > keychainCmdLimit {
		return "", keyring.ErrSetDataTooBig
	}
	return cmd, nil
}

// keyringSet stores a secret in the macOS keychain. Unlike go-keyring's
// Set it reaps the `security` child on every path, including the errors.
func keyringSet(service, username, password string) error {
	command, err := keychainAddCommand(service, username, password)
	if err != nil {
		return err
	}

	cmd := exec.Command(securityBin, "-i")
	stdIn, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// From here on the child exists, so every exit goes through Wait().
	// Closing stdin first lets `security` see EOF and exit; on a write
	// error we close anyway so Wait() cannot block forever.
	writeErr := func() error {
		if _, err := io.WriteString(stdIn, command); err != nil {
			return err
		}
		return nil
	}()
	closeErr := stdIn.Close()
	waitErr := cmd.Wait()

	switch {
	case writeErr != nil:
		return writeErr
	case closeErr != nil:
		return closeErr
	default:
		return waitErr
	}
}
