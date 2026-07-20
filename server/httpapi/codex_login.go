package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	openai "github.com/hoveychen/foxy-switcher/server/openai"
)

// codexLoginTTL bounds how long an in-flight OAuth login stays completable.
// The authorize page + browser round-trip usually finishes in a minute; 15
// gives generous headroom without letting abandoned sessions accumulate.
const codexLoginTTL = 15 * time.Minute

// codexLoginStore is an in-memory state→verifier index for in-flight Codex
// OAuth logins, mirroring authz.PKCEStore's role for the Claude paste-code
// flow: the PKCE verifier never leaves the server; the client only holds the
// opaque state and pastes the callback URL back with it. The zero value is
// ready to use.
type codexLoginStore struct {
	mu      sync.Mutex
	entries map[string]codexLoginEntry
}

type codexLoginEntry struct {
	verifier string
	expires  time.Time
}

func (c *codexLoginStore) start(state, verifier string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]codexLoginEntry)
	}
	for k, e := range c.entries { // opportunistic sweep of expired sessions
		if now.After(e.expires) {
			delete(c.entries, k)
		}
	}
	c.entries[state] = codexLoginEntry{verifier: verifier, expires: now.Add(codexLoginTTL)}
}

// consume looks up and removes the verifier for state. The second return is
// false when state is unknown or expired.
func (c *codexLoginStore) consume(state string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[state]
	if !ok {
		return "", false
	}
	delete(c.entries, state)
	if now.After(e.expires) {
		return "", false
	}
	return e.verifier, true
}

// handleCodexLoginStart begins a Codex OAuth login. It generates a PKCE pair +
// state, stashes the verifier server-side keyed by state, and returns the
// authorize URL for the user to open. Unlike handleImportCodex (which reads the
// local `codex login` auth.json and is therefore rejected in vault mode) this
// flow needs no local Codex CLI, so it works in both combined and vault
// deployments — and unlike the old device-code flow it does not depend on the
// ChatGPT workspace's device-code toggle.
func (s *Server) handleCodexLoginStart(w http.ResponseWriter, _ *http.Request) {
	verifier, challenge, err := openai.NewPKCEPair()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	state, err := openai.NewState()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.codexLogins.start(state, verifier, time.Now())
	writeJSON(w, http.StatusOK, map[string]any{
		"authorize_url": openai.AuthorizeURL(challenge, state),
		"state":         state,
	})
}

type codexCallbackReq struct {
	State  string `json:"state"`  // the state returned from /codex-login
	Pasted string `json:"pasted"` // the callback URL copied from the browser address bar
}

// handleCodexLoginCallback finishes a Codex OAuth login: consume the verifier
// for the state, parse the pasted callback URL for the authorization code,
// verify the state round-trips, exchange the code for tokens, then store and
// activate the account. It mirrors handleLoginCallback (the Claude flow).
func (s *Server) handleCodexLoginCallback(w http.ResponseWriter, r *http.Request) {
	var req codexCallbackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	verifier, ok := s.codexLogins.consume(req.State, time.Now())
	if !ok {
		http.Error(w, "unknown or expired Codex login session", http.StatusBadRequest)
		return
	}
	code, pastedState, err := openai.ParsePastedCode(req.Pasted)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// OpenAI echoes state back in the redirect; when present it must match the
	// one we issued. A bare code paste (no state) is still accepted.
	if pastedState != "" && pastedState != req.State {
		http.Error(w, "state mismatch — paste corresponds to a different login attempt", http.StatusBadRequest)
		return
	}
	a, err := openai.CompleteLogin(r.Context(), verifier, code)
	if err != nil {
		http.Error(w, "complete Codex login: "+err.Error(), http.StatusBadGateway)
		return
	}
	if err := s.Store.Upsert(r.Context(), a); err != nil {
		http.Error(w, "save Codex account: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.Bus.EmitInfo(activity.TypeAccountAdded, a.ID,
		fmt.Sprintf("Added %s (%s)", a.Name, a.Plan))
	if s.Codex != nil {
		if err := s.Codex.Reconcile(r.Context()); err != nil {
			http.Error(w, "activate Codex account: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"account": toView(*a)})
}
