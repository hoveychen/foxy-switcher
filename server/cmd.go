package main

import (
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pairFlags is the parsed flag set for `foxy-switcher pair`.
type pairFlags struct {
	vaultURL     string
	deviceName   string
	dataDir      string
	pollInterval time.Duration
}

// serverFlags is the parsed flag set for `foxy-switcher server` (and the
// legacy `--server` alias used by the Tauri sidecar).
type serverFlags struct {
	dataDir      string
	port         int
	parentPID    int
	noCredInject bool
	mode         string
	bindHost     string
}

// tuiFlags is the parsed flag set for `foxy-switcher tui` and the bare
// `foxy-switcher` invocation.
type tuiFlags struct {
	dataDir      string
	noCredInject bool
}

// rewriteLegacyServerFlag rewrites a top-level `--server` / `-server` argv
// (the form the Tauri sidecar still passes) into the `server` subcommand so
// cobra picks the right command tree without us having to register a
// duplicate flag at the root. Idempotent: if `--server` is absent the argv
// is left alone. Must run before cobra.Execute.
func rewriteLegacyServerFlag() {
	for i, a := range os.Args[1:] {
		if a != "--server" && a != "-server" {
			continue
		}
		// Slice in `server` as the first non-binary argument. The remaining
		// flags (--port, --data-dir, --parent-pid, ...) are already correct
		// for serverCmd's flag set.
		out := make([]string, 0, len(os.Args))
		out = append(out, os.Args[0], "server")
		out = append(out, os.Args[1:1+i]...)
		out = append(out, os.Args[1+i+1:]...)
		os.Args = out
		return
	}
}

func newRootCmd() *cobra.Command {
	var tui tuiFlags
	cmd := &cobra.Command{
		Use:   "foxy-switcher",
		Short: "Hand Claude Code a fresh OAuth token from a pool of subscription accounts.",
		Long: `foxy-switcher is a single-user localhost daemon that hands Claude Code
a fresh OAuth access_token from a pool of subscription accounts. Run as a
Tauri sidecar (the GUI manages its lifecycle), as a standalone daemon
(` + "`server`" + `), as a paired agent talking to a remote vault (` + "`pair`" + ` first,
then ` + "`server`" + `), or as an interactive TUI.

With no subcommand the bare ` + "`foxy-switcher`" + ` invocation launches the TUI,
embedding a daemon if none is already serving on the data dir.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTUI(tui)
		},
	}
	bindTUIFlags(cmd.Flags(), &tui)
	cmd.AddCommand(newPairCmd())
	cmd.AddCommand(newUnpairCmd())
	cmd.AddCommand(newTUICmd())
	cmd.AddCommand(newServerCmd())
	cmd.AddCommand(newCredCmd())
	return cmd
}

// newCredCmd groups the machine-facing credential helpers — commands another
// program executes, not ones a human types. Their contract is stricter than a
// normal subcommand's: stdout is the value, nothing else.
func newCredCmd() *cobra.Command {
	var dataDir string
	cmd := &cobra.Command{
		Use:   "cred",
		Short: "Credential helpers invoked by other tools (not usually run by hand).",
	}
	cmd.PersistentFlags().StringVar(&dataDir, "data-dir", "",
		"directory containing the daemon's port file (default ~/.foxy-switcher)")

	token := &cobra.Command{
		Use:   "openrouter-token",
		Short: "Print this device's current OpenRouter API key (used by codex).",
		Long: `openrouter-token prints the OpenRouter runtime key the vault issued to
this device, and nothing else — codex reads the whole of stdout as the
bearer token.

codex invokes this itself: foxy writes an
` + "`[model_providers.openrouter.auth] command = …`" + ` entry into config.toml
so the key is fetched from the running daemon on demand instead of being
stored on disk. Running it by hand is only useful for debugging.

Exits non-zero (with a message on stderr) when the daemon isn't running or
this device isn't authorised for OpenRouter, so codex reports an auth
failure rather than sending an empty token.`,
		SilenceUsage: true,
		Args:         cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runCredOpenRouterToken(dataDir, c.OutOrStdout())
		},
	}
	cmd.AddCommand(token)
	return cmd
}

func newUnpairCmd() *cobra.Command {
	var f unpairFlags
	cmd := &cobra.Command{
		Use:   "unpair",
		Short: "Forget the paired vault and fall back to local mode.",
		Long: `unpair removes the device token written by ` + "`pair`" + ` (the
agent-config.json file in the data dir). The next daemon / TUI launch will
auto-detect that no pairing exists and fall back to local combined mode.

This only deletes the local file; the vault retains its record of the
device. To revoke from the vault side, sign in to the vault's admin UI
and remove the device from the device list.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUnpair(f, os.Stdout)
		},
	}
	cmd.Flags().StringVar(&f.dataDir, "data-dir", "", "directory containing agent-config.json (default ~/.foxy-switcher)")
	return cmd
}

