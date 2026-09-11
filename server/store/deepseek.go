package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// deepseek.go holds the one table the DeepSeek provider needs on top of the
// shared `accounts` row.
//
// The split mirrors OpenRouter's and for the same reason: the agent-facing
// GET /agent/v1/accounts serialises store.Account verbatim, so anything left on
// the row is visible to every paired device. A DeepSeek accounts row therefore
// carries no secret at all — access_token / refresh_token stay empty and
// credential_json is unused — and the key lives here.
//
// What DeepSeek does NOT have, and OpenRouter does:
//
//   - No key-minting API. Keys are created by hand in the DeepSeek console, so
//     there is nothing to derive per device and no device_deepseek_keys table.
//     Every authorised device is served the account's own key, which is exactly
//     OpenRouter's non-provisioning branch. The cost — revoking one device cannot
//     revoke the key — is stated in the grant and in the admin UI.
//   - No per-key model allowlist or spend cap. The `deepseek-official` route in
//     the harness serves a fixed catalogue, so there is no policy template to
//     store and no config document on the account row.
//
// There are no lease rows for DeepSeek either: it bills per token, so parallel
// use of one key across devices is harmless and the LRU/lease machinery would
// only make devices fight each other. See ProviderDeepSeek.

const deepseekSchema = `
CREATE TABLE IF NOT EXISTS deepseek_credentials (
  account_id     INTEGER PRIMARY KEY,
  -- api_key is the sk-… key the admin pasted from the DeepSeek console. It is
  -- served to authorised devices as-is; there is no provisioning tier to detect.
  api_key        TEXT    NOT NULL,
  -- Balance state, refreshed by the balance poller from GET /user/balance.
  -- balance_available is DeepSeek's own is_available boolean — its answer to
  -- "can this key still serve requests" — which is why there is no local
  -- currency-dependent floor to guess at. amount/currency are for display.
  balance_available  INTEGER NOT NULL DEFAULT 0,
  balance_amount     REAL    NOT NULL DEFAULT 0,
  balance_currency   TEXT    NOT NULL DEFAULT '',
  -- 0 means "never polled", which is treated as usable: a never-polled or
  -- poll-failing account must not be silently locked out of its own pool.
  balance_checked_at INTEGER NOT NULL DEFAULT 0,
  updated_at     INTEGER NOT NULL
);
`

// DeepSeekCredential is an account's stored API key plus what the last balance
// poll saw.
type DeepSeekCredential struct {
	AccountID int64
	// APIKey is the console-issued key. Served to every authorised device.
	APIKey string
	// BalanceAvailable is DeepSeek's is_available from GET /user/balance.
	BalanceAvailable bool
	// BalanceAmount / BalanceCurrency are the primary balance_infos entry
	// (total_balance and its currency), kept for display only.
	BalanceAmount   float64
	BalanceCurrency string
	// BalanceCheckedAt is when the poller last succeeded (unix millis). 0 = never.
	BalanceCheckedAt int64
	UpdatedAt        int64
}

// HasBalance reports whether the account looks fundable.
//
// A never-polled account counts as fundable, for the same reason OpenRouter's
// does: the poller can fail for reasons that have nothing to do with the
// balance (offline vault, a network blip), and treating "we don't know" as "no
// money" would lock an operator out of their own pool over a failed HTTP call.
//
// When we HAVE polled, the answer is DeepSeek's own is_available flag rather
// than a local threshold on the amount. The amount arrives in whatever currency
// the account bills in (CNY or USD), so any floor we picked would be wrong for
// one of them — and the provider already publishes the boolean we would be
// trying to reconstruct.
func (c DeepSeekCredential) HasBalance() bool {
	if c.BalanceCheckedAt == 0 {
		return true
	}
	return c.BalanceAvailable
}

// SetDeepSeekCredential stores (or replaces) an account's API key. Balance
// state resets on every write: a different key may be looking at a different
// DeepSeek account, so carrying the old figure over would report someone
// else's money.
//
// An empty key deletes the row — that is how an admin un-configures an account
// without deleting it.
func (s *Store) SetDeepSeekCredential(ctx context.Context, accountID int64, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return s.DeleteDeepSeekCredential(ctx, accountID)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deepseek_credentials
		   (account_id, api_key, balance_available, balance_amount,
		    balance_currency, balance_checked_at, updated_at)
		 VALUES (?, ?, 0, 0, '', 0, ?)
		 ON CONFLICT(account_id) DO UPDATE SET
		   api_key            = excluded.api_key,
		   balance_available  = 0,
		   balance_amount     = 0,
		   balance_currency   = '',
		   balance_checked_at = 0,
		   updated_at         = excluded.updated_at`,
		accountID, apiKey, time.Now().UnixMilli())
	return err
}

// SetDeepSeekBalance records a successful balance poll. ErrNotFound when the
// key was removed while the poll was in flight — the poller must not resurrect
// a credential row the admin just deleted.
func (s *Store) SetDeepSeekBalance(ctx context.Context, accountID int64, available bool, amount float64, currency string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deepseek_credentials
		    SET balance_available = ?, balance_amount = ?, balance_currency = ?,
		        balance_checked_at = ?
		  WHERE account_id = ?`,
		boolToInt(available), amount, currency, time.Now().UnixMilli(), accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeepSeekCredential returns the account's credential. ErrNotFound when the
// admin hasn't entered a key yet — callers surface that as "this account isn't
// usable" rather than attempting an unauthenticated call.
func (s *Store) DeepSeekCredential(ctx context.Context, accountID int64) (DeepSeekCredential, error) {
	c := DeepSeekCredential{AccountID: accountID}
	var available int
	err := s.db.QueryRowContext(ctx,
		`SELECT api_key, balance_available, balance_amount, balance_currency,
		        balance_checked_at, updated_at
		   FROM deepseek_credentials WHERE account_id = ?`, accountID).
		Scan(&c.APIKey, &available, &c.BalanceAmount, &c.BalanceCurrency,
			&c.BalanceCheckedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DeepSeekCredential{}, ErrNotFound
	}
	if err != nil {
		return DeepSeekCredential{}, err
	}
	c.BalanceAvailable = available != 0
	return c, nil
}

// HasDeepSeekCredential reports whether a key is on file. The admin UI renders
// "configured / not configured" off this without ever fetching the secret.
func (s *Store) HasDeepSeekCredential(ctx context.Context, accountID int64) (bool, error) {
	_, err := s.DeepSeekCredential(ctx, accountID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// DeleteDeepSeekCredential drops the stored key and its balance state.
// Idempotent.
func (s *Store) DeleteDeepSeekCredential(ctx context.Context, accountID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM deepseek_credentials WHERE account_id = ?`, accountID)
	return err
}
