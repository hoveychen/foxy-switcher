package tui

import (
	"strings"
	"testing"
)

// The TUI rendered every account in Claude's shape: three subscription usage
// windows (5h / 7d Opus / 7d scoped) and an OAuth token countdown. An
// OpenRouter account has none of those — it is a long-lived API key with no
// expiry and no Anthropic usage windows — so those rows printed "no data" and
// "expired" on a perfectly healthy account. Same bug the web UI carried.
func TestInlineDetail_OpenRouterHasNoClaudeShape(t *testing.T) {
	a := Account{
		ID: 51, Name: "foxy", Provider: "openrouter",
		Status: "active", Plan: "OpenRouter",
		ExpiresAt: 0, // no OAuth token — the vault never sets one
	}
	m := &model{width: 140, height: 40}
	out := m.renderInlineDetail(a, 120)
	for _, banned := range []string{"5h", "7d Opus", "token"} {
		if strings.Contains(out, banned) {
			t.Errorf("OpenRouter inline detail still renders Claude-shaped %q:\n%s", banned, out)
		}
	}
}

func TestDetailPanel_OpenRouterHasNoClaudeShape(t *testing.T) {
	a := Account{
		ID: 51, Name: "foxy", Provider: "openrouter",
		Status: "active", Plan: "OpenRouter", ExpiresAt: 0,
	}
	m := &model{width: 140, height: 40, accounts: []Account{a}, cursor: 0}
	out := m.renderDetailBody(60)
	for _, banned := range []string{"7d Opus", "Token"} {
		if strings.Contains(out, banned) {
			t.Errorf("OpenRouter detail panel still renders Claude-shaped %q:\n%s", banned, out)
		}
	}
}

// Codex has two usage windows, not three, and they are not Anthropic's — the
// web UI already labels them Primary/Secondary and drops the scoped bar.
func TestDetailPanel_CodexUsesItsOwnWindowLabels(t *testing.T) {
	a := Account{
		ID: 3, Name: "harry", Provider: "codex",
		Status: "active", Plan: "Codex Team",
		FiveHour: &UsageWindow{Utilization: 10},
		SevenDay: &UsageWindow{Utilization: 20},
	}
	m := &model{width: 140, height: 40, accounts: []Account{a}, cursor: 0}
	out := m.renderDetailBody(60)
	if strings.Contains(out, "7d Opus") {
		t.Errorf("Codex detail panel labels a window as Claude's 7d Opus:\n%s", out)
	}
	if !strings.Contains(out, "Primary") || !strings.Contains(out, "Secondary") {
		t.Errorf("Codex detail panel should use Primary/Secondary labels:\n%s", out)
	}
}

// A Claude account must be untouched by all of the above.
func TestDetailPanel_ClaudeUnchanged(t *testing.T) {
	a := Account{
		ID: 1, Name: "alpha", Provider: "claude",
		Status: "active", Plan: "Claude Team Premium",
		ExpiresAt: 1 << 40,
		FiveHour:  &UsageWindow{Utilization: 10},
	}
	m := &model{width: 140, height: 40, accounts: []Account{a}, cursor: 0}
	out := m.renderDetailBody(60)
	for _, want := range []string{"5h", "7d Opus", "Token"} {
		if !strings.Contains(out, want) {
			t.Errorf("Claude detail panel lost %q:\n%s", want, out)
		}
	}
}
