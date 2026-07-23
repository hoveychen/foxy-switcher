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
// TestDeviceMetaRoundTrip exercises the device-meta migration end-to-end:
// InsertDevice writes hostname / OS / model / arch / app_version /
// client_type, and FindDeviceByTokenHash + ListDevices read every column
// back. Guards against accidental column-list drift between Insert and
// the two read paths.
func TestDeviceMetaRoundTrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	dev := Device{
		ID:         "dev-meta-1",
		Name:       "harry-mbp",
		TokenHash:  "hash-1",
		Hostname:   "Harrys-MacBook-Pro.local",
		OS:         "darwin",
		OSVersion:  "26.3.1",
		Arch:       "arm64",
		Model:      "Mac16,1",
		AppVersion: "v1.2.3",
		ClientType: "cli",
	}
	if err := st.InsertDevice(ctx, dev); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	got, err := st.FindDeviceByTokenHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindDeviceByTokenHash: %v", err)
	}
	if got.Hostname != dev.Hostname || got.OS != dev.OS ||
		got.OSVersion != dev.OSVersion || got.Arch != dev.Arch ||
		got.Model != dev.Model || got.AppVersion != dev.AppVersion ||
		got.ClientType != dev.ClientType {
		t.Fatalf("FindDeviceByTokenHash meta mismatch: got %+v want %+v", got, dev)
	}

	devs, err := st.ListDevices(ctx)
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devs) != 1 {
		t.Fatalf("ListDevices len: got %d want 1", len(devs))
	}
	if devs[0].Hostname != dev.Hostname || devs[0].Model != dev.Model ||
		devs[0].ClientType != dev.ClientType {
		t.Fatalf("ListDevices meta mismatch: got %+v want %+v", devs[0], dev)
	}
}

// TestUpdateDeviceName covers the admin-rename path: changing a row's
// display name should be reflected in subsequent reads, unknown ids should
// surface as ErrNotFound (so the handler can 404), and empty input should
// be rejected at the store boundary.
func TestUpdateDeviceName(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	dev := Device{ID: "dev-1", Name: "old-name", TokenHash: "hash-1"}
	if err := st.InsertDevice(ctx, dev); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}

	if err := st.UpdateDeviceName(ctx, "dev-1", "new-name"); err != nil {
		t.Fatalf("UpdateDeviceName: %v", err)
	}
	got, err := st.FindDeviceByTokenHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("FindDeviceByTokenHash: %v", err)
	}
	if got.Name != "new-name" {
		t.Fatalf("name not updated: got %q want %q", got.Name, "new-name")
	}

	if err := st.UpdateDeviceName(ctx, "missing", "x"); err != ErrNotFound {
		t.Fatalf("unknown id: got err=%v want ErrNotFound", err)
	}
	if err := st.UpdateDeviceName(ctx, "", "x"); err == nil {
		t.Fatalf("empty id: expected error")
	}
	if err := st.UpdateDeviceName(ctx, "dev-1", ""); err == nil {
		t.Fatalf("empty name: expected error")
	}
}

// TestPairingMetaPropagates verifies that meta supplied at InsertPairing
// time survives until FindPairingByNonce reads it back — the path
// handlePairPoll uses to copy meta from pairings into devices when a
// device is approved.
func TestPairingMetaPropagates(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	p := Pairing{
		ClientNonce: "nonce-x",
		UserCode:    "ABCD1234",
		DeviceName:  "harry-mbp",
		ExpiresAt:   time.Now().Add(time.Hour).UnixMilli(),
		Hostname:    "Harrys-MacBook-Pro.local",
		OS:          "darwin",
		OSVersion:   "26.3.1",
		Arch:        "arm64",
		Model:       "Mac16,1",
		AppVersion:  "v1.2.3",
		ClientType:  "desktop",
	}
	if err := st.InsertPairing(ctx, p); err != nil {
		t.Fatalf("InsertPairing: %v", err)
	}
	got, err := st.FindPairingByNonce(ctx, "nonce-x")
	if err != nil {
		t.Fatalf("FindPairingByNonce: %v", err)
	}
	if got.Hostname != p.Hostname || got.Model != p.Model ||
		got.ClientType != p.ClientType {
		t.Fatalf("Pairing meta mismatch: got %+v want %+v", got, p)
	}
}

