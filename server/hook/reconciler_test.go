package hook

import (
	"path/filepath"
	"testing"
)

func TestReconcileTransitions(t *testing.T) {
	home := sandbox(t)
	dataDir := filepath.Join(home, ".foxy-switcher")

	// Start: not installed, no available -> nothing to do.
	if changed, err := Reconcile(dataDir, false); err != nil || changed {
		t.Fatalf("noop case: changed=%v err=%v", changed, err)
	}
	if IsInstalled(dataDir) {
		t.Fatal("hook should remain uninstalled")
	}

	// Available -> install.
	changed, err := Reconcile(dataDir, true)
	if err != nil {
		t.Fatalf("install transition: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when transitioning to installed")
	}
	if !IsInstalled(dataDir) {
		t.Fatal("hook should be installed after Reconcile(true)")
	}

	// Idempotent: still available, already installed -> noop.
	if changed, err := Reconcile(dataDir, true); err != nil || changed {
		t.Fatalf("idempotent install: changed=%v err=%v", changed, err)
	}

	// Unavailable -> uninstall.
	changed, err = Reconcile(dataDir, false)
	if err != nil {
		t.Fatalf("uninstall transition: %v", err)
	}
	if !changed {
		t.Fatal("expected changed=true when transitioning to uninstalled")
	}
	if IsInstalled(dataDir) {
		t.Fatal("hook should be uninstalled after Reconcile(false)")
	}

	// Idempotent again: still unavailable, already uninstalled.
	if changed, err := Reconcile(dataDir, false); err != nil || changed {
		t.Fatalf("idempotent uninstall: changed=%v err=%v", changed, err)
	}
}
