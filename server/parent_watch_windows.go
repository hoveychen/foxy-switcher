//go:build windows

package main

import "os"

// parentAlive returns true if the given pid still refers to a live process.
// On Windows os.FindProcess opens a handle via OpenProcess, which fails for
// PIDs that no longer exist — so a fresh FindProcess each tick is enough.
func parentAlive(pid int) bool {
	_, err := os.FindProcess(pid)
	return err == nil
}
