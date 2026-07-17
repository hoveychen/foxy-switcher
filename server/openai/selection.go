package openai

import (
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// chooseStickyCodex mirrors Claude's sticky/manual policy. A nil account with
// nil error means auto mode should ask the provider-aware LRU picker.
func chooseStickyCodex(accounts []store.Account, currentID int64, deviceID string, autoEnabled bool, now time.Time) (*store.Account, error) {
	var current, pinned *store.Account
	for i := range accounts {
		a := accounts[i]
		if a.Provider != store.ProviderCodex || !selector.IsEligible(a, now) {
			continue
		}
		if a.ID == currentID {
			copy := a
			current = &copy
		}
		if pinnedForDevice(a, deviceID) && (pinned == nil || a.ID < pinned.ID) {
			copy := a
			pinned = &copy
		}
	}
	if pinned != nil && pinned.ID != currentID {
		return pinned, nil
	}
	if current != nil {
		return current, nil
	}
	if !autoEnabled {
		return nil, selector.ErrNoAvailable
	}
	return nil, nil
}

func pinnedForDevice(a store.Account, deviceID string) bool {
	if a.PinnedDeviceID != "" {
		return a.PinnedDeviceID == deviceID
	}
	return a.LastUsedAt == 0
}
