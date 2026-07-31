package vault

import (
	"context"
	"log"
	"time"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// openrouter_credit.go polls each OpenRouter account's balance so the selector
// can skip the ones that have run out. It is the OpenRouter analogue of
// refresh.UsagePoller, and lives on the vault side for the same reason that one
// does: it needs the account credential, which never leaves the vault.
//
// This does not contradict the design's "OpenRouter stays out of the loops"
// rule. That rule is about the DEVICE side, where a derived key needs no
// renewal and no reverse-sync. A vault-side balance read is the only way to know
// an account is empty before handing its key to a device that then 402s.

// CreditPollInterval is how often balances are refreshed.
//
// Fifteen minutes, deliberately slower than the usage poller's five: a balance
// moves only as fast as real spend, the figure is a rotation input rather than
// something a user watches tick, and each tick is one HTTP call per configured
// account. The floor in store.MinUsableCredit is what absorbs drift between
// ticks — that's why it isn't zero.
const CreditPollInterval = 15 * time.Minute

// CreditPoller refreshes store.OpenRouterCredential balances.
type CreditPoller struct {
	st     *store.Store
	logger *log.Logger

	// Interval overrides CreditPollInterval. Tests set it small.
	Interval time.Duration

	// newClient builds a client for one account's key. A field so tests can
	// substitute a fake upstream.
	newClient func(apiKey string) CreditReader

	stop chan struct{}
	done chan struct{}
}

// CreditReader is the slice of the openrouter client this poller uses.
type CreditReader interface {
	AccountCredits(ctx context.Context) (openrouter.Credits, error)
}

func NewCreditPoller(st *store.Store, logger *log.Logger) *CreditPoller {
	if logger == nil {
		logger = log.Default()
	}
	return &CreditPoller{
		st:     st,
		logger: logger,
		newClient: func(apiKey string) CreditReader {
			return &openrouter.Client{APIKey: apiKey}
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// SetClientFactory swaps the client constructor. Tests only.
func (p *CreditPoller) SetClientFactory(f func(apiKey string) CreditReader) { p.newClient = f }

// Start launches the poll goroutine, sweeping once immediately so a freshly
// started vault knows its balances before the first device asks — otherwise the
// first grant after every restart would be decided on "unknown".
func (p *CreditPoller) Start(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = CreditPollInterval
	}
	go func() {
		defer close(p.done)
		p.Tick(ctx)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			case <-t.C:
				p.Tick(ctx)
			}
		}
	}()
}

func (p *CreditPoller) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

// Tick refreshes every configured OpenRouter account's balance.
//
// Failures are logged and skipped, never propagated: one unreachable account
// must not stop the others being polled, and a failed read deliberately leaves
// the previous figure (or "unknown") in place rather than writing a zero that
// would read as broke.
func (p *CreditPoller) Tick(ctx context.Context) {
	accs, err := p.st.ListProvider(ctx, store.ProviderOpenRouter)
	if err != nil {
		p.logger.Printf("[openrouter] credit poll: list accounts: %v", err)
		return
	}
	for i := range accs {
		acc := accs[i]
		cred, err := p.st.OpenRouterCredential(ctx, acc.ID)
		if err != nil {
			// No key on file yet — nothing to poll with. Not worth logging every
			// tick for a half-configured account.
			continue
		}
		credits, err := p.newClient(cred.APIKey).AccountCredits(ctx)
		if err != nil {
			p.logger.Printf("[openrouter] credit poll: account %d (%s): %v", acc.ID, acc.Name, err)
			continue
		}
		remaining := credits.Remaining()
		if err := p.st.SetOpenRouterCredit(ctx, acc.ID, credits.Total, remaining); err != nil {
			p.logger.Printf("[openrouter] credit poll: account %d (%s): store: %v", acc.ID, acc.Name, err)
			continue
		}
		// Log the crossing, not every reading: a line per account every 15 minutes
		// is noise, but "this account just became unusable" is the event an
		// operator needs to see, and it explains the rotation that follows.
		if cred.HasCredit() && remaining < store.MinUsableCredit {
			p.logger.Printf("[openrouter] account %d (%s) is out of credit ($%.2f left, floor $%.2f) — "+
				"devices will roll onto the next funded account",
				acc.ID, acc.Name, remaining, store.MinUsableCredit)
		} else if !cred.HasCredit() && remaining >= store.MinUsableCredit {
			p.logger.Printf("[openrouter] account %d (%s) is funded again ($%.2f left)",
				acc.ID, acc.Name, remaining)
		}
	}
}
