package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cred.go implements `foxy-switcher cred openrouter-token`, the command codex
// executes (via [model_providers.openrouter.auth].command) every time it needs
// a bearer token.
//
// The whole point is that no OpenRouter key is ever written to disk: config.toml
// names this command instead of carrying a key, and the command asks the local
// daemon, which holds the key in memory. env_key would have been the obvious
// alternative, but codex is spawned by Fleet — foxy has no way to put anything
// in that process's environment.

// credTokenTimeout bounds the loopback call. codex applies its own timeout_ms
// (5s by default); staying under it means a stuck daemon surfaces as our clear
// error message rather than codex's generic timeout.
const credTokenTimeout = 3 * time.Second

// runCredOpenRouterToken prints the current OpenRouter runtime key to stdout
// and nothing else — codex reads the whole of stdout as the token, so a stray
// log line would become part of the bearer value. Diagnostics go to stderr and
// a non-zero exit, which codex reports as an auth failure instead of silently
// sending an empty token.
func runCredOpenRouterToken(dataDir string, stdout io.Writer) error {
	dir, err := resolveDataDir(dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	port, err := readDaemonPort(dir)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: credTokenTimeout}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/cred/openrouter-token", port))
	if err != nil {
		return fmt.Errorf("reach the foxy-switcher daemon on port %d: %w", port, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("daemon refused to issue an OpenRouter token: %s", msg)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		// Defensive: an empty 200 would otherwise become an empty bearer token
		// and produce an opaque 401 from OpenRouter.
		return fmt.Errorf("daemon returned an empty OpenRouter token")
	}
	_, err = io.WriteString(stdout, token)
	return err
}

// readDaemonPort reads the port file the daemon writes on bind. A missing file
// is the common case when the daemon isn't running, so it gets its own message
// — codex surfaces the command's stderr, and "port file not found" is more
// actionable than "connection refused".
func readDaemonPort(dir string) (int, error) {
	path := filepath.Join(dir, "port")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, fmt.Errorf("foxy-switcher daemon is not running (%s does not exist)", path)
		}
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("port file %s contains %q", path, strings.TrimSpace(string(raw)))
	}
	return port, nil
}
