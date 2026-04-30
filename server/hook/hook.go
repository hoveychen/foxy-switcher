// Package hook installs and removes the apiKeyHelper bridge that Claude Code
// invokes to fetch tokens from foxy-switcher. The hook is owned by the server
// because it is only useful while the server is listening — Install runs at
// server startup, Uninstall runs on graceful shutdown.
//
// Behaviour matches the curl-based install.sh / uninstall.sh in
// server/assets: drop ~/.foxy-switcher/get-token.sh and patch the
// `apiKeyHelper` key in ~/.claude/settings.json. Install overwrites any prior
// apiKeyHelper without backup; Uninstall removes the key unconditionally.
package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hoveychen/foxy-switcher/server/assets"
)

// HelperPath returns the absolute path of the bridge script under the given
// data dir (~/.foxy-switcher/get-token.sh in the default layout).
func HelperPath(dataDir string) string {
	return filepath.Join(dataDir, "get-token.sh")
}

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// Install drops the helper script into dataDir and points apiKeyHelper at it.
// Existing apiKeyHelper values are overwritten without backup.
func Install(dataDir string) error {
	helper := HelperPath(dataDir)
	if err := os.MkdirAll(filepath.Dir(helper), 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(helper), err)
	}
	if err := os.WriteFile(helper, []byte(assets.HelperScript()), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", helper, err)
	}

	settings, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(settings), err)
	}

	data := map[string]any{}
	if b, err := os.ReadFile(settings); err == nil {
		_ = json.Unmarshal(b, &data)
	}
	data["apiKeyHelper"] = helper
	out, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(settings, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", settings, err)
	}
	return nil
}

// Uninstall removes apiKeyHelper from settings.json and deletes the helper
// script. Safe to call when nothing is installed.
func Uninstall(dataDir string) error {
	helper := HelperPath(dataDir)

	settings, err := settingsPath()
	if err == nil {
		if b, err := os.ReadFile(settings); err == nil {
			data := map[string]any{}
			if json.Unmarshal(b, &data) == nil {
				if _, ok := data["apiKeyHelper"]; ok {
					delete(data, "apiKeyHelper")
					if out, err := json.MarshalIndent(data, "", "  "); err == nil {
						out = append(out, '\n')
						_ = os.WriteFile(settings, out, 0o644)
					}
				}
			}
		}
	}

	_ = os.Remove(helper)
	return nil
}

// IsInstalled returns true iff the helper exists and apiKeyHelper points at it.
func IsInstalled(dataDir string) bool {
	helper := HelperPath(dataDir)
	if _, err := os.Stat(helper); err != nil {
		return false
	}
	settings, err := settingsPath()
	if err != nil {
		return false
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		return false
	}
	data := map[string]any{}
	if err := json.Unmarshal(b, &data); err != nil {
		return false
	}
	cur, _ := data["apiKeyHelper"].(string)
	return cur == helper
}
