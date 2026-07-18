package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	openai "github.com/hoveychen/foxy-switcher/server/openai"
)

// codexLoginTTL bounds how long a device-code login session stays pollable.
// codex-rs gives the user 15 minutes to enter the one-time code, so we expire
// the server-side session on the same schedule.
const codexLoginTTL = 15 * time.Minute

// codexLoginStore keeps in-flight device-code logins keyed by an opaque session
// id handed to the client. It mirrors authz.PKCEStore's role for the Claude
// paste-code flow: the secret (here the device_auth_id + user_code) never leaves
// the server; the client only holds the session handle and polls with it. The
// zero value is ready to use.
type codexLoginStore struct {
	mu      sync.Mutex
	entries map[string]codexLoginEntry
}

type codexLoginEntry struct {
	auth    *openai.DeviceAuth
	expires time.Time
}

func (c *codexLoginStore) start(da *openai.DeviceAuth, now time.Time) (string, error) {
	sid, err := randomSessionID()
	if err != nil {
		return "", err
	}
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
	c.entries[sid] = codexLoginEntry{auth: da, expires: now.Add(codexLoginTTL)}
	return sid, nil
}

func (c *codexLoginStore) get(sid string, now time.Time) (*openai.DeviceAuth, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[sid]
	if !ok {
		return nil, false
	}
	if now.After(e.expires) {
		delete(c.entries, sid)
		return nil, false
	}
	return e.auth, true
}

func (c *codexLoginStore) remove(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, sid)
}

func randomSessionID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// handleCodexLoginStart begins a Codex device-code login. Unlike
// handleImportCodex (which reads the local `codex login` auth.json and is
// therefore rejected in vault mode), this flow needs no local Codex CLI, so it
// works in both combined and vault deployments.
func (s *Server) handleCodexLoginStart(w http.ResponseWriter, r *http.Request) {
	da, err := openai.StartDeviceLogin(r.Context())
	if err != nil {
		if errors.Is(err, openai.ErrDeviceCodeUnsupported) {
			http.Error(w, err.Error(), http.StatusNotImplemented)
			return
		}
		http.Error(w, "start Codex login: "+err.Error(), http.StatusBadGateway)
		return
	}
	sid, err := s.codexLogins.start(da, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session":          sid,
		"user_code":        da.UserCode,
		"verification_url": da.VerificationURL,
		"interval":         da.Interval,
	})
}

type codexPollReq struct {
	Session string `json:"session"`
}

// handleCodexLoginPoll performs one poll of the device token endpoint. It
// returns {"status":"pending"} until the user approves the code, then exchanges
// the code, stores the account, and returns {"status":"complete", account}.
// The client is expected to poll on the interval returned by start.
func (s *Server) handleCodexLoginPoll(w http.ResponseWriter, r *http.Request) {
	var req codexPollReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	da, ok := s.codexLogins.get(req.Session, time.Now())
	if !ok {
		http.Error(w, "unknown or expired Codex login session", http.StatusBadRequest)
		return
	}
	a, err := openai.PollDeviceLogin(r.Context(), da)
	if err != nil {
		if errors.Is(err, openai.ErrAuthorizationPending) {
			writeJSON(w, http.StatusOK, map[string]any{"status": "pending"})
			return
		}
		// A hard failure (timeout, exchange error) burns the session — the
		// device_auth_id is single-use, so the client must start over.
		s.codexLogins.remove(req.Session)
		http.Error(w, "complete Codex login: "+err.Error(), http.StatusBadGateway)
		return
	}
	s.codexLogins.remove(req.Session)
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
	writeJSON(w, http.StatusOK, map[string]any{"status": "complete", "account": toView(*a)})
}
