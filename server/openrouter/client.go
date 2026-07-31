// Package openrouter is a thin client for OpenRouter's *management* API — the
// endpoints that mint, constrain, and revoke runtime API keys. It is used only
// by the vault; devices never see a management key.
//
// Why the derivation is three calls and not one: POST /api/v1/keys accepts
// name / limit / limit_reset / expires_at / include_byok_in_limit /
// workspace_id / creator_user_id and nothing else — there is no model
// allowlist on a key. Model restriction lives on a Guardrail, which is created
// separately and then assigned to the key. So a fully constrained device key is
// create-guardrail → create-key → assign, and a failure partway has to unwind
// (see DeriveDeviceKey).
//
// WIRE SHAPES VERIFIED against the live API on 2026-07-31 with a real
// provisioning key. Confirmed:
//
//   - Guardrails ARE available on a personal account — the design's open
//     question. `POST /guardrails` returned 201, so ErrGuardrailsUnavailable is
//     a fallback that has not been observed in practice.
//   - Creation returns 201 (not 200); `POST /keys` answers
//     {"data":{"hash":…}, "key":"sk-or-…"} with the plaintext at top level.
//   - Assignment wants {"key_hashes":[…]} — plural, an array.
//   - A guardrail with limit_usd MUST carry reset_interval; `/keys` does not
//     (a `limit` with no `limit_reset` is a lifetime cap on that key).
//   - allowed_models entries must be real model slugs; "openrouter/auto" is
//     rejected.
//   - DELETE returns 200 {"deleted":true}; a repeat returns 404.
//
// The first three of those were wrong in the original implementation and were
// only caught by making the calls — the assignment field name in particular
// would have made every derivation fail. live_contract_test.go pins each one.
// Response decoding stays lenient about the data-envelope vs flat placement of
// the key secret, since only the observed shape is proven.
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

// defaultTimeout bounds a single management call. Derivation makes three in
// sequence, so this is also roughly a third of the worst-case derive latency —
// kept short because an agent is blocked on it during first authorisation.
const defaultTimeout = 20 * time.Second

// ErrUnauthorized means the credential we sent isn't a valid management key.
// OpenRouter answers a plain inference key with 401 "Invalid management key",
// which is the single most likely misconfiguration: an admin pastes the sk-or-
// key they use for chat instead of a provisioning key.
var ErrUnauthorized = errors.New("openrouter: not a valid management key")

// ErrGuardrailsUnavailable means the account can create keys but not
// guardrails — the documented-but-unverified possibility that guardrails
// require a team/org plan. Callers may degrade to "spend cap only, model
// allowlist is advisory", but must surface that they did.
var ErrGuardrailsUnavailable = errors.New("openrouter: guardrails not available for this account")

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
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w (%s)", ErrUnauthorized, msg)
		case http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
			// Only the guardrail endpoints treat these as "feature absent";
			// mapping happens at the call site, which knows what it asked for.
			return &APIError{Op: op, Status: resp.StatusCode, Message: msg}
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
// assuming a single shape: OpenRouter has been seen to return
// {"error":{"message":…}} and {"error":"…"}, and a proxy in front of it may
// return neither.
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

// --- guardrails -----------------------------------------------------------

// GuardrailSpec is a server-side constraint set: which models a key may call
// and how much it may spend.
type GuardrailSpec struct {
	Name          string   `json:"name"`
	AllowedModels []string `json:"allowed_models,omitempty"`
	LimitUSD      float64  `json:"limit_usd,omitempty"`
	// ResetInterval is "daily" / "weekly" / "monthly"; empty = a lifetime cap.
	ResetInterval string `json:"reset_interval,omitempty"`
}

