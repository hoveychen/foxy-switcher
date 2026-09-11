package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/hoveychen/foxy-switcher/server/deepseek"
	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/vault"
)

// deepseek_agent.go is the device side of the DeepSeek contract: fetch this
// machine's grant from the vault and render it into the harness's credential
// file.
//
// Like OpenRouter's writer next door, this is NOT part of credinject's
// reconcile loop. Claude and Codex need a 5s loop because their leases expire
// and their tokens rotate underneath us. A DeepSeek key does neither. What can
// change is the authorisation (an admin granting or withdrawing the provider)
// and which account the pool routes to (the one in use running out of money),
// and with no push channel from the vault a slow poll is the only way to
// notice either.
//
// One thing does differ from OpenRouter, and it is not a choice we get to
// make: the key lands on disk. codex can be pointed at a token *command*, so
// its secret never leaves memory; dsh resolves credentials from files and the
// environment only. The credential file is written 0600, which is also what
// dsh itself requires.
const deepSeekSyncInterval = 5 * time.Minute

// deepSeekGrantSource is where a grant comes from. Agent mode uses the vault
// httpclient; combined mode uses the in-process grant service. Both answer
// selector.ErrNoAvailable for "nothing for this device".
type deepSeekGrantSource interface {
	DeepSeekConfig(ctx context.Context) (*vault.DeepSeekGrant, error)
}

// inprocDeepSeekSource adapts the vault-internal grant service for combined
// mode, where the daemon is its own vault. The device id is the local one
// credinject persists — combined mode has no devices row, and
// DeviceAllowsProvider deliberately leaves un-paired ids un-gated.
type inprocDeepSeekSource struct {
	grants   *vault.DeepSeekGrants
	deviceID string
}

func (s inprocDeepSeekSource) DeepSeekConfig(ctx context.Context) (*vault.DeepSeekGrant, error) {
	grant, err := s.grants.EnsureDeviceGrant(ctx, s.deviceID)
	if err != nil {
		return nil, err
	}
	return &grant, nil
}

// deepSeekWriter owns the harness credential file.
type deepSeekWriter struct {
	src    deepSeekGrantSource
	dsh    deepseek.DshConfig
	logger *log.Logger

	mu        sync.Mutex
	applied   bool
	accountID int64
}

func newDeepSeekWriter(src deepSeekGrantSource, home string, logger *log.Logger) *deepSeekWriter {
	if logger == nil {
		logger = log.Default()
	}
	return &deepSeekWriter{
		src:    src,
		dsh:    deepseek.DshConfig{Home: home},
		logger: logger,
	}
}

// Start runs an immediate sync, then re-syncs on the slow interval, and tears
// the credential back out when ctx ends.
func (w *deepSeekWriter) Start(ctx context.Context) {
	go func() {
		w.syncLogged(ctx)
		t := time.NewTicker(deepSeekSyncInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				if err := w.Teardown(); err != nil {
					w.logger.Printf("[deepseek] teardown: %v", err)
				}
				return
			case <-t.C:
				w.syncLogged(ctx)
			}
		}
	}()
}

func (w *deepSeekWriter) syncLogged(ctx context.Context) {
	if err := w.Sync(ctx); err != nil {
		w.logger.Printf("[deepseek] sync: %v", err)
	}
}

// Sync reconciles the harness credential with what the vault says this device
// may have. Losing the grant (revoked, suspended, provider withdrawn, every
// account drained) removes the credential, so dsh stops authenticating as an
// account the device is no longer entitled to.
func (w *deepSeekWriter) Sync(ctx context.Context) error {
	grant, err := w.src.DeepSeekConfig(ctx)
	if errors.Is(err, selector.ErrNoAvailable) || errors.Is(err, vault.ErrNoDeepSeekAccount) {
		return w.Teardown()
	}
	if err != nil {
		// A transient vault outage must NOT tear the credential out: dsh would
		// lose its key mid-session over a blip. Leave the last known-good value
		// in place and retry on the next tick.
		return err
	}
	if err := w.dsh.Apply(grant.APIKey); err != nil {
		return err
	}
	w.mu.Lock()
	first := !w.applied
	rolled := w.applied && w.accountID != grant.AccountID
	w.applied = true
	w.accountID = grant.AccountID
	w.mu.Unlock()

	switch {
	case first:
		w.logger.Printf("[deepseek] configured the DeepSeek Harness from account %q", grant.AccountName)
		if deepseek.EnvShadowed() {
			// The environment layer outranks the file, so the key we just wrote
			// would be ignored. Nothing on disk can fix that, and a silent
			// no-op here is exactly the sort of thing nobody debugs.
			w.logger.Printf("[deepseek] WARNING: DEEPSEEK_API_KEY is set in the environment, " +
				"which the harness ranks above its credential file — unset it, or dsh will keep " +
				"using that key instead of the one foxy manages")
		}
	case rolled:
		// The pool rotated us onto another account, almost always because the
		// previous one ran out of money. Worth a line: it explains a bill
		// landing on a different account.
		w.logger.Printf("[deepseek] rolled onto account %q", grant.AccountName)
	}
	return nil
}

// Teardown removes foxy's managed block, restoring the user's own key if they
// had one. Idempotent.
func (w *deepSeekWriter) Teardown() error {
	w.mu.Lock()
	had := w.applied
	w.applied = false
	w.accountID = 0
	w.mu.Unlock()
	if err := w.dsh.Remove(); err != nil {
		return err
	}
	if had {
		w.logger.Printf("[deepseek] removed the managed credential (no grant for this device)")
	}
	return nil
}
