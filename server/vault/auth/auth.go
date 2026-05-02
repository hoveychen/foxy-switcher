// Package auth holds the cryptographic primitives for vault Step 3 auth:
// bcrypt password hashing, sha256 fingerprinting for bearer tokens, and
// random string generators for opaque IDs / user codes.
//
// The package is small on purpose — vault Service implementations stay free
// of crypto details, and the schema layer in package store doesn't import
// any of this.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// HashPassword returns a bcrypt blob suitable for store.SetPasswordHash.
// Cost defaults to bcrypt.DefaultCost — the daemon does single-user auth
// so we don't need to tune.
func HashPassword(plain string) (string, error) {
	if len(plain) == 0 {
		return "", fmt.Errorf("password is empty")
	}
	buf, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

// VerifyPassword reports whether plain matches the stored bcrypt hash.
// Returns false (no error) on mismatch; non-nil error only on malformed
// hashes — the caller treats both as "bad password".
func VerifyPassword(hash, plain string) bool {
	if hash == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// NewToken returns a 256-bit random token, hex-encoded. Used as the bearer
// token agents send in `Authorization: Bearer …` and as the opaque cookie
// value for Web UI sessions.
func NewToken() string {
	return randomHex(32)
}

// NewID returns a 128-bit random hex string, used for device.id and
// pairing.client_nonce. Shorter than NewToken because it's referenced in
// URLs (the vault's web UI links and the agent's pair-poll body).
func NewID() string {
	return randomHex(16)
}

// HashToken is the index key the Bearer middleware looks up. Constant-time
// secrecy isn't a goal here — the hash is what's persisted, the plaintext
// only ever touches the wire.
func HashToken(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewUserCode produces a human-friendly XXXX-NNNN code (4 base32 chars
// dash 4 digits) for the device-flow approval page. The character set
// excludes I/O/0/1 to dodge OCR-prone confusion when the user types it
// from a phone screen.
//
// Entropy: 28 distinct chars over 4 positions × 10⁴ digits ≈ 6.1B values.
// Uniqueness inside the pairings table is enforced by the column UNIQUE
// constraint; collisions are vanishingly rare and the caller retries on
// constraint violation.
const userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ2345"

func NewUserCode() (string, error) {
	letters := make([]byte, 4)
	if err := readRandom(letters); err != nil {
		return "", err
	}
	for i, b := range letters {
		letters[i] = userCodeAlphabet[int(b)%len(userCodeAlphabet)]
	}
	digits := make([]byte, 4)
	if err := readRandom(digits); err != nil {
		return "", err
	}
	for i, b := range digits {
		digits[i] = '0' + b%10
	}
	return string(letters) + "-" + string(digits), nil
}

// NormaliseUserCode strips spaces / lowercase / surrounding whitespace so
// the user can paste the code from a phone autocomplete that mangled it.
func NormaliseUserCode(s string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if err := readRandom(buf); err != nil {
		// crypto/rand failure is fatal-grade — generating predictable IDs
		// would silently weaken the entire auth surface.
		panic("auth: rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

func readRandom(buf []byte) error {
	_, err := rand.Read(buf)
	return err
}
