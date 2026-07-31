package vault

// openrouter.go carries the OpenRouter boundary types. The derivation service
// itself (OpenRouterKeys) is vault-internal and lives alongside it; only these
// data shapes cross to the agent.
//
// Note what is deliberately absent: OpenRouter appears nowhere on Service. A
// remote agent can ask "what is MY key" (the vault reads its device id from the
// bearer token) but has no vocabulary for pick / acquire / renew / release,
// because a pay-as-you-go key isn't a scarce resource to be leased. See
// store.ProviderOpenRouter.

// OpenRouterGrant is everything a device needs to render its OpenRouter setup:
// the runtime key to authenticate with, and the models it may select. Both come
// from the same account-level template, so the profile files the device writes
// can never advertise a model the guardrail would reject.
type OpenRouterGrant struct {
	// AccountID is the accounts row the key was derived from. Lets the agent
	// notice "the admin pointed me at a different OpenRouter account" and rewrite
	// its config rather than assuming one account forever.
	AccountID int64 `json:"account_id"`
	// AccountName is the human label shown in the desktop / TUI.
	AccountName string `json:"account_name,omitempty"`
	// APIKey is the derived sk-or-… runtime key. Scoped to this device, capped by
	// the account's guardrail, and revocable on its own without touching any
	// other device.
	APIKey string `json:"api_key"`
	// BaseURL is the OpenRouter endpoint to point codex at. Carried on the wire
	// (rather than hardcoded on the device) so a self-hosted or regional
	// deployment can be switched vault-side.
	BaseURL string `json:"base_url"`
	// AllowedModels are the OpenRouter model slugs this device may use — one
	// profile file each. Ordered deterministically by the store so an unchanged
	// allowlist produces byte-identical config and the writer stays idempotent.
	AllowedModels []string `json:"allowed_models"`
	// ExpiresAt is the key's upstream expiry (unix millis); 0 = never.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// GuardrailEnforced reports whether AllowedModels is backed by a
	// server-side OpenRouter guardrail. False means the account's plan didn't
	// expose the guardrails API, so the allowlist is advisory (client-side
	// visibility only) and the spend cap is the sole hard limit. Surfaced so the
	// admin UI can say so out loud instead of implying enforcement that isn't
	// there.
	GuardrailEnforced bool `json:"guardrail_enforced"`
}

// DefaultOpenRouterBaseURL is the public OpenRouter API root. Used when the
// account doesn't override it.
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
