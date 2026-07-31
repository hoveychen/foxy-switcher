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
	openai "github.com/hoveychen/foxy-switcher/server/openai"
	"github.com/hoveychen/foxy-switcher/server/store"
	"github.com/hoveychen/foxy-switcher/server/vault/httpclient"
)

// runAgent is the --mode=agent entrypoint. It runs the credential injector
// against a remote vault — no local store, no scheduler, no usage poller.
// Frontend traffic on the local listener is proxied to the vault verbatim
// (modulo /api/cred/status which the agent owns) so the existing Tauri /
// TUI clients see the same surface they always have.
func runAgent(ctx context.Context, opts daemonOpts, ready func(port int)) error {
	// Wrap ctx so /api/quit can cancel the same plumbing SIGTERM uses.
	// Same rationale as runDaemon — see comment there.
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
	var codexRemote *openai.RemoteManager
	if !suppressCredInject {
		backend, err := credinject.NewBackend()
		if err != nil {
			return fmt.Errorf("credinject backend: %w", err)
		}
		cc = credinject.New(client, backend, opts.DataDir, logger, cfg.DeviceID)
		cc.SetBus(bus)
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
		// Read auto-switch from the agent-local store rather than the
		// remote vault. The desktop's settings page writes the toggle to
		// agent-activity.db via /api/auto-switch (see registerLocalPrefRoutes
		// below); without this wiring credinject's choose() would consult
		// the vault's global auto-switch and silently ignore the user's
		// per-agent choice.
		cc.SetAutoSwitchSource(agentStore.GetAutoSwitch)
		settings, settingsErr := agentStore.GetSettings(ctx)
		if settingsErr != nil {
			return fmt.Errorf("load agent settings: %w", settingsErr)
		}
		cc.SetRestoreOnQuit(settings.RestoreNativeOnQuit)
		codexStorage, storageErr := openai.DefaultCredentialStorage()
		if storageErr != nil {
			logger.Printf("warning: resolve Codex credential storage: %v (remote Codex injection disabled)", storageErr)
		} else {
			codexRemote = openai.NewRemoteManager(client, codexStorage, cfg.DeviceID, logger)
			codexRemote.SetRestoreOnQuit(settings.RestoreNativeOnQuit)
			codexRemote.SetAutoSwitchSource(agentStore.GetAutoSwitch)
			codexRemote.Start(ctx)
			defer codexRemote.Stop()
		}
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
		statusRaw, _ := json.Marshal(cc.Status())
		var status map[string]any
		_ = json.Unmarshal(statusRaw, &status)
		if status == nil {
			status = map[string]any{}
		}
		if codexRemote != nil {
			status["codex_managed_account_id"] = codexRemote.ManagedAccountID()
		}
		writeJSONResponse(w, http.StatusOK, status)
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
	// /api/quit lets the desktop sidecar gracefully stop a daemon it
	// doesn't own (e.g. the autostart sibling that's holding the port
	// file from a stale launch). Loopback-gated; see quit.go.
	mux.HandleFunc("POST /api/quit", loopbackOnly(quitHandler(logger, quit)))

	// Lease-friendly routes proxy through to the vault's /agent/v1/api/*
	// surface (the bearer-only path the deployment whitelists past any
	// outer SSO). agentAPIWhitelist on the vault side enforces the same
	// allow-list — defense in depth, so a buggy / compromised agent
	// can't reach admin writes by hitting a different proxy path.
	proxy := newVaultAPIProxy(target, cfg.DeviceToken)
	// Vault is authoritative for the kpis.in_use[] list (one entry per
	// active lease, joined with each holding device's display name).
	// Agent has the local Cred — patch ONLY its own entry on the way
	// back so the desktop's "in use" highlight tracks local injection
	// even when vault lags a tick behind. Other devices' entries pass
	// through verbatim.
	proxy.ModifyResponse = patchAgentInUseSelf(
		cc.DeviceID(),
		func() []int64 {
			ids := []int64{cc.Status().ManagedAccountID}
			if codexRemote != nil {
				ids = append(ids, codexRemote.ManagedAccountID())
			}
			return ids
		},
	)
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
		"POST /api/accounts/import-codex",
		"POST /api/accounts/codex-login",
		"POST /api/accounts/codex-login/callback",
		"POST /api/accounts/openrouter",
		"POST /api/accounts/{id}/openrouter",
		"POST /api/accounts/{id}/openrouter/check",
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
	registerLocalPrefRoutes(mux, agentStore, func(settings store.Settings) {
		cc.SetRestoreOnQuit(settings.RestoreNativeOnQuit)
		if codexRemote != nil {
			codexRemote.SetRestoreOnQuit(settings.RestoreNativeOnQuit)
		}
	})
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

// patchDashboardInUseSelf returns a ReverseProxy.ModifyResponse hook
// that reconciles the agent's own entry in /api/dashboard's
// `kpis.in_use[]` list against the local credinject Coordinator's view.
// Vault is authoritative for OTHER devices' entries; this hook only
// touches the entry where mine==true (or appends one if missing) so the
// desktop's "in use" highlight stays accurate even when the vault
// hasn't yet observed a freshly-acquired lease.
//
// Behaviour:
//   - non-dashboard paths: pass through verbatim.
//   - accountIDFn()==0 (nothing injected locally): pass through verbatim.
//     Vault's view is the only authority when we hold no lease.
//   - mine entry exists: overwrite its account_id with the local truth.
//   - mine entry absent: append { account_id, device_id, mine: true }.
//   - other entries (mine==false) are never touched.
//
// deviceID is the agent's own ID (Coordinator.DeviceID()); accountIDFn
// is read on every response so a mid-flight rotation lands immediately.
func patchDashboardInUseSelf(deviceID string, accountIDFn func() int64) func(*http.Response) error {
	return patchAgentInUseSelf(deviceID, func() []int64 { return []int64{accountIDFn()} })
}

func patchAgentInUseSelf(deviceID string, accountIDsFn func() []int64) func(*http.Response) error {
	return func(resp *http.Response) error {
		if resp.StatusCode != http.StatusOK {
			return nil
		}
		if resp.Request == nil {
			return nil
		}
		path := resp.Request.URL.Path
		isDashboard := strings.HasSuffix(path, "/api/dashboard")
		isAccounts := strings.HasSuffix(path, "/api/accounts")
		if !isDashboard && !isAccounts {
			return nil
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		rewriteBody := func(b []byte) {
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
		}
		ids := make([]int64, 0, 2)
		idSet := make(map[int64]bool, 2)
		for _, id := range accountIDsFn() {
			if id > 0 && !idSet[id] {
				ids = append(ids, id)
				idSet[id] = true
			}
		}
		if len(ids) == 0 {
			rewriteBody(body)
			return nil
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			// Defensive: malformed body slips through unchanged.
			rewriteBody(body)
			return nil
		}
		if isAccounts {
			rawAccounts, _ := doc["accounts"].([]any)
			for _, item := range rawAccounts {
				account, ok := item.(map[string]any)
				if !ok {
					continue
				}
				id, _ := account["id"].(float64)
				account["in_use"] = idSet[int64(id)]
			}
		} else {
			kpis, ok := doc["kpis"].(map[string]any)
			if !ok {
				rewriteBody(body)
				return nil
			}
			rawList, _ := kpis["in_use"].([]any)
			filtered := make([]any, 0, len(rawList)+len(ids))
			for _, item := range rawList {
				entry, ok := item.(map[string]any)
				if ok {
					if mine, _ := entry["mine"].(bool); mine {
						continue
					}
				}
				filtered = append(filtered, item)
			}
			for _, id := range ids {
				filtered = append(filtered, map[string]any{
					"account_id": id, "device_id": deviceID,
					"device_name": "", "mine": true, "expires_at": 0,
				})
			}
			kpis["in_use"] = filtered
		}
		out, err := json.Marshal(doc)
		if err != nil {
			rewriteBody(body)
			return nil
		}
		rewriteBody(out)
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
func registerLocalPrefRoutes(mux *http.ServeMux, st *store.Store, onSettings ...func(store.Settings)) {
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
		for _, notify := range onSettings {
			if notify != nil {
				notify(out)
			}
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
