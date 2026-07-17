package openai

import (
	"errors"
	"testing"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

func eligibleCodex(id int64, lastUsed int64) store.Account {
	return store.Account{
		ID: id, Provider: store.ProviderCodex, Status: store.StatusActive,
		AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		LastUsedAt: lastUsed, FiveHourThreshold: 95, SevenDayThreshold: 95, SevenDaySonnetThreshold: 95,
	}
}

func TestChooseStickyCodexKeepsEligibleCurrent(t *testing.T) {
	accounts := []store.Account{eligibleCodex(1, 200), eligibleCodex(2, 100)}
	got, err := chooseStickyCodex(accounts, 1, "device", true, time.Now())
	if err != nil || got == nil || got.ID != 1 {
		t.Fatalf("choose = %+v, %v", got, err)
	}
}

func TestChooseStickyCodexHonorsDevicePin(t *testing.T) {
	accounts := []store.Account{eligibleCodex(1, 200), eligibleCodex(2, 100)}
	accounts[1].PinnedDeviceID = "device"
	got, err := chooseStickyCodex(accounts, 1, "device", true, time.Now())
	if err != nil || got == nil || got.ID != 2 {
		t.Fatalf("choose = %+v, %v", got, err)
	}
}

func TestChooseStickyCodexManualDoesNotSpontaneouslyPick(t *testing.T) {
	accounts := []store.Account{eligibleCodex(1, 100)}
	got, err := chooseStickyCodex(accounts, 0, "device", false, time.Now())
	if got != nil || !errors.Is(err, selector.ErrNoAvailable) {
		t.Fatalf("choose = %+v, %v", got, err)
	}
}
