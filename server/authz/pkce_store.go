package authz

import (
	"fmt"
	"sync"
	"time"
)

// PKCESessionTTL is how long a /oauth/start state is valid. Anthropic's
// authorize page typically completes in under a minute; ten gives headroom
// without letting abandoned sessions accumulate.
const PKCESessionTTL = 10 * time.Minute

type pkceSession struct {
	Verifier  string
	State     string
	CreatedAt time.Time
}

// PKCEStore is an in-memory state→verifier index for in-flight OAuth logins.
// Single-tenant deployment doesn't need persistence: if the process restarts
// mid-flow the user just clicks "Add Account" again.
type PKCEStore struct {
	mu       sync.Mutex
	sessions map[string]*pkceSession
}

func NewPKCEStore() *PKCEStore {
	return &PKCEStore{sessions: make(map[string]*pkceSession)}
}

// Start generates a verifier + state, indexes them, and returns the
// authorization URL the caller redirects (or presents) to the user.
func (s *PKCEStore) Start() (authURL, state string, err error) {
	verifier, challenge, err := NewPKCEPair()
	if err != nil {
		return "", "", err
	}
	st, err := NewState()
	if err != nil {
		return "", "", fmt.Errorf("pkce state: %w", err)
	}

	s.mu.Lock()
	s.gcLocked()
	s.sessions[st] = &pkceSession{Verifier: verifier, State: st, CreatedAt: time.Now()}
	s.mu.Unlock()

	return AuthorizeURL(challenge, st), st, nil
}

// Consume looks up and removes the verifier for state. The second return is
// false when state is unknown or expired.
func (s *PKCEStore) Consume(state string) (verifier string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked()
	sess, ok := s.sessions[state]
	if !ok {
		return "", false
	}
	delete(s.sessions, state)
	return sess.Verifier, true
}

func (s *PKCEStore) gcLocked() {
	cutoff := time.Now().Add(-PKCESessionTTL)
	for k, v := range s.sessions {
		if v.CreatedAt.Before(cutoff) {
			delete(s.sessions, k)
		}
	}
}