// validate rejects a spec OpenRouter would refuse, before spending a round trip
// on it. A budget limit with no reset interval is the one combination
// /guardrails rejects outright ("Reset interval is required when setting a
// budget limit"), and it is reachable from the UI, so the error names the field
// to fix rather than surfacing a raw Zod dump at first derivation.
//
// This is guardrail-specific: /keys happily accepts a `limit` with no
// `limit_reset`, which is a lifetime cap on that key (verified).
func (s GuardrailSpec) validate() error {
	if s.LimitUSD > 0 && strings.TrimSpace(s.ResetInterval) == "" {
		return errors.New("openrouter: a spend cap needs a reset interval " +
			"(daily/weekly/monthly) — OpenRouter has no lifetime budget window")
	}
	return nil
}

// idResponse decodes an id from either a bare object or a {"data":{…}}
// envelope. Both shapes appear across OpenRouter's management endpoints.
type idResponse struct {
	ID   string `json:"id"`
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

func (r idResponse) id() string {
	if r.Data.ID != "" {
		return r.Data.ID
	}
	return r.ID
}

// CreateGuardrail creates a constraint set and returns its id. A 402/403/404
// is reported as ErrGuardrailsUnavailable: those are how a plan that doesn't
// expose guardrails presents (payment required / forbidden / route absent), and
// the caller's fallback path is the same for all three.
func (c *Client) CreateGuardrail(ctx context.Context, spec GuardrailSpec) (string, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return "", errors.New("openrouter: guardrail name required")
	}
	if err := spec.validate(); err != nil {
		return "", err
	}
	var resp idResponse
	if err := c.do(ctx, "create guardrail", http.MethodPost, "/guardrails", spec, &resp); err != nil {
		if unavailable(err) {
			return "", fmt.Errorf("%w: %v", ErrGuardrailsUnavailable, err)
		}
		return "", err
	}
	id := resp.id()
	if id == "" {
		return "", errors.New("openrouter: create guardrail returned no id")
	}
	return id, nil
}

