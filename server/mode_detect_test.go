package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDetectModeFromConfig pins the implicit mode rule the desktop
// onboarding leans on: agent-config.json present means the user has
// paired with a remote vault and the daemon should reverse-proxy to it
// (--mode=agent); absent means today's local-first default
// (--mode=combined). The explicit --mode= flag overrides this — that
// path is exercised by the runServer flag-parsing code, not here.
func TestDetectModeFromConfig(t *testing.T) {
	t.Run("missing agent-config.json -> combined", func(t *testing.T) {
		dir := t.TempDir()
		got := detectModeFromConfig(dir)
		if got != modeCombined {
			t.Fatalf("detectModeFromConfig(no config) = %v, want modeCombined", got)
		}
	})

	t.Run("present agent-config.json -> agent", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, AgentConfigName)
		if err := os.WriteFile(path, []byte(`{"vault_url":"https://x","device_id":"d","device_token":"t"}`), 0o600); err != nil {
			t.Fatalf("write agent-config.json: %v", err)
		}
		got := detectModeFromConfig(dir)
		if got != modeAgent {
			t.Fatalf("detectModeFromConfig(with config) = %v, want modeAgent", got)
		}
	})

	t.Run("non-existent dir -> combined", func(t *testing.T) {
		// Defensive: the caller is expected to have created the data dir
		// before invoking us, but if they haven't, we still want a sane
		// fallback rather than panicking.
		dir := filepath.Join(t.TempDir(), "does-not-exist")
		got := detectModeFromConfig(dir)
		if got != modeCombined {
			t.Fatalf("detectModeFromConfig(missing dir) = %v, want modeCombined", got)
		}
	})
}
