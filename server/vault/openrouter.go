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
// the runtime key to authenticate with, and the models to offer. Both come from
// the same account-level template, so one admin edit moves them together.
type OpenRouterGrant struct {
	// AccountID is the accounts row the key was derived from. Lets the agent
	// notice "the admin pointed me at a different OpenRouter account" and rewrite
	// its config rather than assuming one account forever.
	AccountID int64 `json:"account_id"`
	// AccountName is the human label shown in the desktop / TUI.
	AccountName string `json:"account_name,omitempty"`
	// APIKey is the derived sk-or-… runtime key. Scoped to this device, carrying
	// its own spend cap and usage counters, and revocable on its own without
	// touching any other device.
	APIKey string `json:"api_key"`
	// BaseURL is the OpenRouter endpoint to point codex at. Carried on the wire
	// (rather than hardcoded on the device) so a self-hosted or regional
	// deployment can be switched vault-side.
	BaseURL string `json:"base_url"`
	// AllowedModels are the OpenRouter model slugs to expose to this device — one
	// profile file each, which is what makes them selectable. Ordered
	// deterministically by the store so an unchanged list produces byte-identical
	// config and the writer stays idempotent.
	//
	// Client-side visibility, not enforcement: the key's spend cap is what bounds
	// cost. See openrouter.Client's package doc for why the guardrail that used to
	// enforce this was dropped.
	AllowedModels []string `json:"allowed_models"`
	// ExpiresAt is the key's upstream expiry (unix millis); 0 = never.
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// DefaultOpenRouterBaseURL is the public OpenRouter API root. Used when the
// account doesn't override it.
const DefaultOpenRouterBaseURL = "https://openrouter.ai/api/v1"
