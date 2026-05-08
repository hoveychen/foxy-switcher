package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// TestRenderAccountRow_ForeignLeaseShowsDeviceBadge: a row whose lease is held
// by another device must render the "held by …" chip and must NOT render the
// "in use" chip — that chip is reserved for the LOCAL daemon's
// ManagedAccountID.
func TestRenderAccountRow_ForeignLeaseShowsDeviceBadge(t *testing.T) {
	now := time.Now().UnixMilli()
	a := Account{
		ID:     1,
		Name:   "alpha@example.com",
		Plan:   "max20",
		Status: "active",
		Lease: &AccountLease{
			DeviceID:   "dev-2",
			DeviceName: "laptop-2",
			Mine:       false,
			AcquiredAt: now - 60_000,
			ExpiresAt:  now + 12*60*1000,
		},
	}
	m := &model{width: 140, height: 40, accounts: []Account{a}}
	out := m.renderAccountRow(a, false, false, 60, now)
	if !strings.Contains(out, "held by laptop-2") {
		t.Errorf("foreign-lease row missing device badge: %q", out)
	}
	if strings.Contains(out, "in use") {
		t.Errorf("foreign-lease row must not carry the in-use chip: %q", out)
	}
}

// TestRenderAccountRow_OwnLeaseSuppressesForeignBadge: when Mine == true the
// "held by" chip is suppressed — Mine accounts read as locally-owned, the
// in-use chip (if applicable) is the right surface, not a foreign-lease badge.
func TestRenderAccountRow_OwnLeaseSuppressesForeignBadge(t *testing.T) {
	now := time.Now().UnixMilli()
	a := Account{
		ID:     1,
		Name:   "alpha@example.com",
		Status: "active",
		Lease: &AccountLease{
			DeviceID:   "dev-1",
			DeviceName: "this-mac",
			Mine:       true,
			ExpiresAt:  now + 5*60*1000,
		},
	}
	m := &model{width: 140, height: 40, accounts: []Account{a}}
	out := m.renderAccountRow(a, false, true, 60, now)
	if strings.Contains(out, "held by") {
		t.Errorf("own lease should not render foreign-lease chip: %q", out)
	}
}

// TestHandleListKey_USkipsForeignLeasedAccount: pressing 'u' on an account
// held by another device must NOT issue a SelectAccount call (which would
// 409 on leases_account_id_uniq) and must surface an inline error naming the
// holding device.
func TestHandleListKey_USkipsForeignLeasedAccount(t *testing.T) {
	m := &model{
		mode:   modeList,
		cursor: 0,
		accounts: []Account{
			{
				ID:   1,
				Name: "alpha",
				Lease: &AccountLease{
					DeviceID:   "dev-2",
					DeviceName: "laptop-2",
					Mine:       false,
					ExpiresAt:  time.Now().Add(10 * time.Minute).UnixMilli(),
				},
			},
		},
	}
	_, cmd := m.handleListKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	if cmd != nil {
		t.Fatalf("'u' on foreign-leased account must not issue a cmd; got non-nil")
	}
	if !strings.Contains(m.statusErr, "laptop-2") {
		t.Errorf("statusErr should name the holding device; got %q", m.statusErr)
	}
}
