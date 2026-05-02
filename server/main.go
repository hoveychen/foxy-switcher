// foxy-switcher server: a single-user localhost daemon that hands Claude Code
// a fresh OAuth access_token from a pool of subscription accounts. Designed
// to run as a Tauri sidecar (the GUI manages its lifecycle), but also runs
// fine as a standalone binary for headless setups.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/authz"
	"github.com/hoveychen/foxy-switcher/server/credinject"
	"github.com/hoveychen/foxy-switcher/server/httpapi"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/tui"
	"github.com/hoveychen/foxy-switcher/server/vault"
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
		dataDir      = flag.String("data-dir", "", "directory for state.db / port file (default ~/.foxy-switcher)")
		port         = flag.Int("port", 0, "TCP port to bind on 127.0.0.1; 0 = random")
		parentPID    = flag.Int("parent-pid", 0, "if non-zero, exit when this pid disappears (sidecar-mode safety net)")
		noCredInject = flag.Bool("no-cred-inject", false, "don't manage Claude Code's credential storage (no inject, no reverse-sync, no restore) — useful when running side-by-side with a real native login for debugging")
	)
	flag.Parse()

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "resolve data dir:", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runDaemon(ctx, daemonOpts{
		DataDir:      dir,
		Port:         *port,
		ParentPID:    *parentPID,
		NoCredInject: *noCredInject,
	}, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// daemonOpts is the input to runDaemon — everything main()'s flags would have
// supplied, packaged so runTUI can spawn an embedded daemon with the same
// surface area.
type daemonOpts struct {
	DataDir      string
	Port         int
	ParentPID    int
	NoCredInject bool
	// LogOutput is where the daemon writes its log lines. nil means os.Stderr.
	// runTUI redirects this to a file so daemon logs don't smear over the
	// bubbletea altscreen.
	LogOutput io.Writer
}

// runDaemon starts the foxy-switcher daemon and blocks until ctx is canceled
// or the HTTP server returns. All cleanup runs before return (store close,
// port file remove, credential restore). ready is invoked once the listener
// is bound and the port file written; pass nil if you don't need the signal.
func runDaemon(ctx context.Context, opts daemonOpts, ready func(port int)) error {
	out := opts.LogOutput
	if out == nil {
		out = os.Stderr
	}
	logger := log.New(out, "", log.LstdFlags|log.Lmicroseconds)

	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", opts.DataDir, err)
	}

	dbPath := filepath.Join(opts.DataDir, "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()
	if err := os.Chmod(dbPath, 0o600); err != nil && !os.IsNotExist(err) {
		logger.Printf("warning: chmod %s: %v", dbPath, err)
	}

	// Activity bus shares state.db so events survive daemon restarts and the
	// Activity page is hot the moment the UI connects. Failures here are
	// fatal: a daemon without observability is exactly what PRD §5.3 was
	// added to prevent.
	bus, err := activity.NewBus(st.DB(), logger)
	if err != nil {
		return fmt.Errorf("activity bus: %w", err)
	}

	// Load persisted user prefs once at startup. UsagePoller's interval and
	// the credinject restore-on-quit flag both honour these. The frontend
	// can mutate them live via PUT /api/settings, but the poller's cadence
	// only re-reads on the next daemon launch (rebuilding the ticker mid-run
	// would race with in-flight ticks, which is more cost than benefit for
	// a one-time-per-session knob).
	settings, err := st.GetSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}

	pkce := authz.NewPKCEStore()
	rf := refresh.New(st, logger)
	rf.Bus = bus
	up := refresh.NewUsagePoller(st, logger)
	up.Bus = bus
	up.Interval = time.Duration(settings.UsagePollIntervalSec) * time.Second

	server := httpapi.New(st, pkce, rf, opts.DataDir)
	server.Bus = bus

	addr := fmt.Sprintf("127.0.0.1:%d", opts.Port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	tcp := ln.Addr().(*net.TCPAddr)
	server.Port = tcp.Port

	portFile := filepath.Join(opts.DataDir, "port")
	if err := writePortFile(portFile, tcp.Port); err != nil {
		ln.Close()
		return fmt.Errorf("write port file: %w", err)
	}
	defer os.Remove(portFile)

	// The credinject Coordinator owns Claude Code's keychain entries while
	// the daemon runs and restores the user's native login on graceful exit.
	// Defer Restore so Ctrl-C / SIGTERM / parent-pid death all converge on
	// the same cleanup path.
	//
	// --no-cred-inject skips the entire lifecycle: no inject, no reverse-sync,
	// no restore. Used to run the daemon alongside a real native login for
	// debugging without clobbering it.
	var cc *credinject.Coordinator
	if !opts.NoCredInject {
		backend, err := credinject.NewBackend()
		if err != nil {
			ln.Close()
			return fmt.Errorf("credinject backend: %w", err)
		}
		cc = credinject.New(vault.NewInProc(st), backend, opts.DataDir, logger)
		cc.SetBus(bus)
		cc.SetRestoreOnQuit(settings.RestoreNativeOnQuit)
		defer func() {
			if err := cc.RestoreOnShutdown(); err != nil {
				logger.Printf("warning: restore native credentials: %v", err)
			}
		}()
	}

	if opts.ParentPID > 0 {
		ctx2, cancel2 := context.WithCancel(ctx)
		defer cancel2()
		go watchParent(ctx2, cancel2, opts.ParentPID, 2*time.Second, logger)
		ctx = ctx2
	}

	if cc != nil {
		server.Cred = cc
		// Wire callbacks BEFORE Start so the spawned goroutines see them on
		// their first tick. Trigger() is non-blocking and idempotent, so it's
		// safe to fire even before cc.Run starts consuming.
		rf.OnChange = cc.Trigger
		up.OnChange = cc.Trigger
		// Skip the currently-injected account in the refresh scheduler:
		// Claude Code owns its rotation while it holds the keychain.
		rf.SkipAccountID = cc.CurrentAccountID
		go cc.Run(ctx)
	} else {
		logger.Print("--no-cred-inject: keychain lifecycle disabled")
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

	logger.Printf("foxy-switcher listening on http://%s (data: %s)", tcp.String(), opts.DataDir)
	bus.EmitInfo(activity.TypeDaemonStarted, 0,
		fmt.Sprintf("Daemon started on port %d", tcp.Port))
	defer bus.EmitInfo(activity.TypeDaemonStopped, 0, "Daemon stopped")
	if ready != nil {
		ready(tcp.Port)
	}
	serveErr := httpSrv.Serve(ln)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		logger.Printf("serve: %v", serveErr)
		return serveErr
	}
	logger.Print("shutdown complete")
	return nil
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

// runTUI handles the `tui` subcommand. The TUI is a foreground tool: if a
// daemon is already serving on dataDir (Tauri sidecar, manual `go run .`,
// etc.) it transparently attaches; otherwise it embeds a daemon for the
// session and shuts it down on exit. Either way the user never has to start
// the daemon by hand.
func runTUI(args []string) {
	fs := flag.NewFlagSet("tui", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "directory containing the daemon's port/state files (default ~/.foxy-switcher)")
	noCredInject := fs.Bool("no-cred-inject", false, "embedded daemon: don't manage Claude Code's credential storage")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, err)
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

	if pingDaemon(dir) {
		// Already serving — attach as a plain HTTP client.
		if err := tui.Run(dir, "attached"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	// No daemon found: embed one. Daemon logs go to a file so they don't
	// smear over the altscreen.
	logPath := filepath.Join(dir, "tui-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open daemon log:", err)
		os.Exit(1)
	}
	defer logFile.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ready := make(chan struct{})
	daemonErr := make(chan error, 1)
	go func() {
		daemonErr <- runDaemon(ctx, daemonOpts{
			DataDir:      dir,
			NoCredInject: *noCredInject,
			LogOutput:    logFile,
		}, func(int) { close(ready) })
	}()

	select {
	case <-ready:
	case err := <-daemonErr:
		fmt.Fprintln(os.Stderr, "embedded daemon failed:", err)
		fmt.Fprintln(os.Stderr, "see", logPath)
		os.Exit(1)
	case <-time.After(5 * time.Second):
		cancel()
		<-daemonErr
		fmt.Fprintln(os.Stderr, "embedded daemon did not become ready within 5s; see", logPath)
		os.Exit(1)
	}

	tuiErr := tui.Run(dir, "embedded")
	cancel()
	<-daemonErr // wait for daemon cleanup (cred restore, store close, port file remove)
	if tuiErr != nil {
		fmt.Fprintln(os.Stderr, tuiErr)
		os.Exit(1)
	}
}

// pingDaemon returns true if a daemon is already healthy at dir. Reads the
// port file, then GETs /healthz with a short timeout.
func pingDaemon(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "port"))
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || port <= 0 {
		return false
	}
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 500
}
