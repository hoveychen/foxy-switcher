package vault

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/deepseek"
	"github.com/hoveychen/foxy-switcher/server/store"
)

type fakeBalanceReader struct {
	bal  deepseek.Balance
	err  error
	seen int
}

func (f *fakeBalanceReader) Balance(context.Context) (deepseek.Balance, error) {
	f.seen++
	return f.bal, f.err
}

func TestBalancePollerWritesAndRotates(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()

	var logs bytes.Buffer
	p := NewBalancePoller(f.st, log.New(&logs, "", 0))
	upstream := &fakeBalanceReader{bal: deepseek.Balance{Available: true, Amount: 88.5, Currency: "CNY"}}
	var sawKey string
	p.SetClientFactory(func(apiKey string) BalanceReader {
		sawKey = apiKey
		return upstream
	})

	p.Tick(ctx)
	if sawKey != "sk-ds-one" {
		t.Fatalf("poller authenticated with %q, want the account's stored key", sawKey)
	}
	cred, err := f.st.DeepSeekCredential(ctx, f.acc.ID)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if !cred.BalanceAvailable || cred.BalanceAmount != 88.5 || cred.BalanceCurrency != "CNY" {
		t.Fatalf("stored balance = %+v", cred)
	}

	// Draining the account must both persist and be announced once, since the
	// rotation that follows is otherwise unexplained.
	upstream.bal = deepseek.Balance{Available: false, Amount: 0, Currency: "CNY"}
	p.Tick(ctx)
	cred, err = f.st.DeepSeekCredential(ctx, f.acc.ID)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if cred.HasBalance() {
		t.Fatal("an unavailable account must drop out of the pool")
	}
	if !strings.Contains(logs.String(), "out of balance") {
		t.Fatalf("crossing must be logged, got %q", logs.String())
	}

	// And recovering must be announced too.
	logs.Reset()
	upstream.bal = deepseek.Balance{Available: true, Amount: 20, Currency: "CNY"}
	p.Tick(ctx)
	if !strings.Contains(logs.String(), "funded again") {
		t.Fatalf("recovery must be logged, got %q", logs.String())
	}
}

// A failed read must leave the last known figure alone: writing "unavailable"
// on a network blip would take a working account out of the pool.
func TestBalancePollerKeepsLastGoodOnError(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()

	p := NewBalancePoller(f.st, log.New(new(bytes.Buffer), "", 0))
	upstream := &fakeBalanceReader{bal: deepseek.Balance{Available: true, Amount: 10, Currency: "USD"}}
	p.SetClientFactory(func(string) BalanceReader { return upstream })
	p.Tick(ctx)

	upstream.err = errors.New("network is down")
	p.Tick(ctx)

	cred, err := f.st.DeepSeekCredential(ctx, f.acc.ID)
	if err != nil {
		t.Fatalf("DeepSeekCredential: %v", err)
	}
	if !cred.BalanceAvailable || cred.BalanceAmount != 10 {
		t.Fatalf("a failed poll overwrote the last good reading: %+v", cred)
	}
}

// An account with no key on file has nothing to poll with, and must not cost
// an upstream call.
func TestBalancePollerSkipsKeylessAccounts(t *testing.T) {
	f := newDeepSeekFixture(t)
	ctx := context.Background()
	if err := f.st.DeleteDeepSeekCredential(ctx, f.acc.ID); err != nil {
		t.Fatalf("DeleteDeepSeekCredential: %v", err)
	}
	// A second provider's account must be ignored entirely.
	if err := f.st.Upsert(ctx, &store.Account{
		Provider: store.ProviderClaude, Name: "claude", AccountUUID: "c-1",
		Status: store.StatusActive,
	}); err != nil {
		t.Fatalf("upsert claude: %v", err)
	}

	upstream := &fakeBalanceReader{}
	p := NewBalancePoller(f.st, log.New(new(bytes.Buffer), "", 0))
	p.SetClientFactory(func(string) BalanceReader { return upstream })
	p.Tick(ctx)

	if upstream.seen != 0 {
		t.Fatalf("poller made %d upstream call(s) with no key on file", upstream.seen)
	}
}
