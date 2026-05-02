package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// tuiConfig is the TUI-local persistence record. We deliberately keep this
// separate from the daemon's `/api/settings` so that the TUI's terminal-only
// preferences (theme, layout hints) don't leak into the desktop UI's settings
// panel and vice-versa. Stored at `<dataDir>/tui.json`.
type tuiConfig struct {
	// Theme is the persisted theme `Name` (matches Theme.Name). Empty / unknown
	// → LURA. Only the TUI reads or writes this field.
	Theme string `json:"theme,omitempty"`
}

// tuiConfigPath returns the on-disk location for the TUI config file. It
// co-locates with the rest of the daemon state directory so users only need to
// know about one path.
func tuiConfigPath(dataDir string) string {
	return filepath.Join(dataDir, "tui.json")
}

// loadTUIConfig reads the config file. Missing file or any decode error
// silently returns a zero-value config — the TUI must never fail to start
// because of a corrupt config.
func loadTUIConfig(dataDir string) tuiConfig {
	var cfg tuiConfig
	b, err := os.ReadFile(tuiConfigPath(dataDir))
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

// saveTUIConfig persists the config atomically (write-then-rename) so a crash
// mid-write can't leave a half-written file. Errors are returned but callers
// generally log-and-continue: config persistence is best-effort.
func saveTUIConfig(dataDir string, cfg tuiConfig) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	path := tuiConfigPath(dataDir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
