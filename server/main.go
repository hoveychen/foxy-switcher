// foxy-switcher server: a single-user localhost daemon that hands Claude Code
// a fresh OAuth access_token from a pool of subscription accounts. Designed
// to run as a Tauri sidecar (the GUI manages its lifecycle), but also runs
// fine as a standalone binary for headless setups.
//
// CLI surface (cobra-managed; `foxy-switcher --help` lists everything):
//
//	foxy-switcher                    # default: TUI (embeds a daemon if none is running)
//	foxy-switcher tui                # explicit TUI subcommand (same as default)
//	foxy-switcher server [flags]     # daemon for the desktop app / headless API consumers
//	foxy-switcher pair --vault-url=… # device-flow pairing for agent mode
//	foxy-switcher unpair             # forget a paired vault, fall back to local mode
//
// Backwards compat: `foxy-switcher --server [flags]` (the form the Tauri
// sidecar passes) is rewritten to `foxy-switcher server [flags]` at startup.
package main

import (
	"context"
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
	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/tui"
	"github.com/hoveychen/foxy-switcher/server/vault"
	"github.com/hoveychen/foxy-switcher/server/vault/httpserver"
	"github.com/hoveychen/foxy-switcher/server/vault/webapp"
)

func main() {
	rewriteLegacyServerFlag()
	if err := newRootCmd().Execute(); err != nil {
		// cobra has already printed the error message via SilenceUsage=true
		// behaviour; just translate it to a non-zero exit.
		os.Exit(1)
	}
}

