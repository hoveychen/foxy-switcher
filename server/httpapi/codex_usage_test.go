package httpapi

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"
)

func TestApplyCodexUsagePreservesWindowDurationInView(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	a := &store.Account{
		Provider: store.ProviderCodex, Name: "business", Email: "codex@example.com",
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
	}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	u := &openai.Usage{
		Primary:   &openai.UsageWindow{UsedPercent: 70, ResetAt: time.Unix(2_000_000_000, 0), WindowSeconds: 604800},
		Secondary: &openai.UsageWindow{UsedPercent: 15, ResetAt: time.Unix(2_000_018_000, 0), WindowSeconds: 18000},
	}
	if err := applyCodexUsage(ctx, st, a.ID, u); err != nil {
		t.Fatalf("applyCodexUsage: %v", err)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	view := toView(*got)
	if view.FiveHour == nil || view.FiveHour.WindowSeconds != 604800 {
		t.Fatalf("primary window = %+v, want window_seconds=604800", view.FiveHour)
	}
	if view.SevenDay == nil || view.SevenDay.WindowSeconds != 18000 {
		t.Fatalf("secondary window = %+v, want window_seconds=18000", view.SevenDay)
	}
}
