package main

import (
	"context"
	"io"
	"log"
	"os/exec"
	"testing"
	"time"
)

// TestWatchParent_CancelsWhenParentDies covers the original bug: GUI dies via
// signal, sidecar should follow. Spawn a real subprocess to act as the
// "parent", point watchParent at it, kill it, and assert ctx gets canceled.
func TestWatchParent_CancelsWhenParentDies(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan struct{})
	go func() {
		watchParent(ctx, cancel, pid, 50*time.Millisecond, log.New(io.Discard, "", 0))
		close(done)
	}()

	// Watchdog must not fire while parent is alive.
	select {
	case <-ctx.Done():
		t.Fatal("ctx canceled while parent still alive")
	case <-time.After(300 * time.Millisecond):
	}

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Logf("reap parent: %v (continuing)", err)
	}

	select {
	case <-ctx.Done():
		// success
	case <-time.After(3 * time.Second):
		t.Fatal("ctx not canceled after parent died")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchParent did not return after canceling")
	}
}

// TestWatchParent_StopsOnContextDone verifies the watchdog exits cleanly when
// its own ctx is canceled (e.g. server shutting down for some other reason).
func TestWatchParent_StopsOnContextDone(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		watchParent(ctx, cancel, cmd.Process.Pid, 50*time.Millisecond, log.New(io.Discard, "", 0))
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchParent did not return after ctx canceled")
	}
}
