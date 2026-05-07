package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnpair_NotPaired(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	if err := runUnpair(unpairFlags{dataDir: dir}, &buf); err != nil {
		t.Fatalf("runUnpair: %v", err)
	}
	if !strings.Contains(buf.String(), "Not paired") {
		t.Fatalf("expected 'Not paired' message, got %q", buf.String())
	}
	// File must not exist either before or after.
	if _, err := os.Stat(filepath.Join(dir, AgentConfigName)); !os.IsNotExist(err) {
		t.Fatalf("expected agent-config.json to not exist, stat returned %v", err)
	}
}

func TestUnpair_RemovesAgentConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AgentConfigName)
	body := []byte(`{"vault_url":"https://x","device_id":"y","device_token":"z"}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("seed agent-config.json: %v", err)
	}

	var buf bytes.Buffer
	if err := runUnpair(unpairFlags{dataDir: dir}, &buf); err != nil {
		t.Fatalf("runUnpair: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected agent-config.json to be removed, stat returned %v", err)
	}
	if !strings.Contains(buf.String(), "Unpaired") {
		t.Fatalf("expected 'Unpaired' message, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "Restart") {
		t.Fatalf("expected 'Restart' hint in output, got %q", buf.String())
	}
}
