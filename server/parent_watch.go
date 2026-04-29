package main

import (
	"context"
	"log"
	"time"
)

// watchParent polls the given pid at the given interval; if the process
// disappears it calls cancel so the rest of the daemon can shut down. It
// returns when ctx is done. Used by sidecar mode (Tauri shell passes its own
// PID via --parent-pid) so the sidecar can't outlive the GUI even if the GUI
// dies via SIGKILL / unclean teardown.
func watchParent(ctx context.Context, cancel context.CancelFunc, pid int, interval time.Duration, logger *log.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !parentAlive(pid) {
				logger.Printf("parent pid %d gone; shutting down", pid)
				cancel()
				return
			}
		}
	}
}
