package selector

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "sel.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestPickSkipsAccountAtThreshold guards the per-account threshold gate: an
// account whose 5h util has reached its 5h threshold must be skipped even
// though Anthropic itself wouldn't 429 yet.
func TestPickSkipsAccountAtThreshold(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	a := &store.Account{Name: "throttled", Email: "t@x", AccessToken: "at-a", RefreshToken: "rt", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	b := &store.Account{Name: "fresh", Email: "f@x", AccessToken: "at-b", RefreshToken: "rt", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	// `a` sits at 96% on 5h with a custom 95% threshold → must be filtered.
	// `b` has no usage row populated yet → not filtered, default thresholds.
	if err := st.SetUsage(ctx, a.ID,
		96.0, "2026-04-30T05:00:00Z",
		10.0, "2026-05-07T00:00:00Z",
		10.0, "2026-05-07T00:00:00Z", "",
	); err != nil {
		t.Fatalf("set usage a: %v", err)
	}
	if err := st.SetThresholds(ctx, a.ID, 95, 100, 100); err != nil {
		t.Fatalf("set thresholds a: %v", err)
	}

	got, err := Pick(ctx, st, time.Now())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != b.ID {
		t.Fatalf("expected fresh account %d, got %d", b.ID, got.ID)
	}
}

// TestPickIgnoresEmptyResetsAt covers the cold-start path: an account whose
// usage row was never populated (resets_at == "") must not be filtered out by
// the threshold gate, otherwise a fresh pool with a 0-default util would lock
// everyone out before the first usage poll lands.
func TestPickIgnoresEmptyResetsAt(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	a := &store.Account{Name: "cold", Email: "c@x", AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Manually drive thresholds to 0 — would normally trip on util=0,
	// but resets_at is still empty (no poll has run) so we must let it through.
	if err := st.SetThresholds(ctx, a.ID, 0, 0, 0); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	got, err := Pick(ctx, st, time.Now())
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != a.ID {
		t.Fatalf("expected cold account picked, got id=%d", got.ID)
	}
}

// TestPickSkipsAccountWithExpiredToken guards the expired-token gate: an
// account whose access_token is past its expires_at must be skipped, even
// when Status==active and no cooldown is set. The selector and the
// credinject sticky path must agree to fall over to a healthy account
// rather than re-injecting a dead token.
func TestPickSkipsAccountWithExpiredToken(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	now := time.Now()
	expired := &store.Account{
		Name: "expired", Email: "e@x", AccessToken: "at-e", RefreshToken: "rt",
		ExpiresAt: now.Add(-5 * time.Minute).UnixMilli(),
	}
	if err := st.Upsert(ctx, expired); err != nil {
		t.Fatalf("upsert expired: %v", err)
	}
	fresh := &store.Account{
		Name: "fresh", Email: "f@x", AccessToken: "at-f", RefreshToken: "rt",
		ExpiresAt: now.Add(2 * time.Hour).UnixMilli(),
	}
	if err := st.Upsert(ctx, fresh); err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}
	// Force the expired account to be the LRU favourite — without the
	// expired filter it would win the pick.
	if err := st.MarkForNextPick(ctx, expired.ID, ""); err != nil {
		t.Fatalf("mark for next pick: %v", err)
	}

	got, err := Pick(ctx, st, now)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != fresh.ID {
		t.Fatalf("expected fresh account %d, got %d (expired token must not be eligible)", fresh.ID, got.ID)
	}

	if IsEligible(*expired, now) {
		t.Fatalf("IsEligible returned true for expired token")
	}
}

// TestPickAllOverThresholdReturnsErrNoAvailable is the "everyone hit their
// cap" path — credinject treats this as the trigger to restore native creds.
func TestPickAllOverThresholdReturnsErrNoAvailable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	a := &store.Account{Name: "a", Email: "a@x", AccessToken: "at-a", RefreshToken: "rt", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	if err := st.SetUsage(ctx, a.ID,
		99.0, "2026-04-30T05:00:00Z",
		0, "", 0, "", "",
	); err != nil {
		t.Fatalf("set usage: %v", err)
	}
	if err := st.SetThresholds(ctx, a.ID, 95, 100, 100); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	if _, err := Pick(ctx, st, time.Now()); !errors.Is(err, ErrNoAvailable) {
		t.Fatalf("expected ErrNoAvailable, got %v", err)
	}
}

// TestPickScopedExceededStaysUsable is the core regression for the
// scoped-model (Fable) soft-degrade: an account whose only maxed window is the
// weekly-scoped one (seven_day_sonnet slot) must remain selectable — the
// account's 5h / 7d headroom means Opus etc. still work, only the scoped model
// is capped. The old behaviour hard-excluded it, so a pool where every account
// had Fable maxed wrongly returned ErrNoAvailable and dropped to native creds.
func TestPickScopedExceededStaysUsable(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()

	a := &store.Account{Name: "fable-maxed", Email: "fm@x", AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 5h + 7d have plenty of headroom; only the scoped (Fable) window is maxed.
	if err := st.SetUsage(ctx, a.ID,
		10, "2026-04-30T05:00:00Z",
		20, "2026-05-07T00:00:00Z",
		99, "2026-05-07T00:00:00Z", "Fable",
	); err != nil {
		t.Fatalf("set usage: %v", err)
	}
	if err := st.SetThresholds(ctx, a.ID, 95, 95, 95); err != nil {
		t.Fatalf("set thresholds: %v", err)
	}

	got, err := Pick(ctx, st, time.Now())
	if err != nil {
		t.Fatalf("scoped-exceeded account must stay usable (5h/7d have headroom), got %v", err)
	}
	if got.ID != a.ID {
		t.Fatalf("expected account %d, got %d", a.ID, got.ID)
	}
}

// TestPickAllScopedExceededStillPicks: when EVERY account has the scoped window
// maxed (but 5h/7d fine), the pool is not exhausted — Pick still returns a
// degraded account (lowest 7d runway within the degraded tier), never
// ErrNoAvailable.
func TestPickAllScopedExceededStillPicks(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()

	lowRunway := &store.Account{Name: "low7d", Email: "l@x", AccessToken: "at1", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	highRunway := &store.Account{Name: "high7d", Email: "h@x", AccessToken: "at2", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, lowRunway); err != nil {
		t.Fatalf("upsert low: %v", err)
	}
	if err := st.Upsert(ctx, highRunway); err != nil {
		t.Fatalf("upsert high: %v", err)
	}
	// Both scoped-maxed; lowRunway has the better (lower) 7d utilization.
	if err := st.SetUsage(ctx, lowRunway.ID, 0, "", 5, "2026-05-07T00:00:00Z", 99, "2026-05-07T00:00:00Z", "Fable"); err != nil {
		t.Fatalf("set usage low: %v", err)
	}
	if err := st.SetUsage(ctx, highRunway.ID, 0, "", 50, "2026-05-07T00:00:00Z", 99, "2026-05-07T00:00:00Z", "Fable"); err != nil {
		t.Fatalf("set usage high: %v", err)
	}
	for _, id := range []int64{lowRunway.ID, highRunway.ID} {
		if err := st.SetThresholds(ctx, id, 95, 95, 95); err != nil {
			t.Fatalf("set thresholds: %v", err)
		}
	}

	got, err := Pick(ctx, st, now)
	if err != nil {
		t.Fatalf("all-scoped-exceeded pool must still pick a degraded account, got %v", err)
	}
	if got.ID != lowRunway.ID {
		t.Fatalf("expected lowest-7d degraded account %d, got %d", lowRunway.ID, got.ID)
	}
}

// TestPickPrefersScopedOkOverDegraded pins the tier ordering: a scoped-OK
// account is chosen over a scoped-exceeded (degraded) one even when the
// degraded account has more 7d runway. Runway only breaks ties WITHIN a tier.
func TestPickPrefersScopedOkOverDegraded(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()

	degradedBetterRunway := &store.Account{Name: "degraded", Email: "d@x", AccessToken: "at1", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	okWorseRunway := &store.Account{Name: "ok", Email: "o@x", AccessToken: "at2", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, degradedBetterRunway); err != nil {
		t.Fatalf("upsert degraded: %v", err)
	}
	if err := st.Upsert(ctx, okWorseRunway); err != nil {
		t.Fatalf("upsert ok: %v", err)
	}
	// degraded: great 7d runway but Fable maxed. ok: worse 7d runway, Fable fine.
	if err := st.SetUsage(ctx, degradedBetterRunway.ID, 0, "", 5, "2026-05-07T00:00:00Z", 99, "2026-05-07T00:00:00Z", "Fable"); err != nil {
		t.Fatalf("set usage degraded: %v", err)
	}
	if err := st.SetUsage(ctx, okWorseRunway.ID, 0, "", 50, "2026-05-07T00:00:00Z", 10, "2026-05-07T00:00:00Z", "Fable"); err != nil {
		t.Fatalf("set usage ok: %v", err)
	}
	for _, id := range []int64{degradedBetterRunway.ID, okWorseRunway.ID} {
		if err := st.SetThresholds(ctx, id, 95, 95, 95); err != nil {
			t.Fatalf("set thresholds: %v", err)
		}
	}

	got, err := Pick(ctx, st, now)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != okWorseRunway.ID {
		t.Fatalf("expected scoped-OK account %d (high tier) over degraded %d despite worse runway, got %d", okWorseRunway.ID, degradedBetterRunway.ID, got.ID)
	}
}

func TestPickProviderKeepsClaudeAndCodexPoolsIsolated(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()

	claude := &store.Account{
		Provider: store.ProviderClaude, Name: "claude", AccountUUID: "same-user",
		AccessToken: "claude-at", RefreshToken: "claude-rt", ExpiresAt: now.Add(time.Hour).UnixMilli(),
	}
	codex := &store.Account{
		Provider: store.ProviderCodex, Name: "codex", AccountUUID: "same-user",
		AccessToken: "codex-at", RefreshToken: "codex-rt", ExpiresAt: now.Add(time.Hour).UnixMilli(),
	}
	for _, account := range []*store.Account{claude, codex} {
		if err := st.Upsert(ctx, account); err != nil {
			t.Fatalf("upsert %s: %v", account.Provider, err)
		}
	}

	gotDefault, err := Pick(ctx, st, now)
	if err != nil {
		t.Fatalf("Pick default: %v", err)
	}
	if gotDefault.ID != claude.ID {
		t.Fatalf("default pool picked provider %q", gotDefault.Provider)
	}

	gotCodex, err := PickProvider(ctx, st, store.ProviderCodex, now)
	if err != nil {
		t.Fatalf("PickProvider codex: %v", err)
	}
	if gotCodex.ID != codex.ID {
		t.Fatalf("codex pool picked provider %q", gotCodex.Provider)
	}
}

// TestPickPrefersLowestSevenDayOverLRU pins the rotation policy: among eligible
// candidates the account with the most weekly runway (lowest 7-day utilization)
// wins, even when it was used more recently than a higher-utilization peer.
// LRU is only the tiebreaker. This holds for Codex too, since the usage poller
// stores Codex's weekly "secondary" window in the same SevenDayUtil field.
func TestPickPrefersLowestSevenDayOverLRU(t *testing.T) {
	st := openStore(t)
	ctx := context.Background()
	now := time.Now()

	// stale: high 7-day utilization but least-recently-used (the old LRU
	// favourite). fresh: low 7-day utilization but most-recently-used.
	stale := &store.Account{Name: "stale-high7d", Email: "s@x", AccessToken: "at-s", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, stale); err != nil {
		t.Fatalf("upsert stale: %v", err)
	}
	fresh := &store.Account{Name: "fresh-low7d", Email: "f@x", AccessToken: "at-f", RefreshToken: "rt", ExpiresAt: now.Add(2 * time.Hour).UnixMilli()}
	if err := st.Upsert(ctx, fresh); err != nil {
		t.Fatalf("upsert fresh: %v", err)
	}

	// Both eligible (well under the default 7-day threshold); only the 7-day
	// utilization differs. resets_at must be non-empty so the value counts.
	if err := st.SetUsage(ctx, stale.ID, 0, "", 50.0, "2026-05-07T00:00:00Z", 0, "", ""); err != nil {
		t.Fatalf("set usage stale: %v", err)
	}
	if err := st.SetUsage(ctx, fresh.ID, 0, "", 10.0, "2026-05-07T00:00:00Z", 0, "", ""); err != nil {
		t.Fatalf("set usage fresh: %v", err)
	}

	// Make `fresh` the MOST-recently-used so plain LRU would reject it and
	// pick `stale` instead. The new policy must still pick `fresh`.
	if err := st.MarkUsed(ctx, fresh.ID); err != nil {
		t.Fatalf("mark used fresh: %v", err)
	}

	got, err := Pick(ctx, st, now)
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if got.ID != fresh.ID {
		t.Fatalf("expected lowest-7day account %d (fresh-low7d), got %d — rotation must prefer weekly runway over LRU", fresh.ID, got.ID)
	}
}
