package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hoveychen/foxy-switcher/server/deviceinfo"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpclient"
)

// unpairFlags is the parsed flag set for `foxy-switcher unpair`.
type unpairFlags struct {
	dataDir string
}

// AgentConfigName is the file in dataDir that holds the device token a
// successful pairing produced. The agent-mode daemon path loads this at
// startup so credinject can authenticate to the remote vault.
const AgentConfigName = "agent-config.json"

// AgentConfig is the persisted shape. Fields are intentionally minimal —
// auth and routing live here; everything else stays on the vault side.
type AgentConfig struct {
	VaultURL    string `json:"vault_url"`
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
}

// runPair is the `foxy-switcher pair` subcommand: it walks the device-
// flow handshake against a remote vault and writes the resulting token
// to dataDir/agent-config.json (mode 0600). The agent-mode daemon path
// reads this on startup to authenticate to the vault.
func runPair(f pairFlags) error {
	if f.vaultURL == "" {
		return errors.New("--vault-url is required")
	}
	dir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	name := f.deviceName
	if name == "" {
		hn, err := os.Hostname()
		if err != nil || hn == "" {
			hn = "unknown-host"
		}
		name = hn
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := httpclient.New(f.vaultURL)
	nonce := vaultauth.NewID()

	info := deviceinfo.Collect()
	meta := &httpclient.PairMetadata{
		Hostname:   info.Hostname,
		OS:         info.OS,
		OSVersion:  info.OSVersion,
		Arch:       info.Arch,
		Model:      info.Model,
		AppVersion: info.AppVersion,
		ClientType: "cli",
	}
	init, err := client.PairInit(ctx, name, nonce, meta)
	if err != nil {
		return fmt.Errorf("pair-init: %w", err)
	}

	fmt.Printf("Open %s and enter the code: %s\n", init.VerificationURL, init.UserCode)
	fmt.Printf("Waiting for approval (timeout %s)\n", time.Duration(init.ExpiresInMillis)*time.Millisecond)

	res, err := client.PairPoll(ctx, nonce, f.pollInterval)
	if err != nil {
		switch {
		case errors.Is(err, httpclient.ErrPairingDenied):
			return errors.New("pairing denied by the vault")
		case errors.Is(err, httpclient.ErrPairingExpired):
			return errors.New("pairing code expired before approval")
		default:
			return fmt.Errorf("pair-poll: %w", err)
		}
	}

	cfg := AgentConfig{
		VaultURL:    f.vaultURL,
		DeviceID:    res.DeviceID,
		DeviceToken: res.DeviceToken,
	}
	path := filepath.Join(dir, AgentConfigName)
	if err := writeAgentConfig(path, cfg); err != nil {
		return fmt.Errorf("write agent config: %w", err)
	}
	fmt.Printf("Pairing approved. Device id %s\n", res.DeviceID)
	fmt.Printf("Token saved to %s (mode 0600).\n", path)
	return nil
}

// runUnpair is the `foxy-switcher unpair` subcommand: it removes the
// agent-config.json file written by `pair`, so the next daemon / TUI
// launch falls back to local combined mode. Writes status messages to
// out (os.Stdout in production, a buffer in tests) so callers can verify
// the user-visible feedback without parsing stdio.
func runUnpair(f unpairFlags, out io.Writer) error {
	dir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	path := filepath.Join(dir, AgentConfigName)
	switch _, err := os.Stat(path); {
	case err == nil:
		// File exists; remove it.
	case os.IsNotExist(err):
		fmt.Fprintf(out, "Not paired (%s does not exist). Nothing to do.\n", path)
		return nil
	default:
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	fmt.Fprintf(out, "Unpaired (removed %s). Restart the daemon or TUI to fall back to local mode.\n", path)
	return nil
}

// writeAgentConfig writes the file via tmp+rename so a crash mid-write
// can't leave the agent with a half-baked token.
func writeAgentConfig(path string, cfg AgentConfig) error {
	buf, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
