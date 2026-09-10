// Package deepseek is a thin client for the one DeepSeek platform endpoint the
// vault needs: GET /user/balance.
//
// It is deliberately small, and the reason is worth stating because it is the
// main difference from the openrouter package next door. DeepSeek issues API
// keys only from its web console — there is no key-minting endpoint — so the
// vault has nothing to derive per device and nothing to revoke upstream. What
// it can do is check that a pasted key works and notice when an account runs
// dry, and both of those are the same call.
//
// WIRE SHAPE (https://api-docs.deepseek.com/api/get-user-balance):
//
//	GET /user/balance      Authorization: Bearer sk-…
//	200 {"is_available":true,
//	     "balance_infos":[{"currency":"CNY",
//	                       "total_balance":"110.00",
//	                       "granted_balance":"10.00",
//	                       "topped_up_balance":"100.00"}]}
//
// Two details drive the code below. The amounts are JSON *strings*, not
// numbers. And `is_available` is DeepSeek's own answer to "can this key still
// serve requests" — which is why nothing here compares the amount against a
// threshold: an account may bill in CNY or in USD, so any floor we invented
// would be wrong for one of them.
package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the public DeepSeek API root. It is the same root the
// harness's `deepseek-official` route posts completions to.
const DefaultBaseURL = "https://api.deepseek.com"

// defaultTimeout bounds a single call. Short: an admin is watching a spinner
// while their pasted key is checked.
const defaultTimeout = 20 * time.Second

// ErrUnauthorized means the key we sent isn't valid. This is the one failure an
// admin actually causes — a typo, or a key that was deleted in the console — so
// it gets its own error rather than being buried in an APIError.
var ErrUnauthorized = errors.New("deepseek: invalid API key")

// APIError is any other non-2xx response.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("deepseek: %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("deepseek: %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// Client talks to one DeepSeek account.
type Client struct {
	// BaseURL defaults to DefaultBaseURL. Overridden by tests.
	BaseURL string
	// APIKey is the console-issued sk-… key.
	APIKey string
	// HTTP defaults to a client with defaultTimeout.
	HTTP *http.Client
}

func (c *Client) baseURL() string {
	if strings.TrimSpace(c.BaseURL) == "" {
		return DefaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

// Balance is what one poll learned about an account.
type Balance struct {
	// Available is DeepSeek's is_available: whether the key can still serve
	// requests. This, not Amount, is the eligibility signal.
	Available bool
	// Amount / Currency are the primary balance_infos entry, for display.
	// Currency is "CNY" or "USD" depending on how the account bills.
	Amount   float64
	Currency string
}

type balanceResp struct {
	IsAvailable  bool `json:"is_available"`
	BalanceInfos []struct {
		Currency string `json:"currency"`
		// Amounts arrive as strings ("110.00"), so they cannot be decoded into
		// a float64 field directly.
		TotalBalance string `json:"total_balance"`
	} `json:"balance_infos"`
}

// Balance reads the account's balance, and by doing so proves the key works.
// A 401 comes back as ErrUnauthorized so the admin API can answer "that key is
// not valid" rather than a generic failure.
//
// An empty balance_infos is not an error: the account is reachable and
// DeepSeek's is_available still answers the question that matters. The
// displayed amount is simply unknown.
func (c *Client) Balance(ctx context.Context) (Balance, error) {
	const op = "get balance"
	if strings.TrimSpace(c.APIKey) == "" {
		return Balance{}, fmt.Errorf("deepseek: %s: no API key configured", op)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL()+"/user/balance", nil)
	if err != nil {
		return Balance{}, fmt.Errorf("deepseek: %s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Balance{}, fmt.Errorf("deepseek: %s: %w", op, err)
	}
	defer resp.Body.Close()
	// Cap the read so a misrouted HTML error page can't be slurped whole into
	// an error message.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := errorMessage(raw)
		if resp.StatusCode == http.StatusUnauthorized {
			return Balance{}, fmt.Errorf("%w (%s)", ErrUnauthorized, msg)
		}
		return Balance{}, &APIError{Op: op, Status: resp.StatusCode, Message: msg}
	}
	var out balanceResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return Balance{}, fmt.Errorf("deepseek: %s: decode response: %w", op, err)
	}
	b := Balance{Available: out.IsAvailable}
	if len(out.BalanceInfos) > 0 {
		info := out.BalanceInfos[0]
		b.Currency = info.Currency
		// A total we can't parse must not fail the call: the availability flag
		// is what the pool acts on, and the amount is only ever displayed.
		if v, err := strconv.ParseFloat(strings.TrimSpace(info.TotalBalance), 64); err == nil {
			b.Amount = v
		}
	}
	return b, nil
}

// errorMessage digs a human-readable string out of an error body without
// assuming a single shape: DeepSeek returns {"error":{"message":…}} for API
// errors, and a proxy in front of it may return neither.
func errorMessage(raw []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && len(envelope.Error) > 0 {
		var s string
		if json.Unmarshal(envelope.Error, &s) == nil && s != "" {
			return s
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &obj) == nil && obj.Message != "" {
			return obj.Message
		}
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}
