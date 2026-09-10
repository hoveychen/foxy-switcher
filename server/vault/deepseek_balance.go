package vault

import (
	"context"
	"log"
	"time"

	"github.com/hoveychen/foxy-switcher/server/deepseek"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// deepseek_balance.go polls each DeepSeek account's balance so the grant
// service can skip the ones that have run out. It is the DeepSeek analogue of
// CreditPoller next door, and lives on the vault side for the same reason: it
// needs the account key, which never leaves the vault.

// BalancePollInterval is how often DeepSeek balances are refreshed. Fifteen
// minutes, matching CreditPollInterval — a balance moves only as fast as real
// spend, and each tick is one HTTP call per configured account.
//
// Unlike OpenRouter there is no local floor absorbing drift between ticks:
// DeepSeek's own is_available flag is what we store, so an account that empties
// mid-interval keeps being handed out until the next poll notices. The
// consequence is a bounded window of failed requests rather than a wrong
// rotation, and a currency-blind floor would not shorten it.
const BalancePollInterval = 15 * time.Minute

// BalanceReader is the slice of the deepseek client this poller uses.
type BalanceReader interface {
	Balance(ctx context.Context) (deepseek.Balance, error)
}

// BalancePoller refreshes store.DeepSeekCredential balances.
type BalancePoller struct {
	st     *store.Store
	logger *log.Logger

	// Interval overrides BalancePollInterval. Tests set it small.
	Interval time.Duration

	// newClient builds a client for one account's key. A field so tests can
	// substitute a fake upstream.
	newClient func(apiKey string) BalanceReader

	stop chan struct{}
	done chan struct{}
}

func NewBalancePoller(st *store.Store, logger *log.Logger) *BalancePoller {
	if logger == nil {
		logger = log.Default()
	}
	return &BalancePoller{
		st:     st,
		logger: logger,
		newClient: func(apiKey string) BalanceReader {
			return &deepseek.Client{APIKey: apiKey}
		},
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// SetClientFactory swaps the client constructor. Tests only.
func (p *BalancePoller) SetClientFactory(f func(apiKey string) BalanceReader) { p.newClient = f }

// Start launches the poll goroutine, sweeping once immediately so a freshly
// started vault knows its balances before the first device asks — otherwise the
// first grant after every restart would be decided on "unknown".
func (p *BalancePoller) Start(ctx context.Context) {
	interval := p.Interval
	if interval <= 0 {
		interval = BalancePollInterval
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

func (p *BalancePoller) Stop() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
	<-p.done
}

// Tick refreshes every configured DeepSeek account's balance.
//
// Failures are logged and skipped, never propagated: one unreachable account
// must not stop the others being polled, and a failed read deliberately leaves
// the previous reading (or "unknown") in place rather than writing an
// unavailable that would take a working account out of the pool.
func (p *BalancePoller) Tick(ctx context.Context) {
	accs, err := p.st.ListProvider(ctx, store.ProviderDeepSeek)
	if err != nil {
		p.logger.Printf("[deepseek] balance poll: list accounts: %v", err)
		return
	}
	for i := range accs {
		acc := accs[i]
		cred, err := p.st.DeepSeekCredential(ctx, acc.ID)
		if err != nil {
			// No key on file yet — nothing to poll with. Not worth logging every
			// tick for a half-configured account.
			continue
		}
		bal, err := p.newClient(cred.APIKey).Balance(ctx)
		if err != nil {
			p.logger.Printf("[deepseek] balance poll: account %d (%s): %v", acc.ID, acc.Name, err)
			continue
		}
		if err := p.st.SetDeepSeekBalance(ctx, acc.ID, bal.Available, bal.Amount, bal.Currency); err != nil {
			p.logger.Printf("[deepseek] balance poll: account %d (%s): store: %v", acc.ID, acc.Name, err)
			continue
		}
		// Log the crossing, not every reading: a line per account every 15
		// minutes is noise, but "this account just became unusable" is the event
		// an operator needs, and it explains the rotation that follows.
		if cred.HasBalance() && !bal.Available {
			p.logger.Printf("[deepseek] account %d (%s) is out of balance (%.2f %s) — "+
				"devices will roll onto the next funded account",
				acc.ID, acc.Name, bal.Amount, bal.Currency)
		} else if !cred.HasBalance() && bal.Available {
			p.logger.Printf("[deepseek] account %d (%s) is funded again (%.2f %s)",
				acc.ID, acc.Name, bal.Amount, bal.Currency)
		}
	}
}
