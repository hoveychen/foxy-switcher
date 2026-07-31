package vault

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/openrouter"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// fakeCredits answers per API key, so a test can make one account's read fail
// while another succeeds.
type fakeCredits struct {
	mu      sync.Mutex
	byKey   map[string]openrouter.Credits
	errs    map[string]error
	seen    []string
	callFor func(key string)
}

func (f *fakeCredits) reader(key string) CreditReader {
	return &fakeCreditsFor{parent: f, key: key}
}

type fakeCreditsFor struct {
	parent *fakeCredits
	key    string
}

func (f *fakeCreditsFor) AccountCredits(context.Context) (openrouter.Credits, error) {
	p := f.parent
	p.mu.Lock()
	p.seen = append(p.seen, f.key)
	err := p.errs[f.key]
	credits := p.byKey[f.key]
	p.mu.Unlock()
	if p.callFor != nil {
		p.callFor(f.key)
	}
	if err != nil {
		return openrouter.Credits{}, err
	}
	return credits, nil
}

func (f *fakeCredits) polled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.seen))
	copy(out, f.seen)
	return out
}

// creditFixture: two configured OpenRouter accounts plus a poller wired to a
// fake upstream. Reuses the openRouterFixture account as the first one.
func newCreditFixture(t *testing.T) (*openRouterFixture, *CreditPoller, *fakeCredits, *bytes.Buffer) {
	t.Helper()
	f := newOpenRouterFixture(t)
	up := &fakeCredits{
		byKey: map[string]openrouter.Credits{},
		errs:  map[string]error{},
	}
	var logs bytes.Buffer
	p := NewCreditPoller(f.st, log.New(&logs, "", 0))
	p.SetClientFactory(up.reader)
	return f, p, up, &logs
}

func TestCreditPollerWritesEachAccountsBalance(t *testing.T) {
	f, p, up, _ := newCreditFixture(t)
	ctx := context.Background()
	second := addAccount(t, f, "pool-b", true)

	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 510, Used: 486.06}
	up.byKey["sk-or-pool-b"] = openrouter.Credits{Total: 20, Used: 1}

	p.Tick(ctx)

	a, err := f.st.OpenRouterCredential(ctx, f.acc.ID)
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	if a.CreditTotal != 510 {
		t.Fatalf("a total = %v", a.CreditTotal)
	}
	if diff := a.CreditRemaining - 23.94; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("a remaining = %v, want ~23.94", a.CreditRemaining)
	}
	if a.CreditCheckedAt == 0 {
		t.Fatal("a successful poll must stamp checked_at, or the balance stays 'unknown'")
	}
	b, _ := f.st.OpenRouterCredential(ctx, second.ID)
	if b.CreditRemaining != 19 {
		t.Fatalf("b remaining = %v, want 19", b.CreditRemaining)
	}
	// Each account is polled with its OWN key.
	got := up.polled()
	if len(got) != 2 {
		t.Fatalf("polled %v, want both accounts", got)
	}
}

// One unreachable account must not stop the others being polled.
func TestCreditPollerContinuesPastAFailure(t *testing.T) {
	f, p, up, logs := newCreditFixture(t)
	ctx := context.Background()
	second := addAccount(t, f, "pool-b", true)

	up.errs["sk-or-mgmt"] = errors.New("upstream down")
	up.byKey["sk-or-pool-b"] = openrouter.Credits{Total: 20, Used: 1}

	p.Tick(ctx)

	if b, _ := f.st.OpenRouterCredential(ctx, second.ID); b.CreditRemaining != 19 {
		t.Fatalf("the healthy account was not polled: %+v", b)
	}
	if !strings.Contains(logs.String(), "upstream down") {
		t.Fatalf("the failure was not logged: %s", logs.String())
	}
}

