package credinject

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return m
}

func TestBuildOAuthAccountOmitsEmptyFields(t *testing.T) {
	// Legacy row: only uuid + email populated, profile columns still "".
	a := &store.Account{AccountUUID: "uuid-1", Email: "a@example.com"}
	acct := buildOAuthAccount(a)

	if acct["accountUuid"] != "uuid-1" || acct["emailAddress"] != "a@example.com" {
		t.Fatalf("expected uuid+email set, got %#v", acct)
	}
	for _, k := range []string{"displayName", "organizationName", "organizationUuid", "organizationRateLimitTier"} {
		if _, ok := acct[k]; ok {
			t.Errorf("empty field %q should be omitted, got %#v", k, acct[k])
		}
	}
}

func TestApplyAccountProfileCreatesFileWhenMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	a := &store.Account{
		AccountUUID:      "uuid-9",
		Email:            "switched@example.com",
		FullName:         "Switched User",
		OrganizationName: "Acme",
		OrganizationUUID: "org-9",
		RateLimitTier:    "default_claude_max_20x",
	}
	if err := applyAccountProfile(path, a); err != nil {
		t.Fatalf("applyAccountProfile: %v", err)
	}

	cfg := readJSON(t, path)
	if cfg[onboardingKey] != true {
		t.Errorf("hasCompletedOnboarding = %#v, want true", cfg[onboardingKey])
	}
	acct, ok := cfg[oauthAccountKey].(map[string]any)
	if !ok {
		t.Fatalf("oauthAccount missing or wrong type: %#v", cfg[oauthAccountKey])
	}
	if acct["emailAddress"] != "switched@example.com" {
		t.Errorf("emailAddress = %#v", acct["emailAddress"])
	}
	if acct["organizationRateLimitTier"] != "default_claude_max_20x" {
		t.Errorf("organizationRateLimitTier = %#v", acct["organizationRateLimitTier"])
	}
}

func TestApplyAccountProfilePreservesOtherFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	// Seed a config that already carries unrelated state + a stale oauthAccount.
	seed := map[string]any{
		"primaryApiKey":          "sk-ant-keep-me",
		"numStartups":            float64(42),
		"oauthAccount":           map[string]any{"emailAddress": "old@example.com"},
		"hasCompletedOnboarding": false,
	}
	if err := writeConfig(path, seed); err != nil {
		t.Fatalf("seed writeConfig: %v", err)
	}

	a := &store.Account{AccountUUID: "uuid-new", Email: "new@example.com"}
	if err := applyAccountProfile(path, a); err != nil {
		t.Fatalf("applyAccountProfile: %v", err)
	}

	cfg := readJSON(t, path)
	if cfg["primaryApiKey"] != "sk-ant-keep-me" {
		t.Errorf("primaryApiKey not preserved: %#v", cfg["primaryApiKey"])
	}
	if cfg["numStartups"] != float64(42) {
		t.Errorf("numStartups not preserved: %#v", cfg["numStartups"])
	}
	if cfg[onboardingKey] != true {
		t.Errorf("hasCompletedOnboarding should be forced true, got %#v", cfg[onboardingKey])
	}
	acct := cfg[oauthAccountKey].(map[string]any)
	if acct["emailAddress"] != "new@example.com" {
		t.Errorf("oauthAccount not replaced: %#v", acct)
	}
}

func TestApplyAccountProfileEmptyPathNoOp(t *testing.T) {
	// configPath == "" must not panic, must not write anything, must return nil.
	if err := applyAccountProfile("", &store.Account{Email: "x@example.com"}); err != nil {
		t.Fatalf("empty path should be a no-op, got %v", err)
	}
}

func TestSnapshotConfigProfileMissingFile(t *testing.T) {
	dir := t.TempDir()
	snap, err := snapshotConfigProfile(filepath.Join(dir, ".claude.json"))
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap == nil || snap.HadOAuthAccount || snap.HadOnboarding {
		t.Fatalf("missing file should snapshot as empty, got %#v", snap)
	}
}

// TestConfigProfileRoundTripPresent: user had a real login; foxy overwrites it;
// restore puts the user's original oauthAccount + onboarding back verbatim,
// while unrelated fields survive throughout.
func TestConfigProfileRoundTripPresent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	original := map[string]any{
		"primaryApiKey":          "sk-keep",
		"oauthAccount":           map[string]any{"emailAddress": "original@example.com", "accountUuid": "orig-uuid"},
		"hasCompletedOnboarding": true,
	}
	if err := writeConfig(path, original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	snap, err := snapshotConfigProfile(path)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.HadOAuthAccount || !snap.HadOnboarding {
		t.Fatalf("expected both fields captured, got %#v", snap)
	}

	// foxy switches in a different account.
	if err := applyAccountProfile(path, &store.Account{Email: "foxy@example.com", AccountUUID: "foxy-uuid"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := readJSON(t, path)[oauthAccountKey].(map[string]any)["emailAddress"]; got != "foxy@example.com" {
		t.Fatalf("after inject expected foxy email, got %#v", got)
	}

	// Shutdown restore.
	if err := restoreConfigProfile(path, snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	cfg := readJSON(t, path)
	if cfg["primaryApiKey"] != "sk-keep" {
		t.Errorf("unrelated field clobbered: %#v", cfg["primaryApiKey"])
	}
	acct := cfg[oauthAccountKey].(map[string]any)
	if acct["emailAddress"] != "original@example.com" || acct["accountUuid"] != "orig-uuid" {
		t.Errorf("oauthAccount not restored to original: %#v", acct)
	}
	if cfg[onboardingKey] != true {
		t.Errorf("onboarding not restored: %#v", cfg[onboardingKey])
	}
}

// TestConfigProfileRoundTripAbsent: user had no oauthAccount / onboarding (or
// no file at all); foxy adds them; restore deletes the foxy-added keys so the
// config returns to its key-less state.
func TestConfigProfileRoundTripAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")

	// Snapshot the not-yet-existing file (foxy is about to create it).
	snap, err := snapshotConfigProfile(path)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if err := applyAccountProfile(path, &store.Account{Email: "foxy@example.com"}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := readJSON(t, path)[oauthAccountKey]; !ok {
		t.Fatalf("foxy should have added oauthAccount")
	}

	if err := restoreConfigProfile(path, snap); err != nil {
		t.Fatalf("restore: %v", err)
	}
	cfg := readJSON(t, path)
	if _, ok := cfg[oauthAccountKey]; ok {
		t.Errorf("foxy-added oauthAccount should be deleted on restore: %#v", cfg[oauthAccountKey])
	}
	if _, ok := cfg[onboardingKey]; ok {
		t.Errorf("foxy-added onboarding should be deleted on restore: %#v", cfg[onboardingKey])
	}
}

func TestRestoreConfigProfileNilNoOp(t *testing.T) {
	// Legacy backup (no ConfigProfile) must not panic or error.
	if err := restoreConfigProfile("/nonexistent/.claude.json", nil); err != nil {
		t.Fatalf("nil snapshot should no-op, got %v", err)
	}
}
