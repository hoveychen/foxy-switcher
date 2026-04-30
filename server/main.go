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
	"github.com/hoveychen/foxy-switcher/server/httpapi"
	"github.com/hoveychen/foxy-switcher/server/refresh"
	"github.com/hoveychen/foxy-switcher/server/store"
)

func main() {
	var (
		dataDir   = flag.String("data-dir", "", "directory for state.db / port file (default ~/.foxy-switcher)")
		port      = flag.Int("port", 0, "TCP port to bind on 127.0.0.1; 0 = random")
		parentPID = flag.Int("parent-pid", 0, "if non-zero, exit when this pid disappears (sidecar-mode safety net)")
	)
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds)

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

	server := httpapi.New(st, pkce, rf)

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

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *parentPID > 0 {
		go watchParent(ctx, cancel, *parentPID, 2*time.Second, logger)
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
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		logger.Fatalf("serve: %v", err)
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
