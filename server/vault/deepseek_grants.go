package vault

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// DeepSeekGrants decides, for a given device id, which DeepSeek account's key
// that device may have. It is vault-internal by construction: nothing on this
// type is reachable through vault.Service, so a remote agent can never ask for
// another device's grant.
//
// Compared with OpenRouterKeys this type does much less, and the missing half
// is the point. There is no minting, no upstream revocation and no
// device_deepseek_keys table, because DeepSeek issues keys only from its web
// console. What remains is authorisation (may this device have DeepSeek?) and
// account choice (which of the pool's keys does it get?).
type DeepSeekGrants struct {
	st     *store.Store
	logger *log.Logger
}

// NewDeepSeekGrants returns the grant service.
func NewDeepSeekGrants(st *store.Store, logger *log.Logger) *DeepSeekGrants {
	if logger == nil {
		logger = log.Default()
	}
	return &DeepSeekGrants{st: st, logger: logger}
}

// ErrNoDeepSeekAccount means no DeepSeek account is usable right now: none
// exists, none is active, none has a key on file, or every one of them is out
// of money. Distinct from "this device isn't allowed DeepSeek", which surfaces
// as selector.ErrNoAvailable, because the operator fix is different (configure
// an account vs grant the device).
var ErrNoDeepSeekAccount = errors.New("no configured DeepSeek account is available")

// EnsureDeviceGrant returns the device's DeepSeek grant. Pure database reads —
// there is nothing to derive — so it is cheap enough for the agent's slow
// config-sync poll to call unconditionally.
func (g *DeepSeekGrants) EnsureDeviceGrant(ctx context.Context, deviceID string) (DeepSeekGrant, error) {
	if deviceID == "" {
		return DeepSeekGrant{}, fmt.Errorf("device id required")
	}
	allowed, err := g.st.DeviceAllowsProvider(ctx, deviceID, store.ProviderDeepSeek)
	if err != nil {
		return DeepSeekGrant{}, err
	}
	if !allowed {
		// Same signal the other pools use for "not for you", so the agent has
		// one not-available branch rather than a per-provider zoo.
		return DeepSeekGrant{}, selector.ErrNoAvailable
	}
	acc, cred, err := g.pickAccount(ctx)
	if err != nil {
		return DeepSeekGrant{}, err
	}
	return DeepSeekGrant{
		AccountID:   acc.ID,
		AccountName: acc.Name,
		APIKey:      cred.APIKey,
		BaseURL:     DefaultDeepSeekBaseURL,
	}, nil
}

// pickAccount chooses which DeepSeek account serves a device.
//
// Ordering is deliberately NOT the LRU selector, for the same reason
// OpenRouter's isn't: spreading devices across accounts buys nothing when
// billing is per token, and it would make a device's key hop between accounts
// on unrelated pool changes. The rule is the most boring stable one — lowest id
// first — so a device keeps using the same account across restarts.
//
// What the ordering IS combined with is eligibility: an account DeepSeek
// reports as unavailable is skipped, so the pool rolls onto the next funded one
// instead of handing out a key that will fail. That is the whole of "rotate
// when the money runs out" — no lease, no LRU, just skip the broke ones and
// keep the order stable.
//
// An account is skipped when it is paused, has no key on file, or is out of
// money. A never-polled balance counts as funded — see
// store.DeepSeekCredential.HasBalance for why "unknown" must not mean "broke".
func (g *DeepSeekGrants) pickAccount(ctx context.Context) (store.Account, store.DeepSeekCredential, error) {
	none := func(err error) (store.Account, store.DeepSeekCredential, error) {
		return store.Account{}, store.DeepSeekCredential{}, err
	}
	accs, err := g.st.ListProvider(ctx, store.ProviderDeepSeek)
	if err != nil {
		return none(err)
	}
	sort.Slice(accs, func(i, j int) bool { return accs[i].ID < accs[j].ID })
	var skippedBroke int
	for _, a := range accs {
		if a.Status != store.StatusActive {
			continue
		}
		cred, err := g.st.DeepSeekCredential(ctx, a.ID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return none(err)
		}
		if !cred.HasBalance() {
			// Say so per account: "DeepSeek stopped working" is much harder to
			// diagnose than "both accounts are out of money".
			g.logger.Printf("[deepseek] account %d (%s): skipping, DeepSeek reports the key as unavailable (balance %.2f %s)",
				a.ID, a.Name, cred.BalanceAmount, cred.BalanceCurrency)
			skippedBroke++
			continue
		}
		return a, cred, nil
	}
	if skippedBroke > 0 {
		return none(fmt.Errorf("%w: %d account(s) skipped for insufficient balance",
			ErrNoDeepSeekAccount, skippedBroke))
	}
	return none(ErrNoDeepSeekAccount)
}
