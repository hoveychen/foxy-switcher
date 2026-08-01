package tui

import "testing"

// planWeight returns CLAUDE Pro-equivalents. The weights only mean anything for
// a Claude subscription, so a non-Claude account must contribute nothing.
//
// The bug this pins: Codex accounts carry subscription_type from the ChatGPT
// plan type (server/openai/auth.go), which for a Codex Team account is the
// string "team" — the same token Claude's legacy fallback maps to 5x. With no
// rate_limit_tier to disambiguate (Codex never sets one), a Codex account was
// weighted as a Claude Max 5x in the pool KPIs.
func TestPlanWeight_NonClaudeWeighsNothing(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		tier     string
		sub      string
	}{
		// The live shape: `codex|team|<empty tier>`.
		{"codex team", "codex", "", "team"},
		{"codex plus", "codex", "", "plus"},
		{"codex pro", "codex", "", "pro"},
		{"openrouter payg", "openrouter", "", "payg"},
		// Defensive: even if a non-Claude row somehow carried a Claude tier,
		// provider is the authority.
		{"openrouter with claude tier", "openrouter", "default_claude_max_20x", "max"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w5h, w7d := planWeight(c.provider, c.tier, c.sub)
			if w5h != 0 || w7d != 0 {
				t.Fatalf("provider %q tier %q sub %q: want (0,0), got (%v,%v)",
					c.provider, c.tier, c.sub, w5h, w7d)
			}
		})
	}
}

// The Claude weights themselves must not shift — this is the regression guard
// for the fix above, which touches the same function.
func TestPlanWeight_ClaudeUnchanged(t *testing.T) {
	cases := []struct {
		tier, sub    string
		want5, want7 float64
	}{
		{"default_claude_pro", "", 1, 1},
		{"default_claude_max_5x", "", 5, 5},
		{"default_claude_max_20x", "", 20, 10},
		// Legacy rows with no tier backfilled yet.
		{"", "pro", 1, 1},
		{"", "max", 5, 5},
		{"", "team", 5, 5},
		{"", "team_premium", 5, 5},
		{"", "", 0, 0},
	}
	for _, c := range cases {
		w5h, w7d := planWeight("claude", c.tier, c.sub)
		if w5h != c.want5 || w7d != c.want7 {
			t.Fatalf("claude tier %q sub %q: want (%v,%v), got (%v,%v)",
				c.tier, c.sub, c.want5, c.want7, w5h, w7d)
		}
	}
	// Legacy rows predating the provider column carry "" — they are Claude.
	if w5h, w7d := planWeight("", "default_claude_max_5x", ""); w5h != 5 || w7d != 5 {
		t.Fatalf("empty provider must be treated as claude, got (%v,%v)", w5h, w7d)
	}
}
