//go:build !darwin

package openai

import keyring "github.com/zalando/go-keyring"

// keyringSet delegates to go-keyring everywhere except macOS. The
// zombie-leaking early return documented in keyring_darwin.go is specific
// to go-keyring's `security -i` path, which only exists on darwin.
func keyringSet(service, username, password string) error {
	return keyring.Set(service, username, password)
}
