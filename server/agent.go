package main

import (
	"bytes"
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
	"strconv"
	"strings"
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
	agentStore, bus, err := openAgentBus(dbPath, logger)
	if err != nil {
		return fmt.Errorf("agent activity bus: %w", err)
	}
	defer agentStore.Close()

	client := httpclient.New(cfg.VaultURL)
	client.SetToken(cfg.DeviceToken)

	suppressCredInject := opts.NoCredInject
	var cc *credinject.Coordinator
	if !suppressCredInject {
		backend, err := credinject.NewBackend()
		if err != nil {
			return fmt.Errorf("credinject backend: %w", err)
		}
		cc = credinject.New(client, backend, opts.DataDir, logger, cfg.DeviceID)
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

	// Lease-friendly routes proxy through to the vault's /agent/v1/api/*
	// surface (the bearer-only path the deployment whitelists past any
	// outer SSO). agentAPIWhitelist on the vault side enforces the same
	// allow-list — defense in depth, so a buggy / compromised agent
	// can't reach admin writes by hitting a different proxy path.
	proxy := newVaultAPIProxy(target, cfg.DeviceToken)
	// Vault has no credinject so its /api/dashboard returns
	// kpis.in_use_account_id = 0. Agent has the local Cred — patch the
	// response on its way back so the desktop's "in use" highlight
	// renders correctly.
	proxy.ModifyResponse = patchDashboardInUseAccount(cc)
	for _, path := range []string{
		"GET /api/accounts",
		"GET /api/dashboard",
		"GET /api/activity",
		"GET /api/activity/stream",
		"GET /api/devices",
		"POST /api/accounts/{id}/refresh",
		"POST /api/accounts/{id}/select",
	} {
		mux.Handle(path, proxy)
	}

	// Admin write routes are 405'd in agent mode — the vault is the
	// single source of truth for account CRUD, and a remote agent is
	// "use only". Frontend in agent mode hides these buttons (P4); the
	// 405 is the safety net for anyone bypassing the UI. Same JSON shape
	// as agentAPIWhitelist on the vault so error rendering is uniform.
	denyAdmin := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "agent mode is read-only; use the vault admin web UI at " + cfg.VaultURL,
		})
	})
	for _, path := range []string{
		"POST /api/accounts/login",
		"POST /api/accounts/callback",
		"DELETE /api/accounts/{id}",
		"POST /api/accounts/{id}/pause",
		"POST /api/accounts/{id}/resume",
		"POST /api/accounts/{id}/thresholds",
		"POST /api/reset",
		"POST /api/pair/init",
		"POST /api/pair/poll",
	} {
		mux.Handle(path, denyAdmin)
	}

	// settings / auto-switch are per-agent local prefs (theme, poll
	// interval, restore-on-quit, switch policy). The agent's own store
	// (backed by agent-activity.db) already exposes Get/Set helpers, so
	// we wire those directly here — no need to reach the vault.
	registerLocalPrefRoutes(mux, agentStore)
	// Anything else (typo, unmapped path) falls through to ServeMux's
	// default 404 — agent mode shouldn't be a generic vault proxy.

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
// restart even though the vault state lives elsewhere. Returns the store
// alongside the bus so the agent can also use it for per-agent prefs
// (settings + auto-switch) — same DB, separate kv rows.
func openAgentBus(path string, logger *log.Logger) (*store.Store, *activity.Bus, error) {
	st, err := store.Open(path)
	if err != nil {
		return nil, nil, err
	}
	bus, err := activity.NewBus(st.DB(), logger)
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, bus, nil
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

// newVaultAPIProxy wraps httputil.ReverseProxy with a Director that:
//  1. Rewrites incoming /api/* paths to /agent/v1/api/* so the request
//     hits the bearer-only agent surface on the vault (which deployments
//     whitelist past their outer SSO). agentAPIWhitelist on the vault
//     enforces lease/admin boundary.
//  2. Injects the agent's bearer token. Token lifecycle is tied to the
//     device row on the vault — revoking the device 401s every call.
//  3. Forces Host to the vault target so virtual-host vaults (caddy /
//     traefik) match the right route.
//
// Paths already prefixed with /agent/v1/ pass through verbatim — the
// agent's lease-flow callers (httpclient) hit that surface directly and
// shouldn't be double-prefixed.
func newVaultAPIProxy(target *url.URL, token string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	origDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/agent/v1/") {
			r.URL.Path = "/agent/v1" + r.URL.Path
			r.URL.RawPath = "" // let net/http re-encode from Path
		}
		origDirector(r)
		r.Host = target.Host
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "vault unreachable: " + err.Error(),
		})
	}
	return proxy
}

