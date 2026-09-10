package vault

// deepseek.go carries the DeepSeek boundary type. The grant service
// (DeepSeekGrants) is vault-internal and lives alongside it; only this data
// shape crosses to the agent.
//
// Note what is deliberately absent, exactly as for OpenRouter: DeepSeek appears
// nowhere on Service. A remote agent can ask "what is MY key" (the vault reads
// its device id from the bearer token) but has no vocabulary for pick / acquire
// / renew / release, because a pay-as-you-go key isn't a scarce resource to be
// leased. See store.ProviderDeepSeek.

// DeepSeekGrant is everything a device needs to configure the DeepSeek Harness:
// the key to authenticate with and the endpoint to send it to.
type DeepSeekGrant struct {
	// AccountID is the accounts row the key came from. Lets the agent notice
	// "the vault rolled me onto a different DeepSeek account" and rewrite its
	// credential file rather than assuming one account forever.
	AccountID int64 `json:"account_id"`
	// AccountName is the human label shown in the desktop / TUI.
	AccountName string `json:"account_name,omitempty"`
	// APIKey is the account's console-issued sk-… key, served as-is.
	//
	// Every authorised device gets the same key. That is not a shortcut: the
	// DeepSeek platform mints keys only from its web console, so there is
	// nothing per-device to derive. The consequence — revoking one device does
	// not revoke this key — reaches the UI through the account view rather than
	// being implied away here.
	APIKey string `json:"api_key"`
	// BaseURL is the DeepSeek endpoint the harness should post to. Carried on
	// the wire rather than hardcoded on the device so a gateway deployment can
	// be switched vault-side.
	BaseURL string `json:"base_url"`
}

// DefaultDeepSeekBaseURL is the public DeepSeek API root, and the default the
// harness's `deepseek-official` route already uses.
const DefaultDeepSeekBaseURL = "https://api.deepseek.com"
