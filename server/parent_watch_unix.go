//go:build unix

package main

import (
	"os"
	"syscall"
)

// parentAlive returns true if the given pid still refers to a live process.
// On Unix os.FindProcess always succeeds for any pid, so we have to actually
// probe with Signal(0): it returns ESRCH for a dead pid and EPERM for a live
// pid we don't own (still alive, just not signalable — which is fine here).
func parentAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}