// patchDashboardInUseAccount returns a ReverseProxy.ModifyResponse hook
// that rewrites kpis.in_use_account_id on /api/dashboard responses to
// the agent-local Cred coordinator's view. The vault-side handler
// returns 0 because vault mode has no credinject; only the agent knows
// which account is currently injected on this machine. Other paths
// pass through verbatim.
func patchDashboardInUseAccount(cc *credinject.Coordinator) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		// Match against the original request path the agent's mux saw,
		// which still bears the /api/* prefix the desktop frontend uses.
		// Using the (post-rewrite) URL.Path would force us to track the
		// /agent/v1 prefix here too, which is a layering smell.
		if resp.Request == nil || !strings.HasSuffix(resp.Request.URL.Path, "/api/dashboard") {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			// Couldn't decode — pass through verbatim. ContentLength has
			// already been computed by the upstream; we just need to
			// reset the body so the proxy can copy it through.
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		}
		if kpis, ok := doc["kpis"].(map[string]any); ok {
			kpis["in_use_account_id"] = cc.Status().ManagedAccountID
		}
		out, err := json.Marshal(doc)
		if err != nil {
			// Fallback to original body on marshal failure (shouldn't
			// happen for any reachable doc shape, but defensive).
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
		}
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
		return nil
	}
}

// registerLocalPrefRoutes wires GET/SET handlers for per-agent prefs —
// settings (theme, poll interval, restore-on-quit, threshold default,
// sidebar mode) and auto-switch (enabled + policy). These are agent-
// local: each desktop / TUI may pick a different theme without forcing
// every other paired device on the same vault to follow. The store is
// the agent's own agent-activity.db (kv rows) so survival across
// restarts is free.
//
// Wire format mirrors httpapi.Server's /api/settings + /api/auto-switch
// exactly so the desktop frontend doesn't need to special-case agent
// mode — same JSON shape, same paths.
func registerLocalPrefRoutes(mux *http.ServeMux, st *store.Store) {
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		v, err := st.GetSettings(r.Context())
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError,
				map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, v)
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		// Decode into a partial-patch shape: missing fields preserve the
		// current value. Mirrors the desktop's optimistic-update flow,
		// which sends only the changed key.
		current, err := st.GetSettings(r.Context())
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError,
				map[string]string{"error": err.Error()})
			return
		}
		patch := current
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeJSONResponse(w, http.StatusBadRequest,
				map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		out, err := st.SetSettings(r.Context(), patch)
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError,
				map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /api/auto-switch", func(w http.ResponseWriter, r *http.Request) {
		v, err := st.GetAutoSwitch(r.Context())
		if err != nil {
			writeJSONResponse(w, http.StatusInternalServerError,
				map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, v)
	})
	mux.HandleFunc("POST /api/auto-switch", func(w http.ResponseWriter, r *http.Request) {
		var req store.AutoSwitch
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONResponse(w, http.StatusBadRequest,
				map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		if err := st.SetAutoSwitch(r.Context(), req); err != nil {
			writeJSONResponse(w, http.StatusInternalServerError,
				map[string]string{"error": err.Error()})
			return
		}
		writeJSONResponse(w, http.StatusOK, req)
	})
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
