package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/hoveychen/foxy-switcher/server/activity"
	"github.com/hoveychen/foxy-switcher/server/credinject"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault/httpclient"
)

// runAgent is the --mode=agent entrypoint. It runs the credential injector
// against a remote vault — no local store, no scheduler, no usage poller.
// Frontend traffic on the local listener is proxied to the vault verbatim
// (modulo /api/cred/status which the agent owns) so the existing Tauri /
// TUI clients see the same surface they always have.
func runAgent(ctx context.Context, opts daemonOpts, ready func(port int)) error {
	out := opts.LogOutput
	if out == nil {
		out = os.Stderr
	}
	logger := log.New(out, "", log.LstdFlags|log.Lmicroseconds)

	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", opts.DataDir, err)
	}

	cfg, err := readAgentConfig(opts.DataDir)
	if err != nil {
		return fmt.Errorf("read agent config: %w (run `foxy-switcher pair` first)", err)
	}
	if cfg.VaultURL == "" || cfg.DeviceToken == "" {
		return errors.New("agent config missing vault_url or device_token; re-run `foxy-switcher pair`")
	}
	target, err := url.Parse(cfg.VaultURL)
	if err != nil {
		return fmt.Errorf("parse vault_url: %w", err)
	}

	// Activity bus is local — pre-Step-5 the bus lived on the vault, but
	// agent mode wants its own ring so the local frontend can show
	// credinject.* events even when offline. Failures here are fatal:
	// without the bus the activity routes 500 instead of behaving like
	// an empty timeline, which is more confusing than crashing loudly.
	dbPath := filepath.Join(opts.DataDir, "agent-activity.db")
	bus, err := openAgentBus(dbPath, logger)
	if err != nil {
		return fmt.Errorf("agent activity bus: %w", err)
	}

	client := httpclient.New(cfg.VaultURL)
	client.SetToken(cfg.DeviceToken)

	suppressCredInject := opts.NoCredInject
	var cc *credinject.Coordinator
	if !suppressCredInject {
		backend, err := credinject.NewBackend()
		if err != nil {
			return fmt.Errorf("credinject backend: %w", err)
		}
		cc = credinject.New(client, backend, opts.DataDir, logger)
		cc.SetBus(bus)
		defer func() {
			if err := cc.RestoreOnShutdown(); err != nil {
				logger.Printf("warning: restore native credentials: %v", err)
			}
		}()
		go cc.Run(ctx)
	} else {
		logger.Print("--no-cred-inject: keychain lifecycle disabled (agent mode)")
	}

	// HTTP listener — same shape as runDaemon so the Tauri sidecar's port
	// file lookup keeps working.
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
	portFile := filepath.Join(opts.DataDir, "port")
	if err := writePortFile(portFile, tcp.Port); err != nil {
		ln.Close()
		return fmt.Errorf("write port file: %w", err)
	}
	defer os.Remove(portFile)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/cred/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(w, http.StatusOK, cc.Status())
	})
	// /api/about gets answered locally so the Settings → Vault card sees
	// mode=agent + this device's upstream URL. Forwarding to the vault
	// would surface mode=vault instead, which is true on the upstream
	// but useless to the frontend trying to decide what UI to render.
	startedAt := time.Now()
	mux.HandleFunc("GET /api/about", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResponse(w, http.StatusOK, agentAbout{
			Mode:        "agent",
			VaultURL:    cfg.VaultURL,
			DeviceID:    cfg.DeviceID,
			Port:        tcp.Port,
			DataDir:     opts.DataDir,
			StartedAtMS: startedAt.UnixMilli(),
		})
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// Everything else proxies to the vault. ReverseProxy preserves request
	// bodies, query strings and SSE streaming out of the box.
	proxy := newVaultProxy(target, cfg.DeviceToken)
	mux.Handle("/", proxy)

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	logger.Printf("foxy-switcher agent listening on http://%s, vault=%s", tcp.String(), cfg.VaultURL)
	bus.EmitInfo(activity.TypeDaemonStarted, 0,
		fmt.Sprintf("Agent started on port %d", tcp.Port))
	defer bus.EmitInfo(activity.TypeDaemonStopped, 0, "Agent stopped")
	if ready != nil {
		ready(tcp.Port)
	}
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Printf("serve: %v", err)
		return err
	}
	logger.Print("agent shutdown complete")
	return nil
}

// openAgentBus is a thin convenience around activity.NewBus. The agent's
// bus persists into its own SQLite file so events survive an agent
// restart even though the vault state lives elsewhere.
func openAgentBus(path string, logger *log.Logger) (*activity.Bus, error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	return activity.NewBus(st.DB(), logger)
}

// readAgentConfig loads ~/.foxy-switcher/agent-config.json (or the override
// dataDir's copy). Missing file → distinct error so the daemon's startup
// can tell the user to run `foxy-switcher pair`.
func readAgentConfig(dataDir string) (*AgentConfig, error) {
	path := filepath.Join(dataDir, AgentConfigName)
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg AgentConfig
	if err := json.Unmarshal(buf, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// newVaultProxy wraps httputil.ReverseProxy with a Director that injects
// the agent's bearer token on every forwarded request. The token's
// lifecycle is tied to the device row on the vault — revoking the device
// makes every proxied call 401.
func newVaultProxy(target *url.URL, token string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		origDirector(r)
		// Override Host so virtual-host vaults (caddy / traefik routing
		// on Host header) match the right route.
		r.Host = target.Host
		// Inject Bearer. Header is set even on calls that vault doesn't
		// require auth for — vault ignores extra Authorization on those
		// routes, so this stays simple.
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}
	// Surface upstream connectivity errors as 502 with a JSON body so the
	// frontend can render "vault unreachable" instead of getting a blank
	// stream.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "vault unreachable: " + err.Error(),
		})
	}
	return proxy
}

// agentAbout is the trimmed shape /api/about returns in agent mode.
// Field names match httpapi.AboutResponse so the React Settings page
// can decode either one with the same type. Fields the agent doesn't
// know (sqlite size, build info, etc.) come back as zero values; the
// frontend tolerates that — there's no useful answer for "the agent's
// SQLite path" because the agent doesn't have one.
type agentAbout struct {
	Mode        string `json:"mode"`
	VaultURL    string `json:"vault_url"`
	DeviceID    string `json:"device_id"`
	Port        int    `json:"port"`
	DataDir     string `json:"data_dir"`
	StartedAtMS int64  `json:"started_at_ms"`
}

// writeJSONResponse mirrors httpapi.writeJSON so the agent can emit
// /api/cred/status without importing the httpapi package (which would
// drag the whole vault stack into agent builds for one helper).
func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Compile-time guard. (Removes `imported and not used` if io is only
// pulled in transitively in some code paths.)
var _ = io.Discard
