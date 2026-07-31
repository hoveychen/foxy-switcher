package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// openrouter_agent.go is the device side of the OpenRouter contract: fetch this
// machine's grant from the vault, render it into the codex config files, and
// serve the runtime key over loopback so codex can fetch it per use without it
// ever touching disk.
//
// Note what this is NOT: it is not part of credinject's reconcile loop. Claude
// and Codex need a 5s loop because their leases expire and their tokens rotate
// underneath us. An OpenRouter key does neither — it is minted once and lives
// until revoked. The only thing that can change is the *authorisation* (an
// admin granting/withdrawing the provider, or editing the model allowlist), and
// with no push channel from the vault a slow poll is the only way to notice.
// Hence openRouterSyncInterval below, deliberately two orders of magnitude
// slower than the reconcile tick.
const openRouterSyncInterval = 5 * time.Minute

// openRouterGrantSource is where a grant comes from. Agent mode uses the vault
// httpclient; combined mode uses the in-process derivation service. Both answer
// selector.ErrNoAvailable for "nothing for this device".
type openRouterGrantSource interface {
	OpenRouterConfig(ctx context.Context) (*vault.OpenRouterGrant, error)
}

// inprocGrantSource adapts the vault-internal derivation service for
// combined mode, where the daemon is its own vault. The device id is the
// local one credinject persists — combined mode has no devices row, and
// DeviceAllowsProvider deliberately leaves un-paired ids un-gated.
type inprocGrantSource struct {
	keys     *vault.OpenRouterKeys
	deviceID string
}

func (s inprocGrantSource) OpenRouterConfig(ctx context.Context) (*vault.OpenRouterGrant, error) {
	grant, err := s.keys.EnsureDeviceKey(ctx, s.deviceID)
	if err != nil {
		return nil, err
	}
	return &grant, nil
}

// openRouterWriter owns the codex config files and the in-memory runtime key.
//
// The key is held in memory only. config.toml points codex at
// `foxy-switcher cred openrouter-token`, which loops back to this process and
// reads the value below — so a secret exists on this machine only for as long
// as the daemon runs.
type openRouterWriter struct {
	src     openRouterGrantSource
	codex   openrouter.CodexConfig
	exePath string
	logger  *log.Logger

	mu      sync.Mutex
	key     string
	applied bool
}

func newOpenRouterWriter(src openRouterGrantSource, home, exePath string, logger *log.Logger) *openRouterWriter {
	if logger == nil {
		logger = log.Default()
	}
	return &openRouterWriter{
		src:     src,
		codex:   openrouter.CodexConfig{Home: home},
		exePath: exePath,
		logger:  logger,
	}
}

// Start runs an immediate sync, then re-syncs on the slow interval, and tears
// the config down when ctx ends.
func (w *openRouterWriter) Start(ctx context.Context) {
	go func() {
		w.syncLogged(ctx)
		t := time.NewTicker(openRouterSyncInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := w.Teardown(); err != nil {
					w.logger.Printf("[openrouter] teardown: %v", err)
				}
				return
			case <-t.C:
				w.syncLogged(ctx)
			}
		}
	}()
}

func (w *openRouterWriter) syncLogged(ctx context.Context) {
	if err := w.Sync(ctx); err != nil {
		w.logger.Printf("[openrouter] sync: %v", err)
	}
}

// Sync reconciles the codex config with what the vault says this device may
// have. Losing the grant (revoked, suspended, provider withdrawn) tears the
// config down, so codex stops offering models the device can no longer use.
func (w *openRouterWriter) Sync(ctx context.Context) error {
	grant, err := w.src.OpenRouterConfig(ctx)
	if errors.Is(err, selector.ErrNoAvailable) || errors.Is(err, vault.ErrNoOpenRouterAccount) {
		return w.Teardown()
	}
	if err != nil {
		// A transient vault outage must NOT tear the config down: codex would
		// lose its provider mid-session over a blip. Leave the last known-good
		// config in place and retry on the next tick.
		return err
	}
	res, err := w.codex.Apply(openrouter.ProviderSpec{
		TokenCommand: []string{w.exePath, "cred", "openrouter-token"},
		BaseURL:      grant.BaseURL,
		Models:       grant.AllowedModels,
	})
	if err != nil {
		return err
	}
	for _, c := range res.Collisions {
		// Never silent: a model missing from the dropdown with no explanation is
		// exactly the kind of thing nobody debugs.
		w.logger.Printf("[openrouter] skipped %s", c)
	}
	w.mu.Lock()
	first := !w.applied
	w.key = grant.APIKey
	w.applied = true
	w.mu.Unlock()
	if first {
		w.logger.Printf("[openrouter] configured %d model(s) from account %q",
			len(res.ProfilesWritten), grant.AccountName)
	}
	return nil
}

// Teardown removes the managed block and profile files and forgets the key.
// Idempotent.
func (w *openRouterWriter) Teardown() error {
	w.mu.Lock()
	had := w.applied
	w.key = ""
	w.applied = false
	w.mu.Unlock()
	if err := w.codex.Remove(); err != nil {
		return err
	}
	if had {
		w.logger.Printf("[openrouter] removed codex configuration (no grant for this device)")
	}
	return nil
}

// Token returns the current runtime key for the loopback endpoint. The error
// is deliberately distinct from "empty string": codex must see a failed command
// rather than authenticate with an empty bearer token and get a confusing 401.
func (w *openRouterWriter) Token() (string, error) {
	if w == nil {
		return "", errors.New("OpenRouter is not configured on this device")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.key == "" {
		return "", errors.New("no OpenRouter key held — this device is not authorised, " +
			"or the daemon has not yet reached the vault")
	}
	return w.key, nil
}

// openRouterTokenHandler serves the runtime key to `foxy-switcher cred
// openrouter-token`. Plain text, no JSON envelope: the CLI writes the body
// through verbatim and codex reads that as the bearer token.
//
// Wrap it in loopbackOnly. It returns a live third-party credential, and it
// carries no auth of its own — being reachable only from this machine IS the
// authorisation, which is sound because codex runs here too.
func openRouterTokenHandler(w *openRouterWriter) http.HandlerFunc {
	return func(rw http.ResponseWriter, _ *http.Request) {
		token, err := w.Token()
		if err != nil {
			// 503, not 200-with-empty-body: codex must treat this as a failed
			// command rather than authenticate with an empty token.
			http.Error(rw, err.Error(), http.StatusServiceUnavailable)
			return
		}
		rw.Header().Set("Content-Type", "text/plain; charset=utf-8")
		rw.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(rw, token)
	}
}

// resolveExePath returns the absolute path codex should execute to fetch a
// token. Absolute because codex runs the command with an unspecified working
// directory, and it is resolved once at startup so a later `cd` can't change
// what gets written.
func resolveExePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return p, nil
}