// runServer is the entry point for `foxy-switcher server` (and the legacy
// `--server` argv). It dispatches to runDaemon or runAgent based on flags
// and the agent-config.json detection rule.
func runServer(f serverFlags) error {
	dir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}

	// Empty --mode= means "infer from agent-config.json"; that's the
	// path the desktop / TUI onboarding relies on so a freshly-paired
	// user gets agent mode on the next daemon launch without anyone
	// having to thread a flag through the Tauri sidecar args. An
	// explicit --mode=combined still forces local mode even with a
	// stale agent-config.json present.
	var dmode daemonMode
	if f.mode == "" {
		dmode = detectModeFromConfig(dir)
	} else {
		dmode, err = parseMode(f.mode)
		if err != nil {
			return err
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	opts := daemonOpts{
		DataDir:      dir,
		Port:         f.port,
		ParentPID:    f.parentPID,
		NoCredInject: f.noCredInject,
		Mode:         dmode,
		BindHost:     f.bindHost,
	}
	if dmode == modeAgent {
		return runAgent(ctx, opts, nil)
	}
	return runDaemon(ctx, opts, nil)
}

// daemonMode is the deployment topology selected by --mode.
type daemonMode int

const (
	// modeCombined runs vault and agent in the same process — today's default.
	modeCombined daemonMode = iota
	// modeVault runs the vault side only: store, refresh, usage poll, frontend
	// httpapi, plus the agent-facing httpserver. credinject does not run.
	modeVault
	// modeAgent runs only credinject + a transparent reverse proxy to a
	// remote vault. No store, scheduler or usage poller. The local
	// listener forwards /api/* to the vault and serves /api/cred/status
	// locally — the existing Tauri / TUI frontend keeps working unchanged.
	modeAgent
)

func parseMode(s string) (daemonMode, error) {
	switch s {
	case "combined", "":
		return modeCombined, nil
	case "vault":
		return modeVault, nil
	case "agent":
		return modeAgent, nil
	default:
		return 0, fmt.Errorf("--mode=%q: expected combined|vault|agent", s)
	}
}

// detectModeFromConfig picks an implicit deployment mode based on
// whether the user has paired with a remote vault: agent-config.json
// present means the daemon should reverse-proxy to that vault
// (modeAgent); absent means today's local-first default (modeCombined).
// runServer / runTUI consult this only when the operator has not passed
// an explicit --mode= flag, so power users keep an escape hatch.
func detectModeFromConfig(dataDir string) daemonMode {
	if _, err := os.Stat(filepath.Join(dataDir, AgentConfigName)); err == nil {
		return modeAgent
	}
	return modeCombined
}

// modeName is the inverse of parseMode — used to surface the active mode
// over /api/about so the frontend can render the Settings → Vault card
// without re-deriving it.
func modeName(m daemonMode) string {
	switch m {
	case modeCombined:
		return "combined"
	case modeVault:
		return "vault"
	case modeAgent:
		return "agent"
	default:
		return ""
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
	// Mode selects the deployment topology. Zero value = combined, which
	// matches today's behaviour and what runTUI's embedded daemon expects.
	Mode daemonMode
	// BindHost is the listen address. Defaults to 127.0.0.1 for combined;
	// vault mode behind a reverse proxy typically wants 0.0.0.0.
	BindHost string
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
	// quit is the cancel hook /api/quit fires. We wrap the inbound ctx so the
	// daemon's existing shutdown-on-ctx.Done plumbing (httpSrv.Shutdown,
	// store.Close, port file removal) works the same whether the trigger is
	// SIGTERM, parent-pid death, or a Tauri-initiated graceful quit.
	ctx, quit := context.WithCancel(ctx)
	defer quit()

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
	// vault.InProc shares the same SQLite database so leases survive a
	// daemon restart (Step 4). The refresh scheduler queries the lease
	// table directly for skip-decisions — going through the Service would
	// make the per-tick check needlessly indirect.
	vaultSvc := vault.NewInProc(st)
	rf := refresh.New(st, logger)
	rf.Bus = bus
	rf.IsAccountInUse = st.IsAccountLeased
	up := refresh.NewUsagePoller(st, logger)
	up.Bus = bus
	up.Interval = time.Duration(settings.UsagePollIntervalSec) * time.Second

	server := httpapi.New(st, pkce, rf, opts.DataDir)
	server.Bus = bus
	server.Mode = modeName(opts.Mode)
	if opts.Mode == modeVault {
		// Vault mode means the frontend httpapi is reachable beyond
		// loopback — gate it behind the same Bearer token agents use,
		// so a misconfigured vault on the public internet can't be
		// hit unauthenticated. Combined mode keeps loopback open.
		server.Middleware = append(server.Middleware, httpserver.BearerAuth(st))
	}

	bindHost := opts.BindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	addr := fmt.Sprintf("%s:%d", bindHost, opts.Port)
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
	// Vault mode is the credentialed cloud-side process — credinject only
	// makes sense on a machine that has its own Claude Code keychain to
	// inject into, so suppress it. The local agent (Step 5) drives credinject
	// and talks to vault via HTTP.
	suppressCredInject := opts.NoCredInject || opts.Mode == modeVault
	var cc *credinject.Coordinator
	var codexManager *openai.Manager
	if !suppressCredInject {
		backend, err := credinject.NewBackend()
		if err != nil {
			ln.Close()
			return fmt.Errorf("credinject backend: %w", err)
		}
		cc = credinject.New(vaultSvc, backend, opts.DataDir, logger, "")
		cc.SetBus(bus)
		cc.SetRestoreOnQuit(settings.RestoreNativeOnQuit)
		if p, err := credinject.DefaultClaudeConfigPath(); err != nil {
			logger.Printf("warning: resolve claude.json path: %v (profile sync disabled)", err)
		} else {
			cc.SetClaudeConfigPath(p)
		}
		if p, err := credinject.DefaultClaudeProjectsDir(); err != nil {
			logger.Printf("warning: resolve claude projects dir: %v (idle-reclaim disabled)", err)
		} else {
			cc.SetActivityDir(p)
		}
		defer func() {
			if err := cc.RestoreOnShutdown(); err != nil {
				logger.Printf("warning: restore native credentials: %v", err)
			}
		}()
		if codexStorage, storageErr := openai.DefaultCredentialStorage(); storageErr != nil {
			logger.Printf("warning: resolve Codex credential storage: %v (Codex injection disabled)", storageErr)
		} else {
			codexManager = openai.NewManagerWithStorage(st, codexStorage, logger)
			codexManager.SetDeviceID(cc.DeviceID())
			server.Codex = codexManager
			server.CodexStorage = codexStorage
		}
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
		notifyCredentialManagers := func() {
			cc.Trigger()
			if codexManager != nil {
				go func() {
					if err := codexManager.Reconcile(ctx); err != nil {
						logger.Printf("[codex] triggered reconcile: %v", err)
					}
				}()
			}
		}
		rf.OnChange = notifyCredentialManagers
		up.OnChange = notifyCredentialManagers
		// rf.IsAccountInUse already points at the lease store; the
		// coordinator now feeds the same store via vault.AcquireLease.
		go cc.Run(ctx)
		if codexManager != nil {
			codexManager.Start(ctx)
			defer codexManager.Stop()
		}
	} else {
		logger.Print("--no-cred-inject: keychain lifecycle disabled")
	}

	rf.Start(ctx)
	defer rf.Stop()
	up.Start(ctx)
	defer up.Stop()

	// Lease sweeper: GC expired rows so leases_account_id_uniq stays
	// unblocked for the next AcquireLease attempt. 30s matches the
	// agent's lease TTL / 2 budget — well under the agent's renew cycle.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := vaultSvc.SweepLeases(ctx); err != nil {
					logger.Printf("[vault] sweep leases: %v", err)
				}
			}
		}
	}()

	// Compose handlers on one listener:
	//   - /agent/v1/*           — agent surface (Bearer-auth'd vault.Service)
	//   - /setup, /login, /pair,
	//     /devices, /password   — admin Web UI (cookie sessions)
	//   - /admin/api/*          — admin SPA JSON surface (cookie sessions)
	//   - /app, /app/, /assets/*— embedded React account panel (cookie or Bearer)
	//   - /api/*, /healthz      — existing frontend httpapi (catch-all)
	// More specific patterns win over the catch-all "/" mount.
	rootMux := http.NewServeMux()
	vaultHTTP := httpserver.New(vaultSvc, st)
	// OpenRouter key derivation is vault-internal — it holds the management key
	// path and decides what a given device id may have — so it hangs off the
	// vault HTTP server rather than vault.Service, which remote agents drive.
	// Constructed unconditionally: with no OpenRouter account configured it
	// simply reports none available.
	openRouterKeys := vault.NewOpenRouterKeys(st, logger)
	vaultHTTP.OpenRouter = openRouterKeys
	rootMux.Handle("/agent/v1/", vaultHTTP.Handler())
	// Re-expose the frontend httpapi under /agent/v1/api/ so a remote
	// agent can drive the same view + lease routes via the bearer-auth'd
	// agent surface — useful for vault deployments that hide /api/*
	// behind an outer SSO. agentAPIWhitelist enforces the lease/admin
	// boundary on top of the BearerAuth middleware that server.Handler()
	// already applies in vault mode, so a compromised agent can't reach
	// admin writes through this mount even with a valid device token.
	// More-specific prefix wins over /agent/v1/, so this catches the
	// /agent/v1/api/* subset before vaultHTTP.Handler() sees it.
	rootMux.Handle("/agent/v1/api/", http.StripPrefix("/agent/v1", agentAPIWhitelist(server.Handler())))
	// Wire the SPA bundle first so RegisterWebRoutes (gateAppRoute on
	// /, /devices, /pair, /password) can hand off to it.
	if webapp.Available() {
		appHandler := webapp.Handler()
		rootMux.Handle("/app", appHandler)
		rootMux.Handle("/app/", appHandler)
		rootMux.Handle("/admin", appHandler)
		rootMux.Handle("/admin/", appHandler)
		rootMux.Handle("/assets/", appHandler)
		vaultHTTP.HasWebApp = true
		vaultHTTP.AppHandler = appHandler
		// Other top-level static files vite emits (favicon, etc.) get
		// captured by the catch-all "/" below — they 404 today and that's
		// fine; they're decorative.
	}
	vaultHTTP.RegisterWebRoutes(rootMux)
	vaultHTTP.RegisterAPIRoutes(rootMux)
	// /healthz must remain reachable without credentials so a load
	// balancer / uptime probe can monitor a public vault. Register it
	// on the root mux above the catch-all so the vault-mode Bearer
	// wrap on httpapi.Server doesn't gate it.
	rootMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// /api/quit is intentionally registered on rootMux (above the
	// catch-all server.Handler() and outside vault-mode BearerAuth) so
	// the desktop sidecar can ask a daemon it doesn't own to gracefully
	// stop without holding a device token. Loopback-gated; see
	// loopbackOnly + quitHandler in quit.go.
	rootMux.HandleFunc("POST /api/quit", loopbackOnly(quitHandler(logger, quit)))
	rootMux.Handle("/", server.Handler())
	httpSrv := &http.Server{
		Handler:           rootMux,
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
func runTUI(f tuiFlags) error {
	dir, err := resolveDataDir(f.dataDir)
	if err != nil {
		return fmt.Errorf("resolve data dir: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	if pingDaemon(dir) {
		// Already serving — attach as a plain HTTP client.
		return tui.Run(dir, "attached")
	}

	// No daemon found: embed one. Daemon logs go to a file so they don't
	// smear over the altscreen.
	logPath := filepath.Join(dir, "tui-daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Same implicit-mode rule as runServer: a paired user's TUI session
	// embeds an agent-mode daemon so the TUI talks to their remote
	// vault, not a fresh local store. Without this, opening the TUI on
	// a paired install would silently reset the user's view to the
	// combined-mode local sqlite.
	embeddedMode := detectModeFromConfig(dir)
	embeddedRun := runDaemon
	if embeddedMode == modeAgent {
		embeddedRun = runAgent
	}

	ready := make(chan struct{})
	daemonErr := make(chan error, 1)
	go func() {
		daemonErr <- embeddedRun(ctx, daemonOpts{
			DataDir:      dir,
			NoCredInject: f.noCredInject,
			Mode:         embeddedMode,
			LogOutput:    logFile,
		}, func(int) { close(ready) })
	}()

	select {
	case <-ready:
	case err := <-daemonErr:
		return fmt.Errorf("embedded daemon failed: %w (see %s)", err, logPath)
	case <-time.After(5 * time.Second):
		cancel()
		<-daemonErr
		return fmt.Errorf("embedded daemon did not become ready within 5s; see %s", logPath)
	}

	tuiErr := tui.Run(dir, "embedded")
	cancel()
	<-daemonErr // wait for daemon cleanup (cred restore, store close, port file remove)
	return tuiErr
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