func newPairCmd() *cobra.Command {
	var f pairFlags
	cmd := &cobra.Command{
		Use:   "pair",
		Short: "Pair this device with a remote vault (device-flow handshake).",
		Long: `pair walks the device-flow handshake against a remote vault and writes
the resulting device token to ~/.foxy-switcher/agent-config.json (mode
0600). Once paired, ` + "`foxy-switcher server`" + ` (or the desktop sidecar)
auto-detects agent mode and reverse-proxies /api/* to the vault.

Use ` + "`unpair`" + ` to undo a pairing.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runPair(f)
		},
	}
	cmd.Flags().StringVar(&f.vaultURL, "vault-url", "", "https://vault.example.com — the remote vault to pair with (required)")
	cmd.Flags().StringVar(&f.deviceName, "device-name", "", "human-readable name shown on the vault's approval page; defaults to hostname")
	cmd.Flags().StringVar(&f.dataDir, "data-dir", "", "directory to write agent-config.json (default ~/.foxy-switcher)")
	cmd.Flags().DurationVar(&f.pollInterval, "poll-interval", 2*time.Second, "how often to ask the vault if the user has approved yet")
	_ = cmd.MarkFlagRequired("vault-url")
	return cmd
}

func newTUICmd() *cobra.Command {
	var f tuiFlags
	cmd := &cobra.Command{
		Use:   "tui",
		Short: "Launch the interactive TUI (default action when no subcommand is given).",
		Long: `tui attaches to a daemon already serving on the data dir, or embeds one
for the session if none is found. The bare ` + "`foxy-switcher`" + ` invocation
runs this same path.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runTUI(f)
		},
	}
	bindTUIFlags(cmd.Flags(), &f)
	return cmd
}

func newServerCmd() *cobra.Command {
	var f serverFlags
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the foxy-switcher daemon (Tauri sidecar / headless API).",
		Long: `server runs the daemon that exposes /api/* on a local TCP port. The
desktop app spawns this as a sidecar; CI / headless deployments invoke it
directly. Mode is auto-detected from the presence of agent-config.json
unless --mode= is set explicitly.

The legacy ` + "`foxy-switcher --server`" + ` argv (still used by the Tauri sidecar)
is rewritten to this subcommand at startup, so both forms work.`,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runServer(f)
		},
	}
	bindServerFlags(cmd.Flags(), &f)
	return cmd
}

func bindTUIFlags(fs *pflag.FlagSet, f *tuiFlags) {
	fs.StringVar(&f.dataDir, "data-dir", "", "directory containing the daemon's port/state files (default ~/.foxy-switcher)")
	fs.BoolVar(&f.noCredInject, "no-cred-inject", false, "embedded daemon: don't manage Claude Code's credential storage")
}

func bindServerFlags(fs *pflag.FlagSet, f *serverFlags) {
	fs.StringVar(&f.dataDir, "data-dir", "", "directory for state.db / port file (default ~/.foxy-switcher)")
	fs.IntVar(&f.port, "port", 0, "TCP port to bind on 127.0.0.1; 0 = random")
	fs.IntVar(&f.parentPID, "parent-pid", 0, "if non-zero, exit when this pid disappears (sidecar-mode safety net)")
	fs.BoolVar(&f.noCredInject, "no-cred-inject", false, "don't manage Claude Code's credential storage (no inject, no reverse-sync, no restore)")
	fs.StringVar(&f.mode, "mode", "", "deployment mode: combined|vault|agent (default: agent if ~/.foxy-switcher/agent-config.json exists, otherwise combined)")
	fs.StringVar(&f.bindHost, "bind-host", "127.0.0.1", "address to bind on; vault mode usually wants 0.0.0.0 behind a reverse proxy")
}
