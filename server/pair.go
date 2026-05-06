package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hoveychen/foxy-switcher/server/deviceinfo"
	vaultauth "github.com/hoveychen/foxy-switcher/server/vault/auth"
	"github.com/hoveychen/foxy-switcher/server/vault/httpclient"
)

// AgentConfigName is the file in dataDir that holds the device token a
// successful pairing produced. Step 5's --mode=agent path will load this
// at startup so credinject can authenticate to the remote vault.
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
// to dataDir/agent-config.json (mode 0600). Step 5 will read this on
// `--mode=agent` startup.
func runPair(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	vaultURL := fs.String("vault-url", "", "https://vault.example.com — the remote vault to pair with")
	deviceName := fs.String("device-name", "", "human-readable name shown on the vault's approval page; defaults to hostname")
	dataDir := fs.String("data-dir", "", "directory to write agent-config.json (default ~/.foxy-switcher)")
	pollInterval := fs.Duration("poll-interval", 2*time.Second, "how often to ask the vault if the user has approved yet")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if *vaultURL == "" {
		fmt.Fprintln(os.Stderr, "pair: --vault-url is required")
		os.Exit(2)
	}
	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve data dir:", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, "mkdir:", err)
		os.Exit(1)
	}
	name := *deviceName
	if name == "" {
		hn, err := os.Hostname()
		if err != nil || hn == "" {
			hn = "unknown-host"
		}
		name = hn
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	client := httpclient.New(*vaultURL)
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
		fmt.Fprintln(os.Stderr, "pair-init:", err)
		os.Exit(1)
	}

	fmt.Printf("Open %s and enter the code: %s\n", init.VerificationURL, init.UserCode)
	fmt.Printf("Waiting for approval (timeout %s)\n", time.Duration(init.ExpiresInMillis)*time.Millisecond)

	res, err := client.PairPoll(ctx, nonce, *pollInterval)
	if err != nil {
		switch {
		case errors.Is(err, httpclient.ErrPairingDenied):
			fmt.Fprintln(os.Stderr, "Pairing denied by the vault.")
		case errors.Is(err, httpclient.ErrPairingExpired):
			fmt.Fprintln(os.Stderr, "Pairing code expired before approval.")
		default:
			fmt.Fprintln(os.Stderr, "pair-poll:", err)
		}
		os.Exit(1)
	}

	cfg := AgentConfig{
		VaultURL:    *vaultURL,
		DeviceID:    res.DeviceID,
		DeviceToken: res.DeviceToken,
	}
	path := filepath.Join(dir, AgentConfigName)
	if err := writeAgentConfig(path, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "write agent config:", err)
		os.Exit(1)
	}
	fmt.Printf("Pairing approved. Device id %s\n", res.DeviceID)
	fmt.Printf("Token saved to %s (mode 0600).\n", path)
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
