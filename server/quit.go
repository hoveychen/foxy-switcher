package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"
)

// loopbackOnly wraps h so only requests with a loopback RemoteAddr pass.
// /api/quit has no authentication — it relies on the listener already
// binding to 127.0.0.1. This wrapper is defense-in-depth for vault mode
// (BindHost can be 0.0.0.0) and for any future config that exposes the
// daemon beyond loopback.
func loopbackOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// quitHandler returns an HTTP handler that triggers cancel after the
// response flushes. The desktop sidecar uses this in attached mode so a
// daemon it doesn't own (TUI embed, autostart-sibling GUI) can still be
// asked to gracefully stop, letting Tauri respawn one that reads the
// freshly-written agent-config.json on its next launch.
//
// Cancel runs after a 50ms delay so the HTTP response can finish before
// httpSrv.Shutdown reaps the connection. The caller is expected to gate
// this with loopbackOnly — there is no authentication.
func quitHandler(logger *log.Logger, cancel context.CancelFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintln(w, `{"status":"quitting"}`)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		if logger != nil {
			logger.Printf("/api/quit invoked by %s; shutting down", r.RemoteAddr)
		}
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
	}
}