// DeleteGuardrail removes a constraint set. A 404 is success — the guardrail is
// already gone, which is all the caller wanted.
func (c *Client) DeleteGuardrail(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return nil
	}
	err := c.do(ctx, "delete guardrail", http.MethodDelete, "/guardrails/"+id, nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// AssignGuardrailToKey binds a constraint set to a runtime key. Until this
// lands the key is unconstrained by model, so callers must treat a failure here
// as fatal and unwind rather than shipping the key.
func (c *Client) AssignGuardrailToKey(ctx context.Context, guardrailID, keyHash string) error {
	if guardrailID == "" || keyHash == "" {
		return errors.New("openrouter: guardrail id and key hash required")
	}
	// key_hashes, plural and an array — verified against the live API. The
	// singular form returns 400 ZodError "expected array, received undefined".
	body := map[string]any{"key_hashes": []string{keyHash}}
	err := c.do(ctx, "assign guardrail", http.MethodPost,
		"/guardrails/"+guardrailID+"/assignments/keys", body, nil)
	if unavailable(err) {
		return fmt.Errorf("%w: %v", ErrGuardrailsUnavailable, err)
	}
	return err
}

// --- keys -----------------------------------------------------------------

// KeySpec is a runtime key request. Note the absence of any model field: that
// is exactly why guardrails exist.
type KeySpec struct {
	Name string `json:"name"`
	// LimitUSD is the key's own spend cap, independent of the guardrail's.
	// Belt and braces: if the guardrail is ever detached upstream, this still
	// bounds the damage.
	LimitUSD float64 `json:"limit,omitempty"`
	// LimitReset is "daily"/"weekly"/"monthly"; empty = lifetime cap.
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

// keyResponse tolerates both documented placements of the plaintext key
// (top-level "key" and data.key) and of the hash.
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

// CreateKey mints a runtime key.
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
// key, which is what makes per-device revocation cheap. A 404 is success.
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
	// KeyName is the upstream key label, conventionally "foxy-<device-id>".
	KeyName string
	// GuardrailName labels the constraint set; conventionally the same.
	GuardrailName string
	// AllowedModels is the model allowlist. Empty means "no model restriction
	// asked for", so no guardrail is created at all.
	AllowedModels []string
	LimitUSD      float64
	LimitReset    string
	WorkspaceID   string
	ExpiresAt     time.Time
	// AllowUnenforcedModels lets derivation proceed when the account can't
	// create guardrails, degrading the allowlist to client-side visibility only.
	// Off by default: silently handing out a key that can call any model is not
	// something to do behind the operator's back.
	AllowUnenforcedModels bool
}

// DerivedKey is the result of a successful derivation.
type DerivedKey struct {
	Key
	// GuardrailID is the constraint set bound to the key, or "" when none was
	// created (no allowlist requested, or the degraded path).
	GuardrailID string
	// GuardrailEnforced reports whether AllowedModels is enforced server-side.
	// False with a non-empty allowlist means the degraded path was taken and
	// the allowlist is advisory.
	GuardrailEnforced bool
}

// DeriveDeviceKey runs create-guardrail → create-key → assign and unwinds
// cleanly on partial failure, so a failed derivation never leaves a live key or
// an orphan guardrail behind.
//
// Failure handling is asymmetric on purpose:
//   - guardrails unsupported by the account (ErrGuardrailsUnavailable): this is
//     the documented degradation. Allowed only when AllowUnenforcedModels is
//     set AND a spend cap exists — an unenforced allowlist with no cap would be
//     a key that can spend anything on anything.
//   - assignment fails after the guardrail was created: fatal. The key exists
//     but is unconstrained, so we delete both and return the error rather than
//     shipping a key that looks restricted and isn't.
func (c *Client) DeriveDeviceKey(ctx context.Context, spec DeriveSpec) (DerivedKey, error) {
	// Validate before touching upstream: a refusal partway through would mean
	// unwinding state we never needed to create. Only when a guardrail is
	// actually going to be created, though — verified that /keys accepts a
	// `limit` with no `limit_reset` (a lifetime cap on the key is fine); it is
	// specifically /guardrails that rejects that combination.
	if len(spec.AllowedModels) > 0 {
		if err := (GuardrailSpec{LimitUSD: spec.LimitUSD, ResetInterval: spec.LimitReset}).validate(); err != nil {
			return DerivedKey{}, err
		}
	}
	var guardrailID string
	enforced := false
	if len(spec.AllowedModels) > 0 {
		id, err := c.CreateGuardrail(ctx, GuardrailSpec{
			Name:          spec.GuardrailName,
			AllowedModels: spec.AllowedModels,
			LimitUSD:      spec.LimitUSD,
			ResetInterval: spec.LimitReset,
		})
		switch {
		case err == nil:
			guardrailID = id
			enforced = true
		case errors.Is(err, ErrGuardrailsUnavailable):
			if !spec.AllowUnenforcedModels {
				return DerivedKey{}, err
			}
			if spec.LimitUSD <= 0 {
				return DerivedKey{}, fmt.Errorf(
					"%w, and no spend cap is set — refusing to mint an unrestricted key; "+
						"set a per-key USD limit on the account first", err)
			}
		default:
			return DerivedKey{}, err
		}
	}

	key, err := c.CreateKey(ctx, KeySpec{
		Name:        spec.KeyName,
		LimitUSD:    spec.LimitUSD,
		LimitReset:  spec.LimitReset,
		ExpiresAt:   spec.ExpiresAt,
		WorkspaceID: spec.WorkspaceID,
	})
	if err != nil {
		// Don't leave an orphan guardrail behind; it would accumulate one per
		// failed authorisation attempt.
		if guardrailID != "" {
			_ = c.DeleteGuardrail(context.WithoutCancel(ctx), guardrailID)
		}
		return DerivedKey{}, err
	}

	if guardrailID != "" {
		if err := c.AssignGuardrailToKey(ctx, guardrailID, key.Hash); err != nil {
			// The key is live but unconstrained. Unwind both — a partially
			// applied restriction is worse than none, because the caller would
			// record GuardrailEnforced=true.
			cleanup := context.WithoutCancel(ctx)
			_ = c.DeleteKey(cleanup, key.Hash)
			_ = c.DeleteGuardrail(cleanup, guardrailID)
			return DerivedKey{}, fmt.Errorf("assign guardrail to new key: %w", err)
		}
	}
	return DerivedKey{Key: key, GuardrailID: guardrailID, GuardrailEnforced: enforced}, nil
}

// RevokeDerivedKey kills a derived key and its guardrail. Best-effort on the
// guardrail (it can only ever leak an unused constraint row) but strict on the
// key, since a surviving key is a credential that still works.
func (c *Client) RevokeDerivedKey(ctx context.Context, keyHash, guardrailID string) error {
	if err := c.DeleteKey(ctx, keyHash); err != nil {
		return err
	}
	if guardrailID != "" {
		if err := c.DeleteGuardrail(ctx, guardrailID); err != nil {
			return fmt.Errorf("key %s revoked but its guardrail %s survives: %w",
				keyHash, guardrailID, err)
		}
	}
	return nil
}

// --- capability probe -----------------------------------------------------

// Capabilities is what CheckCapabilities found out about an account.
type Capabilities struct {
	// ManagementKeyValid is false when the credential isn't a management key at
	// all (the usual "pasted the wrong key" case).
	ManagementKeyValid bool
	// GuardrailsAvailable answers the design's one open question: can this
	// account create guardrails, or does that need a team/org plan?
	GuardrailsAvailable bool
	// Detail is a human-readable note for the admin UI (why not, if not).
	Detail string
}

// CheckCapabilities probes the account by creating a throwaway guardrail and
// immediately deleting it. This is the design's blocking P0 verification, made
// runnable by an operator instead of a developer: point it at a real management
// key and it answers whether server-side model enforcement is actually
// available before anything depends on it.
//
// Creating and deleting is the only honest probe — a GET on /guardrails can
// succeed on a plan that still refuses writes. The throwaway is named
// distinctively and deleted in the same call; a leaked one is inert (assigned
// to no key).
func (c *Client) CheckCapabilities(ctx context.Context) (Capabilities, error) {
	// Name only. Verified: a name-only guardrail creates fine (201), whereas the
	// obvious richer probe fails for reasons that have nothing to do with
	// capability — "openrouter/auto" is rejected as an allowed_models entry, and
	// a limit_usd without a reset_interval is rejected too. Every extra field is
	// another way for the answer to come back as a spurious validation error.
	id, err := c.CreateGuardrail(ctx, GuardrailSpec{Name: "foxy-capability-probe"})
	switch {
	case err == nil:
		if delErr := c.DeleteGuardrail(ctx, id); delErr != nil {
			return Capabilities{ManagementKeyValid: true, GuardrailsAvailable: true,
				Detail: "guardrails work, but the probe guardrail could not be deleted: " + delErr.Error(),
			}, nil
		}
		return Capabilities{ManagementKeyValid: true, GuardrailsAvailable: true,
			Detail: "management key valid; guardrails available (server-side model allowlist enforced)"}, nil
	case errors.Is(err, ErrUnauthorized):
		return Capabilities{Detail: "not a valid management key — paste a provisioning key, not an inference key"}, nil
	case errors.Is(err, ErrGuardrailsUnavailable):
		return Capabilities{ManagementKeyValid: true,
			Detail: "management key valid, but guardrails are unavailable on this plan — " +
				"the model allowlist can only limit which profiles appear on devices; " +
				"spend caps remain the sole hard limit"}, nil
	default:
		return Capabilities{}, err
	}
}

// --- helpers --------------------------------------------------------------

// unavailable reports whether err is one of the statuses a missing guardrails
// feature presents as.
func unavailable(err error) bool {
	return isStatus(err, http.StatusPaymentRequired) ||
		isStatus(err, http.StatusForbidden) ||
		isStatus(err, http.StatusNotFound)
}

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
