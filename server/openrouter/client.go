// Package openrouter is a thin client for OpenRouter's *management* API — the
// endpoints that mint and revoke runtime API keys. It is used only by the
// vault; devices never see a management key.
//
// Deriving a device key is one call. A key carries everything the per-device
// model needs on its own:
//
//	limit / limit_reset                       per-device spend cap
//	usage, usage_daily / weekly / monthly     per-device usage tracking
//	DELETE /keys/{hash}                       per-device revocation
//
// An earlier version also created a Guardrail per device to restrict which
// models the key could call. That was dropped: the spend cap already bounds the
// money, so enforcement only changed how much work the capped dollars bought,
// while costing an extra upstream object per device, an extra derivation step,
// and an extra failure mode. The model list still reaches the device — it drives
// the codex profile files, and therefore what appears in the model picker.
//
// WIRE SHAPES VERIFIED against the live API on 2026-07-31 with a real
// provisioning key:
//
//   - `POST /keys` returns 201 (not 200) with
//     {"data":{"hash":…}, "key":"sk-or-…"} — plaintext at the top level, and
//     returned exactly once.
//   - A `limit` with no `limit_reset` is accepted: that's a lifetime cap.
//   - `DELETE /keys/{hash}` returns 200 {"deleted":true}; a repeat returns 404,
//     so idempotent revocation is safe.
//   - `GET /key` (singular) is self-introspection: it reports
//     `is_provisioning_key` / `is_management_key` plus this key's own `limit`,
//     `limit_remaining` and `usage*`. That flag is how a pasted key's kind is
//     detected rather than declared.
//   - `GET /credits` reports account-level `total_credits` / `total_usage`;
//     remaining is the difference. This is the "out of money" signal.
//
// Response decoding stays lenient about data-envelope vs flat placement of the
// key secret and hash, since only the observed shape is proven.
package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL is the public OpenRouter API root.
const DefaultBaseURL = "https://openrouter.ai/api/v1"

// defaultTimeout bounds a single management call. Kept short because an agent
// is blocked on derivation during its first authorisation.
const defaultTimeout = 20 * time.Second

// ErrUnauthorized means the credential we sent isn't a valid management key.
// OpenRouter answers a plain inference key with 401 "Invalid management key",
// which is the single most likely misconfiguration: an admin pastes the sk-or-
// key they use for chat instead of a provisioning key.
var ErrUnauthorized = errors.New("openrouter: not a valid management key")