// A failed read must leave the previous figure alone. Writing a zero would read
// as "broke" and take a perfectly funded account out of the pool over a network
// blip — the exact failure mode HasCredit's unknown-is-usable rule exists to
// avoid, and this is the other half of it.
func TestCreditPollerDoesNotZeroABalanceOnFailure(t *testing.T) {
	f, p, up, _ := newCreditFixture(t)
	ctx := context.Background()

	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 100, Used: 10}
	p.Tick(ctx)
	before, _ := f.st.OpenRouterCredential(ctx, f.acc.ID)
	if before.CreditRemaining != 90 {
		t.Fatalf("setup: remaining = %v", before.CreditRemaining)
	}

	up.errs["sk-or-mgmt"] = errors.New("network")
	p.Tick(ctx)

	after, _ := f.st.OpenRouterCredential(ctx, f.acc.ID)
	if after.CreditRemaining != 90 {
		t.Fatalf("a failed poll changed the balance to %v", after.CreditRemaining)
	}
	if !after.HasCredit() {
		t.Fatal("a failed poll made a funded account look broke")
	}
}

// An account with no key configured yet is simply skipped — there is nothing to
// poll with, and logging it every tick would be noise.
func TestCreditPollerSkipsUnconfiguredAccounts(t *testing.T) {
	f, p, up, logs := newCreditFixture(t)
	ctx := context.Background()
	bare := &store.Account{
		Provider: store.ProviderOpenRouter, Name: "bare", AccountUUID: "or-bare",
		Status: store.StatusActive,
	}
	if err := f.st.Upsert(ctx, bare); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 10, Used: 1}

	p.Tick(ctx)

	if got := up.polled(); len(got) != 1 || got[0] != "sk-or-mgmt" {
		t.Fatalf("polled %v, want only the configured account", got)
	}
	if strings.Contains(logs.String(), "bare") {
		t.Fatalf("an unconfigured account was logged every tick: %s", logs.String())
	}
}

// The crossing is the event worth logging — it explains the rotation that
// follows. A steady reading every 15 minutes is not.
func TestCreditPollerLogsTheCrossingOnly(t *testing.T) {
	_, p, up, logs := newCreditFixture(t)
	ctx := context.Background()

	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 100, Used: 10}
	p.Tick(ctx)
	if strings.Contains(logs.String(), "out of credit") {
		t.Fatalf("a healthy reading logged a crossing: %s", logs.String())
	}
	logs.Reset()

	// Funded → broke.
	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 100, Used: 100}
	p.Tick(ctx)
	if !strings.Contains(logs.String(), "out of credit") {
		t.Fatalf("the exhaustion crossing was not logged: %s", logs.String())
	}
	logs.Reset()

	// Still broke — no repeat.
	p.Tick(ctx)
	if strings.Contains(logs.String(), "out of credit") {
		t.Fatalf("a still-broke account logged again: %s", logs.String())
	}
	logs.Reset()

	// Topped up.
	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 200, Used: 100}
	p.Tick(ctx)
	if !strings.Contains(logs.String(), "funded again") {
		t.Fatalf("the recovery crossing was not logged: %s", logs.String())
	}
}

// TestCreditPollerSweepsBeforeTheFirstTick: without an immediate sweep, the
// first grant after every vault restart would be decided on an unknown balance.
func TestCreditPollerSweepsBeforeTheFirstTick(t *testing.T) {
	f, p, up, _ := newCreditFixture(t)
	up.byKey["sk-or-mgmt"] = openrouter.Credits{Total: 100, Used: 10}

	polled := make(chan struct{}, 1)
	up.callFor = func(string) {
		select {
		case polled <- struct{}{}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Interval = time.Hour // so only the immediate sweep can fire
	p.Start(ctx)
	defer p.Stop()

	select {
	case <-polled:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not poll before the first ticker fire")
	}
	// And it landed in the store.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := f.st.OpenRouterCredential(ctx, f.acc.ID); err == nil && c.CreditCheckedAt != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the immediate sweep never reached the store")
}

func TestCreditPollerStopIsIdempotent(t *testing.T) {
	_, p, _, _ := newCreditFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Interval = time.Hour
	p.Start(ctx)
	p.Stop()
	p.Stop()
}
