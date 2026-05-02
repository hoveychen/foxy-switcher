package vault

import (
	"context"
	"time"

	"github.com/hoveychen/foxy-switcher/server/selector"
	"github.com/hoveychen/foxy-switcher/server/store"
)

// InProc is the in-process implementation of Service. It delegates straight
// to store + selector with no caching or wrapping — combined-mode callers
// pay the same cost they did before the boundary existed.
type InProc struct {
	st *store.Store
}

// NewInProc returns a Service backed directly by the given store.
func NewInProc(st *store.Store) *InProc {
	return &InProc{st: st}
}

// Compile-time assertion that InProc satisfies Service.
var _ Service = (*InProc)(nil)

func (s *InProc) ListAccounts(ctx context.Context) ([]Account, error) {
	return s.st.List(ctx)
}

func (s *InProc) GetAutoSwitch(ctx context.Context) (AutoSwitch, error) {
	return s.st.GetAutoSwitch(ctx)
}

func (s *InProc) Pick(ctx context.Context, now time.Time) (*Account, error) {
	return selector.Pick(ctx, s.st, now)
}

func (s *InProc) MarkUsed(ctx context.Context, accountID int64) error {
	return s.st.MarkUsed(ctx, accountID)
}

func (s *InProc) UpdateTokens(ctx context.Context, accountID int64, accessToken, refreshToken string, expiresAt int64) error {
	return s.st.UpdateTokens(ctx, accountID, accessToken, refreshToken, expiresAt)
}
