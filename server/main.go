// foxy-switcher server: a single-user localhost daemon that hands Claude Code
// a fresh OAuth access_token from a pool of subscription accounts. Designed
// to run as a Tauri sidecar (the GUI manages its lifecycle), but also runs
// fine as a standalone binary for headless setups.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/hook"
	"github.com/hoveychen/foxy-switcher/server/httpapi"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/tui"
)

func main() {
	// Subcommand dispatch. Default (no args, or flag-prefixed args) keeps
	// the daemon's existing CLI surface intact so the Tauri sidecar
	// invocation `foxy-switcher --port=N --parent-pid=P …` still works
	// unchanged.
	if len(os.Args) > 1 && os.Args[1] == "tui" {
		runTUI(os.Args[2:])
		return
	}

	var (
		dataDir   = flag.String("data-dir", "", "directory for state.db / port file (default ~/.foxy-switcher)")
		port      = flag.Int("port", 0, "TCP port to bind on 127.0.0.1; 0 = random")
		parentPID = flag.Int("parent-pid", 0, "if non-zero, exit when this pid disappears (sidecar-mode safety net)")
		noHook    = flag.Bool("no-hook", false, "don't manage the apiKeyHelper hook (no install/uninstall, no reconcile loop) — useful for debugging /api/token in isolation")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

	// Use os.Exit through a deferred closure so cleanup defers (hook.Uninstall,
	// store.Close, port file removal) run before we surface a non-zero exit
	// code. os.Exit itself skips defers — that's why log.Fatalf is unsafe here.
	exitCode := 0
	defer func() { os.Exit(exitCode) }()

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		logger.Fatalf("resolve data dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		logger.Fatalf("mkdir %s: %v", dir, err)
	}

	dbPath := filepath.Join(dir, "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		logger.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := os.Chmod(dbPath, 0o600); err != nil && !os.IsNotExist(err) {
		logger.Printf("warning: chmod %s: %v", dbPath, err)
	}

	pkce := authz.NewPKCEStore()
	rf := refresh.New(st, logger)
	up := refresh.NewUsagePoller(st, logger)

	server := httpapi.New(st, pkce, rf, dir)

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Fatalf("listen %s: %v", addr, err)
	}
	tcp := ln.Addr().(*net.TCPAddr)
	server.Port = tcp.Port

	portFile := filepath.Join(dir, "port")
	if err := writePortFile(portFile, tcp.Port); err != nil {
		ln.Close()
		logger.Fatalf("write port file: %v", err)
	}
	defer os.Remove(portFile)

	// Don't install the hook eagerly — let the coordinator decide based on
	// pool availability (avoid blocking Claude Code during bootstrap when the
	// pool is empty, or during periods when every account is rate-limited).
	// The deferred Uninstall is the final safety net for graceful shutdown.
	//
	// --no-hook skips both sides: nothing to uninstall on shutdown, no
	// reconcile loop on startup. Used to debug /api/token without the helper
	// rewriting ~/.claude/settings.json.
	if !*noHook {
		defer func() {
			if err := hook.Uninstall(dir); err != nil {
				logger.Printf("warning: uninstall apiKeyHelper hook: %v", err)
			} else {
				logger.Print("apiKeyHelper hook removed")
			}
		}()
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *parentPID > 0 {
		go watchParent(ctx, cancel, *parentPID, 2*time.Second, logger)
	}

	if !*noHook {
		hookCoord := server.NewHookCoordinator(logger, 5*time.Second)
		server.Hook = hookCoord
		// Wire callbacks BEFORE Start so the spawned goroutines see them on
		// their first tick. Trigger() is non-blocking and idempotent, so it's
		// safe to fire even before hookCoord.Run starts consuming.
		rf.OnChange = hookCoord.Trigger
		up.OnChange = hookCoord.Trigger
		go hookCoord.Run(ctx)
	} else {
		logger.Print("--no-hook: apiKeyHelper hook lifecycle disabled")
	}

	rf.Start(ctx)
	defer rf.Stop()
	up.Start(ctx)
	defer up.Stop()

	httpSrv := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Printf("foxy-switcher listening on http://%s (data: %s)", tcp.String(), dir)
	serveErr := httpSrv.Serve(ln)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		// Don't log.Fatalf here — os.Exit would skip the deferred
		// hook.Uninstall. Log it; arrange exit(1) via a defer scheduled
		// AFTER all the cleanup defers so they run first (LIFO order).
		logger.Printf("serve: %v", serveErr)
		exitCode = 1
	}
	logger.Print("shutdown complete")
}

func resolveDataDir(override string) (string, error) {
	if override != "" {
		abs, err := filepath.Abs(override)
		return abs, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".foxy-switcher"), nil
}

// writePortFile writes the listening port atomically (write tmp + rename) so
// the helper script never reads a half-written file.
func writePortFile(path string, port int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runTUI handles the `tui` subcommand. Re-parses flags from the post-`tui`
// argv tail so `foxy-switcher tui --data-dir=…` works the same way the
// daemon understands its --data-dir flag.
func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "directory containing the daemon's port/state files (default ~/.foxy-switcher)")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve data dir:", err)
		os.Exit(1)
	}
	if err := tui.Run(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
