package tui

import (
	"strings"
	"testing"
)

// TestRenderVaultLabel_Empty: before /api/about resolves the banner is empty
// so the statusbar shows the daemon label alone, not a placeholder.
func TestRenderVaultLabel_Empty(t *testing.T) {
	if got := renderVaultLabel("", ""); got != "" {
		t.Fatalf("expected empty string for empty mode, got %q", got)
	}
}

// TestRenderVaultLabel_Local: combined mode (no agent-config.json) renders
// the local label so users on a fresh install can tell they're on the
// in-process vault.
func TestRenderVaultLabel_Local(t *testing.T) {
	got := renderVaultLabel("combined", "")
	if !strings.Contains(got, "vault: local") {
		t.Fatalf("expected 'vault: local' for combined mode, got %q", got)
	}
}

// TestRenderVaultLabel_Cloud_WithHost: agent mode + a vault URL renders the
// host name (not the full URL — keeps the bar compact, and queryparams /
// path / scheme are noise for at-a-glance recognition).
func TestRenderVaultLabel_Cloud_WithHost(t *testing.T) {
	got := renderVaultLabel("agent", "https://my.vault.example.com:8443/admin?x=1")
	if !strings.Contains(got, "cloud") {
		t.Fatalf("expected 'cloud' marker, got %q", got)
	}
	if !strings.Contains(got, "my.vault.example.com:8443") {
		t.Fatalf("expected host 'my.vault.example.com:8443' in label, got %q", got)
	}
	if strings.Contains(got, "/admin") {
		t.Fatalf("path should be stripped from banner, got %q", got)
	}
}

// TestRenderVaultLabel_Cloud_NoURL: agent mode without a URL (shouldn't
// happen in practice once decideOnboarding has run, but defensive — must
// not panic and must still mark the mode as cloud.
func TestRenderVaultLabel_Cloud_NoURL(t *testing.T) {
	got := renderVaultLabel("agent", "")
	if !strings.Contains(got, "cloud") {
		t.Fatalf("expected 'cloud' marker even with empty URL, got %q", got)
	}
}

// TestRenderVaultLabel_VaultMode: vault-only daemon (the cloud-side
// process). Distinct from local/cloud so a vault operator running the TUI
// against their own vault doesn't see a misleading "local".
func TestRenderVaultLabel_VaultMode(t *testing.T) {
	got := renderVaultLabel("vault", "")
	if !strings.Contains(got, "serving") {
		t.Fatalf("expected 'serving' marker for vault mode, got %q", got)
	}
}

// TestStatusbar_IncludesVaultLabel: end-to-end check that the vault label
// makes it into the rendered bar.
func TestStatusbar_IncludesVaultLabel(t *testing.T) {
	state := statusbarState{
		DaemonDot:   "●",
		DaemonLabel: "attached",
		VaultLabel:  "vault: cloud · my.vault.example.com",
		Breadcrumb:  "Accounts",
		NavHint:     "1-4 pages · ? help",
	}
	out := renderStatusbar(state, 200)
	if !strings.Contains(out, "vault: cloud · my.vault.example.com") {
		t.Fatalf("statusbar should contain vault label; got %q", out)
	}
}
