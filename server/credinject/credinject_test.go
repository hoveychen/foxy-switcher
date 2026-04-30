package credinject

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hoveychen/foxy-switcher/server/store"
)

func TestBuildOAuthBlob_FieldShape(t *testing.T) {
	a := &store.Account{
		AccessToken:      "sk-ant-oat01-aaaa",
		RefreshToken:     "sk-ant-ort01-bbbb",
		ExpiresAt:        1700000000000,
		Scopes:           "user:inference user:profile",
		SubscriptionType: "max",
		OrganizationUUID: "11111111-2222-3333-4444-555555555555",
	}
	raw, err := buildOAuthBlob(a)
	if err != nil {
		t.Fatalf("buildOAuthBlob: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["organizationUuid"] != a.OrganizationUUID {
		t.Errorf("organizationUuid: got %q want %q", got["organizationUuid"], a.OrganizationUUID)
	}
	oauth, ok := got["claudeAiOauth"].(map[string]any)
	if !ok {
		t.Fatalf("claudeAiOauth missing or wrong type: %#v", got["claudeAiOauth"])
	}
	if oauth["accessToken"] != a.AccessToken {
		t.Errorf("accessToken mismatch")
	}
	if oauth["refreshToken"] != a.RefreshToken {
		t.Errorf("refreshToken mismatch")
	}
	// JSON deserialises numbers as float64; cast back for comparison.
	if int64(oauth["expiresAt"].(float64)) != a.ExpiresAt {
		t.Errorf("expiresAt mismatch: %v", oauth["expiresAt"])
	}
	if oauth["clientId"] != claudeCodeClientID {
		t.Errorf("clientId not pinned: got %q", oauth["clientId"])
	}
	if oauth["rateLimitTier"] != "default_claude_max_5x" {
		t.Errorf("rateLimitTier for max: got %q", oauth["rateLimitTier"])
	}
	if oauth["subscriptionType"] != "max" {
		t.Errorf("subscriptionType passthrough failed")
	}
	scopes, ok := oauth["scopes"].([]any)
	if !ok {
		t.Fatalf("scopes not an array: %#v", oauth["scopes"])
	}
	if len(scopes) != 2 || scopes[0] != "user:inference" || scopes[1] != "user:profile" {
		t.Errorf("scopes parse: got %v", scopes)
	}
}

func TestParseScopes(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{"user:inference", "user:profile"}},
		{"   ", []string{"user:inference", "user:profile"}},
		{"user:inference", []string{"user:inference"}},
		{"user:inference user:profile org:read", []string{"user:inference", "user:profile", "org:read"}},
		{"  user:inference   user:profile  ", []string{"user:inference", "user:profile"}},
	}
	for _, tc := range cases {
		if got := parseScopes(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseScopes(%q): got %v want %v", tc.in, got, tc.want)
		}
	}
}

func TestDeriveRateLimitTier(t *testing.T) {
	cases := map[string]string{
		"max":          "default_claude_max_5x",
		"team":         "default_claude_max_5x",
		"team_premium": "default_claude_max_20x",
		"pro":          "default_claude_pro",
		"":             "",
		"weird-value":  "",
	}
	for sub, want := range cases {
		if got := deriveRateLimitTier(sub); got != want {
			t.Errorf("deriveRateLimitTier(%q): got %q want %q", sub, got, want)
		}
	}
}

func TestExtractAccessToken(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-XYZ","refreshToken":"r"}}`)
	if got := extractAccessToken(blob); got != "sk-ant-oat01-XYZ" {
		t.Errorf("extractAccessToken: got %q", got)
	}
	if got := extractAccessToken([]byte("not json")); got != "" {
		t.Errorf("malformed input: expected empty, got %q", got)
	}
}

func TestExtractRotation(t *testing.T) {
	blob := []byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1700000000000}}`)
	at, rt, exp, ok := extractRotation(blob)
	if !ok {
		t.Fatalf("extractRotation: ok=false")
	}
	if at != "a" || rt != "r" || exp != 1700000000000 {
		t.Errorf("extractRotation: got (%q,%q,%d)", at, rt, exp)
	}
}