// APIError is any other non-2xx response, carrying enough to diagnose without
// re-running the call.
type APIError struct {
	Op      string
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("openrouter: %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("openrouter: %s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// Client talks to one OpenRouter account's management API.
type Client struct {
	// BaseURL defaults to DefaultBaseURL. Overridden by tests and by
	// self-hosted / regional deployments.
	BaseURL string
	// ManagementKey authenticates every call in this package. Never leaves the
	// vault process.
	ManagementKey string
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

// do issues one management call. `out` may be nil for responses we don't read.
func (c *Client) do(ctx context.Context, op, method, path string, body, out any) error {
	if strings.TrimSpace(c.ManagementKey) == "" {
		return fmt.Errorf("openrouter: %s: no management key configured", op)
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("openrouter: %s: encode body: %w", op, err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL()+path, reader)
	if err != nil {
		return fmt.Errorf("openrouter: %s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.ManagementKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("openrouter: %s: %w", op, err)
	}
	defer resp.Body.Close()
	// Management responses are small; a cap keeps a misrouted HTML error page
	// from being slurped whole into the error message.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := errorMessage(raw)
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("%w (%s)", ErrUnauthorized, msg)
		}
		return &APIError{Op: op, Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("openrouter: %s: decode response: %w", op, err)
	}
	return nil
}

// errorMessage digs a human-readable string out of an error body without
// assuming a single shape: OpenRouter returns {"error":{"message":…}} for some
// failures and {"error":"…"} for others, and a proxy in front of it may return
// neither.
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

// --- keys -----------------------------------------------------------------

// KeySpec is a runtime key request.
type KeySpec struct {
	Name string `json:"name"`
	// LimitUSD is the key's spend cap — the mechanism that actually bounds what
	// a device can cost. 0 = uncapped.
	LimitUSD float64 `json:"limit,omitempty"`
	// LimitReset is "daily"/"weekly"/"monthly". Empty is valid and means a
	// lifetime cap (verified against the live API).
	LimitReset string `json:"limit_reset,omitempty"`
	// ExpiresAt is an absolute expiry, RFC3339. Zero omits it (never expires).
	ExpiresAt time.Time `json:"-"`
	// WorkspaceID scopes the key; empty = the management key's default.
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// MarshalJSON renders ExpiresAt as RFC3339 only when set, so a zero time
// doesn't become "0001-01-01T00:00:00Z" and expire the key on creation.
func (s KeySpec) MarshalJSON() ([]byte, error) {
	type plain KeySpec // avoid recursing into this method
	out := map[string]any{}
	raw, err := json.Marshal(plain(s))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if !s.ExpiresAt.IsZero() {
		out["expires_at"] = s.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return json.Marshal(out)
}

// Key is a freshly minted runtime key. Secret is returned by OpenRouter exactly
// once, at creation — losing it means the only way to serve the device again is
// to rotate, so the caller must persist it.
type Key struct {
	// Hash is OpenRouter's handle for the key and the only way to revoke it.
	Hash string
	// Secret is the sk-or-… bearer token.
	Secret string
	// ExpiresAt is unix millis; 0 = never.
	ExpiresAt int64
}

// keyResponse tolerates both placements of the plaintext key and the hash.
type keyResponse struct {
	Key  string `json:"key"`
	Hash string `json:"hash"`
	Data struct {
		Key       string `json:"key"`
		Hash      string `json:"hash"`
		ExpiresAt string `json:"expires_at"`
	} `json:"data"`
	ExpiresAt string `json:"expires_at"`
}

// CreateKey mints a runtime key. This is the whole of derivation.
func (c *Client) CreateKey(ctx context.Context, spec KeySpec) (Key, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return Key{}, errors.New("openrouter: key name required")
	}
	var resp keyResponse
	if err := c.do(ctx, "create key", http.MethodPost, "/keys", spec, &resp); err != nil {
		return Key{}, err
	}
	out := Key{
		Hash:   firstNonEmpty(resp.Data.Hash, resp.Hash),
		Secret: firstNonEmpty(resp.Key, resp.Data.Key),
	}
	if out.Hash == "" {
		// Without the hash we could never revoke this key. Refuse to hand back a
		// key we can't kill — better to fail derivation loudly.
		return Key{}, errors.New("openrouter: create key returned no hash (key would be unrevocable)")
	}
	if out.Secret == "" {
		return Key{}, errors.New("openrouter: create key returned no key secret")
	}
	if ts := firstNonEmpty(resp.Data.ExpiresAt, resp.ExpiresAt); ts != "" {
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			out.ExpiresAt = t.UnixMilli()
		}
	}
	return out, nil
}

// DeleteKey revokes a runtime key. Effective immediately and scoped to that one
// key, which is what makes per-device revocation cheap. A 404 is success — the
// key is already gone, which is all the caller wanted.
func (c *Client) DeleteKey(ctx context.Context, hash string) error {
	if strings.TrimSpace(hash) == "" {
		return nil
	}
	err := c.do(ctx, "delete key", http.MethodDelete, "/keys/"+hash, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// --- derivation -----------------------------------------------------------

// DeriveSpec is one device's key request, assembled from the account template.
type DeriveSpec struct {
	// KeyName is the upstream key label, conventionally "foxy-<device-id>", so an
	// operator reading OpenRouter's own key list can tell which machine a key
	// belongs to without consulting the vault.
	KeyName     string
	LimitUSD    float64
	LimitReset  string
	WorkspaceID string
	ExpiresAt   time.Time
}

// DeriveDeviceKey mints one device's runtime key.
//
// Deliberately a thin wrapper over CreateKey rather than the multi-step,
// self-unwinding sequence it used to be: with guardrails gone there is only one
// upstream object, so there is no partial state to clean up on failure.
func (c *Client) DeriveDeviceKey(ctx context.Context, spec DeriveSpec) (Key, error) {
	return c.CreateKey(ctx, KeySpec{
		Name:        spec.KeyName,
		LimitUSD:    spec.LimitUSD,
		LimitReset:  spec.LimitReset,
		ExpiresAt:   spec.ExpiresAt,
		WorkspaceID: spec.WorkspaceID,
	})
}

// RevokeDerivedKey kills a derived key.
func (c *Client) RevokeDerivedKey(ctx context.Context, keyHash string) error {
	return c.DeleteKey(ctx, keyHash)
}

// --- key introspection ----------------------------------------------------

// KeyInfo is what a key says about itself via GET /key. Used for two things:
// deciding whether a pasted key can mint per-device keys, and reading that one
// key's own remaining allowance.
type KeyInfo struct {
	// IsProvisioning means this key can create and delete other keys. That, not
	// anything the admin declares, is what decides whether foxy derives a
	// per-device key or hands this one out directly.
	IsProvisioning bool
	// Label is OpenRouter's redacted display form ("sk-or-v1-abc...xyz"). Safe to
	// show; it is not the key.
	Label string
	// Limit / LimitRemaining are this key's own cap. Nil when uncapped —
	// distinguished from zero, which would otherwise read as "exhausted".
	Limit          *float64
	LimitRemaining *float64
	// Usage is spend on this key so far, and the per-device tracking signal.
	Usage float64
}

// keyInfoResponse mirrors the observed {"data":{…}} envelope.
type keyInfoResponse struct {
	Data struct {
		Label             string   `json:"label"`
		IsProvisioningKey bool     `json:"is_provisioning_key"`
		IsManagementKey   bool     `json:"is_management_key"`
		Limit             *float64 `json:"limit"`
		LimitRemaining    *float64 `json:"limit_remaining"`
		Usage             float64  `json:"usage"`
	} `json:"data"`
}

// KeySelf reports what the configured key says about itself.
//
// This is the whole of key-kind detection, and it is why the admin never has to
// declare which sort of key they pasted: OpenRouter tells us. It is also
// read-only, so running it on every save costs nothing but a round trip.
func (c *Client) KeySelf(ctx context.Context) (KeyInfo, error) {
	var resp keyInfoResponse
	if err := c.do(ctx, "get key", http.MethodGet, "/key", nil, &resp); err != nil {
		return KeyInfo{}, err
	}
	return KeyInfo{
		// Either flag means it can mint: OpenRouter reports both for a
		// provisioning key, and accepting either avoids depending on which name
		// they settle on.
		IsProvisioning: resp.Data.IsProvisioningKey || resp.Data.IsManagementKey,
		Label:          resp.Data.Label,
		Limit:          resp.Data.Limit,
		LimitRemaining: resp.Data.LimitRemaining,
		Usage:          resp.Data.Usage,
	}, nil
}

// --- account balance ------------------------------------------------------

// Credits is the account-level balance — the "out of money" signal that decides
// whether an account should still be handed out.
type Credits struct {
	Total float64
	Used  float64
}

// Remaining is what's left to spend.
func (c Credits) Remaining() float64 { return c.Total - c.Used }

type creditsResponse struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

// AccountCredits reads the account's balance.
//
// Account-level rather than per-key on purpose: rotating between accounts is
// about which account still has money, and a per-key cap is a different
// question (answered by KeySelf.LimitRemaining).
func (c *Client) AccountCredits(ctx context.Context) (Credits, error) {
	var resp creditsResponse
	if err := c.do(ctx, "get credits", http.MethodGet, "/credits", nil, &resp); err != nil {
		return Credits{}, err
	}
	return Credits{Total: resp.Data.TotalCredits, Used: resp.Data.TotalUsage}, nil
}

// --- key probe ------------------------------------------------------------

// Capabilities is what CheckManagementKey found out about a credential.
type Capabilities struct {
	// KeyValid is false when the credential isn't accepted by OpenRouter at all.
	KeyValid bool
	// ManagementKeyValid means it can additionally mint per-device keys.
	ManagementKeyValid bool
	// CreditTotal / CreditRemaining are the account balance, when readable.
	CreditTotal     float64
	CreditRemaining float64
	// CreditKnown distinguishes "balance is zero" from "couldn't read it".
	CreditKnown bool
	// Detail is a human-readable note for the admin UI.
	Detail string
}

// CheckManagementKey verifies the configured key works and reports what it can
// do — mint per-device keys or not — plus the account balance when readable.
//
// Read-only throughout: GET /key and GET /credits. Nothing is created on the
// operator's account, and neither call can fail for a reason unrelated to what
// is being asked.
func (c *Client) CheckManagementKey(ctx context.Context) (Capabilities, error) {
	info, err := c.KeySelf(ctx)
	if errors.Is(err, ErrUnauthorized) {
		return Capabilities{Detail: "OpenRouter rejected this key"}, nil
	}
	if err != nil {
		return Capabilities{}, err
	}
	caps := Capabilities{KeyValid: true, ManagementKeyValid: info.IsProvisioning}
	if info.IsProvisioning {
		caps.Detail = "provisioning key — each authorised device gets its own key, " +
			"revocable on its own"
	} else {
		caps.Detail = "ordinary API key — every authorised device shares it, so revoking " +
			"one device cannot revoke this key. Fine for a single machine; paste a " +
			"provisioning key to get per-device keys"
	}
	// A balance we can't read is reported as unknown rather than as an error: the
	// key itself is already known good, and that is the question being asked.
	if credits, cerr := c.AccountCredits(ctx); cerr == nil {
		caps.CreditTotal = credits.Total
		caps.CreditRemaining = credits.Remaining()
		caps.CreditKnown = true
		caps.Detail += fmt.Sprintf(" · $%.2f credit remaining", credits.Remaining())
	}
	return caps, nil
}

// --- helpers --------------------------------------------------------------

func isStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