// TestSetUsageScopedLabelRoundTrip guards the seven_day_scoped_label column
// end-to-end: SetUsage writes it and List reads it back. Catches column-list
// drift between the UPDATE, the SELECT column list, and scanAccounts — the same
// failure mode TestDeviceMetaRoundTrip guards for devices.
func TestSetUsageScopedLabelRoundTrip(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	a := &Account{Name: "fable-user", Email: "f@example.com", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.SetUsage(ctx, a.ID,
		12.5, "2026-05-01T00:00:00Z",
		78.0, "2026-05-07T00:00:00Z",
		42.0, "2026-05-07T12:00:00Z",
		"Fable",
	); err != nil {
		t.Fatalf("SetUsage: %v", err)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 account, got %d", len(list))
	}
	got := list[0]
	if got.SevenDaySonnetUtil != 42.0 || got.SevenDaySonnetResetsAt != "2026-05-07T12:00:00Z" {
		t.Errorf("scoped window = %v @ %q, want 42.0 @ 2026-05-07T12:00:00Z", got.SevenDaySonnetUtil, got.SevenDaySonnetResetsAt)
	}
	if got.SevenDayScopedLabel != "Fable" {
		t.Errorf("SevenDayScopedLabel = %q, want %q", got.SevenDayScopedLabel, "Fable")
	}
}

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

// TestUpsertSameUUIDDifferentEmailMerges covers the bug where re-logging in
// with the same Anthropic account but a different surfaced email (alias
// swap, primary-email migration on Anthropic's side) created a duplicate row
// instead of refreshing the original. The dedup key must be the stable
// `account.uuid`, not email.
func TestUpsertSameUUIDDifferentEmailMerges(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	first := &Account{
		Name: "alice", Email: "alice@old.com", AccountUUID: "uuid-1",
		AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: 1,
	}
	if err := st.Upsert(ctx, first); err != nil {
		t.Fatalf("upsert first: %v", err)
	}
	second := &Account{
		Name: "alice", Email: "alice@new.com", AccountUUID: "uuid-1",
		AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: 2,
	}
	if err := st.Upsert(ctx, second); err != nil {
		t.Fatalf("upsert second: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same id (uuid dedup), got first=%d second=%d", first.ID, second.ID)
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 row after uuid-keyed merge, got %d (%+v)", len(list), list)
	}
	got := list[0]
	if got.AccessToken != "at-2" || got.RefreshToken != "rt-2" || got.Email != "alice@new.com" {
		t.Fatalf("merge did not refresh tokens/email: %+v", got)
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

// TestUpsertNewAccountSeedsThresholdsFromSettings proves that a brand-new
// account inherits the pool-wide per-window default thresholds configured in
// Settings, rather than the bare schema default of 95. Before this fix the
// INSERT never referenced the settings, so the "default threshold" knob was
// inert — every new account silently got 95 regardless of what the operator
// configured.
func TestUpsertNewAccountSeedsThresholdsFromSettings(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	if _, err := st.SetSettings(ctx, Settings{
		UsagePollIntervalSec:           60,
		DefaultFiveHourThreshold:       80,
		DefaultSevenDayThreshold:       70,
		DefaultSevenDaySonnetThreshold: 60,
		RestoreNativeOnQuit:            true,
	}); err != nil {
		t.Fatalf("SetSettings: %v", err)
	}

	a := &Account{Name: "bob", Email: "bob@example.com", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.FiveHourThreshold != 80 || got.SevenDayThreshold != 70 || got.SevenDaySonnetThreshold != 60 {
		t.Fatalf("new account should inherit configured default thresholds 80/70/60, got %v/%v/%v",
			got.FiveHourThreshold, got.SevenDayThreshold, got.SevenDaySonnetThreshold)
	}
}

// TestApplyThresholdsToAll proves the bulk-apply path overwrites every
// account's per-window thresholds with the supplied values (clamped),
// including accounts that had been manually tuned — the operator's "apply to
// all accounts" action is an intentional, indiscriminate overwrite.
func TestApplyThresholdsToAll(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	mk := func(name, email string) int64 {
		a := &Account{Name: name, Email: email, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
		if err := st.Upsert(ctx, a); err != nil {
			t.Fatalf("upsert %s: %v", name, err)
		}
		return a.ID
	}
	id1 := mk("a", "a@example.com")
	id2 := mk("b", "b@example.com")
	// Manually tune id2 so we can prove the bulk apply steamrolls it.
	if err := st.SetThresholds(ctx, id2, 30, 40, 50); err != nil {
		t.Fatalf("SetThresholds: %v", err)
	}

	n, err := st.ApplyThresholdsToAll(ctx, 88, 77, 150) // 150 clamps to 100
	if err != nil {
		t.Fatalf("ApplyThresholdsToAll: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 accounts updated, got %d", n)
	}
	for _, id := range []int64{id1, id2} {
		got, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %d: %v", id, err)
		}
		if got.FiveHourThreshold != 88 || got.SevenDayThreshold != 77 || got.SevenDaySonnetThreshold != 100 {
			t.Fatalf("account %d not overwritten: got %v/%v/%v want 88/77/100",
				id, got.FiveHourThreshold, got.SevenDayThreshold, got.SevenDaySonnetThreshold)
		}
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
		Theme:                          "dark",
		SidebarMode:                    "expanded",
		UsagePollIntervalSec:           5,   // below min, must clamp to 30
		DefaultFiveHourThreshold:       250, // above max, must clamp to 100
		DefaultSevenDayThreshold:       80,
		DefaultSevenDaySonnetThreshold: 60,
		RestoreNativeOnQuit:            false,
	}
	out, err := st.SetSettings(ctx, in)
	if err != nil {
		t.Fatalf("SetSettings: %v", err)
	}
	if out.UsagePollIntervalSec != 30 {
		t.Fatalf("expected interval clamped to 30, got %d", out.UsagePollIntervalSec)
	}
	if out.DefaultFiveHourThreshold != 100 {
		t.Fatalf("expected five-hour threshold clamped to 100, got %v", out.DefaultFiveHourThreshold)
	}
	if out.DefaultSevenDayThreshold != 80 || out.DefaultSevenDaySonnetThreshold != 60 {
		t.Fatalf("per-window defaults not preserved: 7d=%v 7d-sonnet=%v",
			out.DefaultSevenDayThreshold, out.DefaultSevenDaySonnetThreshold)
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

// TestSettingsLegacyCooldownThresholdJSON proves that a kv blob persisted
// before the cooldown→threshold rename still surfaces its threshold via the
// new field name. The on-disk JSON shipped with key
// `cooldown_threshold_percent`; old installs that already wrote a non-default
// value must not silently revert to 95% just because the Go field was
// renamed. Reason: forced reset would change rate-limit behaviour for users
// who had deliberately tuned the threshold.
func TestSettingsLegacyCooldownThresholdJSON(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	legacy := `{"theme":"dark","sidebar_mode":"auto","usage_poll_interval_sec":60,"cooldown_threshold_percent":67,"restore_native_on_quit":true}`
	_, err := st.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingsKey, legacy, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("seed legacy kv: %v", err)
	}

	got, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.DefaultFiveHourThreshold != 67 || got.DefaultSevenDayThreshold != 67 || got.DefaultSevenDaySonnetThreshold != 67 {
		t.Fatalf("legacy cooldown_threshold_percent should lift into all three per-window defaults: 5h=%v 7d=%v 7d-sonnet=%v",
			got.DefaultFiveHourThreshold, got.DefaultSevenDayThreshold, got.DefaultSevenDaySonnetThreshold)
	}
}

// TestSettingsLegacySingleDefaultThresholdJSON proves that a kv blob written
// after the cooldown→default rename but before the per-window split — i.e. one
// carrying a single `default_threshold_percent` — lifts that value into all
// three per-window defaults. Without this, installs that tuned the single
// default would silently revert to 95 across every window on upgrade.
func TestSettingsLegacySingleDefaultThresholdJSON(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	legacy := `{"theme":"dark","sidebar_mode":"auto","usage_poll_interval_sec":60,"default_threshold_percent":82,"restore_native_on_quit":true}`
	_, err := st.db.ExecContext(ctx,
		`INSERT INTO kv (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		settingsKey, legacy, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("seed legacy kv: %v", err)
	}

	got, err := st.GetSettings(ctx)
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if got.DefaultFiveHourThreshold != 82 || got.DefaultSevenDayThreshold != 82 || got.DefaultSevenDaySonnetThreshold != 82 {
		t.Fatalf("legacy default_threshold_percent should lift into all three per-window defaults: 5h=%v 7d=%v 7d-sonnet=%v",
			got.DefaultFiveHourThreshold, got.DefaultSevenDayThreshold, got.DefaultSevenDaySonnetThreshold)
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

// TestFirstActiveLease covers the vault-mode dashboard fallback: with at
// least one live lease, the helper must return its account_id; with no
// live leases, it must report not-found. Two leases with different
// acquired_at timestamps assert the longest-held one wins so the
// dashboard doesn't flap between concurrent agents.
func TestFirstActiveLease(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	id, found, err := st.FirstActiveLease(ctx)
	if err != nil {
		t.Fatalf("FirstActiveLease (empty): %v", err)
	}
	if found || id != 0 {
		t.Fatalf("empty store: got id=%d found=%v want 0,false", id, found)
	}

	a := &Account{Name: "alice", AccessToken: "at-a", RefreshToken: "rt-a", ExpiresAt: 1}
	if err := st.Upsert(ctx, a); err != nil {
		t.Fatalf("upsert a: %v", err)
	}
	b := &Account{Name: "bob", AccessToken: "at-b", RefreshToken: "rt-b", ExpiresAt: 1}
	if err := st.Upsert(ctx, b); err != nil {
		t.Fatalf("upsert b: %v", err)
	}

	if _, err := st.AcquireLease(ctx, "lease-b", b.ID, "device-old", time.Minute); err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := st.AcquireLease(ctx, "lease-a", a.ID, "device-new", time.Minute); err != nil {
		t.Fatalf("acquire a: %v", err)
	}

	id, found, err = st.FirstActiveLease(ctx)
	if err != nil {
		t.Fatalf("FirstActiveLease: %v", err)
	}
	if !found {
		t.Fatal("expected found=true with two live leases")
	}
	if id != b.ID {
		t.Fatalf("expected longest-held lease (bob, id=%d); got id=%d", b.ID, id)
	}
}

func TestProviderDefaultsAndDedupAreProviderScoped(t *testing.T) {
	st := openTempStore(t)
	ctx := context.Background()

	claude := &Account{Name: "claude", AccountUUID: "user-1", Email: "same@example.com", AccessToken: "cat", RefreshToken: "crt", ExpiresAt: 1}
	if err := st.Upsert(ctx, claude); err != nil {
		t.Fatalf("upsert legacy/default provider: %v", err)
	}
	if claude.Provider != ProviderClaude {
		t.Fatalf("default provider = %q, want %q", claude.Provider, ProviderClaude)
	}

	codex := &Account{Provider: ProviderCodex, Name: "codex", AccountUUID: "user-1", Email: "same@example.com", AccessToken: "oat", RefreshToken: "ort", ExpiresAt: 1}
	if err := st.Upsert(ctx, codex); err != nil {
		t.Fatalf("upsert codex with same identity: %v", err)
	}
	if codex.ID == claude.ID {
		t.Fatalf("provider-scoped identities collapsed onto id %d", codex.ID)
	}

	claudeRows, err := st.ListProvider(ctx, ProviderClaude)
	if err != nil || len(claudeRows) != 1 || claudeRows[0].ID != claude.ID {
		t.Fatalf("ListProvider claude = %+v, %v", claudeRows, err)
	}
	codexRows, err := st.ListProvider(ctx, ProviderCodex)
	if err != nil || len(codexRows) != 1 || codexRows[0].ID != codex.ID {
		t.Fatalf("ListProvider codex = %+v, %v", codexRows, err)
	}
}
