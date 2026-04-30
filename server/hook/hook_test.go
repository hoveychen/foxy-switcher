package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// To keep the test from touching the real ~/.claude/settings.json, point HOME
// at a tmp dir so settingsPath() resolves into the sandbox.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

func TestInstallThenUninstall(t *testing.T) {
	home := sandbox(t)
	dataDir := filepath.Join(home, ".foxy-switcher")

	if IsInstalled(dataDir) {
		t.Fatal("expected not installed before Install")
	}

	if err := Install(dataDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if !IsInstalled(dataDir) {
		t.Fatal("expected installed after Install")
	}

	settings := filepath.Join(home, ".claude", "settings.json")
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if got, want := data["apiKeyHelper"], HelperPath(dataDir); got != want {
		t.Fatalf("apiKeyHelper = %v, want %v", got, want)
	}

	helperBody, err := os.ReadFile(HelperPath(dataDir))
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	if len(helperBody) == 0 {
		t.Fatal("helper script is empty")
	}

	if err := Uninstall(dataDir); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if IsInstalled(dataDir) {
		t.Fatal("expected not installed after Uninstall")
	}
	if _, err := os.Stat(HelperPath(dataDir)); !os.IsNotExist(err) {
		t.Fatalf("helper script should be removed, stat err = %v", err)
	}

	b, err = os.ReadFile(settings)
	if err != nil {
		t.Fatalf("read settings post-uninstall: %v", err)
	}
	var dataAfter map[string]any
	if err := json.Unmarshal(b, &dataAfter); err != nil {
		t.Fatalf("unmarshal post-uninstall: %v", err)
	}
	if _, present := dataAfter["apiKeyHelper"]; present {
		t.Fatal("apiKeyHelper should be absent after Uninstall")
	}
}

func TestInstallPreservesUnrelatedKeys(t *testing.T) {
	home := sandbox(t)
	dataDir := filepath.Join(home, ".foxy-switcher")

	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	prior := map[string]any{
		"theme":      "dark",
		"otherThing": map[string]any{"nested": true},
	}
	priorBytes, _ := json.MarshalIndent(prior, "", "  ")
	if err := os.WriteFile(settings, priorBytes, 0o644); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := Install(dataDir); err != nil {
		t.Fatalf("Install: %v", err)
	}

	b, _ := os.ReadFile(settings)
	var data map[string]any
	_ = json.Unmarshal(b, &data)
	if data["theme"] != "dark" {
		t.Errorf("theme key lost: %v", data["theme"])
	}
	if data["apiKeyHelper"] != HelperPath(dataDir) {
		t.Errorf("apiKeyHelper not set: %v", data["apiKeyHelper"])
	}

	if err := Uninstall(dataDir); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	b, _ = os.ReadFile(settings)
	var dataAfter map[string]any
	_ = json.Unmarshal(b, &dataAfter)
	if dataAfter["theme"] != "dark" {
		t.Errorf("theme key lost after uninstall: %v", dataAfter["theme"])
	}
	if _, present := dataAfter["apiKeyHelper"]; present {
		t.Errorf("apiKeyHelper should be gone")
	}
}
