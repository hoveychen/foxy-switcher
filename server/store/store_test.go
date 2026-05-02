package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTempStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestUpsertDistinctEmailsCoexist is the regression test for the bug where
// adding a second account silently overwrote the first. The original schema
// had UNIQUE(organization_uuid) and Upsert did ON CONFLICT(organization_uuid),
// but the OAuth profile API doesn't surface organization_uuid — so every new
// login landed with org="" and overwrote the previous "" row.
func TestUpsertDistinctEmailsCoexist(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "alice", Email: "alice@example.com", AccessToken: "at-a", RefreshToken: "rt-a", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert alice: %v", err)
	}
	b := &Account{Name: "bob", Email: "bob@example.com", AccessToken: "at-b", RefreshToken: "rt-b", ExpiresAt: 2}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert bob: %v", err)
	}
	if a.ID == 0 || b.ID == 0 || a.ID == b.ID {
		t.Fatalf("expected distinct ids, got a=%d b=%d", a.ID, b.ID)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts, got %d (%+v)", len(list), list)
	}
}

// TestUpsertSameEmailUpdatesTokens is the dedup half: re-logging in with the
// same email refreshes the row in place rather than creating a duplicate.
func TestUpsertSameEmailUpdatesTokens(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	first := &Account{Name: "alice", Email: "alice@example.com", AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: 1}
	if err := st.Upsert(ctx, first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	second := &Account{Name: "alice", Email: "alice@example.com", AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: 2}
	if err := st.Upsert(ctx, second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same id, got first=%d second=%d", first.ID, second.ID)
	}
	got, err := st.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AccessToken != "at-2" || got.RefreshToken != "rt-2" || got.ExpiresAt != 2 {
		t.Fatalf("tokens not refreshed: %+v", got)
	}
}

// TestOpenMigratesLegacyOrgUniqueSchema covers the upgrade path from a DB
// created by the old (buggy) schema with `UNIQUE (organization_uuid)`.
// After Open, that constraint must be gone and Upsert must accept multiple
// rows with empty organization_uuid.
func TestOpenMigratesLegacyOrgUniqueSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	const legacySchema = `
CREATE TABLE accounts (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  name              TEXT    NOT NULL,
  access_token      TEXT    NOT NULL,
  refresh_token     TEXT    NOT NULL,
  expires_at        INTEGER NOT NULL,
  scopes            TEXT    NOT NULL DEFAULT '',
  subscription_type TEXT    NOT NULL DEFAULT '',
  organization_uuid TEXT    NOT NULL DEFAULT '',
  status            TEXT    NOT NULL DEFAULT 'active',
  cooldown_until    INTEGER NOT NULL DEFAULT 0,
  last_used_at      INTEGER NOT NULL DEFAULT 0,
  last_429_at       INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL,
  UNIQUE (organization_uuid)
);
INSERT INTO accounts (name, access_token, refresh_token, expires_at, created_at, updated_at)
VALUES ('legacy', 'at-old', 'rt-old', 999, 1, 1);
`
	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := rawDB.Exec(legacySchema); err != nil {
		rawDB.Close()
		t.Fatalf("legacy schema: %v", err)
	}
	rawDB.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open after legacy schema: %v", err)
	}
	defer st.Close()

	// The pre-existing row must survive the rebuild.
	got, err := st.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if got.AccessToken != "at-old" {
		t.Fatalf("legacy row corrupted after migration: %+v", got)
	}

	// And new email-less inserts must coexist (the bug we are fixing).
	a := &Account{Name: "n2", AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: 2}
	if err := st.Upsert(context.Background(), a); err != nil {
		t.Fatalf("upsert post-migration: %v", err)
	}
	list, err := st.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected legacy + new = 2 rows, got %d", len(list))
	}
}

// TestUpsertEmptyEmailsCoexist covers the fallback path where the profile API
// returns no email. We must not collapse those into one row — they're distinct
// real users even if we couldn't label them.
func TestUpsertEmptyEmailsCoexist(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "Account 1", AccessToken: "at-a", RefreshToken: "rt-a", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	b := &Account{Name: "Account 2", AccessToken: "at-b", RefreshToken: "rt-b", ExpiresAt: 2}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert b: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("expected distinct ids for empty-email accounts, both=%d", a.ID)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(list))
	}
}

// TestMigrateDisabledToPaused covers the rename of the off-state status
// literal. Existing databases on disk store rows with status='disabled'; on
// upgrade those must become status='paused' so SetStatus and the UI no longer
// see the legacy value.
func TestMigrateDisabledToPaused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy_status.db")

	rawDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := rawDB.Exec(tableSchema); err != nil {
		rawDB.Close()
		t.Fatalf("apply schema: %v", err)
	}
	if _, err := rawDB.Exec(
		`INSERT INTO accounts (name, access_token, refresh_token, expires_at, status, created_at, updated_at)
		 VALUES ('legacy-paused', 'at', 'rt', 1, 'disabled', 1, 1)`); err != nil {
		rawDB.Close()
		t.Fatalf("insert legacy paused row: %v", err)
	}
	rawDB.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open after legacy data: %v", err)
	}
	defer st.Close()

	got, err := st.Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("get legacy row: %v", err)
	}
	if got.Status != "paused" {
		t.Fatalf("legacy status not migrated: got %q want %q", got.Status, "paused")
	}
}

