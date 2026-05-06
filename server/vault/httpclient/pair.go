package httpclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PairInitResponse is what /agent/v1/devices/pair-init returns. Agents
// surface UserCode + VerificationURL to the user so they know where to
// approve the pairing.
type PairInitResponse struct {
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresInMillis int64  `json:"expires_in_ms"`
}

// PairResult holds the device credentials the vault hands back after
// approval. The agent persists DeviceToken (typically into
// ~/.foxy-switcher/agent-config.json) and uses it for every subsequent
// request via Client.SetToken.
type PairResult struct {
	DeviceID    string
	DeviceToken string
}

// Errors the pair-flow surfaces. The CLI subcommand turns these into
// human messages; programmatic callers can errors.Is them.
var (
	ErrPairingDenied  = errors.New("pairing denied")
	ErrPairingExpired = errors.New("pairing expired before approval")
)

// PairInit kicks off the device flow. The agent supplies a deviceName
// (shown to the human approving the pairing) and a clientNonce (any
// random string — the agent's own random id is fine; nothing trusts it
// for security, it only correlates init↔poll on the vault side).
func (c *Client) PairInit(ctx context.Context, deviceName, clientNonce string) (*PairInitResponse, error) {
	body := map[string]any{
		"client_nonce": clientNonce,
		"device_name":  deviceName,
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/agent/v1/devices/pair-init", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, decodeError(resp)
	}
	var out PairInitResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PairStatus is the four-state device-flow outcome the vault reports.
// Exposed so non-blocking callers (the local daemon HTTP proxy) can
// surface the status field verbatim to their own clients.
type PairStatus string

const (
	PairPending  PairStatus = "pending"
	PairApproved PairStatus = "approved"
	PairDenied   PairStatus = "denied"
	PairExpired  PairStatus = "expired"
)

// PairPollOnce performs a single pair-poll request and returns the raw
// status. When the status is PairApproved the *PairResult is populated
// AND the token is installed via SetToken; otherwise the result is nil.
// PairDenied / PairExpired return their sentinel errors as well so
// existing CLI callers can errors.Is them; PairPending returns
// (PairPending, nil, nil).
func (c *Client) PairPollOnce(ctx context.Context, clientNonce string) (PairStatus, *PairResult, error) {
	body, _ := json.Marshal(map[string]string{"client_nonce": clientNonce})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/agent/v1/devices/pair-poll", bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", nil, err
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("pair-poll: %s: %s", resp.Status, string(raw))
	}
	var out struct {
		Status      string `json:"status"`
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", nil, fmt.Errorf("pair-poll decode: %w", err)
	}
	switch PairStatus(out.Status) {
	case PairApproved:
		c.SetToken(out.DeviceToken)
		return PairApproved, &PairResult{DeviceID: out.DeviceID, DeviceToken: out.DeviceToken}, nil
	case PairDenied:
		return PairDenied, nil, ErrPairingDenied
	case PairExpired:
		return PairExpired, nil, ErrPairingExpired
	case PairPending:
		return PairPending, nil, nil
	default:
		return "", nil, fmt.Errorf("pair-poll: unknown status %q", out.Status)
	}
}

// PairPoll runs the device-flow polling loop. It blocks until the user
// approves / denies the pairing, or until ctx fires (e.g. the user hit
// Ctrl-C in the CLI). pollInterval defaults to 2 seconds; pass 0 for
// the default.
//
// On success, the returned token is also installed via SetToken so
// callers can immediately make authenticated requests.
func (c *Client) PairPoll(ctx context.Context, clientNonce string, pollInterval time.Duration) (*PairResult, error) {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	for {
		status, res, err := c.PairPollOnce(ctx, clientNonce)
		if err != nil {
			return nil, err
		}
		if status == PairApproved {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}
