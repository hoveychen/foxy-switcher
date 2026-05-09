package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLoopbackOnly_RejectsNonLoopback(t *testing.T) {
	called := false
	wrapped := loopbackOnly(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/api/quit", nil)
	req.RemoteAddr = "203.0.113.4:5555"
	rec := httptest.NewRecorder()
	wrapped(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback caller, got %d", rec.Code)
	}
	if called {
		t.Fatal("inner handler must not run for non-loopback caller")
	}
}

func TestLoopbackOnly_AcceptsLoopback(t *testing.T) {
	cases := []string{
		"127.0.0.1:1234",
		"[::1]:1234",
	}
	for _, addr := range cases {
		t.Run(addr, func(t *testing.T) {
			called := false
			wrapped := loopbackOnly(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusAccepted)
			})
			req := httptest.NewRequest(http.MethodPost, "/api/quit", nil)
			req.RemoteAddr = addr
			rec := httptest.NewRecorder()
			wrapped(rec, req)
			if !called {
				t.Fatalf("loopback caller %s should reach inner handler", addr)
			}
			if rec.Code != http.StatusAccepted {
				t.Fatalf("expected inner status 202, got %d", rec.Code)
			}
		})
	}
}

func TestQuitHandler_TriggersCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := log.New(io.Discard, "", 0)
	h := quitHandler(logger, cancel)

	req := httptest.NewRequest(http.MethodPost, "/api/quit", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("expected JSON content-type, got %q", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "quitting") {
		t.Fatalf("expected body to mention 'quitting', got %q", body)
	}

	// quitHandler defers the cancel by 50ms so the response can flush
	// before httpSrv.Shutdown reaps the connection. Wait up to 1s for
	// ctx to be canceled.
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("cancel was not invoked within 1s of /api/quit")
	}
}