// TestThresholdsDefaultAndSet covers the per-account utilization-threshold
// surface: a freshly-inserted row gets 100 on every window (i.e. "do not
// skip"), and SetThresholds writes the supplied values back, clamped to
// [0, 100].
func TestThresholdsDefaultAndSet(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "alice", Email: "alice@example.com", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FiveHourThreshold != 95 || got.SevenDayThreshold != 95 || got.SevenDaySonnetThreshold != 95 {
		t.Fatalf("expected default thresholds = 95, got %v / %v / %v",
			got.FiveHourThreshold, got.SevenDayThreshold, got.SevenDaySonnetThreshold)
	}

	if err := st.SetThresholds(ctx, a.ID, 95, 80, 150); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}
	got, err = st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after set: %v", err)
	}
	if got.FiveHourThreshold != 95 {
		t.Errorf("FiveHourThreshold: got %v want 95", got.FiveHourThreshold)
	}
	if got.SevenDayThreshold != 80 {
		t.Errorf("SevenDayThreshold: got %v want 80", got.SevenDayThreshold)
	}
	if got.SevenDaySonnetThreshold != 100 { // clamped from 150
		t.Errorf("SevenDaySonnetThreshold: got %v want 100 (clamped)", got.SevenDaySonnetThreshold)
	}

	if err := st.SetThresholds(ctx, a.ID, -10, 0, 50); err != nil {
		t.Fatalf("SetThresholds (negative): %v", err)
	}
	got, err = st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after negative: %v", err)
	}
	if got.FiveHourThreshold != 0 { // clamped from -10
		t.Errorf("FiveHourThreshold: got %v want 0 (clamped)", got.FiveHourThreshold)
	}
}

// TestAutoSwitchDefaultsAndRoundTrip covers the kv-backed auto-switch knob:
// a fresh DB returns the daemon defaults (enabled, lru), and SetAutoSwitch
// then GetAutoSwitch round-trips the toggle so the credinject coordinator
// can read it back on the next reconcile tick.
func TestAutoSwitchDefaultsAndRoundTrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	got, err := st.GetAutoSwitch(ctx)
	if err != nil {
		t.Fatalf("GetAutoSwitch (defaults): %v", err)
	}
	if !got.Enabled || got.Policy != "lru" {
		t.Fatalf("expected defaults {true, lru}, got %+v", got)
	}

	if err := st.SetAutoSwitch(ctx, AutoSwitch{Enabled: false, Policy: "lowest"}); err != nil {
		t.Fatalf("SetAutoSwitch: %v", err)
	}
	got, err = st.GetAutoSwitch(ctx)
	if err != nil {
		t.Fatalf("GetAutoSwitch (after set): %v", err)
	}
	if got.Enabled || got.Policy != "lowest" {
		t.Fatalf("round-trip: got %+v, want {false, lowest}", got)
	}

	// Overwrite confirms the upsert path (INSERT ON CONFLICT) doesn't append a
	// second row; subsequent reads should reflect only the latest write.
	if err := st.SetAutoSwitch(ctx, AutoSwitch{Enabled: true, Policy: "rr"}); err != nil {
		t.Fatalf("SetAutoSwitch (overwrite): %v", err)
	}
	got, err = st.GetAutoSwitch(ctx)
	if err != nil {
		t.Fatalf("GetAutoSwitch (after overwrite): %v", err)
	}
	if !got.Enabled || got.Policy != "rr" {
		t.Fatalf("overwrite: got %+v, want {true, rr}", got)
	}
}

// TestSettingsDefaultsAndRoundTrip covers the kv-backed user-prefs blob:
// fresh DB returns DefaultSettings; Set persists clamped values; partial
// objects merge into defaults rather than zeroing every untouched field.
func TestSettingsDefaultsAndRoundTrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	got, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings (defaults): %v", err)
	}
	if got != DefaultSettings {
		t.Fatalf("defaults: got %+v want %+v", got, DefaultSettings)
	}

	in := Settings{
		Theme:                    "dark",
		SidebarMode:              "expanded",
		UsagePollIntervalSec:     5, // below min, must clamp to 30
		CooldownThresholdPercent: 250, // above max, must clamp to 100
		RestoreNativeOnQuit:      false,
	}
	out, err := st.SetSettings(ctx, in)
	if err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if out.UsagePollIntervalSec != 30 {
		t.Fatalf("expected interval clamped to 30, got %d", out.UsagePollIntervalSec)
	}
	if out.CooldownThresholdPercent != 100 {
		t.Fatalf("expected threshold clamped to 100, got %v", out.CooldownThresholdPercent)
	}
	if out.RestoreNativeOnQuit {
		t.Fatalf("RestoreNativeOnQuit should be false")
	}

	// Round-trip the persisted blob.
	got, err = st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings (after set): %v", err)
	}
	if got != out {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, out)
	}
}

// TestUpsertDefaultsLastUsedAtToNow guards the credinject sticky-selection
// invariant: only MarkForNextPick should produce last_used_at = 0. New
// accounts inserted with the zero default would otherwise be indistinguishable
// from manually pinned ones, causing the daemon to rotate through every fresh
// account on first start.
func TestUpsertDefaultsLastUsedAtToNow(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	before := time.Now().UnixMilli()
	a := &Account{Name: "alice", Email: "alice@example.com", AccessToken: "at-a", RefreshToken: "rt-a", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastUsedAt < before {
		t.Errorf("LastUsedAt not stamped on insert: got %d, want >= %d", got.LastUsedAt, before)
	}
}
